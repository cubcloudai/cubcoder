package agent

import (
	"context"
	"strings"
	"testing"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

func TestApplyTodosMergeAndReplace(t *testing.T) {
	cur := []Todo{{ID: "1", Content: "a", Status: TodoPending}}
	got, err := ApplyTodos(cur, []Todo{{ID: "1", Content: "a", Status: TodoInProgress}, {ID: "2", Content: "b", Status: TodoPending}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Status != TodoInProgress || got[1].ID != "2" {
		t.Fatalf("merge = %+v", got)
	}

	got, err = ApplyTodos(got, []Todo{{ID: "x", Content: "only", Status: TodoPending}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "x" {
		t.Fatalf("replace = %+v", got)
	}
}

func TestApplyTodosOneInProgress(t *testing.T) {
	got, err := ApplyTodos(nil, []Todo{
		{ID: "1", Content: "a", Status: TodoInProgress},
		{ID: "2", Content: "b", Status: TodoInProgress},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Status != TodoPending || got[1].Status != TodoInProgress {
		t.Fatalf("last in_progress should win: %+v", got)
	}
}

func TestApplyTodosRejectsBad(t *testing.T) {
	if _, err := ApplyTodos(nil, []Todo{{ID: "", Content: "a", Status: TodoPending}}, false); err == nil {
		t.Error("empty id")
	}
	if _, err := ApplyTodos(nil, []Todo{{ID: "1", Content: "", Status: TodoPending}}, false); err == nil {
		t.Error("empty content")
	}
	if _, err := ApplyTodos(nil, []Todo{{ID: "1", Content: "a", Status: "nope"}}, false); err == nil {
		t.Error("bad status")
	}
	if _, err := ApplyTodos(nil, []Todo{
		{ID: "1", Content: "a", Status: TodoPending},
		{ID: "1", Content: "b", Status: TodoPending},
	}, false); err == nil {
		t.Error("duplicate id")
	}
}

func TestFormatTodos(t *testing.T) {
	s := FormatTodos([]Todo{
		{ID: "1", Content: "inspect", Status: TodoCompleted},
		{ID: "2", Content: "edit", Status: TodoInProgress},
		{ID: "3", Content: "test", Status: TodoPending},
	})
	if !strings.Contains(s, "[x] 1 inspect") || !strings.Contains(s, "[~] 2 edit") || !strings.Contains(s, "[ ] 3 test") {
		t.Fatalf("got %q", s)
	}
}

func TestTodoWriteTool(t *testing.T) {
	a := New(&stubProvider{}, tools.New())
	tl, ok := a.Tools.Get("todo_write")
	if !ok {
		t.Fatal("todo_write not registered")
	}
	out, err := tl.Run(context.Background(), `{"todos":[{"id":"1","content":"inspect","status":"pending"}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[ ] 1 inspect") {
		t.Fatalf("result = %q", out)
	}
	if len(a.Todos()) != 1 {
		t.Fatalf("todos = %+v", a.Todos())
	}
	if _, err := tl.Run(context.Background(), `{"merge":false,"todos":[]}`); err != nil {
		t.Fatal(err)
	}
	if len(a.Todos()) != 0 {
		t.Fatalf("cleared todos = %+v", a.Todos())
	}
}

func TestPlanModeBlocksWrites(t *testing.T) {
	stub := &stubProvider{responses: []*provider.Response{
		{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "write_file", Args: `{"path":"x.go","content":"x"}`}}},
		{Content: "here is the plan"},
	}}
	a := New(stub, tools.New())
	a.Confirm = func(string, string) bool { return true }
	if err := a.RunPlan(context.Background(), "add a health endpoint"); err != nil {
		t.Fatal(err)
	}
	var sawBlock bool
	for _, m := range a.Messages() {
		if m.Role == "tool" && strings.Contains(m.Content, "plan mode is read-only") {
			sawBlock = true
		}
	}
	if !sawBlock {
		t.Fatal("write_file in plan mode should be refused")
	}
}

func TestTruncateMessages(t *testing.T) {
	a := New(&stubProvider{}, tools.New())
	a.messages = append(a.messages,
		provider.Message{Role: "user", Content: "one"},
		provider.Message{Role: "assistant", Content: "ok"},
		provider.Message{Role: "user", Content: "two"},
	)
	a.TruncateMessages(2)
	if a.MessageCount() != 2 || a.messages[1].Content != "one" {
		t.Fatalf("messages = %+v", a.messages)
	}
}

func TestPlanModeSchemasHideWrites(t *testing.T) {
	a := New(&stubProvider{}, tools.New())
	a.planMode = true
	var todo bool
	for _, s := range a.schemas() {
		if planBlocked(s.Name) {
			t.Errorf("plan mode advertised %s", s.Name)
		}
		if s.Name == "todo_write" {
			todo = true
		}
	}
	if !todo {
		t.Error("todo_write must stay available in plan mode")
	}
}

func TestResetClearsTodos(t *testing.T) {
	a := New(&stubProvider{}, tools.New())
	a.SetTodos([]Todo{{ID: "1", Content: "a", Status: TodoPending}})
	a.Reset()
	if len(a.Todos()) != 0 {
		t.Fatal("reset should drop the plan")
	}
}
