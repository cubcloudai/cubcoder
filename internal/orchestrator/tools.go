package orchestrator

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"cubcoder/internal/tools"
)

// Tools returns the orchestration tools bound to c, for the lead agent's
// registry. Workers do not receive these — delegation is one level deep.
func Tools(c *Coordinator) []*tools.Tool {
	return []*tools.Tool{
		delegateTool(c),
		waitTool(c),
		checkTool(c),
		logTool(c),
	}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func delegateTool(c *Coordinator) *tools.Tool {
	return &tools.Tool{
		Name:        "delegate_task",
		Description: "Start a background worker agent on a subtask. It runs in its own git worktree on branch cubcoder/<name>, isolated from other workers, and returns immediately. Issue several to run them in parallel, then call wait_for_agents.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name": strProp("short, distinct, filesystem-safe name for the worker and its branch (e.g. \"frontend\", \"db\")"),
				"task": strProp("the full subtask for the worker, written as a self-contained instruction. The worker only sees this text."),
			},
			"required": []string{"name", "task"},
		},
		Run: func(_ context.Context, args string) (string, error) {
			var a struct{ Name, Task string }
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Task) == "" {
				return "", fmt.Errorf("task is required")
			}
			w, err := c.Delegate(a.Name, a.Task)
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("started worker %s (%q) on branch %s in worktree %s. It is running in the background; call wait_for_agents to collect its result.",
				w.ID, w.Name, w.Branch, w.Worktree), nil
		},
	}
}

func waitTool(c *Coordinator) *tools.Tool {
	return &tools.Tool{
		Name:        "wait_for_agents",
		Quiet:       true, // renders a live worker-status block while it blocks
		Description: "Block until the given workers finish (omit ids to wait for all running workers). Returns each worker's final status, branch, and summary. A finished worker's changes are committed to its branch for you to review with git_diff(ref=\"cubcoder/<name>\") and merge.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "worker ids to wait for (e.g. [\"w1\",\"w2\"]); omit to wait for all running workers",
				},
			},
		},
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				IDs []string `json:"ids"`
			}
			_ = json.Unmarshal([]byte(args), &a)
			// The turn context, so Esc breaks out of the wait; the workers keep
			// running and can be waited on again.
			snaps := c.Wait(ctx, a.IDs)
			if len(snaps) == 0 {
				return "No matching running workers.", nil
			}
			var b strings.Builder
			for _, s := range snaps {
				writeResult(&b, s)
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

func checkTool(c *Coordinator) *tools.Tool {
	return &tools.Tool{
		Name:        "check_agents",
		Description: "Non-blocking snapshot of all workers: id, name, status, elapsed time, and last action. Use to poll without waiting.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Run: func(context.Context, string) (string, error) {
			snaps := c.List()
			if len(snaps) == 0 {
				return "No workers have been started.", nil
			}
			var b strings.Builder
			for _, s := range snaps {
				fmt.Fprintf(&b, "%s %-12s %-8s %s", s.ID, s.Name, s.Status, s.Elapsed)
				if s.LastLine != "" {
					fmt.Fprintf(&b, "  %s", s.LastLine)
				}
				b.WriteByte('\n')
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

func logTool(c *Coordinator) *tools.Tool {
	return &tools.Tool{
		Name:        "read_agent_log",
		Description: "Return the full captured log of one worker (its streamed text and tool calls). Use to debug a failed worker.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"id": strProp("worker id, e.g. \"w1\"")},
			"required":   []string{"id"},
		},
		Run: func(_ context.Context, args string) (string, error) {
			var a struct{ ID string }
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return "", err
			}
			log, ok := c.Log(a.ID)
			if !ok {
				return "", fmt.Errorf("no agent %q", a.ID)
			}
			if strings.TrimSpace(log) == "" {
				return "(no output yet)", nil
			}
			return log, nil
		},
	}
}

// writeResult renders one finished worker for the wait_for_agents result.
func writeResult(b *strings.Builder, s Snapshot) {
	fmt.Fprintf(b, "[%s %q] %s on branch %s (%s)\n", s.ID, s.Name, s.Status, s.Branch, s.Elapsed)
	switch s.Status {
	case StatusDone:
		if s.Summary != "" {
			fmt.Fprintf(b, "%s\n", s.Summary)
		} else {
			fmt.Fprintf(b, "(finished with no summary text)\n")
		}
	case StatusFailed:
		fmt.Fprintf(b, "FAILED: %s\n(use read_agent_log(%q) for the full log)\n", s.ErrMsg, s.ID)
	case StatusStopped:
		fmt.Fprintf(b, "(stopped by the user)\n")
	case StatusRunning:
		fmt.Fprintf(b, "(still running)\n")
	}
	b.WriteByte('\n')
}
