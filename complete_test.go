package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCompleteSlashCommands(t *testing.T) {
	from, repl, matches := completeAt([]rune("/pl"), 3)
	if from != 0 {
		t.Fatalf("from = %d", from)
	}
	if len(matches) != 1 || matches[0] != "/plan" {
		t.Fatalf("matches = %q", matches)
	}
	if repl != "/plan " {
		t.Fatalf("repl = %q want /plan ", repl)
	}

	_, repl, matches = completeAt([]rune("/re"), 3)
	if !contains(matches, "/reset") || !contains(matches, "/resume") || !contains(matches, "/rewind") {
		t.Fatalf("/re matches = %q", matches)
	}
	if repl != "/re" {
		t.Fatalf("common prefix of /reset,/resume,/rewind should stay /re, got %q", repl)
	}
}

func TestCompleteAutoOnOff(t *testing.T) {
	_, repl, matches := completeAt([]rune("/auto o"), 7)
	if !contains(matches, "on") || !contains(matches, "off") {
		t.Fatalf("matches = %q", matches)
	}
	if repl != "o" {
		t.Fatalf("common prefix = %q", repl)
	}
	_, repl, matches = completeAt([]rune("/auto on"), 8)
	if len(matches) != 1 || repl != "on " {
		t.Fatalf("on: matches=%q repl=%q", matches, repl)
	}
}

func TestCompletePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	_, repl, matches := completeAt([]rune("/attach a"), 9)
	if !contains(matches, "alpha.go") || !contains(matches, "app/") {
		t.Fatalf("attach a: %q", matches)
	}
	if repl != "a" {
		t.Fatalf("common prefix = %q", repl)
	}

	_, repl, matches = completeAt([]rune("/attach alp"), 11)
	if len(matches) != 1 || repl != "alpha.go " {
		t.Fatalf("alp: matches=%q repl=%q", matches, repl)
	}

	_, repl, matches = completeAt([]rune("/attach app"), 11)
	if len(matches) != 1 || repl != "app/" {
		t.Fatalf("app dir: matches=%q repl=%q", matches, repl)
	}

	// Hidden files stay hidden unless the token starts with a dot.
	_, _, matches = completeAt([]rune("/attach "), 8)
	if contains(matches, ".hidden") {
		t.Fatalf("listed hidden: %q", matches)
	}
}

func TestCompleteLooksLikePath(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "internal", "agent.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	_, repl, matches := completeAt([]rune("please read internal/ag"), 23)
	if len(matches) != 1 || repl != "internal/agent.go " {
		t.Fatalf("path in prose: matches=%q repl=%q", matches, repl)
	}
}

func TestOddTrailingBackslash(t *testing.T) {
	if !oddTrailingBackslash(`hello \`) {
		t.Error("single trailing backslash should continue")
	}
	if oddTrailingBackslash(`hello \\`) {
		t.Error("escaped backslash should submit")
	}
	if !oddTrailingBackslash("hello\nworld \\") {
		t.Error("last line with backslash should continue")
	}
	if oddTrailingBackslash(`hello`) {
		t.Error("no backslash")
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
