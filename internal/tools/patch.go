package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Change is one file operation planned from a patch, before anything is written.
type Change struct {
	Path string // path as named in the patch (for display)
	Op   string // "add", "update", "delete"
	Old  string
	New  string
	dest string // resolved write path (jailed)
}

func applyPatch(root string) *Tool {
	return &Tool{
		Name: "apply_patch",
		Description: `Apply a patch to one or more files. Prefer this over edit_file for existing files: matching is whitespace- and indent-tolerant, and one call can change several files. Format:

*** Begin Patch
*** Update File: path
@@
 context
-old line
+new line
*** Add File: path
+contents
*** Delete File: path
*** End Patch

A standard unified diff (--- a/ +++ b/) is also accepted. Do not include read_file line numbers. Matching is unique per hunk; add context if a hunk is ambiguous.`,
		Mutating: true,
		Parameters: obj(map[string]any{
			"patch": str("the full patch text (Begin/End Patch or a unified diff)"),
		}, "patch"),
		Run: func(_ context.Context, args string) (string, error) {
			var a struct {
				Patch string `json:"patch"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			changes, err := PlanPatch(root, a.Patch)
			if err != nil {
				return "", err
			}
			for _, c := range changes {
				if err := commitChange(c); err != nil {
					return "", err
				}
			}
			return summarizeChanges(changes), nil
		},
	}
}

// PlanPatch parses a patch and computes the resulting file contents without
// writing. Every path is resolved against root (the worker jail). Failure
// leaves the filesystem untouched.
func PlanPatch(root, patch string) ([]Change, error) {
	files, err := parsePatch(patch)
	if err != nil {
		return nil, err
	}
	out := make([]Change, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		full, err := resolve(root, f.path)
		if err != nil {
			return nil, err
		}
		if seen[full] {
			return nil, fmt.Errorf("patch names %s more than once", f.path)
		}
		seen[full] = true
		ch, err := planFile(full, f)
		if err != nil {
			return nil, err
		}
		ch.Path = f.path
		ch.dest = full
		out = append(out, ch)
	}
	return out, nil
}

// PatchPaths returns the file paths named by a patch, for UI summaries and
// stale-read eviction. Invalid patches yield nil.
func PatchPaths(patch string) []string {
	files, err := parsePatch(patch)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.path)
	}
	return out
}

func commitChange(c Change) error {
	target := c.dest
	if target == "" {
		target = c.Path
	}
	switch c.Op {
	case "delete":
		if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	case "add", "update":
		if dir := filepath.Dir(target); dir != "" {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
		}
		return os.WriteFile(target, []byte(c.New), 0o644)
	}
	return fmt.Errorf("unknown patch op %q", c.Op)
}

func summarizeChanges(cs []Change) string {
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch c.Op {
		case "add":
			fmt.Fprintf(&b, "added %s (%d bytes)", c.Path, len(c.New))
		case "delete":
			fmt.Fprintf(&b, "deleted %s", c.Path)
		default:
			fmt.Fprintf(&b, "updated %s", c.Path)
		}
	}
	if len(cs) == 0 {
		return "patch applied (no file changes)"
	}
	return b.String()
}

func planFile(full string, f patchFile) (Change, error) {
	switch f.op {
	case "add":
		old, err := os.ReadFile(full)
		if err == nil {
			if string(old) == f.add {
				return Change{Op: "add", Path: full, Old: string(old), New: f.add}, nil
			}
			return Change{}, fmt.Errorf("%s already exists; use Update File to change it", f.path)
		}
		if !os.IsNotExist(err) {
			return Change{}, err
		}
		return Change{Op: "add", Path: full, New: f.add}, nil
	case "delete":
		old, err := os.ReadFile(full)
		if err != nil {
			if os.IsNotExist(err) {
				return Change{Op: "delete", Path: full}, nil
			}
			return Change{}, err
		}
		return Change{Op: "delete", Path: full, Old: string(old)}, nil
	case "update":
		data, err := os.ReadFile(full)
		if err != nil {
			return Change{}, fmt.Errorf("%s: %w", f.path, err)
		}
		content := string(data)
		for i, h := range f.hunks {
			oldB := strings.Join(h.old, "\n")
			newB := strings.Join(h.new, "\n")
			if oldB == newB {
				continue
			}
			if oldB == "" {
				if strings.TrimSpace(content) == "" {
					content = newB
					if newB != "" && !strings.HasSuffix(content, "\n") {
						content += "\n"
					}
					continue
				}
				return Change{}, fmt.Errorf("%s hunk %d has no context to match; add surrounding lines", f.path, i+1)
			}
			// Hunks are whole lines: the trailing newline lets a pure
			// deletion (no + lines) remove the line rather than blank it.
			next, _, err := replaceSpan(content, oldB+"\n", newB+"\n", false)
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					if _, n, e2 := replaceSpan(content, newB, newB, false); e2 == nil && n == 1 {
						return Change{}, fmt.Errorf("%s hunk %d appears already applied", f.path, i+1)
					}
				}
				return Change{}, fmt.Errorf("%s hunk %d: %w", f.path, i+1, err)
			}
			content = next
		}
		return Change{Op: "update", Path: full, Old: string(data), New: content}, nil
	}
	return Change{}, fmt.Errorf("unknown patch op %q", f.op)
}

type patchFile struct {
	op    string
	path  string
	hunks []hunk
	add   string
}

type hunk struct {
	old, new []string
}

func parsePatch(raw string) ([]patchFile, error) {
	raw = stripFences(raw)
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	if strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("patch is empty")
	}
	lines := strings.Split(raw, "\n")
	var files []patchFile
	for i := 0; i < len(lines); {
		line := lines[i]
		if op, path, ok := fileHeader(line); ok {
			f := patchFile{op: op, path: path}
			i++
			var next int
			switch op {
			case "add":
				f.add, next = parseAddBody(lines, i)
			case "delete":
				next = skipHunkNoise(lines, i)
			default:
				f.hunks, next = parseHunks(lines, i)
				if len(f.hunks) == 0 {
					return nil, fmt.Errorf("Update File %s has no hunks", path)
				}
			}
			files = append(files, f)
			i = next
			continue
		}
		if strings.HasPrefix(line, "--- ") {
			f, next, err := parseUnifiedFile(lines, i)
			if err != nil {
				return nil, err
			}
			files = append(files, f)
			i = next
			continue
		}
		i++
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("patch names no files (need *** Update/Add/Delete File: or a unified diff)")
	}
	return files, nil
}

func fileHeader(line string) (op, path string, ok bool) {
	s := strings.TrimSpace(line)
	if !strings.HasPrefix(s, "***") {
		return "", "", false
	}
	s = strings.TrimSpace(strings.TrimPrefix(s, "***"))
	lower := strings.ToLower(s)
	var rest string
	switch {
	case strings.HasPrefix(lower, "add file"):
		op, rest = "add", s[len("add file"):]
	case strings.HasPrefix(lower, "update file"):
		op, rest = "update", s[len("update file"):]
	case strings.HasPrefix(lower, "delete file"):
		op, rest = "delete", s[len("delete file"):]
	default:
		return "", "", false
	}
	rest = strings.TrimSpace(rest)
	rest = strings.TrimPrefix(rest, ":")
	path = strings.TrimSpace(rest)
	if path == "" {
		return "", "", false
	}
	return op, path, true
}

func parseAddBody(lines []string, i int) (string, int) {
	var body []string
	for i < len(lines) {
		if isFileBoundary(lines[i]) {
			break
		}
		body = append(body, stripAddLine(lines[i]))
		i++
	}
	s := strings.Join(body, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return s, i
}

func stripAddLine(line string) string {
	if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
		return line[1:]
	}
	return line
}

func skipHunkNoise(lines []string, i int) int {
	for i < len(lines) && !isFileBoundary(lines[i]) {
		i++
	}
	return i
}

func parseHunks(lines []string, i int) ([]hunk, int) {
	var hunks []hunk
	var cur *hunk
	flush := func() {
		if cur == nil {
			return
		}
		if len(cur.old) > 0 || len(cur.new) > 0 {
			hunks = append(hunks, *cur)
		}
		cur = nil
	}
	for i < len(lines) {
		line := lines[i]
		if isFileBoundary(line) {
			break
		}
		// Next unified-diff file. Inside a hunk, a deleted line can also start
		// with "--- "; only treat it as a header when the following line is +++.
		if strings.HasPrefix(line, "--- ") && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ") {
			break
		}
		if strings.HasPrefix(line, "@@") {
			flush()
			cur = &hunk{}
			i++
			continue
		}
		if cur == nil {
			cur = &hunk{}
		}
		if line == "" && i+1 < len(lines) {
			next := lines[i+1]
			if isFileBoundary(next) || strings.HasPrefix(next, "@@") ||
				(strings.HasPrefix(next, "--- ") && i+2 < len(lines) && strings.HasPrefix(lines[i+2], "+++ ")) {
				i++
				continue
			}
		}
		kind, text, ok := hunkLine(line)
		if !ok {
			i++
			continue
		}
		switch kind {
		case ' ':
			cur.old = append(cur.old, text)
			cur.new = append(cur.new, text)
		case '-':
			cur.old = append(cur.old, text)
		case '+':
			cur.new = append(cur.new, text)
		}
		i++
	}
	flush()
	return hunks, i
}

func hunkLine(line string) (kind byte, text string, ok bool) {
	if line == "\\ No newline at end of file" || strings.HasPrefix(line, "\\ No newline") {
		return 0, "", false
	}
	if line == "" {
		return ' ', "", true
	}
	switch line[0] {
	case '+':
		return '+', line[1:], true
	case '-':
		return '-', line[1:], true
	case ' ', '\t':
		return ' ', line[1:], true
	}
	// No prefix: treat as context. Local models drop the leading space.
	if strings.HasPrefix(line, "***") || strings.HasPrefix(line, "diff --git") || strings.HasPrefix(line, "index ") {
		return 0, "", false
	}
	return ' ', line, true
}

func isFileBoundary(line string) bool {
	s := strings.TrimSpace(line)
	if strings.EqualFold(s, "*** End Patch") {
		return true
	}
	if _, _, ok := fileHeader(line); ok {
		return true
	}
	return false
}

func parseUnifiedFile(lines []string, i int) (patchFile, int, error) {
	oldPath := strings.TrimSpace(strings.TrimPrefix(lines[i], "--- "))
	oldPath = stripDiffPath(oldPath)
	i++
	if i >= len(lines) || !strings.HasPrefix(lines[i], "+++ ") {
		return patchFile{}, i, fmt.Errorf("unified diff: %s not followed by +++", oldPath)
	}
	newPath := strings.TrimSpace(strings.TrimPrefix(lines[i], "+++ "))
	newPath = stripDiffPath(newPath)
	i++
	op, path := "update", newPath
	switch {
	case oldPath == "/dev/null":
		op, path = "add", newPath
	case newPath == "/dev/null":
		op, path = "delete", oldPath
	}
	if path == "" || path == "/dev/null" {
		return patchFile{}, i, fmt.Errorf("unified diff names no file")
	}
	f := patchFile{op: op, path: path}
	if op == "update" || op == "add" {
		hunks, next := parseHunks(lines, i)
		if op == "add" {
			var body []string
			for _, h := range hunks {
				body = append(body, h.new...)
			}
			s := strings.Join(body, "\n")
			if s != "" && !strings.HasSuffix(s, "\n") {
				s += "\n"
			}
			f.add = s
		} else {
			if len(hunks) == 0 {
				return patchFile{}, next, fmt.Errorf("unified diff for %s has no hunks", path)
			}
			f.hunks = hunks
		}
		return f, next, nil
	}
	return f, skipHunkNoise(lines, i), nil
}

func stripDiffPath(p string) string {
	if tab := strings.IndexByte(p, '\t'); tab >= 0 {
		p = p[:tab]
	}
	p = strings.TrimSpace(p)
	if strings.HasPrefix(p, "a/") || strings.HasPrefix(p, "b/") {
		return p[2:]
	}
	return p
}

func stripFences(s string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "```") {
		return s
	}
	s = strings.TrimPrefix(s, "```")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}
