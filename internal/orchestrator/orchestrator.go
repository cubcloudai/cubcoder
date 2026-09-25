// Package orchestrator runs several cubcoder agents concurrently on one
// project. A lead agent decomposes a goal and delegates subtasks; each subtask
// runs as a background worker agent in its own git worktree on its own branch,
// so concurrent writers never clobber each other. When workers finish, their
// work is committed to their branch and the lead reviews and merges it.
package orchestrator

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/tools"
)

// LeadSystemPrompt instructs the lead agent on the delegate/wait/review/merge
// workflow. Workers run the default cubcoder prompt; only the lead orchestrates.
const LeadSystemPrompt = `You are the lead engineer in cubcoder. You coordinate a team of worker agents to build software faster by working in parallel.

You have the normal coding tools (read_file, search_code, git_diff, write_file, apply_patch, edit_file, run_command, todo_write), which operate on the main repository, plus orchestration tools:
- delegate_task(name, task): start a worker agent on a subtask. It runs in the background, in its own git worktree on branch cubcoder/<name>, isolated from the other workers. Returns immediately. Use a short, distinct, filesystem-safe name (e.g. "frontend", "db").
- wait_for_agents(ids): block until the given workers finish (omit ids to wait for all running workers). Returns each worker's final status and summary.
- check_agents(): a non-blocking status snapshot of all workers.
- read_agent_log(id): the full log of one worker, for debugging a failure.

Workflow:
1. For a multi-step goal, call todo_write with the overall plan before delegating or editing.
2. Break the goal into subtasks that touch DIFFERENT files/directories so their work merges cleanly (e.g. a frontend in web/ and a database layer in db/). Independent subtasks are the whole point — avoid two workers editing the same files.
3. Call delegate_task once per subtask. Issue them together so they run concurrently.
4. Call wait_for_agents to collect results.
5. A finished worker's changes are committed to its branch cubcoder/<name>. Review each with git_diff(ref="cubcoder/<name>"), then merge the good ones into the current branch with run_command (e.g. "git merge --no-edit cubcoder/<name>"). Resolve any conflicts.
6. If a worker failed, read_agent_log to see why; fix it yourself or delegate a follow-up.
7. Give a short final summary: what each worker built and which branches you merged.

For small tasks you can just do the work yourself with the normal tools. Delegate when subtasks are genuinely independent and parallelizable.`

// Status is a worker's lifecycle state.
type Status string

const (
	StatusRunning Status = "running"
	StatusDone    Status = "done"
	StatusFailed  Status = "failed"
	StatusStopped Status = "stopped"
)

// Worker is one background agent running a subtask in its own worktree.
type Worker struct {
	ID       string
	Name     string
	Task     string
	Branch   string
	Worktree string
	Started  time.Time

	done   chan struct{}
	cancel context.CancelFunc

	mu       sync.Mutex
	status   Status
	summary  string // final assistant text
	errMsg   string
	finished time.Time
	log      bytes.Buffer
	lastText bytes.Buffer // accumulates current assistant text; reset on each tool call
	lastLine string       // most recent action, for the status view
}

// Snapshot is an immutable view of a worker for display.
type Snapshot struct {
	ID, Name, Task, Branch, Worktree string
	Status                           Status
	Summary, ErrMsg, LastLine        string
	Elapsed                          time.Duration
}

func (w *Worker) snapshot() Snapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	end := w.finished
	if end.IsZero() {
		end = time.Now()
	}
	return Snapshot{
		ID: w.ID, Name: w.Name, Task: w.Task, Branch: w.Branch, Worktree: w.Worktree,
		Status: w.status, Summary: w.summary, ErrMsg: w.errMsg, LastLine: w.lastLine,
		Elapsed: end.Sub(w.Started).Round(time.Second),
	}
}

// agent output hooks → captured into the worker's log (workers are silent to
// stdout so concurrent output never interleaves on the terminal).
func (w *Worker) onText(d string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log.WriteString(d)
	w.lastText.WriteString(d)
}
func (w *Worker) onToolCall(name, args string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	line := "→ " + name
	if brief := toolBrief(name, args); brief != "" {
		line += " " + brief
	}
	fmt.Fprintf(&w.log, "\n%s\n", line)
	w.lastLine = line
	w.lastText.Reset() // text before a tool call isn't the final answer
}
func (w *Worker) onToolResult(name, result string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(&w.log, "  %s\n", oneLine(truncate(result, 160)))
}

func (w *Worker) appendLog(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log.WriteString(s)
}

func (w *Worker) finish(runErr error) {
	w.mu.Lock()
	w.finished = time.Now()
	switch {
	case w.status == StatusStopped:
		// keep: it was cancelled by the user
	case runErr != nil:
		w.status = StatusFailed
		w.errMsg = runErr.Error()
	default:
		w.status = StatusDone
		w.summary = strings.TrimSpace(w.lastText.String())
	}
	w.mu.Unlock()
	close(w.done)
}

// WorkerFunc builds a bare worker agent rooted at worktree: its own provider,
// a tool registry scoped to worktree, and auto-approve (background workers
// can't prompt). The Coordinator attaches logging hooks afterward. Supplied by
// main so this package stays free of config/provider construction.
type WorkerFunc func(worktree string) (*agent.Agent, error)

// Coordinator owns worker lifecycle and the worktrees they run in.
type Coordinator struct {
	repoRoot string
	make     WorkerFunc

	// OnProgress, if set, is called with a snapshot of every worker on a fixed
	// interval while Wait blocks, so a UI can show live progress instead of a
	// frozen spinner. It runs on a single goroutine that Wait joins before
	// returning, so the callback may write to stdout without racing the caller.
	OnProgress func([]Snapshot)

	mu      sync.Mutex
	workers map[string]*Worker
	order   []string
	seq     int
}

// progressInterval is how often Wait repaints worker progress. A var so tests
// can shrink it.
var progressInterval = time.Second

func New(repoRoot string, make WorkerFunc) *Coordinator {
	return &Coordinator{repoRoot: repoRoot, make: make, workers: map[string]*Worker{}}
}

// Delegate creates a git worktree on a fresh branch, builds a worker agent
// rooted there, and runs the task in the background. It returns as soon as the
// worker is launched.
func (c *Coordinator) Delegate(name, task string) (*Worker, error) {
	name = sanitize(name)

	c.mu.Lock()
	c.seq++
	id := fmt.Sprintf("w%d", c.seq)
	c.mu.Unlock()

	dir, err := os.MkdirTemp("", "cubcoder-"+name+"-")
	if err != nil {
		return nil, fmt.Errorf("create worktree dir: %w", err)
	}
	// Create the worktree on a new branch off the current HEAD. If the friendly
	// branch name is taken, fall back to a unique one.
	branch := "cubcoder/" + name
	if out, err := c.git("", "worktree", "add", dir, "-b", branch); err != nil {
		branch = "cubcoder/" + name + "-" + id
		if out2, err2 := c.git("", "worktree", "add", dir, "-b", branch); err2 != nil {
			os.RemoveAll(dir)
			return nil, fmt.Errorf("git worktree add failed: %s", strings.TrimSpace(out+out2))
		}
	}

	ag, err := c.make(dir)
	if err != nil {
		c.git("", "worktree", "remove", "--force", dir)
		os.RemoveAll(dir)
		return nil, fmt.Errorf("build worker: %w", err)
	}

	w := &Worker{
		ID: id, Name: name, Task: task, Branch: branch, Worktree: dir,
		Started: time.Now(), status: StatusRunning, done: make(chan struct{}),
		lastLine: "thinking",
	}
	ag.OnText = w.onText
	ag.OnToolCall = w.onToolCall
	ag.OnToolResult = w.onToolResult

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	c.mu.Lock()
	c.workers[id] = w
	c.order = append(c.order, id)
	c.mu.Unlock()

	go func() {
		runErr := ag.Run(ctx, task)
		if runErr == nil {
			c.commitWork(w)
		}
		w.finish(runErr)
	}()
	return w, nil
}

// commitWork captures the worker's changes on its branch so the lead can review
// and merge them. A worker that wrote nothing commits nothing (ignored).
func (c *Coordinator) commitWork(w *Worker) {
	if out, err := c.git(w.Worktree, "add", "-A"); err != nil {
		w.appendLog("\n[commit] git add failed: " + strings.TrimSpace(out) + "\n")
		return
	}
	msg := "cubcoder(" + w.Name + "): " + oneLine(truncate(w.Task, 60))
	out, err := c.git(w.Worktree, "commit", "-m", msg)
	if err != nil {
		// Most commonly "nothing to commit"; record it without treating as fatal.
		w.appendLog("\n[commit] " + strings.TrimSpace(out) + "\n")
		return
	}
	w.appendLog("\n[commit] " + strings.TrimSpace(out) + "\n")
}

// Wait blocks until the given workers reach a terminal state (or ctx is done).
// With no ids, it waits for every currently-running worker. Returns snapshots.
// While it blocks, it repaints OnProgress on a ticker so the user sees each
// worker's latest action instead of a stalled spinner.
func (c *Coordinator) Wait(ctx context.Context, ids []string) []Snapshot {
	ws := c.select_(ids)
	stopProgress := c.startProgress(len(ws) > 0)
	defer stopProgress()
	for _, w := range ws {
		select {
		case <-w.done:
		case <-ctx.Done():
			return snapshots(ws)
		}
	}
	return snapshots(ws)
}

// startProgress launches the OnProgress repaint loop and returns a function
// that stops it and joins its goroutine (so no stray repaint races whatever the
// caller prints next). When there is nothing to show — no callback or no
// workers — it returns a no-op.
func (c *Coordinator) startProgress(active bool) func() {
	if c.OnProgress == nil || !active {
		return func() {}
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(progressInterval)
		defer t.Stop()
		c.OnProgress(c.List()) // paint immediately, don't wait a full tick
		for {
			select {
			case <-stop:
				c.OnProgress(c.List()) // final state before handing the terminal back
				return
			case <-t.C:
				c.OnProgress(c.List())
			}
		}
	}()
	return func() { close(stop); <-finished }
}

// select_ resolves ids to workers; empty ids means all running workers.
func (c *Coordinator) select_(ids []string) []*Worker {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ws []*Worker
	if len(ids) == 0 {
		for _, id := range c.order {
			if w := c.workers[id]; w.snapshotStatus() == StatusRunning {
				ws = append(ws, w)
			}
		}
		return ws
	}
	for _, id := range ids {
		if w, ok := c.workers[strings.TrimSpace(id)]; ok {
			ws = append(ws, w)
		}
	}
	return ws
}

func (w *Worker) snapshotStatus() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

// List returns a snapshot of every worker, in creation order.
func (c *Coordinator) List() []Snapshot {
	c.mu.Lock()
	ws := make([]*Worker, 0, len(c.order))
	for _, id := range c.order {
		ws = append(ws, c.workers[id])
	}
	c.mu.Unlock()
	return snapshots(ws)
}

// Get returns one worker's snapshot.
func (c *Coordinator) Get(id string) (Snapshot, bool) {
	c.mu.Lock()
	w, ok := c.workers[strings.TrimSpace(id)]
	c.mu.Unlock()
	if !ok {
		return Snapshot{}, false
	}
	return w.snapshot(), true
}

// Log returns one worker's full captured log.
func (c *Coordinator) Log(id string) (string, bool) {
	c.mu.Lock()
	w, ok := c.workers[strings.TrimSpace(id)]
	c.mu.Unlock()
	if !ok {
		return "", false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.log.String(), true
}

// Stop cancels a running worker. Its goroutine unwinds and is marked stopped.
func (c *Coordinator) Stop(id string) error {
	c.mu.Lock()
	w, ok := c.workers[strings.TrimSpace(id)]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("no agent %q", id)
	}
	w.mu.Lock()
	running := w.status == StatusRunning
	if running {
		w.status = StatusStopped
	}
	cancel := w.cancel
	w.mu.Unlock()
	if !running {
		return fmt.Errorf("agent %q is not running", id)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// Count reports how many workers are currently running.
func (c *Coordinator) Count() (running int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range c.order {
		if c.workers[id].snapshotStatus() == StatusRunning {
			running++
		}
	}
	return running
}

// Cleanup stops running workers and removes every worktree and its branch.
// Returns human-readable lines describing what happened.
func (c *Coordinator) Cleanup() []string {
	c.mu.Lock()
	ws := make([]*Worker, 0, len(c.order))
	for _, id := range c.order {
		ws = append(ws, c.workers[id])
	}
	c.mu.Unlock()

	var msgs []string
	for _, w := range ws {
		if w.snapshotStatus() == StatusRunning {
			_ = c.Stop(w.ID)
			<-w.done // let the run unwind before removing its worktree
		}
		if out, err := c.git("", "worktree", "remove", "--force", w.Worktree); err != nil {
			os.RemoveAll(w.Worktree)
			msgs = append(msgs, fmt.Sprintf("%s: worktree remove warning: %s", w.Name, strings.TrimSpace(out)))
		}
		if out, err := c.git("", "branch", "-D", w.Branch); err != nil {
			msgs = append(msgs, fmt.Sprintf("%s: branch -D warning: %s", w.Name, strings.TrimSpace(out)))
		} else {
			msgs = append(msgs, fmt.Sprintf("%s: removed worktree and branch %s", w.Name, w.Branch))
		}
	}
	c.mu.Lock()
	c.workers = map[string]*Worker{}
	c.order = nil
	c.mu.Unlock()
	return msgs
}

// git runs a git command. An empty dir runs it in the repo root.
func (c *Coordinator) git(dir string, args ...string) (string, error) {
	if dir == "" {
		dir = c.repoRoot
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func snapshots(ws []*Worker) []Snapshot {
	out := make([]Snapshot, len(ws))
	for i, w := range ws {
		out[i] = w.snapshot()
	}
	return out
}

// sanitize turns a free-form name into a filesystem- and branch-safe slug.
func sanitize(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "agent"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// toolBrief pulls a scannable noun out of tool JSON (a path, a command) so
// lastLine isn't a blob of arguments. Keep in sync with main.toolCallSummary.
func toolBrief(name, args string) string {
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return oneLine(truncate(args, 80))
	}
	str := func(k string) string {
		s, _ := obj[k].(string)
		return s
	}
	switch name {
	case "read_file", "write_file", "edit_file", "list_dir":
		return str("path")
	case "apply_patch":
		return strings.Join(tools.PatchPaths(str("patch")), ", ")
	case "run_command":
		return str("command")
	case "search_code":
		return str("pattern")
	case "git_diff":
		return str("ref") + str("path")
	case "web_search":
		return str("query")
	case "fetch_url":
		return str("url")
	case "todo_write":
		if arr, ok := obj["todos"].([]any); ok {
			return fmt.Sprintf("%d items", len(arr))
		}
		return ""
	}
	return ""
}

// Record is a persistable snapshot of a worker, including its log. Running
// workers are recorded as stopped: they cannot be restarted from a session.
type Record struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Task     string    `json:"task"`
	Branch   string    `json:"branch"`
	Worktree string    `json:"worktree"`
	Status   Status    `json:"status"`
	Summary  string    `json:"summary,omitempty"`
	ErrMsg   string    `json:"err,omitempty"`
	LastLine string    `json:"last_line,omitempty"`
	Log      string    `json:"log,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished,omitempty"`
}

const recordLogCap = 64 * 1024

// Records exports every worker for session persistence.
func (c *Coordinator) Records() []Record {
	c.mu.Lock()
	ws := make([]*Worker, 0, len(c.order))
	for _, id := range c.order {
		ws = append(ws, c.workers[id])
	}
	c.mu.Unlock()
	if len(ws) == 0 {
		return nil
	}
	out := make([]Record, 0, len(ws))
	for _, w := range ws {
		w.mu.Lock()
		status := w.status
		errMsg := w.errMsg
		finished := w.finished
		summary := w.summary
		lastLine := w.lastLine
		log := w.log.String()
		w.mu.Unlock()
		if status == StatusRunning {
			status = StatusStopped
			if errMsg == "" {
				errMsg = "session ended while running"
			}
			if finished.IsZero() {
				finished = time.Now()
			}
		}
		if len(log) > recordLogCap {
			keep := recordLogCap / 2
			log = log[:keep] + "\n... [log truncated] ...\n" + log[len(log)-keep:]
		}
		out = append(out, Record{
			ID: w.ID, Name: w.Name, Task: w.Task, Branch: w.Branch, Worktree: w.Worktree,
			Status: status, Summary: summary, ErrMsg: errMsg, LastLine: lastLine,
			Log: log, Started: w.Started, Finished: finished,
		})
	}
	return out
}

// Restore loads worker records from a saved session so /agents and
// read_agent_log still work. IDs that already exist (a running worker in this
// process) are left alone. Returns how many records were added.
func (c *Coordinator) Restore(recs []Record) int {
	if len(recs) == 0 {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	added := 0
	for _, rec := range recs {
		if rec.ID == "" {
			continue
		}
		if _, exists := c.workers[rec.ID]; exists {
			continue
		}
		status := rec.Status
		if status == StatusRunning || status == "" {
			status = StatusStopped
		}
		w := &Worker{
			ID: rec.ID, Name: rec.Name, Task: rec.Task, Branch: rec.Branch, Worktree: rec.Worktree,
			Started: rec.Started, status: status, summary: rec.Summary, errMsg: rec.ErrMsg,
			lastLine: rec.LastLine, finished: rec.Finished, done: make(chan struct{}),
		}
		if rec.Finished.IsZero() {
			w.finished = rec.Started
		}
		w.log.WriteString(rec.Log)
		close(w.done)
		c.workers[rec.ID] = w
		c.order = append(c.order, rec.ID)
		if n := workerSeq(rec.ID); n > c.seq {
			c.seq = n
		}
		added++
	}
	return added
}

func workerSeq(id string) int {
	if !strings.HasPrefix(id, "w") {
		return 0
	}
	n, err := strconv.Atoi(id[1:])
	if err != nil {
		return 0
	}
	return n
}
