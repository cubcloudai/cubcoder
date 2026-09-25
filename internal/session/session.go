// Package session persists conversations to disk so a run survives exit,
// a gateway hiccup, or a closed laptop: every turn is saved as it completes
// and `cubcoder -continue` / `-resume <id>` picks it back up.
//
// Layout: ~/.cubcoder/sessions/<id>.json, one file per session, mode 0600
// (transcripts contain the user's code). The id is a timestamp plus a short
// random suffix, so files sort chronologically and a unique prefix is enough
// to name one.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cubcoder/internal/agent"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/provider"
)

// Session is one saved conversation.
type Session struct {
	ID       string             `json:"id"`
	Cwd      string             `json:"cwd"`
	Model    string             `json:"model"`
	Title    string             `json:"title"` // first user request, trimmed
	Created  time.Time          `json:"created"`
	Updated  time.Time          `json:"updated"`
	Messages []provider.Message `json:"messages"`
	// Workers are snapshots of orchestrator workers (including logs) so
	// /agents and read_agent_log still work after resume. Running workers
	// are saved as stopped.
	Workers []orchestrator.Record `json:"workers,omitempty"`
	// Todos is the session plan (todo_write). Survives resume independently
	// of the transcript, so compaction cannot drop it.
	Todos []agent.Todo `json:"todos,omitempty"`
	// Checkpoints are git snapshots taken at the start of each user turn,
	// so /undo can restore the tree and the conversation together.
	Checkpoints []Checkpoint `json:"checkpoints,omitempty"`
}

// Checkpoint is one git snapshot plus the conversation/plan state at the
// moment it was taken (before the user turn that follows it).
type Checkpoint struct {
	N        int          `json:"n"`
	Created  time.Time    `json:"created"`
	Head     string       `json:"head"`
	Commit   string       `json:"commit"`
	Branch   string       `json:"branch"`
	Repo     string       `json:"repo,omitempty"`
	Prompt   string       `json:"prompt,omitempty"`
	Messages int          `json:"messages"`
	Todos    []agent.Todo `json:"todos,omitempty"`
}

// CheckpointKeep is how many git snapshots a session retains.
const CheckpointKeep = 20

// NextCheckpointN is one past the highest existing checkpoint number.
func (s *Session) NextCheckpointN() int {
	n := 0
	for _, cp := range s.Checkpoints {
		if cp.N > n {
			n = cp.N
		}
	}
	return n + 1
}

// keep is how many sessions are retained on disk; the oldest beyond it are
// pruned on every save so the directory can't grow without bound.
const keep = 100

// Dir returns the sessions directory, honoring $CUBCODER_SESSIONS.
func Dir() (string, error) {
	if d := os.Getenv("CUBCODER_SESSIONS"); d != "" {
		return d, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cubcoder", "sessions"), nil
}

// NewID mints a session id: UTC timestamp to the second plus 4 hex chars.
func NewID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// New starts an unsaved session for cwd.
func New(cwd, model string) *Session {
	now := time.Now()
	return &Session{ID: NewID(), Cwd: cwd, Model: model, Created: now, Updated: now}
}

// Save writes the session atomically (temp file + rename) and prunes old
// sessions. Sessions with no user turn are not written: nothing to resume.
func (s *Session) Save() error {
	if !hasUserTurn(s.Messages) {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	s.Updated = time.Now()
	if s.Title == "" {
		s.Title = titleOf(s.Messages)
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	final := filepath.Join(dir, s.ID+".json")
	tmp, err := os.CreateTemp(dir, "."+s.ID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)
		return err
	}
	prune(dir)
	return nil
}

// Load reads a session by id. A unique prefix of the id is accepted, so
// `-resume 20260826-15` works when only one session matches.
func Load(id string) (*Session, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	id = strings.TrimSuffix(strings.TrimSpace(id), ".json")
	path := filepath.Join(dir, id+".json")
	if _, err := os.Stat(path); err != nil {
		names, _ := filepath.Glob(filepath.Join(dir, id+"*.json"))
		switch len(names) {
		case 0:
			return nil, fmt.Errorf("no session %q (see `cubcoder sessions`)", id)
		case 1:
			path = names[0]
		default:
			return nil, fmt.Errorf("session id %q is ambiguous (%d matches); give more of it", id, len(names))
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	}
	return &s, nil
}

// Meta is a session's header, for listings.
type Meta struct {
	ID       string
	Cwd      string
	Model    string
	Title    string
	Updated  time.Time
	Messages int
}

// List returns every saved session, newest first. Unreadable files are
// skipped: a listing should never fail because one session is corrupt.
func List() ([]Meta, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	names, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var out []Meta
	for _, n := range names {
		data, err := os.ReadFile(n)
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		if s.ID == "" {
			s.ID = strings.TrimSuffix(filepath.Base(n), ".json")
		}
		out = append(out, Meta{ID: s.ID, Cwd: s.Cwd, Model: s.Model, Title: s.Title, Updated: s.Updated, Messages: len(s.Messages)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated.After(out[j].Updated) })
	return out, nil
}

// ErrNone is returned by Latest when no session exists for the directory.
var ErrNone = errors.New("no saved session for this directory")

// Latest loads the most recently updated session started in cwd.
func Latest(cwd string) (*Session, error) {
	metas, err := List()
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Cwd == cwd {
			return Load(m.ID)
		}
	}
	return nil, ErrNone
}

// prune deletes all but the newest keep session files. Errors are ignored:
// pruning is housekeeping, never worth failing a save over.
func prune(dir string) {
	names, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	if len(names) <= keep {
		return
	}
	sort.Strings(names) // ids are timestamp-prefixed, so lexical order is chronological
	for _, n := range names[:len(names)-keep] {
		os.Remove(n)
	}
}

func hasUserTurn(msgs []provider.Message) bool {
	for _, m := range msgs {
		if m.Role == "user" {
			return true
		}
	}
	return false
}

// titleOf takes the first user request, minus any attachment preamble, cut to
// one short line.
func titleOf(msgs []provider.Message) string {
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		text := m.Content
		// Attachments are fenced ahead of the request; the request is what
		// follows the last fence.
		if i := strings.LastIndex(text, "---\n\n"); i >= 0 {
			text = text[i+len("---\n\n"):]
		}
		text = strings.TrimSpace(strings.Join(strings.Fields(text), " "))
		if r := []rune(text); len(r) > 72 {
			text = string(r[:72]) + "…"
		}
		return text
	}
	return ""
}
