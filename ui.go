package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/session"
	"cubcoder/internal/tools"
)

// Prompt % appears once compaction is in play (agent compactThreshold = 0.75).
// At warnHot (0.85) the number turns yellow — the same edge as the headroom
// warning, so the prompt and the warning agree on "this is tight".
const promptHotPct = 75

const (
	previewMaxLines = 20
	previewContext  = 3
)

// replPrompt is the REPL input prefix. Once context use crosses promptHotPct
// it carries a trailing "76% " so the user sees pressure without /status.
func replPrompt(used, window, workers int) string {
	p := cyan + "› " + reset
	if workers > 0 {
		p += dim + fmt.Sprintf("%dw ", workers) + reset
	}
	if window <= 0 {
		return p
	}
	pct := used * 100 / window
	if pct < promptHotPct {
		return p
	}
	color := dim
	if float64(used)/float64(window) >= warnHot {
		color = yellow
	}
	return p + color + fmt.Sprintf("%d%% ", pct) + reset
}

// workerSpinnerSuffix folds running workers into the lead spinner line.
// width of 0 means unlimited.
func workerSpinnerSuffix(snaps []orchestrator.Snapshot, width int) string {
	var parts []string
	for _, s := range snaps {
		if s.Status != orchestrator.StatusRunning {
			continue
		}
		bit := s.ID
		if s.LastLine != "" {
			bit += " " + s.LastLine
		} else if s.Name != "" {
			bit += " " + s.Name
		}
		parts = append(parts, bit)
	}
	if len(parts) == 0 {
		return ""
	}
	suffix := "  ·  " + strings.Join(parts, "  ·  ")
	if width > 0 {
		// The spinner prefix is roughly "| thinking… 12s" (~18 cols). Leave it.
		budget := width - 20
		if budget > 8 && len(suffix) > budget {
			suffix = truncate(suffix, budget)
		}
	}
	return suffix
}

func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return "<1s"
	}
	sec := int(d.Round(time.Second).Seconds())
	if sec < 60 {
		return fmt.Sprintf("%ds", sec)
	}
	m, s := sec/60, sec%60
	if s == 0 {
		return fmt.Sprintf("%dm", m)
	}
	return fmt.Sprintf("%dm%ds", m, s)
}

// turnFooter is the one-line summary under a finished turn. window of 0
// drops the token span (unknown context).
func turnFooter(nTools int, elapsed time.Duration, before, after, window int) string {
	var parts []string
	switch {
	case nTools == 1:
		parts = append(parts, "1 tool")
	case nTools > 1:
		parts = append(parts, fmt.Sprintf("%d tools", nTools))
	}
	parts = append(parts, formatElapsed(elapsed))
	if window > 0 {
		parts = append(parts, fmt.Sprintf("%s → %s tokens", commas(before), commas(after)))
	}
	return strings.Join(parts, " · ")
}

// toolCallSummary pulls a human-readable noun out of the tool's JSON args
// (a path, a command, a query) so the call line is scannable. Empty means
// the tool has nothing worth showing next to its name.
func toolCallSummary(name, args string) string {
	switch name {
	case "read_file", "write_file", "edit_file", "list_dir":
		return jsonString(args, "path")
	case "apply_patch":
		return strings.Join(tools.PatchPaths(jsonString(args, "patch")), ", ")
	case "search_code":
		pat := jsonString(args, "pattern")
		if p := jsonString(args, "path"); p != "" && p != "." {
			if pat == "" {
				return p
			}
			return pat + "  " + p
		}
		return pat
	case "git_diff":
		ref, path := jsonString(args, "ref"), jsonString(args, "path")
		switch {
		case ref != "" && path != "":
			return ref + "  " + path
		case ref != "":
			return ref
		case path != "":
			return path
		default:
			if jsonBool(args, "staged") {
				return "--staged"
			}
			return ""
		}
	case "run_command":
		return jsonString(args, "command")
	case "web_search":
		return jsonString(args, "query")
	case "fetch_url":
		return jsonString(args, "url")
	case "delegate_task":
		n, task := jsonString(args, "name"), jsonString(args, "task")
		if n != "" && task != "" {
			return n + " — " + task
		}
		return n + task
	case "read_agent_log":
		return jsonString(args, "id")
	case "wait_for_agents":
		ids := jsonStringList(args, "ids")
		return strings.Join(ids, ", ")
	case "check_agents":
		return ""
	case "todo_write":
		obj := jsonObject(args)
		if obj == nil {
			return ""
		}
		arr, _ := obj["todos"].([]any)
		if len(arr) == 1 {
			if m, ok := arr[0].(map[string]any); ok {
				if s, ok := m["content"].(string); ok && s != "" {
					return s
				}
			}
		}
		if arr != nil {
			return fmt.Sprintf("%d items", len(arr))
		}
		return ""
	}
	s := strings.TrimSpace(args)
	if s == "" || s == "{}" {
		return ""
	}
	return oneLine(s)
}

func formatToolCall(name, args string, width int) string {
	summary := toolCallSummary(name, args)
	var b strings.Builder
	fmt.Fprintf(&b, "%s→ %s%s", cyan, name, reset)
	if summary != "" {
		n := 80
		if width > len(name)+6 {
			n = width - len(name) - 6 // "→ " + name + "  "
		}
		fmt.Fprintf(&b, "  %s%s%s", dim, truncate(oneLine(summary), n), reset)
	}
	b.WriteByte('\n')
	return b.String()
}

func isToolError(result string) bool {
	s := strings.TrimSpace(result)
	if strings.HasPrefix(s, "Error:") || strings.HasPrefix(s, "error:") {
		return true
	}
	// run_command leads with exit_code=N; anything other than 0 is a failure.
	if strings.HasPrefix(s, "exit_code=") {
		rest := s[len("exit_code="):]
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			rest = rest[:i]
		}
		return strings.TrimSpace(rest) != "0"
	}
	return false
}

func formatToolResult(result string, width int) string {
	line := oneLine(strings.TrimSpace(result))
	if width > 4 {
		line = truncate(line, width-2)
	} else {
		line = truncate(line, 120)
	}
	color := dim
	if isToolError(result) {
		color = red
	}
	return color + "  " + line + reset + "\n"
}

func jsonObject(args string) map[string]any {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return nil
	}
	return obj
}

func jsonString(args, key string) string {
	obj := jsonObject(args)
	if obj == nil {
		return ""
	}
	switch v := obj[key].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}

func jsonBool(args, key string) bool {
	obj := jsonObject(args)
	if obj == nil {
		return false
	}
	b, _ := obj[key].(bool)
	return b
}

func jsonStringList(args, key string) []string {
	obj := jsonObject(args)
	if obj == nil {
		return nil
	}
	arr, ok := obj[key].([]any)
	if !ok {
		return nil
	}
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// preview renders what a mutating tool will do, before the confirm prompt.
// write_file and edit_file show a unified diff against the file on disk;
// run_command shows the command and the cwd it will run in.
func preview(name, args string) string {
	switch name {
	case "write_file":
		return previewWrite(args)
	case "edit_file":
		return previewEdit(args)
	case "apply_patch":
		return previewPatch(args)
	case "run_command":
		return previewCommand(args)
	}
	return ""
}

func previewWrite(args string) string {
	var a struct{ Path, Content string }
	_ = json.Unmarshal([]byte(args), &a)
	var b strings.Builder
	fmt.Fprintf(&b, "%swrite %s%s\n", yellow, a.Path, reset)
	old, err := os.ReadFile(a.Path)
	if err != nil {
		fmt.Fprintf(&b, "%s(new file)%s\n", dim, reset)
		writePrefixed(&b, a.Content, "+", green, previewMaxLines)
		return b.String()
	}
	if string(old) == a.Content {
		fmt.Fprintf(&b, "%s(no changes)%s\n", dim, reset)
		return b.String()
	}
	writeDiff(&b, string(old), a.Content)
	return b.String()
}

func previewEdit(args string) string {
	var a struct{ Path, Search, Replace string }
	_ = json.Unmarshal([]byte(args), &a)
	var b strings.Builder
	fmt.Fprintf(&b, "%sedit %s%s\n", yellow, a.Path, reset)
	data, err := os.ReadFile(a.Path)
	if err != nil {
		fmt.Fprintf(&b, "%s(%v)%s\n", dim, err, reset)
		writePrefixed(&b, a.Search, "-", red, 10)
		writePrefixed(&b, a.Replace, "+", green, 10)
		return b.String()
	}
	content := string(data)
	n := strings.Count(content, a.Search)
	if n != 1 {
		if n == 0 {
			fmt.Fprintf(&b, "%s(search text not found)%s\n", red, reset)
		} else {
			fmt.Fprintf(&b, "%s(search text matches %d places)%s\n", yellow, n, reset)
		}
		writePrefixed(&b, a.Search, "-", red, 10)
		writePrefixed(&b, a.Replace, "+", green, 10)
		return b.String()
	}
	updated := strings.Replace(content, a.Search, a.Replace, 1)
	writeDiff(&b, content, updated)
	return b.String()
}

func previewPatch(args string) string {
	var a struct{ Patch string }
	_ = json.Unmarshal([]byte(args), &a)
	var b strings.Builder
	fmt.Fprintf(&b, "%sapply_patch%s\n", yellow, reset)
	changes, err := tools.PlanPatch("", a.Patch)
	if err != nil {
		fmt.Fprintf(&b, "%s(%v)%s\n", red, err, reset)
		writePrefixed(&b, a.Patch, " ", dim, 12)
		return b.String()
	}
	for _, c := range changes {
		fmt.Fprintf(&b, "%s%s %s%s\n", yellow, c.Op, c.Path, reset)
		switch c.Op {
		case "add":
			fmt.Fprintf(&b, "%s(new file)%s\n", dim, reset)
			writePrefixed(&b, c.New, "+", green, previewMaxLines)
		case "delete":
			writePrefixed(&b, c.Old, "-", red, previewMaxLines)
		default:
			if c.Old == c.New {
				fmt.Fprintf(&b, "%s(no changes)%s\n", dim, reset)
				continue
			}
			writeDiff(&b, c.Old, c.New)
		}
	}
	return b.String()
}

func previewCommand(args string) string {
	var a struct{ Command string }
	_ = json.Unmarshal([]byte(args), &a)
	var b strings.Builder
	fmt.Fprintf(&b, "%s$ %s%s\n", yellow, a.Command, reset)
	if cwd, err := os.Getwd(); err == nil {
		fmt.Fprintf(&b, "%s  cwd %s%s\n", dim, cwd, reset)
	}
	return b.String()
}

func writeDiff(b *strings.Builder, old, new string) {
	for _, ln := range unifiedDiff(old, new, previewMaxLines) {
		b.WriteString(colorDiffLine(ln))
		b.WriteByte('\n')
	}
}

func writePrefixed(b *strings.Builder, body, mark, color string, max int) {
	for _, ln := range head(body, max) {
		if strings.HasPrefix(ln, "…") {
			fmt.Fprintf(b, "%s%s%s\n", dim, ln, reset)
			continue
		}
		fmt.Fprintf(b, "%s%s%s%s\n", color, mark, ln, reset)
	}
}

func colorDiffLine(ln string) string {
	switch {
	case strings.HasPrefix(ln, "…"):
		return dim + ln + reset
	case strings.HasPrefix(ln, "@@"):
		return cyan + ln + reset
	case strings.HasPrefix(ln, "+"):
		return green + ln + reset
	case strings.HasPrefix(ln, "-"):
		return red + ln + reset
	default:
		return dim + ln + reset
	}
}

// unifiedDiff is a single-hunk prefix/suffix diff: the common head and tail
// stay as context, everything between is the change. Enough for an approval
// preview (edit_file is one unique span; write_file of a rewritten file still
// shows what moved). Returns nil when old and new are identical.
func unifiedDiff(old, new string, maxLines int) []string {
	a, b := splitDiffLines(old), splitDiffLines(new)
	if slicesEqual(a, b) {
		return nil
	}
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	as, bs := len(a), len(b)
	for as > pre && bs > pre && a[as-1] == b[bs-1] {
		as--
		bs--
	}
	oldStart := max(0, pre-previewContext)
	oldEnd := min(len(a), as+previewContext)
	newEnd := min(len(b), bs+previewContext)
	oldCount := oldEnd - oldStart
	newCount := newEnd - oldStart // shared leading context, so both hunks start at oldStart
	// Hunk start in the new file is the same as oldStart: both share the
	// leading context. Pure insert into an empty file is the git-style 0,0.
	oldLine, newLine := oldStart+1, oldStart+1
	if len(a) == 0 {
		oldLine, oldCount = 0, 0
	}
	if len(b) == 0 {
		newLine, newCount = 0, 0
	}

	lines := []string{fmt.Sprintf("@@ -%d,%d +%d,%d @@", oldLine, oldCount, newLine, newCount)}
	for i := oldStart; i < pre; i++ {
		lines = append(lines, " "+a[i])
	}
	for i := pre; i < as; i++ {
		lines = append(lines, "-"+a[i])
	}
	for i := pre; i < bs; i++ {
		lines = append(lines, "+"+b[i])
	}
	for i := as; i < oldEnd; i++ {
		lines = append(lines, " "+a[i])
	}
	if maxLines > 0 && len(lines) > maxLines {
		omitted := len(lines) - maxLines
		lines = append(lines[:maxLines], fmt.Sprintf("… (%d more lines)", omitted))
	}
	return lines
}

func splitDiffLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return []string{""}
	}
	return strings.Split(s, "\n")
}

// turnUI paints one user turn: tool lines, blank-line separation around tool
// batches, and the footer. Reset at the start of each Run.
type turnUI struct {
	tools     int
	afterTool bool
}

func (t *turnUI) reset() {
	t.tools = 0
	t.afterTool = false
}

func (t *turnUI) onText(delta string) {
	if t.afterTool && delta != "" {
		fmt.Print("\n")
		t.afterTool = false
	}
	fmt.Print(delta)
}

func (t *turnUI) onToolCall(name, args string) {
	t.tools++
	if !t.afterTool {
		fmt.Print("\n")
	}
	fmt.Print(formatToolCall(name, args, termWidth(os.Stdout.Fd())))
	t.afterTool = true
}

func (t *turnUI) onToolResult(name, result string) {
	if name == "todo_write" && !isToolError(result) {
		for _, line := range strings.Split(strings.TrimRight(result, "\n"), "\n") {
			fmt.Printf("%s  %s%s\n", dim, line, reset)
		}
		return
	}
	width := termWidth(os.Stdout.Fd())
	if width <= 0 {
		width = 120
	}
	fmt.Print(formatToolResult(result, width))
}

func formatPlan(todos []agent.Todo) string {
	if len(todos) == 0 {
		return dim + "no plan yet — /plan <task> to write one" + reset + "\n"
	}
	var b strings.Builder
	for _, t := range todos {
		color := dim
		mark := "[ ]"
		switch t.Status {
		case agent.TodoInProgress:
			color, mark = cyan, "[~]"
		case agent.TodoCompleted:
			mark = "[x]"
		case agent.TodoCancelled:
			mark = "[-]"
		}
		fmt.Fprintf(&b, "%s  %s %s %s%s\n", color, mark, t.ID, t.Content, reset)
	}
	return b.String()
}

func formatCheckpoints(cps []session.Checkpoint) string {
	if len(cps) == 0 {
		return dim + "no checkpoints yet — they are taken at the start of each turn in a git repo" + reset + "\n"
	}
	var b strings.Builder
	for _, cp := range cps {
		head := cp.Head
		if len(head) > 7 {
			head = head[:7]
		}
		prompt := cp.Prompt
		if prompt == "" {
			prompt = "(untitled)"
		}
		fmt.Fprintf(&b, "  %s%3d%s  %s  %s %-9s %2d msgs  %s\n",
			cyan, cp.N, reset, dim, head, cp.Branch, cp.Messages, reset)
		fmt.Fprintf(&b, "      %s\n", prompt)
	}
	fmt.Fprintf(&b, "%s/undo restores #%d; /rewind <n> restores an earlier one%s\n",
		dim, cps[len(cps)-1].N, reset)
	return b.String()
}

func (t *turnUI) printFooter(elapsed time.Duration, before, after, window int) {
	fmt.Printf("\n%s%s%s\n", dim, turnFooter(t.tools, elapsed, before, after, window), reset)
}

// notifyAfter is how long a turn must run before we ding the terminal: short
// turns finish while the user is still watching; long ones are when people
// walk away.
const notifyAfter = 8 * time.Second

// notifyTurnDone rings the bell and sends a terminal notification for a long
// turn. OSC 9 is iTerm2/WezTerm/Windows Terminal; unknown sequences are ignored.
func notifyTurnDone(elapsed time.Duration) {
	if elapsed < notifyAfter || !isTerminal(os.Stdout) {
		return
	}
	fmt.Print("\a")
	fmt.Print("\033]9;cubcoder: turn finished\a")
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func head(s string, n int) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = append(lines[:n], fmt.Sprintf("… (%d more lines)", len(lines)-n))
	}
	return lines
}
