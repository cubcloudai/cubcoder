// Package checkpoint snapshots a git worktree so a cubcoder session can rewind
// the tree, not just the transcript. Each snapshot is a dangling commit
// hanging off refs/cubcoder/cp/<session>/<n>, built with a temporary index so
// the user's HEAD, index, and worktree are not touched while taking it.
package checkpoint

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNotRepo is returned when Take is asked to snapshot a directory that is
// not a git worktree (or has no HEAD yet).
var ErrNotRepo = errors.New("not a git repository")

// Snap is the git identity of one checkpoint. The session layer stores the
// rest (prompt, message count, todos).
type Snap struct {
	Head   string // HEAD at snapshot time
	Commit string // cubcoder snapshot commit (full worktree tree)
	Branch string
	Repo   string // git toplevel
}

// Ref names the git ref that keeps a snapshot reachable.
func Ref(sessionID string, n int) string {
	return fmt.Sprintf("refs/cubcoder/cp/%s/%d", sessionID, n)
}

// IsRepo reports whether dir is inside a git worktree with a HEAD commit.
func IsRepo(dir string) bool {
	_, err := Toplevel(dir)
	return err == nil
}

// Toplevel returns the git common worktree root for dir.
func Toplevel(dir string) (string, error) {
	out, err := git(dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotRepo
	}
	top := strings.TrimSpace(out)
	if top == "" {
		return "", ErrNotRepo
	}
	if _, err := git(top, nil, "rev-parse", "--verify", "HEAD"); err != nil {
		return "", ErrNotRepo
	}
	return top, nil
}

// Take writes a snapshot of dir's worktree (tracked + untracked, gitignored
// files skipped) and stores it at ref. The user's index and worktree are left
// as they were.
func Take(dir, ref, message string) (Snap, error) {
	top, err := Toplevel(dir)
	if err != nil {
		return Snap{}, err
	}
	head, err := git(top, nil, "rev-parse", "HEAD")
	if err != nil {
		return Snap{}, ErrNotRepo
	}
	head = strings.TrimSpace(head)
	branch, _ := git(top, nil, "rev-parse", "--abbrev-ref", "HEAD")
	branch = strings.TrimSpace(branch)

	tmp, err := os.CreateTemp("", "cubcoder-idx-")
	if err != nil {
		return Snap{}, err
	}
	idx := tmp.Name()
	tmp.Close()
	os.Remove(idx) // git writes a real index; an empty file is not one
	defer os.Remove(idx)

	idxEnv := []string{"GIT_INDEX_FILE=" + idx}
	if _, err := git(top, idxEnv, "read-tree", "HEAD"); err != nil {
		return Snap{}, fmt.Errorf("read-tree: %w", err)
	}
	if _, err := git(top, idxEnv, "add", "-A"); err != nil {
		return Snap{}, fmt.Errorf("git add: %w", err)
	}
	tree, err := git(top, idxEnv, "write-tree")
	if err != nil {
		return Snap{}, fmt.Errorf("write-tree: %w", err)
	}
	tree = strings.TrimSpace(tree)
	if message == "" {
		message = "cubcoder checkpoint"
	}
	commit, err := git(top, idxEnv, "commit-tree", tree, "-p", head, "-m", message)
	if err != nil {
		return Snap{}, fmt.Errorf("commit-tree: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if _, err := git(top, nil, "update-ref", ref, commit); err != nil {
		return Snap{}, fmt.Errorf("update-ref: %w", err)
	}
	return Snap{Head: head, Commit: commit, Branch: branch, Repo: top}, nil
}

// Restore resets dir to the snapshot: HEAD moves back to head, the worktree
// matches commit (including files that were untracked at snapshot time), and
// files created after the snapshot are removed. Gitignored files are left
// alone. Refuses if a merge is in progress.
func Restore(dir, head, commit string) error {
	top, err := Toplevel(dir)
	if err != nil {
		return err
	}
	if _, err := git(top, nil, "rev-parse", "-q", "--verify", "MERGE_HEAD"); err == nil {
		return fmt.Errorf("a merge is in progress; finish or abort it before restoring")
	}
	if _, err := git(top, nil, "cat-file", "-e", head+"^{commit}"); err != nil {
		return fmt.Errorf("checkpoint HEAD %s is missing from git", short(head))
	}
	if _, err := git(top, nil, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return fmt.Errorf("checkpoint snapshot %s is missing from git (gc?)", short(commit))
	}

	if _, err := git(top, nil, "reset", "--hard", "-q", head); err != nil {
		return fmt.Errorf("git reset --hard: %w", err)
	}
	// read-tree --reset -u matches the worktree to the snapshot, including
	// deletions and an empty tree (checkout <commit> -- . fails on those).
	if _, err := git(top, nil, "read-tree", "--reset", "-u", commit); err != nil {
		return fmt.Errorf("git read-tree snapshot: %w", err)
	}
	if _, err := git(top, nil, "reset", "-q", head); err != nil {
		return fmt.Errorf("git reset: %w", err)
	}

	keepList, err := listed(top, "ls-tree", "-r", "--name-only", commit)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, f := range keepList {
		keep[f] = true
	}
	untracked, err := listed(top, "ls-files", "-o", "--exclude-standard")
	if err != nil {
		return err
	}
	for _, f := range untracked {
		if !keep[f] {
			os.RemoveAll(filepath.Join(top, f))
		}
	}
	return nil
}

// DeleteRef drops a checkpoint ref. Missing refs are ignored.
func DeleteRef(dir, ref string) {
	top, err := Toplevel(dir)
	if err != nil {
		return
	}
	_, _ = git(top, nil, "update-ref", "-d", ref)
}

func listed(dir string, args ...string) ([]string, error) {
	out, err := git(dir, nil, args...)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func git(dir string, extra []string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(extra)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := stdout.String()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(out)
		}
		if msg == "" {
			msg = err.Error()
		}
		return out, errors.New(msg)
	}
	return out, nil
}

func gitEnv(extra []string) []string {
	override := map[string]string{
		"PAGER":               "cat",
		"GIT_PAGER":           "cat",
		"GIT_AUTHOR_NAME":     "cubcoder",
		"GIT_AUTHOR_EMAIL":    "cubcoder@local",
		"GIT_COMMITTER_NAME":  "cubcoder",
		"GIT_COMMITTER_EMAIL": "cubcoder@local",
		"GIT_CONFIG_COUNT":    "1",
		"GIT_CONFIG_KEY_0":    "commit.gpgsign",
		"GIT_CONFIG_VALUE_0":  "false",
	}
	for _, e := range extra {
		k, v, _ := strings.Cut(e, "=")
		override[k] = v
	}
	env := os.Environ()
	out := make([]string, 0, len(env)+len(override))
	for _, e := range env {
		key, _, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		if _, skip := override[key]; skip {
			continue
		}
		out = append(out, e)
	}
	for k, v := range override {
		out = append(out, k+"="+v)
	}
	return out
}
