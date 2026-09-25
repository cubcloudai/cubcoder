package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/session"
)

var ansiRE = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string { return ansiRE.ReplaceAllString(s, "") }

func TestToolCallSummary(t *testing.T) {
	cases := []struct{ name, args, want string }{
		{"write_file", `{"path":"internal/tools/tools.go","content":"x"}`, "internal/tools/tools.go"},
		{"edit_file", `{"path":"main.go","search":"a","replace":"b"}`, "main.go"},
		{"apply_patch", "{\"patch\":\"*** Update File: foo.go\\n@@\\n-a\\n+b\\n\"}", "foo.go"},
		{"read_file", `{"path":"README.md","offset":1}`, "README.md"},
		{"list_dir", `{"path":"internal"}`, "internal"},
		{"run_command", `{"command":"go test ./internal/tools/"}`, "go test ./internal/tools/"},
		{"search_code", `{"pattern":"turnUI"}`, "turnUI"},
		{"search_code", `{"pattern":"turnUI","path":"."}`, "turnUI"},
		{"search_code", `{"pattern":"turnUI","path":"ui.go"}`, "turnUI  ui.go"},
		{"git_diff", `{"ref":"HEAD"}`, "HEAD"},
		{"git_diff", `{"path":"main.go"}`, "main.go"},
		{"git_diff", `{"staged":true}`, "--staged"},
		{"web_search", `{"query":"go json"}`, "go json"},
		{"fetch_url", `{"url":"https://example.com"}`, "https://example.com"},
		{"delegate_task", `{"name":"frontend","task":"build the ui"}`, "frontend — build the ui"},
		{"read_agent_log", `{"id":"w1"}`, "w1"},
		{"wait_for_agents", `{"ids":["w1","w2"]}`, "w1, w2"},
		{"check_agents", `{}`, ""},
		{"todo_write", `{"todos":[{"id":"1","content":"inspect the router","status":"pending"}]}`, "inspect the router"},
		{"todo_write", `{"todos":[{"id":"1","content":"a","status":"pending"},{"id":"2","content":"b","status":"pending"}]}`, "2 items"},
		{"write_file", `{}`, ""},
	}
	for _, c := range cases {
		if got := toolCallSummary(c.name, c.args); got != c.want {
			t.Errorf("%s %s: got %q want %q", c.name, c.args, got, c.want)
		}
	}
}

func TestFormatPlan(t *testing.T) {
	empty := stripANSI(formatPlan(nil))
	if !strings.Contains(empty, "no plan yet") {
		t.Fatalf("empty: %q", empty)
	}
	s := stripANSI(formatPlan([]agent.Todo{
		{ID: "1", Content: "inspect", Status: agent.TodoCompleted},
		{ID: "2", Content: "edit", Status: agent.TodoInProgress},
	}))
	if !strings.Contains(s, "[x] 1 inspect") || !strings.Contains(s, "[~] 2 edit") {
		t.Fatalf("got %q", s)
	}
}

func TestFormatCheckpoints(t *testing.T) {
	empty := stripANSI(formatCheckpoints(nil))
	if !strings.Contains(empty, "no checkpoints yet") {
		t.Fatalf("empty: %q", empty)
	}
	s := stripANSI(formatCheckpoints([]session.Checkpoint{
		{N: 1, Head: "abcdef0", Branch: "main", Prompt: "add health", Messages: 1},
		{N: 2, Head: "1234567", Branch: "main", Prompt: "add tests", Messages: 5},
	}))
	if !strings.Contains(s, "add health") || !strings.Contains(s, "/undo restores #2") {
		t.Fatalf("got %q", s)
	}
}

func TestFormatToolCallOmitsJSON(t *testing.T) {
	s := stripANSI(formatToolCall("write_file", `{"path":"main.go","content":"package main"}`, 80))
	if strings.Contains(s, `"path"`) || strings.Contains(s, "package main") {
		t.Errorf("call line still has JSON: %q", s)
	}
	if !strings.Contains(s, "→ write_file") || !strings.Contains(s, "main.go") {
		t.Errorf("call line = %q", s)
	}
}

func TestIsToolError(t *testing.T) {
	if !isToolError("Error: path \"../x\" is outside the working directory") {
		t.Error("Error: prefix should be an error")
	}
	if !isToolError("exit_code=1\nFAIL") {
		t.Error("non-zero exit should be an error")
	}
	if isToolError("exit_code=0\nok") {
		t.Error("exit 0 is success")
	}
	if isToolError("wrote 1842 bytes to main.go") {
		t.Error("write success is not an error")
	}
}

func TestFormatToolResultColorsError(t *testing.T) {
	ok := formatToolResult("wrote 10 bytes to x.go", 80)
	fail := formatToolResult("Error: search text not found in x.go", 80)
	if !strings.Contains(ok, dim) || strings.Contains(ok, red) {
		t.Errorf("success should be dim, not red: %q", ok)
	}
	if !strings.Contains(fail, red) {
		t.Errorf("error should be red: %q", fail)
	}
	if !strings.HasPrefix(stripANSI(fail), "  Error:") {
		t.Errorf("result indent: %q", stripANSI(fail))
	}
}

func TestTurnFooter(t *testing.T) {
	got := turnFooter(3, 14*time.Second, 12481, 14002, 262144)
	want := "3 tools · 14s · 12,481 → 14,002 tokens"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
	got = turnFooter(1, 500*time.Millisecond, 100, 100, 0)
	if got != "1 tool · <1s" {
		t.Errorf("no-window footer = %q", got)
	}
	got = turnFooter(0, 2*time.Second, 10, 20, 100)
	if got != "2s · 10 → 20 tokens" {
		t.Errorf("no-tools footer = %q", got)
	}
}

func TestFormatElapsed(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Millisecond: "<1s",
		time.Second:            "1s",
		14 * time.Second:       "14s",
		60 * time.Second:       "1m",
		90 * time.Second:       "1m30s",
	}
	for d, want := range cases {
		if got := formatElapsed(d); got != want {
			t.Errorf("formatElapsed(%v)=%q want %q", d, got, want)
		}
	}
}

func TestReplPromptHot(t *testing.T) {
	plain := stripANSI(replPrompt(10, 100, 0))
	if plain != "› " {
		t.Errorf("cool prompt = %q", plain)
	}
	hot := stripANSI(replPrompt(76, 100, 0))
	if hot != "› 76% " {
		t.Errorf("hot prompt = %q want %q", hot, "› 76% ")
	}
	warn := replPrompt(90, 100, 0)
	if !strings.Contains(warn, yellow) {
		t.Error("warnHot percent should be yellow")
	}
	if stripANSI(replPrompt(50, 0, 0)) != "› " {
		t.Error("unknown window should stay bare")
	}
	if got := stripANSI(replPrompt(10, 100, 2)); got != "› 2w " {
		t.Errorf("workers prompt = %q", got)
	}
	if got := stripANSI(replPrompt(80, 100, 1)); got != "› 1w 80% " {
		t.Errorf("workers+hot prompt = %q", got)
	}
}

func TestUnifiedDiffChange(t *testing.T) {
	old := "aaa\nbbb\nccc\nddd\neee\nfff\nggg\nhhh\niii\n"
	new := "aaa\nbbb\nccc\nXXX\neee\nfff\nggg\nhhh\niii\n"
	lines := unifiedDiff(old, new, 0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "-ddd") || !strings.Contains(joined, "+XXX") {
		t.Errorf("diff missing the change:\n%s", joined)
	}
	if !strings.Contains(joined, " ccc") || !strings.Contains(joined, " eee") {
		t.Errorf("diff missing context:\n%s", joined)
	}
	if unifiedDiff(old, old, 0) != nil {
		t.Error("identical inputs should return nil")
	}
}

func TestUnifiedDiffNewFile(t *testing.T) {
	lines := unifiedDiff("", "one\ntwo\n", 0)
	joined := strings.Join(lines, "\n")
	if !strings.HasPrefix(lines[0], "@@ -0,0 +1,") {
		t.Errorf("new-file hunk header = %q", lines[0])
	}
	if !strings.Contains(joined, "+one") || !strings.Contains(joined, "+two") {
		t.Errorf("new-file diff:\n%s", joined)
	}
}

func TestUnifiedDiffTruncates(t *testing.T) {
	var old, new strings.Builder
	for i := 0; i < 50; i++ {
		old.WriteString("old\n")
		new.WriteString("new\n")
	}
	lines := unifiedDiff(old.String(), new.String(), 8)
	if len(lines) != 9 { // 8 kept + "… N more"
		t.Fatalf("len=%d want 9: %q", len(lines), lines)
	}
	if !strings.HasPrefix(lines[len(lines)-1], "… (") {
		t.Errorf("last line = %q", lines[len(lines)-1])
	}
}

func TestPreviewWriteNewAndExisting(t *testing.T) {
	dir := t.TempDir()
	fresh := filepath.Join(dir, "fresh.go")
	got := stripANSI(preview("write_file", `{"path":`+quoteJSON(fresh)+`,"content":"package fresh\n"}`))
	if !strings.Contains(got, "write "+fresh) || !strings.Contains(got, "(new file)") || !strings.Contains(got, "+package fresh") {
		t.Errorf("new file preview:\n%s", got)
	}

	exist := filepath.Join(dir, "exist.go")
	if err := os.WriteFile(exist, []byte("package old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got = stripANSI(preview("write_file", `{"path":`+quoteJSON(exist)+`,"content":"package new\n"}`))
	if !strings.Contains(got, "-package old") || !strings.Contains(got, "+package new") {
		t.Errorf("existing file preview:\n%s", got)
	}
	if strings.Contains(got, "(new file)") {
		t.Error("existing file should not say new")
	}
}

func TestPreviewEditDiff(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("func A() {}\nfunc B() {}\nfunc C() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := `{"path":` + quoteJSON(p) + `,"search":"func B() {}","replace":"func B() { return }"}`
	got := stripANSI(preview("edit_file", args))
	if !strings.Contains(got, "edit "+p) {
		t.Errorf("header:\n%s", got)
	}
	if !strings.Contains(got, "-func B() {}") || !strings.Contains(got, "+func B() { return }") {
		t.Errorf("edit diff:\n%s", got)
	}
	if !strings.Contains(got, " func A()") || !strings.Contains(got, " func C()") {
		t.Errorf("missing context:\n%s", got)
	}
}

func TestWorkerSpinnerSuffix(t *testing.T) {
	snaps := []orchestrator.Snapshot{
		{ID: "w1", Name: "fe", Status: orchestrator.StatusRunning, LastLine: "→ edit_file web/app.go"},
		{ID: "w2", Name: "db", Status: orchestrator.StatusDone, LastLine: "→ write_file db.sql"},
	}
	got := workerSpinnerSuffix(snaps, 0)
	if !strings.Contains(got, "w1 → edit_file web/app.go") {
		t.Fatalf("got %q", got)
	}
	if strings.Contains(got, "w2") {
		t.Fatalf("done worker should be omitted: %q", got)
	}
	if workerSpinnerSuffix(nil, 80) != "" {
		t.Fatal("no running workers should be empty")
	}
}

func TestPreviewPatch(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Update File: " + p + "\n@@\n-old\n+new\n"
	args, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	got := stripANSI(preview("apply_patch", string(args)))
	if !strings.Contains(got, "update "+p) || !strings.Contains(got, "-old") || !strings.Contains(got, "+new") {
		t.Errorf("preview:\n%s", got)
	}
}

func TestPreviewCommandCwd(t *testing.T) {
	got := stripANSI(preview("run_command", `{"command":"go test ./..."}`))
	if !strings.Contains(got, "$ go test ./...") {
		t.Errorf("command: %q", got)
	}
	cwd, _ := os.Getwd()
	if !strings.Contains(got, "cwd "+cwd) {
		t.Errorf("cwd missing: %q", got)
	}
}

func quoteJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
