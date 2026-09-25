package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Interactive run_command is a pty, so git/man/less would otherwise wait for
// q. The child must see cat, even when the parent session has PAGER=less.
func TestCommandEnvDisablesPagers(t *testing.T) {
	t.Setenv("PAGER", "less")
	t.Setenv("GIT_PAGER", "less")
	t.Setenv("KEEP_ME", "yes")
	env := commandEnv()
	got := map[string]string{}
	counts := map[string]int{}
	for _, e := range env {
		k, v, ok := strings.Cut(e, "=")
		if !ok {
			continue
		}
		got[k] = v
		counts[k]++
	}
	want := map[string]string{
		"PAGER":         "cat",
		"GIT_PAGER":     "cat",
		"GH_PAGER":      "cat",
		"MANPAGER":      "cat",
		"SYSTEMD_PAGER": "cat",
		"AWS_PAGER":     "",
		"LESS":          "FRX",
		"KEEP_ME":       "yes",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q, want %q", k, got[k], v)
		}
		if counts[k] != 1 {
			t.Errorf("%s appears %d times; want a single override", k, counts[k])
		}
	}
}

func TestRunCommandOverridesParentPager(t *testing.T) {
	t.Setenv("PAGER", "less")
	t.Setenv("GIT_PAGER", "less")
	out, _ := runWith(t, 5*time.Second, 300*time.Millisecond,
		`printf 'PAGER=%s GIT_PAGER=%s LESS=%s\n' "$PAGER" "$GIT_PAGER" "$LESS"`)
	if strings.Contains(out, "less") {
		t.Fatalf("child inherited a pager; got %q", out)
	}
	if !strings.Contains(out, "PAGER=cat") || !strings.Contains(out, "GIT_PAGER=cat") {
		t.Fatalf("PAGER/GIT_PAGER must be cat; got %q", out)
	}
	if !strings.Contains(out, "LESS=FRX") {
		t.Fatalf("LESS must be FRX; got %q", out)
	}
}

// runWith executes the run_command tool with shrunken timeouts and restores
// them afterwards.
func runWith(t *testing.T, timeout, waitDelay time.Duration, command string) (string, time.Duration) {
	t.Helper()
	savedT, savedW := commandTimeout, commandWaitDelay
	commandTimeout, commandWaitDelay = timeout, waitDelay
	t.Cleanup(func() { commandTimeout, commandWaitDelay = savedT, savedW })

	tool := runCommand("", nil)
	start := time.Now()
	out, err := tool.Run(context.Background(), fmt.Sprintf(`{"command": %q}`, command))
	if err != nil {
		t.Fatalf("run_command errored: %v", err)
	}
	return out, time.Since(start)
}

// A background child that inherits the output pipes (a dev server, a forgotten
// `&`) must not block the tool after sh exits. Without WaitDelay this blocked
// until the child died — the multi-minute "running" hang.
func TestRunCommandReturnsDespiteBackgroundPipeHolder(t *testing.T) {
	out, took := runWith(t, 30*time.Second, 300*time.Millisecond, "sleep 30 & echo started")
	if took > 5*time.Second {
		t.Fatalf("tool blocked %v on a background pipe holder; want prompt return", took)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("output captured before the pipes closed must be returned; got %q", out)
	}
	if !strings.Contains(out, "exit_code=0") {
		t.Fatalf("sh exited cleanly, want exit_code=0; got %q", out)
	}
}

// A per-call timeout argument must override the default for that one command,
// and the timeout note must quote the deadline actually used.
func TestRunCommandPerCallTimeout(t *testing.T) {
	savedT, savedW := commandTimeout, commandWaitDelay
	commandTimeout, commandWaitDelay = 30*time.Second, 300*time.Millisecond
	t.Cleanup(func() { commandTimeout, commandWaitDelay = savedT, savedW })

	tool := runCommand("", nil)
	start := time.Now()
	out, err := tool.Run(context.Background(), `{"command": "sleep 30 && echo unreachable", "timeout": 1}`)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("run_command errored: %v", err)
	}
	if took > 5*time.Second {
		t.Fatalf("per-call timeout took %v to enforce; want ~1s", took)
	}
	if !strings.Contains(out, "timed out after 1s") {
		t.Fatalf("timeout note must quote the per-call deadline; got %q", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("command continued past the per-call timeout; got %q", out)
	}
}

// Cancelling the turn context (the user's Esc) must kill a running command
// promptly and say so in the result — before this, a command ran to completion
// (or its 120s timeout) after an interrupt.
func TestRunCommandCancelKillsRunningCommand(t *testing.T) {
	savedW := commandWaitDelay
	commandWaitDelay = 300 * time.Millisecond
	t.Cleanup(func() { commandWaitDelay = savedW })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	tool := runCommand("", nil)
	start := time.Now()
	out, err := tool.Run(ctx, `{"command": "echo started && sleep 30 && echo unreachable"}`)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("run_command errored: %v", err)
	}
	if took > 5*time.Second {
		t.Fatalf("cancel took %v to stop the command; want prompt group kill", took)
	}
	if !strings.Contains(out, "interrupted by the user") {
		t.Fatalf("interrupt must be reported to the model; got %q", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("command continued past the cancel; got %q", out)
	}
}

// On timeout the whole process group must die, including children sh spawned —
// killing only sh left a `node server.js` alive holding the pipes forever.
func TestRunCommandTimeoutKillsProcessGroup(t *testing.T) {
	out, took := runWith(t, 300*time.Millisecond, 300*time.Millisecond, "cd . && sleep 30 && echo unreachable")
	if took > 5*time.Second {
		t.Fatalf("timeout took %v to enforce; want prompt group kill", took)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("timeout must be reported to the model; got %q", out)
	}
	if strings.Contains(out, "unreachable") {
		t.Fatalf("command continued past the kill; got %q", out)
	}
}
