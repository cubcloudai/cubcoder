package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceSpanExact(t *testing.T) {
	got, n, err := replaceSpan("func A() {}\nfunc B() {}\n", "func B() {}", "func B() { return }", false)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 || got != "func A() {}\nfunc B() { return }\n" {
		t.Fatalf("got %q n=%d", got, n)
	}
}

func TestReplaceSpanUniqueRequired(t *testing.T) {
	_, _, err := replaceSpan("foo\nfoo\n", "foo", "bar", false)
	if err == nil || !strings.Contains(err.Error(), "2 places") {
		t.Fatalf("want unique-match error, got %v", err)
	}
	got, n, err := replaceSpan("foo\nfoo\n", "foo", "bar", true)
	if err != nil || n != 2 || got != "bar\nbar\n" {
		t.Fatalf("replace_all: got %q n=%d err=%v", got, n, err)
	}
}

func TestReplaceSpanIndentAndLineNumbers(t *testing.T) {
	file := "package p\n\nfunc F() {\n\treturn 1\n}\n"
	// Model under-indented the search (dropped the tab).
	got, n, err := replaceSpan(file, "return 1", "return 2", false)
	if err != nil || n != 1 {
		t.Fatalf("under-indented search: n=%d err=%v", n, err)
	}
	if !strings.Contains(got, "\treturn 2") {
		t.Fatalf("pad should keep the file indent: %q", got)
	}

	// Copied from read_file, with line numbers.
	search := "     4\treturn 1"
	got, n, err = replaceSpan(file, search, "     4\treturn 3", false)
	if err != nil || n != 1 {
		t.Fatalf("line-number search: n=%d err=%v got=%q", n, err, got)
	}
	if !strings.Contains(got, "\treturn 3") {
		t.Fatalf("line numbers should strip, indent stay: %q", got)
	}
}

func TestReplaceSpanTrailingSpaceAndCRLF(t *testing.T) {
	got, n, err := replaceSpan("hello world  \nnext\n", "hello world", "hello there", false)
	if err != nil || n != 1 || !strings.HasPrefix(got, "hello there") {
		t.Fatalf("rtrim: got %q n=%d err=%v", got, n, err)
	}
	got, n, err = replaceSpan("a\r\nb\r\n", "b", "c", false)
	if err != nil || n != 1 || got != "a\r\nc\r\n" {
		t.Fatalf("crlf: got %q n=%d err=%v", got, n, err)
	}
}

func TestReplaceSpanNearbyHint(t *testing.T) {
	_, _, err := replaceSpan("func Alpha() {}\nfunc Beta() {}\n", "func Betta() {}", "x", false)
	if err == nil || !strings.Contains(err.Error(), "nearby lines") {
		t.Fatalf("want nearby hint, got %v", err)
	}
}

func TestApplyPatchUpdateAddDelete(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gone.go"), []byte("obsolete\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Update File: a.go",
		"@@",
		" package a",
		" ",
		"-func A() { return 1 }",
		"+func A() { return 2 }",
		"*** Add File: b.go",
		"+package b",
		"+",
		"+func B() {}",
		"*** Delete File: gone.go",
		"*** End Patch",
	}, "\n")

	tool := applyPatch(dir)
	out, err := tool.Run(context.Background(), marshalPatch(t, patch))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "updated a.go") || !strings.Contains(out, "added b.go") || !strings.Contains(out, "deleted gone.go") {
		t.Fatalf("summary: %q", out)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.go"))
	if !strings.Contains(string(got), "return 2") {
		t.Fatalf("a.go: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "gone.go")); !os.IsNotExist(err) {
		t.Fatal("gone.go should be deleted")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "b.go"))
	if !strings.Contains(string(b), "func B()") {
		t.Fatalf("b.go: %s", b)
	}
}

func TestApplyPatchFuzzyIndent(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.go"), []byte("func F() {\n\treturn 1\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"*** Update File: f.go",
		"@@",
		" func F() {",
		"-return 1",
		"+return 2",
		" }",
	}, "\n")
	if _, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "f.go"))
	if string(got) != "func F() {\n\treturn 2\n}\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyPatchTwoHunksSeparatedByBlank(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "t.go"), []byte("a = 1\nb = 2\nc = 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"*** Update File: t.go",
		"@@",
		"-a = 1",
		"+a = 10",
		"",
		"@@",
		"-c = 3",
		"+c = 30",
	}, "\n")
	if _, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "t.go"))
	if string(got) != "a = 10\nb = 2\nc = 30\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyPatchUnifiedDiff(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "u.go"), []byte("x = 1\ny = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"--- a/u.go",
		"+++ b/u.go",
		"@@ -1,2 +1,2 @@",
		" x = 1",
		"-y = 2",
		"+y = 3",
	}, "\n")
	if _, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "u.go"))
	if string(got) != "x = 1\ny = 3\n" {
		t.Fatalf("got %q", got)
	}
}

func TestApplyPatchAtomic(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "keep.go"), []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"*** Update File: keep.go",
		"@@",
		"-keep",
		"+changed",
		"*** Update File: missing.go",
		"@@",
		"-nope",
		"+yes",
	}, "\n")
	_, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	got, _ := os.ReadFile(filepath.Join(dir, "keep.go"))
	if string(got) != "keep\n" {
		t.Fatalf("first file must not be written on a later failure: %q", got)
	}
}

func TestApplyPatchJail(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("classified\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Update File: " + outside + "\n@@\n-classified\n+pwned\n"
	_, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch))
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("want jail error, got %v", err)
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "classified\n" {
		t.Fatal("outside file was written")
	}
}

func TestApplyPatchAlreadyApplied(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("func A() { return 2 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := strings.Join([]string{
		"*** Update File: a.go",
		"@@",
		"-func A() { return 1 }",
		"+func A() { return 2 }",
	}, "\n")
	_, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch))
	if err == nil || !strings.Contains(err.Error(), "already applied") {
		t.Fatalf("want already-applied, got %v", err)
	}
}

func TestApplyPatchFencesAndLineNumbers(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "n.go"), []byte("package n\n\nfunc N() { return 1 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "```\n*** Update File: n.go\n@@\n-     3\tfunc N() { return 1 }\n+     3\tfunc N() { return 2 }\n```"
	if _, err := applyPatch(dir).Run(context.Background(), marshalPatch(t, patch)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "n.go"))
	if !strings.Contains(string(got), "return 2") {
		t.Fatalf("got %s", got)
	}
}

func TestEditFileReplaceAllAndFuzzy(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "e.go")
	if err := os.WriteFile(p, []byte("foo\nfoo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := editFile(dir)
	_, err := tool.Run(context.Background(), `{"path":"e.go","search":"foo","replace":"bar"}`)
	if err == nil || !strings.Contains(err.Error(), "replace_all") {
		t.Fatalf("want unique error mentioning replace_all, got %v", err)
	}
	out, err := tool.Run(context.Background(), `{"path":"e.go","search":"foo","replace":"bar","replace_all":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "2 replacements") {
		t.Fatalf("summary: %q", out)
	}
	got, _ := os.ReadFile(p)
	if string(got) != "bar\nbar\n" {
		t.Fatalf("got %q", got)
	}
}

func TestPatchPaths(t *testing.T) {
	patch := "*** Update File: a.go\n@@\n-a\n+b\n*** Add File: b.go\n+x\n"
	got := PatchPaths(patch)
	if len(got) != 2 || got[0] != "a.go" || got[1] != "b.go" {
		t.Fatalf("got %v", got)
	}
}

func marshalPatch(t *testing.T, patch string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"patch": patch})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestReplaceSpanWholeLineNewline(t *testing.T) {
	// search and replace both carry a trailing newline: no blank line appears.
	got, _, err := replaceSpan("a\nfoo\nb\n", "foo\n", "bar\n", false)
	if err != nil || got != "a\nbar\nb\n" {
		t.Fatalf("line replace: got %q err=%v", got, err)
	}
	// deleting a whole line removes its newline too.
	got, _, err = replaceSpan("a\nfoo\nb\n", "foo\n", "", false)
	if err != nil || got != "a\nb\n" {
		t.Fatalf("line delete: got %q err=%v", got, err)
	}
	// indent-tolerant match keeps the same guarantee.
	got, _, err = replaceSpan("a\n\tfoo\nb\n", "foo\n", "bar\n", false)
	if err != nil || got != "a\n\tbar\nb\n" {
		t.Fatalf("indent line replace: got %q err=%v", got, err)
	}
}

func TestApplyPatchDeleteOnlyHunk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("a\nfoo\nb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "*** Begin Patch\n*** Update File: f.txt\n@@\n-foo\n*** End Patch\n"
	args, _ := json.Marshal(map[string]string{"patch": patch})
	if _, err := applyPatch(dir).Run(context.Background(), string(args)); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "a\nb\n" {
		t.Fatalf("delete-only hunk left %q", got)
	}
}
