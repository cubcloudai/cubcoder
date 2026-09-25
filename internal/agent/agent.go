// Package agent runs the model-driven tool loop: send the conversation plus
// tool schemas to the provider, execute any tool calls locally (gated by a
// permission callback for mutating tools), feed results back, repeat until the
// model returns a final answer.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

const SystemPrompt = `You are cubcoder, a coding assistant running locally in the user's working directory.

You have tools to read, search, write, and edit files and to run shell commands. They operate on the user's real files in the current directory. Use read_file and search_code to understand the code before changing it. Prefer apply_patch for edits to existing files — one patch can update several places or files, and matching is whitespace-tolerant. Use edit_file for a single snippet. Prefer either over write_file for existing files. Run commands to build and test your work.

For any task with several steps, call todo_write with the plan before mutating files. Mark the current step in_progress and complete items as you finish them. Do not stop calling tools until the plan is done or blocked.

Rules:
- Do not invent file contents or command output. Use the tools and read the real results.
- Make minimal, correct changes that match the surrounding code.
- Stop and give a short final summary once the task is done.
- Be concise.`

// Agent owns the conversation and drives the loop.
type Agent struct {
	Provider provider.Provider
	Tools    *tools.Registry
	MaxIters int

	// Confirm gates mutating tools (write/edit/run). Return false to deny.
	Confirm func(toolName, args string) bool
	// Output hooks.
	OnText       func(delta string) // streamed assistant text
	OnToolCall   func(name, args string)
	OnToolResult func(name, result string)
	// Spinner shows progress while waiting on the model or a running tool.
	Spinner Spinner

	systemPrompt string
	messages     []provider.Message
	attachments  []Attachment // staged documents, consumed by the next Run
	todos        []Todo
	planMode     bool // /plan: hide mutating tools for this Run

	// Server-reported token accounting. usageTokens is the exact prompt size of
	// the last request; usageCovers is how many history messages that request
	// included, so messages appended since can be estimated on top. usageCovers
	// of 0 means no valid reading (none yet, or history was rewritten).
	usageTokens int
	usageCovers int
}

// Attachment is a user-supplied document staged for the next turn (via /attach
// or the -attach flag). It rides inside the next user message rather than a
// separate one, keeping role alternation valid for both wire formats.
type Attachment struct {
	Name    string
	Content string
	// Image is set for a picture attachment (screenshot, diagram); Content then
	// holds a one-line description that stands in for it in the text.
	Image *provider.Image
}

// Attach stages a document to be included with the next user turn.
func (a *Agent) Attach(name, content string) {
	a.attachments = append(a.attachments, Attachment{Name: name, Content: content})
}

// AttachImage stages a picture to ride the next user turn as image content.
// desc is a short description (format, dimensions) shown in the text.
func (a *Agent) AttachImage(name, desc string, img provider.Image) {
	a.attachments = append(a.attachments, Attachment{Name: name, Content: desc, Image: &img})
}

// Attachments returns the staged documents not yet sent.
func (a *Agent) Attachments() []Attachment {
	return append([]Attachment(nil), a.attachments...)
}

// ClearAttachments drops all staged documents.
func (a *Agent) ClearAttachments() { a.attachments = nil }

// consumeAttachments prepends the staged documents to the user's message and
// clears the staging area. Each document is fenced with named markers so the
// model can tell where one ends, the next begins, and the actual request
// starts — and can cite documents by name.
func (a *Agent) consumeAttachments(input string) (string, []provider.Image) {
	if len(a.attachments) == 0 {
		return input, nil
	}
	var b strings.Builder
	var images []provider.Image
	for _, at := range a.attachments {
		if at.Image != nil {
			images = append(images, *at.Image)
			fmt.Fprintf(&b, "--- attached image %d: %s (%s) ---\n\n", len(images), at.Name, at.Content)
			continue
		}
		fmt.Fprintf(&b, "--- attached document: %s ---\n%s\n--- end of document: %s ---\n\n",
			at.Name, strings.TrimRight(at.Content, "\n"), at.Name)
	}
	b.WriteString(input)
	a.attachments = nil
	return b.String(), images
}

// Spinner is an in-place progress indicator. Implementations live in the UI.
type Spinner interface {
	Start(label string)
	Stop()
}

func (a *Agent) spin(label string) {
	if a.Spinner != nil {
		a.Spinner.Start(label)
	}
}
func (a *Agent) spinStop() {
	if a.Spinner != nil {
		a.Spinner.Stop()
	}
}

func New(p provider.Provider, t *tools.Registry) *Agent {
	return NewWithSystem(p, t, SystemPrompt)
}

// NewWithSystem is New with a caller-supplied system prompt. The lead
// (orchestrator) and a worker run the same loop with different instructions.
func NewWithSystem(p provider.Provider, t *tools.Registry, system string) *Agent {
	a := &Agent{
		Provider:     p,
		Tools:        t,
		MaxIters:     30,
		systemPrompt: system,
		messages:     []provider.Message{{Role: "system", Content: system}},
	}
	if t != nil {
		t.Register(todoTool(a))
	}
	return a
}

// Messages returns a copy of the conversation, system prompt included. It is
// the unit of session persistence.
func (a *Agent) Messages() []provider.Message {
	return append([]provider.Message(nil), a.messages...)
}

// Restore replaces the conversation with a saved one. The saved system prompt
// is dropped in favor of the agent's current one, so a changed agents.md (or a
// newer cubcoder) applies to the resumed session. Token accounting starts
// over: the first request re-anchors it from the server's reading.
func (a *Agent) Restore(msgs []provider.Message) {
	rest := msgs
	if len(rest) > 0 && rest[0].Role == "system" {
		rest = rest[1:]
	}
	a.messages = append([]provider.Message{{Role: "system", Content: a.systemPrompt}}, rest...)
	a.usageCovers = 0
}

// TruncateMessages keeps the first n messages (including the system prompt)
// and drops the rest. Used by /undo to rewind the transcript with the tree.
func (a *Agent) TruncateMessages(n int) {
	if n < 1 {
		n = 1
	}
	if n > len(a.messages) {
		return
	}
	a.messages = a.messages[:n]
	a.usageCovers = 0
}

// Reset clears the conversation back to a fresh system prompt.
func (a *Agent) Reset() {
	a.messages = []provider.Message{{Role: "system", Content: a.systemPrompt}}
	a.usageCovers = 0
	a.todos = nil
}

// inputSize is the size of the conversation as the next request would send it:
// the server's exact reading of the last request, when one exists, plus the
// estimate for messages appended since — or the pure estimate when no server
// reading is valid. Server numbers include the chat-template and tool-schema
// overhead the estimator can only guess at.
func (a *Agent) inputSize(schemas []provider.ToolSchema) int {
	if a.usageCovers > 0 && a.usageCovers <= len(a.messages) {
		return a.usageTokens + provider.EstimateInputTokens(a.messages[a.usageCovers:], nil)
	}
	return provider.EstimateInputTokens(a.messages, schemas)
}

// ContextUsage reports the input-token size of the current conversation
// (server-reported when available, estimated otherwise) and the model's
// context window. window is 0 when the backend's window is unknown. It is the
// same accounting the loop budgets and compacts against.
func (a *Agent) ContextUsage() (used, window int) {
	used = a.inputSize(a.schemas())
	if note := a.planNote(); note != "" {
		used += provider.EstimateInputTokens([]provider.Message{{Role: "system", Content: note}}, nil)
	}
	// Staged attachments haven't entered the history yet but will ride the next
	// turn; count them so /status and the headroom warning see what's coming.
	for _, at := range a.attachments {
		used += provider.EstimateInputTokens([]provider.Message{{Role: "user", Content: at.Content}}, nil)
		if at.Image != nil {
			used += provider.ImageTokenEstimate
		}
	}
	return used, a.Provider.ContextWindow()
}

// MessageCount is the number of messages in the conversation, including the
// system prompt.
func (a *Agent) MessageCount() int { return len(a.messages) }

// Run sends one user turn and drives the tool loop to a final answer.
func (a *Agent) Run(ctx context.Context, userInput string) error {
	text, images := a.consumeAttachments(userInput)
	a.messages = append(a.messages, provider.Message{Role: "user", Content: text, Images: images})
	schemas := a.schemas()

	for i := 0; i < a.MaxIters; i++ {
		// Keep the conversation inside the model's window: when input grows past
		// the threshold, summarize the oldest rounds and drop their raw text.
		a.maybeCompact(ctx, schemas)

		// Wrap the text stream: drop leading whitespace so turns don't open
		// with blank lines, and track whether any real text was shown so we
		// only emit a trailing newline (and skip it on tool-only turns).
		printed := false
		stream := func(delta string) {
			if a.OnText == nil {
				return
			}
			if !printed {
				delta = strings.TrimLeft(delta, " \t\r\n")
				if delta == "" {
					return
				}
				printed = true
				a.spinStop() // clear "thinking" before the first text
			}
			a.OnText(delta)
		}

		a.spin("thinking")
		sent := len(a.messages)
		resp, err := a.Provider.Chat(ctx, a.requestMessages(), schemas, stream)
		a.spinStop() // also covers tool-only turns (no text streamed)
		if err != nil {
			return err
		}
		if resp.Usage.InputTokens > 0 {
			// Anchor the exact reading to the messages that request included; the
			// response and tool results appended below are estimated on top.
			a.usageTokens = resp.Usage.InputTokens
			a.usageCovers = sent
		}

		// A turn cut off at the output token limit has incomplete tool-call
		// arguments (unterminated JSON). Recording them would both run tools on
		// garbage and poison the history: the gateway re-parses the broken
		// arguments on the next request and rejects the whole conversation with
		// a 400. Drop the broken calls, keep only the safe text, and nudge the
		// model to retry with a smaller step.
		if (resp.Truncated || hasIncompleteToolCall(resp.ToolCalls)) && len(resp.ToolCalls) > 0 {
			if printed && a.OnText != nil {
				a.OnText("\n")
			}
			a.messages = append(a.messages,
				provider.Message{Role: "assistant", Content: resp.Content},
				provider.Message{Role: "user", Content: truncationNudge})
			if a.OnText != nil {
				a.OnText("[output truncated before the tool call finished — nothing was written; retrying with a smaller step]\n")
			}
			continue
		}

		// Record the assistant turn (text + any tool calls).
		a.messages = append(a.messages, provider.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})
		// End the streamed line only if we actually printed text.
		if printed && a.OnText != nil {
			a.OnText("\n")
		}

		if len(resp.ToolCalls) == 0 {
			return nil // final answer
		}

		for _, tc := range resp.ToolCalls {
			// After a user interrupt (Esc) stop launching tools, but still
			// answer every recorded call: a tool_call left without a result
			// makes the gateway reject the whole conversation next turn.
			var result string
			var images []provider.Image
			if ctx.Err() != nil {
				result = "Error: interrupted by the user before this tool ran."
			} else {
				result, images = a.dispatch(ctx, tc)
			}
			a.messages = append(a.messages, provider.Message{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
				Name:       tc.Name,
				Images:     images,
			})
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return fmt.Errorf("stopped after %d tool iterations without finishing; changes made so far are kept — send another message to continue, or raise the cap with -max-iters / CUBCODER_MAX_ITERS", a.MaxIters)
}

const truncationNudge = "Your previous response was cut off at the output token limit before the tool call finished, so the arguments were incomplete and nothing was run. Do not repeat that call verbatim. For large files, write them in smaller pieces (an initial write_file followed by edit_file appends), or split the work into smaller steps, then continue."

// hasIncompleteToolCall reports whether any tool call carries arguments that
// are not valid, complete JSON — the signature of a turn truncated mid-call.
// Empty arguments are valid (no-parameter tools), so they're skipped.
func hasIncompleteToolCall(calls []provider.ToolCall) bool {
	for _, tc := range calls {
		args := strings.TrimSpace(tc.Args)
		if args == "" {
			continue
		}
		if !json.Valid([]byte(args)) {
			return true
		}
	}
	return false
}

// ---- compaction ----
//
// When the conversation outgrows the model's window, replace the oldest units
// with a model-written summary and keep recent units verbatim. A "unit" is a
// user message, or an assistant turn plus the tool results answering it.
// Cutting on unit boundaries keeps every tool-call / tool-result pair intact,
// which both wire formats require — and, unlike cutting on user-message
// boundaries, it can compact inside a single long agentic task, where one user
// request drives dozens of tool iterations with no user message to cut on.
const (
	compactThreshold = 0.75 // compact once estimated input exceeds this fraction of the window
	retainUnits      = 6    // most recent units kept verbatim...
	retainFraction   = 0.30 // ...but no more than this fraction of the window
)

const summarySystemPrompt = `You are compacting a coding session so the assistant can continue without the full transcript. Write a dense summary that preserves: the task and its current state, what has been done, what remains, key file contents and decisions, and any errors. List the paths of every file that was read or modified so they can be re-read if needed. Output only the summary, with no preamble.`

func (a *Agent) maybeCompact(ctx context.Context, schemas []provider.ToolSchema) {
	limit := a.Provider.ContextWindow()
	if limit <= 0 {
		return // unknown window: nothing to budget against
	}
	threshold := int(compactThreshold * float64(limit))
	if a.inputSize(schemas) <= threshold {
		return
	}
	// Stage one: elide superseded read_file results in place. Cheap, needs no
	// model call, and recoverable (the model can re-read); often frees enough
	// to skip the lossy summary entirely.
	if a.evictStaleReads() > 0 && a.inputSize(schemas) <= threshold {
		return
	}
	a.spin("compacting")
	a.compact(ctx, limit)
	a.spinStop()
}

// Compact forces a compaction pass now — stale-read eviction, then the same
// summarize-and-retain the automatic path runs — regardless of the threshold.
// It returns the input-token accounting before and after, so the caller can
// report what was reclaimed. Backing the /compact command.
func (a *Agent) Compact(ctx context.Context) (before, after int) {
	schemas := a.schemas()
	before = a.inputSize(schemas)
	a.evictStaleReads()
	a.spin("compacting")
	a.compact(ctx, a.Provider.ContextWindow())
	a.spinStop()
	return before, a.inputSize(schemas)
}

func (a *Agent) compact(ctx context.Context, limit int) {
	if len(a.messages) == 0 || a.messages[0].Role != "system" {
		return
	}
	system := a.messages[0]
	units := splitUnits(a.messages[1:])
	if len(units) <= 1 {
		return // only the current unit exists; nothing old enough to drop
	}

	// Retain the trailing units: always at least the most recent one, then back
	// to retainUnits, but only while they fit the retain budget.
	retainBudget := int(retainFraction * float64(limit))
	keep, used := 0, 0
	for i := len(units) - 1; i >= 0 && keep < retainUnits; i-- {
		cost := provider.EstimateInputTokens(units[i], nil)
		if keep >= 1 && used+cost > retainBudget {
			break
		}
		used += cost
		keep++
	}
	if keep >= len(units) {
		return // everything fits the retain set; nothing to summarize
	}
	older := units[:len(units)-keep]
	retained := flatten(units[len(units)-keep:])

	summary, err := a.summarize(ctx, older)
	if err != nil || strings.TrimSpace(summary) == "" {
		return // never corrupt history on a failed summarize; the cap is the backstop
	}

	// Prepend the summary into the first retained user turn. This avoids a
	// standalone message (which would break Anthropic's role-alternation rules)
	// and leaves the system prompt byte-identical so its prompt cache survives.
	// History is rewritten below; the server's last exact reading no longer
	// describes it.
	a.usageCovers = 0

	head := "Summary of earlier work in this session:\n" + summary + "\n\n---\n\n"
	if len(retained) > 0 && retained[0].Role == "user" {
		retained[0].Content = head + retained[0].Content
		a.messages = append([]provider.Message{system}, retained...)
		return
	}
	// Mid-task cut: the retained set starts at an assistant unit (the original
	// user request was summarized away). Carry the summary in a synthetic user
	// message before it — user → assistant(tool_calls) → tool results is valid
	// in both wire formats.
	rebuilt := append([]provider.Message{system},
		provider.Message{Role: "user", Content: head + "(The session continues mid-task below; keep going.)"})
	a.messages = append(rebuilt, retained...)
}

// summarizeReserve is window room left for the summary system prompt, chat
// template overhead, and the summary output when sizing the summarize request.
const summarizeReserve = 16384

// summarize asks the model to compress the dropped units to plain text. No
// tools are advertised, so the response can't trip the truncated-tool-call path.
func (a *Agent) summarize(ctx context.Context, older [][]provider.Message) (string, error) {
	var b strings.Builder
	for _, r := range older {
		for _, m := range r {
			b.WriteString(m.Role)
			b.WriteString(": ")
			b.WriteString(m.Content)
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&b, "\n[called %s %s]", tc.Name, tc.Args)
			}
			b.WriteByte('\n')
		}
	}
	// Bound the summarize request itself: a transcript already past the window
	// would fail the very request meant to rescue it. 3 bytes/token matches the
	// provider package's conservative estimate.
	transcript := b.String()
	if limit := a.Provider.ContextWindow(); limit > 0 {
		if maxChars := (limit - summarizeReserve) * 3; maxChars > 0 && len(transcript) > maxChars {
			transcript = cutMiddle(transcript, maxChars, maxChars/2,
				"\n... [%d bytes of transcript omitted to fit the summarize request] ...\n")
		}
	}
	resp, err := a.Provider.Chat(ctx, []provider.Message{
		{Role: "system", Content: summarySystemPrompt},
		{Role: "user", Content: transcript},
	}, nil, nil)
	if err != nil {
		return "", err
	}
	return resp.Content, nil
}

// splitUnits groups messages into compaction units: a user message stands
// alone, and an assistant turn plus the tool results answering it stay
// together. Tool messages attach to the preceding assistant unit, so cutting
// between units never orphans a tool result.
func splitUnits(msgs []provider.Message) [][]provider.Message {
	var units [][]provider.Message
	var cur []provider.Message
	for _, m := range msgs {
		if (m.Role == "user" || m.Role == "assistant") && len(cur) > 0 {
			units = append(units, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		units = append(units, cur)
	}
	return units
}

func flatten(rounds [][]provider.Message) []provider.Message {
	var out []provider.Message
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// ---- stale tool-result eviction ----
//
// In a long session the same file gets read repeatedly as it evolves; every
// stale copy sits in history at up to ~8k tokens until compaction summarizes
// everything old — lossily. Eviction is the near-lossless first line: replace
// a read_file result in place with a one-line stub when a later event made it
// stale, keeping the newest copy verbatim. In-place edits preserve tool-call /
// tool-result pairing, so both wire formats stay valid.
//
// A read is stale when a later read_file call re-read the same path and range,
// or a later write_file / apply_patch replaced the file (the call's own
// arguments carry the newer content). edit_file does not evict: it changes one
// span, and the model still works from the rest of the old read. A write_file
// that was denied or failed evicts too — the stub's remedy (re-read the file)
// stays correct either way.

// evictStubMin: results already near stub size aren't worth rewriting — the
// history edit would invalidate the gateway's prefix cache for no real gain.
const evictStubMin = 600

func (a *Agent) evictStaleReads() (elided int) {
	// Tool results carry only the call ID; the path and range live in the
	// assistant message that issued the call.
	type readArgs struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	calls := map[string]readArgs{} // read_file call ID → parsed args
	for _, m := range a.messages {
		for _, tc := range m.ToolCalls {
			if tc.Name != "read_file" {
				continue
			}
			var ra readArgs
			if json.Unmarshal([]byte(tc.Args), &ra) != nil || ra.Path == "" {
				continue
			}
			// Normalize to the tool's own defaults so "read x.go" and
			// "read x.go from line 1" compare equal.
			if ra.Offset < 1 {
				ra.Offset = 1
			}
			if ra.Limit <= 0 {
				ra.Limit = 2000
			}
			calls[tc.ID] = ra
		}
	}
	if len(calls) == 0 {
		return 0
	}

	// Walk newest → oldest: the first sighting of a (path, range) is the copy
	// to keep; anything older it shadows is stale. A write_file or apply_patch
	// marks its path so every earlier read of it is stale regardless of range.
	seen := map[readArgs]bool{}
	written := map[string]bool{}
	for i := len(a.messages) - 1; i >= 0; i-- {
		m := &a.messages[i]
		for _, tc := range m.ToolCalls {
			switch tc.Name {
			case "write_file":
				var wa struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(tc.Args), &wa) == nil && wa.Path != "" {
					written[wa.Path] = true
				}
			case "apply_patch":
				var pa struct {
					Patch string `json:"patch"`
				}
				if json.Unmarshal([]byte(tc.Args), &pa) == nil {
					for _, p := range tools.PatchPaths(pa.Patch) {
						written[p] = true
					}
				}
			}
		}
		if m.Role != "tool" || m.Name != "read_file" || (len(m.Content) < evictStubMin && len(m.Images) == 0) {
			continue
		}
		ra, ok := calls[m.ToolCallID]
		if !ok {
			continue
		}
		if seen[ra] || written[ra.Path] {
			m.Content = fmt.Sprintf("[superseded: this read of %s was replaced by a newer read or write later in the session; call read_file again if the content matters]", ra.Path)
			m.Images = nil
			elided++
			continue
		}
		seen[ra] = true
	}
	if elided > 0 {
		// History was rewritten; the server's last exact reading is stale.
		a.usageCovers = 0
	}
	return elided
}

// ---- tool-result capping ----
//
// One unbounded tool result (a recursive git rm listing, a huge build log) can
// leap the conversation from under the compaction threshold straight past the
// model's window in a single step, where compaction can no longer save it. Cap
// every result at the dispatch chokepoint — this also covers tools registered
// from outside the tools package, like the orchestrator's. The tail is kept
// because command output puts errors and summaries at the end.
const (
	toolResultCapChars  = 24000 // ≈8k tokens by the provider's estimate
	toolResultTailChars = 8000
)

func capToolResult(s string) string {
	return cutMiddle(s, toolResultCapChars, toolResultTailChars,
		"\n... [tool output truncated: %d bytes omitted; rerun with a narrower command or path if the middle matters] ...\n")
}

// cutMiddle shrinks s to roughly max bytes by removing the middle, keeping the
// final tailLen bytes. Cuts land on UTF-8 rune boundaries. noteFmt receives the
// omitted byte count.
func cutMiddle(s string, max, tailLen int, noteFmt string) string {
	if len(s) <= max || max <= tailLen {
		return s
	}
	headEnd := max - tailLen
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}
	tailStart := len(s) - tailLen
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}
	return s[:headEnd] + fmt.Sprintf(noteFmt, tailStart-headEnd) + s[tailStart:]
}

// dispatch runs one tool call. ctx is the turn's context; passing it into
// Tool.Run is what lets a user interrupt (Esc) kill a tool already running,
// instead of waiting for it to finish on its own.
func (a *Agent) dispatch(ctx context.Context, tc provider.ToolCall) (string, []provider.Image) {
	if a.OnToolCall != nil {
		a.OnToolCall(tc.Name, tc.Args)
	}
	tool, ok := a.Tools.Get(tc.Name)
	if !ok {
		return fmt.Sprintf("Error: unknown tool %q", tc.Name), nil
	}
	// Validate arguments before prompting or running: a weaker local model often
	// omits a required field. Hand it a specific correction instead of running
	// the tool on bad input (or asking the user to approve a doomed call).
	if err := tool.Validate(tc.Args); err != nil {
		out := fmt.Sprintf("Error: invalid arguments for %s: %v. Re-issue the call with corrected arguments.", tc.Name, err)
		if a.OnToolResult != nil {
			a.OnToolResult(tc.Name, out)
		}
		return out, nil
	}
	if a.planMode && planBlocked(tc.Name) {
		out := "Error: plan mode is read-only; use todo_write to record the plan, then stop."
		if a.OnToolResult != nil {
			a.OnToolResult(tc.Name, out)
		}
		return out, nil
	}
	if tool.Mutating && a.Confirm != nil && !a.Confirm(tc.Name, tc.Args) {
		return "Error: the user denied permission to run this tool.", nil
	}
	// Quiet tools (e.g. wait_for_agents) render their own live progress, so the
	// spinner would fight them for the terminal line — leave it off for those.
	if !tool.Quiet {
		a.spin("running " + tc.Name)
	}
	var out string
	var images []provider.Image
	var err error
	if tool.RunRich != nil {
		out, images, err = tool.RunRich(ctx, tc.Args)
	} else {
		out, err = tool.Run(ctx, tc.Args)
	}
	if !tool.Quiet {
		a.spinStop()
	}
	if err != nil {
		out, images = "Error: "+err.Error(), nil
	}
	out = capToolResult(out) // what the model sees is what the display shows
	if a.OnToolResult != nil {
		a.OnToolResult(tc.Name, out)
	}
	return out, images
}
