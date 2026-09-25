package main

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHistoryLoadSave(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "history")
	t.Setenv("CUBCODER_HISTORY", p)

	entries := []string{"first", "second with spaces", "multi\nline"}
	if err := saveHistory(entries); err != nil {
		t.Fatal(err)
	}
	got := loadHistory()
	if len(got) != len(entries) {
		t.Fatalf("loadHistory len=%d want %d: %q", len(got), len(entries), got)
	}
	for i := range entries {
		if got[i] != entries[i] {
			t.Errorf("entry %d = %q want %q", i, got[i], entries[i])
		}
	}
}

func TestAddHistorySkipsNoise(t *testing.T) {
	t.Setenv("CUBCODER_HISTORY", filepath.Join(t.TempDir(), "history"))
	lr := &lineReader{}
	lr.addHistory("hello")
	lr.addHistory("hello") // consecutive dup
	lr.addHistory("   ")
	lr.addHistory("exit")
	lr.addHistory("quit")
	lr.addHistory("world")
	if want := []string{"hello", "world"}; !eqStrings(lr.history, want) {
		t.Errorf("history = %q want %q", lr.history, want)
	}
	got := loadHistory()
	if !eqStrings(got, lr.history) {
		t.Errorf("persisted %q want %q", got, lr.history)
	}
}

func TestReadLineCursorAndHistory(t *testing.T) {
	// Left twice, insert X: "hello" → "helXlo"
	if got := readLineRaw(t, nil, "hello\x1b[D\x1b[DX\r"); got != "helXlo" {
		t.Errorf("left+insert = %q want helXlo", got)
	}
	// Ctrl-A then X: "abc" → "Xabc"
	if got := readLineRaw(t, nil, "abc\x01X\r"); got != "Xabc" {
		t.Errorf("Ctrl-A insert = %q want Xabc", got)
	}
	// Ctrl-A, X, Ctrl-E, Y: "abc" → "XabcY"
	if got := readLineRaw(t, nil, "abc\x01X\x05Y\r"); got != "XabcY" {
		t.Errorf("Ctrl-A/E = %q want XabcY", got)
	}
	// Ctrl-W deletes the previous word
	if got := readLineRaw(t, nil, "hello world\x17\r"); got != "hello " {
		t.Errorf("Ctrl-W = %q want %q", got, "hello ")
	}
	// Delete key removes the char under the cursor
	if got := readLineRaw(t, nil, "abc\x1b[D\x1b[3~\r"); got != "ab" {
		t.Errorf("Delete = %q want ab", got)
	}
	// Up recalls the previous prompt
	if got := readLineRaw(t, []string{"first", "second"}, "\x1b[A\r"); got != "second" {
		t.Errorf("up = %q want second", got)
	}
	if got := readLineRaw(t, []string{"first", "second"}, "\x1b[A\x1b[A\r"); got != "first" {
		t.Errorf("up up = %q want first", got)
	}
	if got := readLineRaw(t, []string{"first", "second"}, "\x1b[A\x1b[B\r"); got != "" {
		t.Errorf("up down (back to draft) = %q want empty", got)
	}
}

func TestReadLineTabComplete(t *testing.T) {
	if got := readLineRaw(t, nil, "/pl\t\r"); got != "/plan " {
		t.Errorf("tab /pl = %q want /plan ", got)
	}
}

func TestReadLineMultiline(t *testing.T) {
	// Ctrl-J (LF) inserts a newline; Enter (CR) submits.
	if got := readLineRaw(t, nil, "hello\nworld\r"); got != "hello\nworld" {
		t.Errorf("Ctrl-J = %q", got)
	}
	// Trailing \ + Enter continues the line.
	if got := readLineRaw(t, nil, "hello \\\rworld\r"); got != "hello \nworld" {
		t.Errorf("backslash continue = %q", got)
	}
	// Escaped backslash (\\) + Enter submits.
	if got := readLineRaw(t, nil, "hello \\\\\r"); got != "hello \\\\" {
		t.Errorf("escaped backslash = %q", got)
	}
}

func TestReadLineReverseSearch(t *testing.T) {
	if got := readLineRaw(t, []string{"hello world", "foo bar"}, "\x12wor\r"); got != "hello world" {
		t.Errorf("Ctrl-R wor = %q want hello world", got)
	}
	// Ctrl-G cancels and restores the draft.
	if got := readLineRaw(t, []string{"hello world"}, "abc\x12hel\x07\r"); got != "abc" {
		t.Errorf("Ctrl-R then Ctrl-G = %q want abc", got)
	}
}

func readLineRaw(t *testing.T, history []string, keys string) string {
	t.Helper()
	lr := &lineReader{
		in:      bufio.NewReader(strings.NewReader(keys)),
		raw:     true,
		history: history,
	}
	s, err := lr.ReadLine("")
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	return s
}

func eqStrings(a, b []string) bool {
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

func TestHistoryKeepCaps(t *testing.T) {
	t.Setenv("CUBCODER_HISTORY", filepath.Join(t.TempDir(), "history"))
	lr := &lineReader{}
	// Don't actually append 1000+ via addHistory (rewrites the file each time);
	// seed a long slice and add one more.
	lr.history = make([]string, historyKeep)
	for i := range lr.history {
		lr.history[i] = "old"
	}
	lr.history[historyKeep-1] = "last-old"
	lr.addHistory("new")
	if len(lr.history) != historyKeep {
		t.Fatalf("len=%d want %d", len(lr.history), historyKeep)
	}
	if lr.history[0] != "old" || lr.history[historyKeep-1] != "new" {
		t.Errorf("cap rotated wrong: first=%q last=%q", lr.history[0], lr.history[historyKeep-1])
	}
}

func TestMain(m *testing.M) {
	// Isolate every package-main test from the developer's real prompt history.
	dir, err := os.MkdirTemp("", "cubcoder-hist-test-")
	if err != nil {
		os.Exit(1)
	}
	os.Setenv("CUBCODER_HISTORY", filepath.Join(dir, "history"))
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
