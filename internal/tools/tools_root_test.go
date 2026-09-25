package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	t.Run("empty root leaves the path as-is", func(t *testing.T) {
		cases := []struct{ path, want string }{
			{"foo.go", "foo.go"},
			{"/abs/foo.go", "/abs/foo.go"},
			{"", "."},
		}
		for _, c := range cases {
			got, err := resolve("", c.path)
			if err != nil {
				t.Errorf("resolve(\"\", %q): %v", c.path, err)
				continue
			}
			if got != c.want {
				t.Errorf("resolve(\"\", %q)=%q want %q", c.path, got, c.want)
			}
		}
	})

	root := t.TempDir()

	t.Run("relative joins onto root", func(t *testing.T) {
		got, err := resolve(root, "sub/foo.go")
		if err != nil {
			t.Fatal(err)
		}
		rootAbs := mustCanonicalRoot(t, root)
		rel, err := filepath.Rel(rootAbs, got)
		if err != nil || rel != filepath.Join("sub", "foo.go") {
			t.Errorf("resolve(sub/foo.go)=%q (rel %q), want under %q", got, rel, rootAbs)
		}
	})

	t.Run("dot is the root itself", func(t *testing.T) {
		got, err := resolve(root, ".")
		if err != nil {
			t.Fatal(err)
		}
		if got != mustCanonicalRoot(t, root) {
			t.Errorf("resolve(., root)=%q want %q", got, mustCanonicalRoot(t, root))
		}
	})

	t.Run("absolute inside the jail is allowed", func(t *testing.T) {
		p := filepath.Join(root, "ok.txt")
		got, err := resolve(root, p)
		if err != nil {
			t.Fatal(err)
		}
		rootAbs := mustCanonicalRoot(t, root)
		rel, err := filepath.Rel(rootAbs, got)
		if err != nil || !inRoot(rel) {
			t.Errorf("absolute inside jail resolved to %q (rel %q)", got, rel)
		}
	})
}

func TestResolveRejectsEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	cases := []string{
		"..",
		"../secret",
		"foo/../../secret",
		filepath.Join(outside, "x"),
		"/etc/passwd",
	}
	for _, p := range cases {
		if _, err := resolve(root, p); err == nil {
			t.Errorf("resolve(%q) allowed a path outside the jail", p)
		} else if !strings.Contains(err.Error(), "outside the working directory") {
			t.Errorf("resolve(%q) error = %v, want outside-jail message", p, err)
		}
	}
}

func TestResolveRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(root, "link"); err == nil {
		t.Fatal("symlink pointing outside the jail must be rejected")
	}

	// A symlink whose target is inside the jail is fine.
	inner := filepath.Join(root, "inner.txt")
	if err := os.WriteFile(inner, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	inlink := filepath.Join(root, "inlink")
	if err := os.Symlink(inner, inlink); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(root, "inlink"); err != nil {
		t.Fatalf("symlink inside the jail: %v", err)
	}
}

func TestResolveRejectsSymlinkParentEscape(t *testing.T) {
	// write_file of sub/x when sub is a symlink out of the jail.
	root := t.TempDir()
	outside := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Symlink(outside, sub); err != nil {
		t.Fatal(err)
	}
	if _, err := resolve(root, "sub/new.txt"); err == nil {
		t.Fatal("path whose parent symlink leaves the jail must be rejected")
	}
}

// TestRootedWriteRead confirms a rooted registry's file ops land inside root,
// not the process CWD — the property that keeps concurrent agents isolated.
func TestRootedWriteRead(t *testing.T) {
	root := t.TempDir()
	r := NewIn(root)

	wf, ok := r.Get("write_file")
	if !ok {
		t.Fatal("write_file not registered")
	}
	if _, err := wf.Run(context.Background(), `{"path":"sub/hello.txt","content":"hi there"}`); err != nil {
		t.Fatalf("write_file: %v", err)
	}

	// The file must exist under root, addressed by its relative path.
	if data, err := os.ReadFile(filepath.Join(root, "sub", "hello.txt")); err != nil {
		t.Fatalf("file not under root: %v", err)
	} else if string(data) != "hi there" {
		t.Fatalf("content = %q", data)
	}

	// read_file with the same relative path reads it back through the root.
	rf, _ := r.Get("read_file")
	out, err := rf.Run(context.Background(), `{"path":"sub/hello.txt"}`)
	if err != nil {
		t.Fatalf("read_file: %v", err)
	}
	if !strings.Contains(out, "hi there") {
		t.Fatalf("read_file output = %q", out)
	}
}

// TestRootedToolsRefuseEscape is the worker-sandbox property: a rooted
// registry auto-approves, so the jail — not the user — has to stop a write
// (or read, list, search, diff) that walks out of the worktree.
func TestRootedToolsRefuseEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("classified"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := NewIn(root)

	abs, _ := json.Marshal(secret)
	dotdot := `{"path":"../secret.txt","content":"pwned"}`
	absWrite := `{"path":` + string(abs) + `,"content":"pwned"}`

	wf, _ := r.Get("write_file")
	for _, args := range []string{dotdot, absWrite} {
		_, err := wf.Run(context.Background(), args)
		if err == nil || !strings.Contains(err.Error(), "outside the working directory") {
			t.Errorf("write_file(%s) = %v, want outside-jail error", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "secret.txt")); err != nil {
		t.Fatal("outside file disappeared")
	}
	if data, _ := os.ReadFile(secret); string(data) != "classified" {
		t.Fatalf("outside file was overwritten: %q", data)
	}

	rf, _ := r.Get("read_file")
	if _, err := rf.Run(context.Background(), `{"path":"../secret.txt"}`); err == nil {
		t.Error("read_file of ../ must fail")
	}
	if _, err := rf.Run(context.Background(), `{"path":`+string(abs)+`}`); err == nil {
		t.Error("read_file of absolute outside path must fail")
	}

	ld, _ := r.Get("list_dir")
	if _, err := ld.Run(context.Background(), `{"path":".."}`); err == nil {
		t.Error("list_dir of .. must fail")
	}

	sc, _ := r.Get("search_code")
	if _, err := sc.Run(context.Background(), `{"pattern":"classified","path":".."}`); err == nil {
		t.Error("search_code of .. must fail")
	}

	gd, _ := r.Get("git_diff")
	if _, err := gd.Run(context.Background(), `{"path":".."}`); err == nil {
		t.Error("git_diff of .. must fail")
	}

	ef, _ := r.Get("edit_file")
	if _, err := ef.Run(context.Background(), `{"path":"../secret.txt","search":"classified","replace":"pwned"}`); err == nil {
		t.Error("edit_file of ../ must fail")
	}

	ap, _ := r.Get("apply_patch")
	_, err := ap.Run(context.Background(), `{"patch":"*** Update File: ../secret.txt\n@@\n-classified\n+pwned\n"}`)
	if err == nil {
		t.Error("apply_patch of ../ must fail")
	}
}

func TestLeadUnrootedStillAllowsAbsolute(t *testing.T) {
	// The lead agent uses an empty root and is gated by the user's confirm
	// prompt, so absolute paths must still work.
	dir := t.TempDir()
	p := filepath.Join(dir, "lead.txt")
	r := NewIn("")
	wf, _ := r.Get("write_file")
	args, _ := json.Marshal(map[string]string{"path": p, "content": "from lead"})
	if _, err := wf.Run(context.Background(), string(args)); err != nil {
		t.Fatalf("lead write_file absolute: %v", err)
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "from lead" {
		t.Fatalf("content = %q", data)
	}
}

func mustCanonicalRoot(t *testing.T, root string) string {
	t.Helper()
	got, err := canonicalRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
