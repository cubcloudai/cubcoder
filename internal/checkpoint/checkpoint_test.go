package checkpoint

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "seed"}, {"commit", "-q", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return string(out)
}

func TestTakeRestoreDirtyAndUntracked(t *testing.T) {
	dir := gitInit(t)
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := Take(dir, Ref("s1", 1), "cp1")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Head == "" || snap.Commit == "" || snap.Commit == snap.Head {
		// Commit can equal HEAD's tree but must be a distinct commit object
		if snap.Commit == snap.Head {
			t.Fatalf("snapshot commit should be a child of HEAD, got %s", snap.Commit)
		}
	}

	// Mutate after the snapshot: edit, add, delete.
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.go"), []byte("package extra\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatal(err)
	}
	gitOut(t, dir, "add", "-A")
	gitOut(t, dir, "commit", "-q", "-m", "agent work")

	if err := Restore(dir, snap.Head, snap.Commit); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(filepath.Join(dir, "seed"))
	if err != nil || string(got) != "dirty\n" {
		t.Fatalf("seed = %q err=%v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "new.txt"))
	if err != nil || string(got) != "untracked\n" {
		t.Fatalf("new.txt = %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.go")); !os.IsNotExist(err) {
		t.Fatal("extra.go created after snapshot should be gone")
	}
	head := strings.TrimSpace(gitOut(t, dir, "rev-parse", "HEAD"))
	if head != snap.Head {
		t.Fatalf("HEAD = %s, want %s", head, snap.Head)
	}
}

func TestTakeRestoreDeletedFile(t *testing.T) {
	dir := gitInit(t)
	if err := os.Remove(filepath.Join(dir, "seed")); err != nil {
		t.Fatal(err)
	}
	snap, err := Take(dir, Ref("s1", 1), "deleted")
	if err != nil {
		t.Fatal(err)
	}
	// Put seed back (as if the agent recreated it).
	if err := os.WriteFile(filepath.Join(dir, "seed"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Restore(dir, snap.Head, snap.Commit); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "seed")); !os.IsNotExist(err) {
		t.Fatal("seed was deleted at snapshot and should stay deleted")
	}
}

func TestTakeNotRepo(t *testing.T) {
	_, err := Take(t.TempDir(), Ref("s", 1), "x")
	if err != ErrNotRepo {
		t.Fatalf("err = %v, want ErrNotRepo", err)
	}
}

func TestIsRepo(t *testing.T) {
	if IsRepo(t.TempDir()) {
		t.Fatal("empty dir is not a repo")
	}
	if !IsRepo(gitInit(t)) {
		t.Fatal("gitInit dir should be a repo")
	}
}
