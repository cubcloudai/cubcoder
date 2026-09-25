package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

// stubProvider returns a scripted sequence of responses, one per Chat call,
// and records the messages it was sent so tests can assert on history.
type stubProvider struct {
	responses []*provider.Response
	calls     int
	lastSent  []provider.Message
	window    int    // ContextWindow; 0 disables compaction
	summary   string // returned for the summarization call, if non-empty
}

func (s *stubProvider) Name() string       { return "stub" }
func (s *stubProvider) ContextWindow() int { return s.window }

func (s *stubProvider) Chat(_ context.Context, messages []provider.Message, _ []provider.ToolSchema, _ provider.StreamFunc) (*provider.Response, error) {
	// The summarization call is the one with the summary system prompt; answer
	// it from the configured summary (or fail, to exercise the skip path) so the
	// loop script isn't consumed by it.
	if len(messages) > 0 && messages[0].Content == summarySystemPrompt {
		if s.summary == "" {
			return nil, errors.New("summarize failed")
		}
		return &provider.Response{Content: s.summary}, nil
	}
	s.lastSent = messages
	r := s.responses[s.calls]
	s.calls++
	return r, nil
}

// A truncated write_file call (unterminated JSON, like a real token-limit cut
// off) must never be recorded into history, and the model must be nudged to
// retry. Recording it would 400 the next request when the gateway re-parses
// the broken arguments.
func TestTruncatedToolCallDoesNotPoisonHistory(t *testing.T) {
	stub := &stubProvider{responses: []*provider.Response{
		{
			Truncated: true,
			ToolCalls: []provider.ToolCall{{
				ID:   "call_1",
				Name: "write_file",
				Args: `{"content": "package convert\n\nimport (`, // cut off mid-string
			}},
		},
		{Content: "done"}, // clean final turn after the retry nudge
	}}

	a := New(stub, tools.New())
	if err := a.Run(context.Background(), "write a big file"); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	// Every assistant message sent back to the provider must carry only valid
	// (or empty) tool-call arguments — never the truncated string.
	for _, m := range stub.lastSent {
		for _, tc := range m.ToolCalls {
			if hasIncompleteToolCall([]provider.ToolCall{tc}) {
				t.Fatalf("history was poisoned with incomplete tool call args: %q", tc.Args)
			}
		}
	}

	// The model should have been nudged to retry rather than left stuck.
	var nudged bool
	for _, m := range stub.lastSent {
		if m.Role == "user" && strings.Contains(m.Content, "cut off at the output token limit") {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("expected a truncation nudge in the conversation, found none")
	}
}

// A turn with no finish_reason flag but invalid JSON args (server didn't report
// the cut off) must still be caught by the structural check.
func TestIncompleteArgsCaughtWithoutTruncatedFlag(t *testing.T) {
	if !hasIncompleteToolCall([]provider.ToolCall{{Name: "write_file", Args: `{"content": "oops`}}) {
		t.Fatal("expected incomplete JSON args to be detected")
	}
	if hasIncompleteToolCall([]provider.ToolCall{{Name: "list_dir", Args: ""}}) {
		t.Fatal("empty args (no-param tool) must be treated as valid")
	}
	if hasIncompleteToolCall([]provider.ToolCall{{Name: "read_file", Args: `{"path":"x.go"}`}}) {
		t.Fatal("complete JSON args must be treated as valid")
	}
}

// ---- compaction ----

func TestSplitUnits(t *testing.T) {
	msgs := []provider.Message{
		{Role: "user", Content: "u1"},
		{Role: "assistant", Content: "a1", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: "{}"}}},
		{Role: "tool", Content: "r1", ToolCallID: "c1"},
		{Role: "assistant", Content: "a1b"},
		{Role: "user", Content: "u2"},
		{Role: "assistant", Content: "a2"},
	}
	units := splitUnits(msgs)
	if len(units) != 5 {
		t.Fatalf("got %d units, want 5", len(units))
	}
	if units[0][0].Content != "u1" || units[3][0].Content != "u2" {
		t.Fatal("user messages must each form their own unit")
	}
	if len(units[1]) != 2 || units[1][1].Role != "tool" {
		t.Fatal("a tool result must stay in its assistant's unit")
	}
}

// sixRounds builds system + 6 rounds (user -> assistant+tool_call -> tool
// result), each moderately sized so the last 3 fit the retain budget.
func sixRounds() []provider.Message {
	msgs := []provider.Message{{Role: "system", Content: SystemPrompt}}
	for r := 0; r < 6; r++ {
		id := fmt.Sprintf("call_%d", r)
		pad := strings.Repeat("x", 200)
		msgs = append(msgs,
			provider.Message{Role: "user", Content: fmt.Sprintf("round %d question %s", r, pad)},
			provider.Message{Role: "assistant", Content: "working " + pad, ToolCalls: []provider.ToolCall{{ID: id, Name: "read_file", Args: `{"path":"x"}`}}},
			provider.Message{Role: "tool", Content: "result " + pad, ToolCallID: id, Name: "read_file"},
		)
	}
	return msgs
}

func TestCompactSummarizesOldRoundsKeepsRecent(t *testing.T) {
	a := New(&stubProvider{window: 4000, summary: "SUMMARY-TEXT"}, tools.New())
	a.messages = sixRounds()

	a.compact(context.Background(), 4000)

	if a.messages[0].Role != "system" || a.messages[0].Content != SystemPrompt {
		t.Fatal("system prompt must be preserved verbatim at index 0")
	}
	if a.messages[1].Role != "user" || !strings.Contains(a.messages[1].Content, "SUMMARY-TEXT") {
		t.Fatalf("summary must splice into the first retained user turn; got %q", a.messages[1].Content)
	}
	for _, r := range []int{3, 4, 5} { // recent rounds retained
		if !containsMsg(a.messages, fmt.Sprintf("round %d question", r)) {
			t.Fatalf("recent round %d should be retained", r)
		}
	}
	for _, r := range []int{0, 1, 2} { // old rounds summarized away
		if containsMsg(a.messages, fmt.Sprintf("round %d question", r)) {
			t.Fatalf("old round %d should have been summarized away", r)
		}
	}
	assertToolPairing(t, a.messages)
}

func TestCompactSkipsWhenSummarizeFails(t *testing.T) {
	a := New(&stubProvider{window: 4000, summary: ""}, tools.New()) // summarize returns error
	a.messages = sixRounds()
	before := len(a.messages)

	a.compact(context.Background(), 4000)

	if len(a.messages) != before {
		t.Fatalf("history must be untouched on summarize failure: %d -> %d", before, len(a.messages))
	}
}

func TestMaybeCompactNoopBelowThreshold(t *testing.T) {
	a := New(&stubProvider{window: 1_000_000}, tools.New()) // huge window, tiny convo
	a.messages = []provider.Message{{Role: "system", Content: SystemPrompt}, {Role: "user", Content: "hi"}}
	before := len(a.messages)

	a.maybeCompact(context.Background(), nil)

	if len(a.messages) != before {
		t.Fatal("must not compact when well below the threshold")
	}
}

// A single agentic task — one user request driving many tool iterations with
// no further user messages — must still compact. This is the session-killer the
// round-based cutter could not handle: with one round, it gave up and the
// window filled until the pre-flight check refused to send.
func TestCompactInsideSingleUserTurn(t *testing.T) {
	a := New(&stubProvider{window: 4000, summary: "SUMMARY-TEXT"}, tools.New())
	msgs := []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "build the todo app"},
	}
	pad := strings.Repeat("x", 400)
	for i := 0; i < 10; i++ {
		id := fmt.Sprintf("call_%d", i)
		msgs = append(msgs,
			provider.Message{Role: "assistant", Content: fmt.Sprintf("step %d %s", i, pad), ToolCalls: []provider.ToolCall{{ID: id, Name: "run_command", Args: `{"command":"ls"}`}}},
			provider.Message{Role: "tool", Content: fmt.Sprintf("result %d %s", i, pad), ToolCallID: id, Name: "run_command"},
		)
	}
	a.messages = msgs

	a.compact(context.Background(), 4000)

	if len(a.messages) >= len(msgs) {
		t.Fatalf("mid-task history must shrink: %d -> %d messages", len(msgs), len(a.messages))
	}
	if a.messages[0].Role != "system" || a.messages[0].Content != SystemPrompt {
		t.Fatal("system prompt must be preserved verbatim at index 0")
	}
	if a.messages[1].Role != "user" || !strings.Contains(a.messages[1].Content, "SUMMARY-TEXT") {
		t.Fatalf("summary must ride in a user message at index 1; got role=%s", a.messages[1].Role)
	}
	if !containsMsg(a.messages, "step 9") {
		t.Fatal("the most recent step must be retained verbatim")
	}
	if containsMsg(a.messages, "step 0 ") {
		t.Fatal("the oldest step should have been summarized away")
	}
	assertToolPairing(t, a.messages)
}

func TestCapToolResult(t *testing.T) {
	if got := capToolResult("short output"); got != "short output" {
		t.Fatalf("small results must pass through untouched, got %q", got)
	}
	huge := strings.Repeat("rm 'backend/node_modules/some/package/file.js'\n", 5000) // ~235KB
	capped := capToolResult(huge)
	if len(capped) > toolResultCapChars+200 { // marker text overhead
		t.Fatalf("capped result is %d bytes, want ≈%d", len(capped), toolResultCapChars)
	}
	if !strings.Contains(capped, "truncated") {
		t.Fatal("capped result must carry the truncation marker")
	}
	if !strings.HasSuffix(capped, "rm 'backend/node_modules/some/package/file.js'\n") {
		t.Fatal("the tail of the output must be preserved")
	}
	// Multi-byte content must not be split mid-rune.
	if capped := capToolResult(strings.Repeat("日本語テキスト", 10000)); !utf8.ValidString(capped) {
		t.Fatal("capping must respect UTF-8 rune boundaries")
	}
}

func containsMsg(msgs []provider.Message, sub string) bool {
	for _, m := range msgs {
		if strings.Contains(m.Content, sub) {
			return true
		}
	}
	return false
}

// assertToolPairing fails if any tool result lacks a preceding tool_call with a
// matching id — the invariant both wire formats require.
func assertToolPairing(t *testing.T, msgs []provider.Message) {
	t.Helper()
	seen := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == "tool" && m.ToolCallID != "" && !seen[m.ToolCallID] {
			t.Fatalf("orphaned tool result %q with no preceding tool_call", m.ToolCallID)
		}
	}
}

// A tool call missing a required argument must not run the tool (or prompt);
// the model gets a specific correction it can retry from.
func TestInvalidToolArgsReturnsCorrectionWithoutRunning(t *testing.T) {
	ran := false
	reg := tools.New()
	reg.Register(&tools.Tool{
		Name:        "needs_path",
		Description: "test tool",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []string{"path"},
		},
		Run: func(context.Context, string) (string, error) { ran = true; return "ok", nil },
	})

	stub := &stubProvider{responses: []*provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "needs_path", Args: `{}`}}},
		{Content: "done"},
	}}
	a := New(stub, reg)

	var lastResult string
	a.OnToolResult = func(_, result string) { lastResult = result }

	if err := a.Run(context.Background(), "go"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ran {
		t.Error("tool ran despite missing required argument")
	}
	if !strings.Contains(lastResult, "invalid arguments") || !strings.Contains(lastResult, "path") {
		t.Errorf("correction message missing or unhelpful: %q", lastResult)
	}
}

// A staged attachment must ride inside the next user message (fenced and
// named), be consumed by that turn, and not leak into later turns.
func TestAttachmentRidesNextUserTurnOnce(t *testing.T) {
	stub := &stubProvider{responses: []*provider.Response{{Content: "ok"}, {Content: "ok2"}}}
	a := New(stub, tools.New())
	a.Attach("notes.md", "DOC-CONTENT-MARKER\n")

	if err := a.Run(context.Background(), "summarize the doc"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var userMsg string
	for _, m := range stub.lastSent {
		if m.Role == "user" {
			userMsg = m.Content
		}
	}
	for _, want := range []string{
		"--- attached document: notes.md ---",
		"DOC-CONTENT-MARKER",
		"--- end of document: notes.md ---",
		"summarize the doc",
	} {
		if !strings.Contains(userMsg, want) {
			t.Errorf("user message missing %q:\n%s", want, userMsg)
		}
	}
	if got := len(a.Attachments()); got != 0 {
		t.Errorf("attachments must be consumed by Run, %d still staged", got)
	}

	// The second turn's user message must not repeat the document.
	if err := a.Run(context.Background(), "now refactor"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	last := stub.lastSent[len(stub.lastSent)-1]
	if last.Role != "user" || strings.Contains(last.Content, "DOC-CONTENT-MARKER") {
		t.Errorf("attachment leaked into a later turn: %q", last.Content)
	}
}

// Staged attachments count toward the reported context usage before they are
// sent, so /status and the headroom warning see what's coming.
func TestContextUsageIncludesStagedAttachments(t *testing.T) {
	a := New(&stubProvider{window: 100000}, tools.New())
	before, _ := a.ContextUsage()
	a.Attach("big.txt", strings.Repeat("x", 30000))
	after, _ := a.ContextUsage()
	if after <= before+5000 {
		t.Errorf("usage must grow with a staged attachment: %d -> %d", before, after)
	}
	a.ClearAttachments()
	if cleared, _ := a.ContextUsage(); cleared != before {
		t.Errorf("usage must return to baseline after clear: %d != %d", cleared, before)
	}
}

func TestContextUsageAndMessageCountGrow(t *testing.T) {
	stub := &stubProvider{responses: []*provider.Response{{Content: "ok"}}, window: 100000}
	a := New(stub, tools.New())

	used0, window := a.ContextUsage()
	if window != 100000 {
		t.Fatalf("window = %d, want 100000", window)
	}
	msgs0 := a.MessageCount()

	if err := a.Run(context.Background(), strings.Repeat("x ", 5000)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	used1, _ := a.ContextUsage()
	if used1 <= used0 {
		t.Errorf("context usage did not grow with conversation: %d -> %d", used0, used1)
	}
	if a.MessageCount() <= msgs0 {
		t.Errorf("message count did not grow: %d -> %d", msgs0, a.MessageCount())
	}
}

// ---- stale-read eviction ----

// An older read_file result must be elided once a later call re-reads the same
// path and range (default args normalize equal to explicit ones), while reads
// of other ranges and the newest copy stay verbatim — and the stub keeps the
// tool-result wiring intact.
func TestEvictStaleReadsElidesSupersededCopies(t *testing.T) {
	big := strings.Repeat("line of code\n", 100) // well past evictStubMin
	a := New(&stubProvider{}, tools.New())
	a.messages = []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "work on main.go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"main.go"}`}}},
		{Role: "tool", Content: "OLD-READ " + big, ToolCallID: "c1", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"main.go","offset":50,"limit":40}`}}},
		{Role: "tool", Content: "RANGE-READ " + big, ToolCallID: "c2", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c3", Name: "read_file", Args: `{"path":"main.go","offset":1,"limit":2000}`}}},
		{Role: "tool", Content: "NEW-READ " + big, ToolCallID: "c3", Name: "read_file"},
	}

	if n := a.evictStaleReads(); n != 1 {
		t.Fatalf("want exactly 1 elided read, got %d", n)
	}
	if containsMsg(a.messages, "OLD-READ") {
		t.Error("superseded read must be elided")
	}
	if !containsMsg(a.messages, "NEW-READ") {
		t.Error("newest read must be kept verbatim")
	}
	if !containsMsg(a.messages, "RANGE-READ") {
		t.Error("a read of a different range is not superseded")
	}
	stub := a.messages[3]
	if stub.Role != "tool" || stub.ToolCallID != "c1" || stub.Name != "read_file" {
		t.Error("the stub must keep the tool-result wiring intact")
	}
	if !strings.Contains(stub.Content, "main.go") {
		t.Errorf("the stub should name the file: %q", stub.Content)
	}
	assertToolPairing(t, a.messages)

	if n := a.evictStaleReads(); n != 0 {
		t.Errorf("a second pass must find nothing new, got %d", n)
	}
}

// A write_file to a path supersedes every earlier read of it, whatever the
// range: the file no longer has that content, and the write call's own
// arguments carry the newer version.
func TestEvictStaleReadsWriteSupersedesEarlierReads(t *testing.T) {
	big := strings.Repeat("x", 1000)
	a := New(&stubProvider{}, tools.New())
	a.messages = []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "rewrite util.go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"util.go","offset":10,"limit":30}`}}},
		{Role: "tool", Content: "STALE-READ " + big, ToolCallID: "c1", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c2", Name: "write_file", Args: `{"path":"util.go","content":"new"}`}}},
		{Role: "tool", Content: "wrote util.go", ToolCallID: "c2", Name: "write_file"},
	}

	if n := a.evictStaleReads(); n != 1 {
		t.Fatalf("want 1 elided read, got %d", n)
	}
	if containsMsg(a.messages, "STALE-READ") {
		t.Error("a read made stale by write_file must be elided")
	}
	assertToolPairing(t, a.messages)
}

func TestEvictStaleReadsApplyPatchSupersedes(t *testing.T) {
	big := strings.Repeat("x", 1000)
	a := New(&stubProvider{}, tools.New())
	a.messages = []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "patch util.go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"util.go"}`}}},
		{Role: "tool", Content: "STALE-READ " + big, ToolCallID: "c1", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c2", Name: "apply_patch", Args: `{"patch":"*** Update File: util.go\n@@\n-old\n+new\n"}`}}},
		{Role: "tool", Content: "updated util.go", ToolCallID: "c2", Name: "apply_patch"},
	}
	if n := a.evictStaleReads(); n != 1 {
		t.Fatalf("want 1 elided read, got %d", n)
	}
	if containsMsg(a.messages, "STALE-READ") {
		t.Error("a read made stale by apply_patch must be elided")
	}
	assertToolPairing(t, a.messages)
}

// Results already small stay untouched: rewriting them saves nothing and
// needlessly invalidates the gateway's prefix cache.
func TestEvictStaleReadsSkipsSmallResults(t *testing.T) {
	a := New(&stubProvider{}, tools.New())
	a.messages = []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "check main.go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"main.go"}`}}},
		{Role: "tool", Content: "SMALL-READ", ToolCallID: "c1", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"main.go"}`}}},
		{Role: "tool", Content: "SMALL-READ-2", ToolCallID: "c2", Name: "read_file"},
	}
	if n := a.evictStaleReads(); n != 0 {
		t.Fatalf("small results must not be elided, got %d", n)
	}
	if !containsMsg(a.messages, "SMALL-READ") {
		t.Error("small superseded read must survive verbatim")
	}
}

// ---- server-reported usage ----

// A server-reported input count must anchor the accounting: the covered prefix
// uses the exact number, only messages appended since are estimated. Reset
// drops the anchor.
func TestServerUsageAnchorsContextUsage(t *testing.T) {
	stub := &stubProvider{window: 100000, responses: []*provider.Response{
		{Content: "ok", Usage: provider.Usage{InputTokens: 777, OutputTokens: 3}},
	}}
	a := New(stub, tools.New())
	if err := a.Run(context.Background(), "hi"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if a.usageTokens != 777 || a.usageCovers != 2 {
		t.Fatalf("anchor not recorded: tokens=%d covers=%d (want 777 covering the 2 sent messages)", a.usageTokens, a.usageCovers)
	}
	used, _ := a.ContextUsage()
	want := 777 + provider.EstimateInputTokens(a.messages[2:], nil)
	if used != want {
		t.Errorf("usage must be anchor + suffix estimate: got %d, want %d", used, want)
	}

	a.Reset()
	if a.usageCovers != 0 {
		t.Error("Reset must drop the usage anchor")
	}
}

// Rewriting history (eviction) must drop the anchor: the server's reading no
// longer describes what would be sent.
func TestEvictionInvalidatesUsageAnchor(t *testing.T) {
	big := strings.Repeat("y", 1000)
	a := New(&stubProvider{}, tools.New())
	a.messages = []provider.Message{
		{Role: "system", Content: SystemPrompt},
		{Role: "user", Content: "go"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"a.go"}`}}},
		{Role: "tool", Content: "OLD " + big, ToolCallID: "c1", Name: "read_file"},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c2", Name: "read_file", Args: `{"path":"a.go"}`}}},
		{Role: "tool", Content: "NEW " + big, ToolCallID: "c2", Name: "read_file"},
	}
	a.usageTokens, a.usageCovers = 5000, 6

	if n := a.evictStaleReads(); n != 1 {
		t.Fatalf("want 1 elided, got %d", n)
	}
	if a.usageCovers != 0 {
		t.Error("eviction rewrote history and must drop the usage anchor")
	}
}

// ---- manual /compact ----

// Compact forces the full pass regardless of the threshold and reports the
// before/after accounting.
func TestManualCompactShrinksHistory(t *testing.T) {
	a := New(&stubProvider{window: 4000, summary: "SUMMARY-TEXT"}, tools.New())
	a.messages = sixRounds()

	before, after := a.Compact(context.Background())

	if after >= before {
		t.Fatalf("Compact must shrink the accounting: %d -> %d", before, after)
	}
	if !containsMsg(a.messages, "SUMMARY-TEXT") {
		t.Error("compacted history must carry the summary")
	}
	assertToolPairing(t, a.messages)
}

// An interrupt (Esc cancels the turn's context) arriving after the model
// returned tool calls must stop the tools from running, yet still record a
// tool result for every call: a dangling tool_call makes the gateway reject
// the whole conversation on the next request.
func TestInterruptAnswersRecordedToolCallsWithoutRunningThem(t *testing.T) {
	ran := false
	reg := tools.New()
	reg.Register(&tools.Tool{
		Name:        "probe",
		Description: "test probe",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Run: func(context.Context, string) (string, error) {
			ran = true
			return "probe ran", nil
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	stub := &stubProvider{responses: []*provider.Response{
		{ToolCalls: []provider.ToolCall{
			{ID: "call_1", Name: "probe", Args: `{}`},
			{ID: "call_2", Name: "probe", Args: `{}`},
		}},
	}}
	a := New(&cancelingProvider{stubProvider: stub, cancel: cancel}, reg)

	err := a.Run(ctx, "do something slow")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("tool ran after the turn was interrupted")
	}

	// Every recorded tool call must have a matching tool result naming the
	// interrupt, so the history stays valid for the next turn.
	results := map[string]string{}
	for _, m := range a.messages {
		if m.Role == "tool" {
			results[m.ToolCallID] = m.Content
		}
	}
	for _, id := range []string{"call_1", "call_2"} {
		got, ok := results[id]
		if !ok {
			t.Fatalf("tool call %s has no tool result in history", id)
		}
		if !strings.Contains(got, "interrupted") {
			t.Fatalf("tool result for %s = %q, want an interrupted marker", id, got)
		}
	}
}

// cancelingProvider cancels the run's context as it returns the scripted
// response — the moment a real Esc lands relative to a completed Chat call.
type cancelingProvider struct {
	*stubProvider
	cancel context.CancelFunc
}

func (c *cancelingProvider) Chat(ctx context.Context, m []provider.Message, s []provider.ToolSchema, f provider.StreamFunc) (*provider.Response, error) {
	r, err := c.stubProvider.Chat(ctx, m, s, f)
	c.cancel()
	return r, err
}
