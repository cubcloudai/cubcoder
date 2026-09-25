package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cubcoder/internal/provider"
	"cubcoder/internal/tools"
)

// Todo is one step in the session plan.
type Todo struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Status  string `json:"status"`
}

const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
	TodoCancelled  = "cancelled"
	maxTodos       = 30
)

const planUserPrefix = "PLAN MODE: Explore with read-only tools and write a step-by-step plan using todo_write. Do not edit files, run commands, or start workers. Stop after the plan is written.\n\n"

// GoPrompt is the user turn /go sends to execute the current plan.
const GoPrompt = "Implement the current plan. Work through the todo list in order; mark the current step in_progress, then completed as you finish it. Do not stop until the plan is done or blocked."

// Todos returns a copy of the current plan.
func (a *Agent) Todos() []Todo {
	return append([]Todo(nil), a.todos...)
}

// SetTodos replaces the plan (used when restoring a session or checkpoint).
func (a *Agent) SetTodos(todos []Todo) {
	a.todos = append([]Todo(nil), todos...)
}

// RunPlan is Run with mutating tools hidden: the model can only inspect and
// write a plan. Used by /plan <task>.
func (a *Agent) RunPlan(ctx context.Context, task string) error {
	a.planMode = true
	defer func() { a.planMode = false }()
	return a.Run(ctx, planUserPrefix+task)
}

func todoTool(a *Agent) *tools.Tool {
	return &tools.Tool{
		Name:        "todo_write",
		Description: "Update the session plan. For any task with several steps, write the plan before mutating files, mark the current step in_progress, and complete items as you finish them. merge=true (default) upserts by id; merge=false replaces the whole list.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"todos": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"id":      map[string]any{"type": "string", "description": "stable id, e.g. \"1\" or \"auth\""},
							"content": map[string]any{"type": "string", "description": "the step"},
							"status": map[string]any{
								"type":        "string",
								"description": "pending, in_progress, completed, or cancelled",
								"enum":        []string{"pending", "in_progress", "completed", "cancelled"},
							},
						},
						"required": []string{"id", "content", "status"},
					},
					"description": "todo items to write",
				},
				"merge": map[string]any{
					"type":        "boolean",
					"description": "if true (default), upsert by id; if false, replace the list",
				},
			},
			"required": []string{"todos"},
		},
		Run: func(_ context.Context, args string) (string, error) {
			var in struct {
				Todos []Todo `json:"todos"`
				Merge *bool  `json:"merge"`
			}
			if err := json.Unmarshal([]byte(args), &in); err != nil {
				return "", err
			}
			merge := true
			if in.Merge != nil {
				merge = *in.Merge
			}
			next, err := ApplyTodos(a.todos, in.Todos, merge)
			if err != nil {
				return "", err
			}
			a.todos = next
			if len(a.todos) == 0 {
				return "(plan cleared)", nil
			}
			return FormatTodos(a.todos), nil
		},
	}
}

// ApplyTodos validates incoming items and either upserts them (merge) or
// replaces the list. At most one item stays in_progress — a newly marked
// in_progress step demotes the previous one back to pending.
func ApplyTodos(cur, incoming []Todo, merge bool) ([]Todo, error) {
	norm := make([]Todo, len(incoming))
	seen := map[string]int{}
	for i, t := range incoming {
		id := strings.TrimSpace(t.ID)
		content := strings.TrimSpace(t.Content)
		if id == "" {
			return nil, fmt.Errorf("todo[%d]: id is required", i)
		}
		if content == "" {
			return nil, fmt.Errorf("todo %s: content is required", id)
		}
		st, err := normalizeStatus(t.Status)
		if err != nil {
			return nil, fmt.Errorf("todo %s: %w", id, err)
		}
		if j, ok := seen[id]; ok {
			return nil, fmt.Errorf("duplicate todo id %q (items %d and %d)", id, j, i)
		}
		seen[id] = i
		norm[i] = Todo{ID: id, Content: content, Status: st}
	}

	var out []Todo
	if !merge {
		out = norm
	} else {
		out = append([]Todo(nil), cur...)
		byID := make(map[string]int, len(out))
		for i, t := range out {
			byID[t.ID] = i
		}
		for _, t := range norm {
			if i, ok := byID[t.ID]; ok {
				out[i] = t
			} else {
				byID[t.ID] = len(out)
				out = append(out, t)
			}
		}
	}
	if len(out) > maxTodos {
		return nil, fmt.Errorf("too many todos (%d); max %d", len(out), maxTodos)
	}
	last := -1
	for i, t := range out {
		if t.Status != TodoInProgress {
			continue
		}
		if last >= 0 {
			out[last].Status = TodoPending
		}
		last = i
	}
	return out, nil
}

func normalizeStatus(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case TodoPending, "todo", "open":
		return TodoPending, nil
	case TodoInProgress, "in-progress", "doing", "active":
		return TodoInProgress, nil
	case TodoCompleted, "complete", "done":
		return TodoCompleted, nil
	case TodoCancelled, "canceled", "skipped":
		return TodoCancelled, nil
	default:
		return "", fmt.Errorf("status %q must be pending, in_progress, completed, or cancelled", s)
	}
}

// FormatTodos renders the plan as a checklist the model and the REPL both use.
func FormatTodos(todos []Todo) string {
	if len(todos) == 0 {
		return "(no plan)"
	}
	var b strings.Builder
	for i, t := range todos {
		if i > 0 {
			b.WriteByte('\n')
		}
		mark := "[ ]"
		switch t.Status {
		case TodoInProgress:
			mark = "[~]"
		case TodoCompleted:
			mark = "[x]"
		case TodoCancelled:
			mark = "[-]"
		}
		fmt.Fprintf(&b, "%s %s %s", mark, t.ID, t.Content)
	}
	return b.String()
}

func planBlocked(name string) bool {
	switch name {
	case "write_file", "edit_file", "apply_patch", "run_command",
		"delegate_task", "wait_for_agents":
		return true
	default:
		return false
	}
}

func (a *Agent) schemas() []provider.ToolSchema {
	all := a.Tools.Schemas()
	if !a.planMode {
		return all
	}
	out := make([]provider.ToolSchema, 0, len(all))
	for _, s := range all {
		if planBlocked(s.Name) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// requestMessages is the conversation sent to the model: the stored history
// plus a live plan appendix on the system prompt, so compaction and session
// files never have to keep a duplicate copy.
func (a *Agent) requestMessages() []provider.Message {
	note := a.planNote()
	if note == "" {
		return a.messages
	}
	out := append([]provider.Message(nil), a.messages...)
	if len(out) > 0 && out[0].Role == "system" {
		out[0].Content = a.systemPrompt + note
	}
	return out
}

func (a *Agent) planNote() string {
	var b strings.Builder
	if a.planMode {
		b.WriteString("\n\nYou are in plan mode: read-only tools and todo_write only. Do not edit files or run commands. Write the plan, then stop.")
	}
	if len(a.todos) > 0 {
		b.WriteString("\n\nCurrent plan:\n")
		b.WriteString(FormatTodos(a.todos))
	}
	return b.String()
}
