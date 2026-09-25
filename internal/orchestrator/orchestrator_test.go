package orchestrator_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

// fakeProvider drives a worker through one write_file tool call, then a final
// answer. Each worker gets its own instance (state isn't shared).
type fakeProvider struct{ calls int }

func (f *fakeProvider) Name() string       { return "fake" }
func (f *fakeProvider) ContextWindow() int { return 100000 }
func (f *fakeProvider) Chat(_ context.Context, _ []provider.Message, _ []provider.ToolSchema, onText provider.StreamFunc) (*provider.Response, error) {
	f.calls++
	if f.calls == 1 {
		return &provider.Response{ToolCalls: []provider.ToolCall{{
			ID:   "1",
			Name: "write_file",
			Args: `{"path":"out.txt","content":"worker was here"}`,
		}}}, nil
	}
	if onText != nil {
		onText("built it")
	}
	return &provider.Response{Content: "built it"}, nil
}

func gitInit(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t.test"},
		{"config", "user.name", "t"},
		{"config", "commit.gpgsign", "false"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func TestDelegateWaitCommit(t *testing.T) {
	repo := gitInit(t)
	make := func(worktree string) (*agent.Agent, error) {
		return agent.New(&fakeProvider{}, tools.NewIn(worktree)), nil
	}
	c := orchestrator.New(repo, make)

	w, err := c.Delegate("tester", "write a file")
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	if w.Branch != "cubcoder/tester" {
		t.Errorf("branch = %q", w.Branch)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	snaps := c.Wait(ctx, nil)
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots", len(snaps))
	}
	s := snaps[0]
	if s.Status != orchestrator.StatusDone {
		t.Fatalf("status = %s (err: %s)", s.Status, s.ErrMsg)
	}
	if s.Summary != "built it" {
		t.Errorf("summary = %q", s.Summary)
	}

	// The worker wrote its file inside its own worktree, not the main repo.
	if _, err := os.Stat(filepath.Join(s.Worktree, "out.txt")); err != nil {
		t.Errorf("worker output missing from worktree: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "out.txt")); !os.IsNotExist(err) {
		t.Errorf("worker output leaked into main repo")
	}

	// commitWork captured the change on the worker's branch.
	cmd := exec.Command("git", "log", "--oneline", s.Branch)
	cmd.Dir = repo
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git log %s: %v: %s", s.Branch, err, out)
	}
	if want := "cubcoder(tester)"; !strings.Contains(string(out), want) {
		t.Errorf("branch %s missing worker commit; log:\n%s", s.Branch, out)
	}

	if !strings.Contains(s.LastLine, "write_file") || strings.Contains(s.LastLine, `"path"`) {
		t.Errorf("lastLine should be a brief tool line, got %q", s.LastLine)
	}

	recs := c.Records()
	if len(recs) != 1 || recs[0].Status != orchestrator.StatusDone || recs[0].Log == "" {
		t.Fatalf("Records = %+v", recs)
	}

	// Cleanup removes the worktree and branch.
	c.Cleanup()
	if _, err := os.Stat(s.Worktree); !os.IsNotExist(err) {
		t.Errorf("worktree not removed: %v", err)
	}
}

// TestWaitOnProgress verifies Wait paints OnProgress while it blocks and that
// the final paint reflects the finished worker. Wait joins the progress
// goroutine before returning, so reading the captured snapshots here is safe.
func TestWaitOnProgress(t *testing.T) {
	repo := gitInit(t)
	make := func(worktree string) (*agent.Agent, error) {
		return agent.New(&fakeProvider{}, tools.NewIn(worktree)), nil
	}
	c := orchestrator.New(repo, make)

	var paints [][]orchestrator.Snapshot
	c.OnProgress = func(s []orchestrator.Snapshot) { paints = append(paints, s) }

	if _, err := c.Delegate("tester", "write a file"); err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Wait(ctx, nil)
	defer c.Cleanup()

	if len(paints) == 0 {
		t.Fatal("OnProgress was never called during Wait")
	}
	last := paints[len(paints)-1]
	if len(last) != 1 || last[0].Status != orchestrator.StatusDone {
		t.Fatalf("final paint = %+v, want one done worker", last)
	}
}

// jailbreakProvider asks write_file to land outside the worktree. The worker
// auto-approves, so the path jail — not a prompt — has to stop it.
type jailbreakProvider struct {
	calls int
	path  string
}

func (f *jailbreakProvider) Name() string       { return "fake" }
func (f *jailbreakProvider) ContextWindow() int { return 100000 }
func (f *jailbreakProvider) Chat(_ context.Context, _ []provider.Message, _ []provider.ToolSchema, onText provider.StreamFunc) (*provider.Response, error) {
	f.calls++
	if f.calls == 1 {
		args, _ := json.Marshal(map[string]string{"path": f.path, "content": "pwned"})
		return &provider.Response{ToolCalls: []provider.ToolCall{{
			ID: "1", Name: "write_file", Args: string(args),
		}}}, nil
	}
	if onText != nil {
		onText("gave up")
	}
	return &provider.Response{Content: "gave up"}, nil
}

func TestWorkerCannotWriteOutsideWorktree(t *testing.T) {
	repo := gitInit(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "pwned.txt")
	make := func(worktree string) (*agent.Agent, error) {
		return agent.New(&jailbreakProvider{path: target}, tools.NewIn(worktree)), nil
	}
	c := orchestrator.New(repo, make)
	w, err := c.Delegate("jail", "write outside")
	if err != nil {
		t.Fatalf("Delegate: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.Wait(ctx, nil)
	defer c.Cleanup()

	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("worker wrote outside its worktree: %s", target)
	}
	log, ok := c.Log(w.ID)
	if !ok {
		t.Fatal("missing worker log")
	}
	if !strings.Contains(log, "outside the working directory") {
		t.Errorf("worker log should report the jail; got:\n%s", log)
	}
}

func TestRestoreWorkers(t *testing.T) {
	repo := gitInit(t)
	c := orchestrator.New(repo, func(string) (*agent.Agent, error) {
		t.Fatal("restore must not spawn a new agent")
		return nil, nil
	})
	n := c.Restore([]orchestrator.Record{
		{ID: "w3", Name: "old", Task: "t", Status: orchestrator.StatusRunning, Log: "hello from w3"},
		{ID: "w1", Name: "done", Task: "t", Status: orchestrator.StatusDone, Log: "done log"},
	})
	if n != 2 {
		t.Fatalf("Restore added %d, want 2", n)
	}
	s, ok := c.Get("w3")
	if !ok || s.Status != orchestrator.StatusStopped {
		t.Fatalf("running record must restore as stopped: %+v", s)
	}
	log, ok := c.Log("w3")
	if !ok || log != "hello from w3" {
		t.Fatalf("log = %q", log)
	}
	// Existing ID is left alone.
	if n := c.Restore([]orchestrator.Record{{ID: "w3", Log: "replaced"}}); n != 0 {
		t.Fatalf("duplicate restore added %d", n)
	}
	if log, _ := c.Log("w3"); log != "hello from w3" {
		t.Fatalf("existing worker was overwritten: %q", log)
	}
}
