package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/provider"
)

func setDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CUBCODER_SESSIONS", dir)
	return dir
}

func msgs(user string) []provider.Message {
	return []provider.Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: user, Images: []provider.Image{{MediaType: "image/png", Data: "AAAA"}}},
		{Role: "assistant", ToolCalls: []provider.ToolCall{{ID: "c1", Name: "read_file", Args: `{"path":"x"}`}}},
		{Role: "tool", ToolCallID: "c1", Name: "read_file", Content: "hi"},
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := setDir(t)
	s := New("/work/proj", "m1")
	s.Messages = msgs("--- attached document: a.md ---\nbody\n--- end of document: a.md ---\n\nfix the login bug")
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, s.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", fi.Mode().Perm())
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "fix the login bug" {
		t.Errorf("title = %q", got.Title)
	}
	if len(got.Messages) != 4 || got.Messages[1].Images[0].Data != "AAAA" || got.Messages[2].ToolCalls[0].Args != `{"path":"x"}` {
		t.Errorf("messages did not round-trip: %+v", got.Messages)
	}
	// Prefix load.
	if _, err := Load(s.ID[:8]); err != nil {
		t.Errorf("prefix load: %v", err)
	}
	if _, err := Load("nope"); err == nil {
		t.Error("expected error for unknown id")
	}
}

func TestSaveLoadWorkers(t *testing.T) {
	setDir(t)
	s := New("/work/proj", "m1")
	s.Messages = msgs("delegate the ui")
	s.Workers = []orchestrator.Record{{
		ID: "w1", Name: "frontend", Task: "build the ui", Branch: "cubcoder/frontend",
		Status: orchestrator.StatusDone, Log: "→ write_file web/app.go\nbuilt it",
	}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Workers) != 1 || got.Workers[0].ID != "w1" || !strings.Contains(got.Workers[0].Log, "write_file") {
		t.Fatalf("workers did not round-trip: %+v", got.Workers)
	}
}

func TestSaveLoadTodosAndCheckpoints(t *testing.T) {
	setDir(t)
	s := New("/work/proj", "m1")
	s.Messages = msgs("add a health endpoint")
	s.Todos = []agent.Todo{{ID: "1", Content: "inspect", Status: agent.TodoInProgress}}
	s.Checkpoints = []Checkpoint{{
		N: 1, Head: "abc", Commit: "def", Branch: "main",
		Prompt: "add a health endpoint", Messages: 1,
		Todos: []agent.Todo{{ID: "1", Content: "inspect", Status: agent.TodoPending}},
	}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Todos) != 1 || got.Todos[0].Content != "inspect" {
		t.Fatalf("todos = %+v", got.Todos)
	}
	if len(got.Checkpoints) != 1 || got.Checkpoints[0].Commit != "def" || got.Checkpoints[0].Messages != 1 {
		t.Fatalf("checkpoints = %+v", got.Checkpoints)
	}
	if s.NextCheckpointN() != 2 {
		t.Fatalf("NextCheckpointN = %d", s.NextCheckpointN())
	}
}

func TestEmptySessionNotSaved(t *testing.T) {
	dir := setDir(t)
	s := New("/w", "m")
	s.Messages = []provider.Message{{Role: "system", Content: "sys"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if names, _ := filepath.Glob(filepath.Join(dir, "*.json")); len(names) != 0 {
		t.Errorf("empty session was written: %v", names)
	}
}

func TestLatestPerDirectory(t *testing.T) {
	setDir(t)
	a := New("/a", "m")
	a.Messages = msgs("task a")
	a.ID = "20260101-000000-aaaa"
	b := New("/b", "m")
	b.Messages = msgs("task b")
	b.ID = "20260101-000001-bbbb"
	a2 := New("/a", "m")
	a2.Messages = msgs("task a2")
	a2.ID = "20260101-000002-cccc"
	for _, s := range []*Session{a, b, a2} {
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond) // distinct Updated stamps
	}
	got, err := Latest("/a")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != a2.ID {
		t.Errorf("latest for /a = %s, want %s", got.ID, a2.ID)
	}
	if _, err := Latest("/none"); err != ErrNone {
		t.Errorf("Latest(/none) err = %v, want ErrNone", err)
	}
	list, _ := List()
	if len(list) != 3 || list[0].ID != a2.ID {
		t.Errorf("list order wrong: %+v", list)
	}
}

func TestPrune(t *testing.T) {
	dir := setDir(t)
	for i := 0; i < keep+5; i++ {
		s := New("/w", "m")
		s.ID = time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC).Format("20060102-150405") + "-0000"
		s.Messages = msgs("x")
		if err := s.Save(); err != nil {
			t.Fatal(err)
		}
	}
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(names) != keep {
		t.Errorf("after prune %d files, want %d", len(names), keep)
	}
	if _, err := Load("20260101-000000-0000"); err == nil {
		t.Error("oldest session should have been pruned")
	}
}
