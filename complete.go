package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"cubcoder/internal/session"
)

// slashCommands is the first-token Tab-complete set. Keep in sync with /help.
var slashCommands = []string{
	"/reset", "/compact", "/plan", "/go", "/undo", "/rewind", "/status",
	"/sessions", "/resume", "/model", "/login", "/auto", "/attach",
	"/sudo", "/label", "/agents", "/agent", "/stop", "/help",
}

// completeAt finds Tab completions for buf at rune index pos.
// from is the start of the token being replaced; replacement is what to
// put there (common prefix, or the unique match plus a trailing space);
// matches is the full list for display when nothing more can be inserted.
func completeAt(buf []rune, pos int) (from int, replacement string, matches []string) {
	if pos < 0 {
		pos = 0
	}
	if pos > len(buf) {
		pos = len(buf)
	}
	from = pos
	for from > 0 && !isTokenSep(buf[from-1]) {
		from--
	}
	token := string(buf[from:pos])
	cmd := firstWord(string(buf))

	inFirstWord := true
	for _, r := range buf[:pos] {
		if r == ' ' || r == '\t' {
			inFirstWord = false
			break
		}
	}

	switch {
	case inFirstWord && strings.HasPrefix(token, "/"):
		matches = prefixMatch(slashCommands, token)
	case cmd == "/attach":
		matches = completeAttach(token)
	case cmd == "/resume":
		matches = completeSessions(token)
	case cmd == "/auto":
		matches = prefixMatch([]string{"on", "off"}, token)
	case cmd == "/sudo":
		matches = prefixMatch([]string{"forget"}, token)
	case cmd == "/label":
		matches = prefixMatch([]string{"clear"}, token)
	case cmd == "/agents":
		matches = prefixMatch([]string{"clean"}, token)
	case looksLikePath(token):
		matches = completePath(token)
	}
	if len(matches) == 0 {
		return from, token, nil
	}
	if len(matches) == 1 {
		return from, decorateComplete(matches[0]), matches
	}
	pre := commonPrefix(matches)
	if len(pre) > len(token) {
		return from, pre, matches
	}
	return from, token, matches
}

func isTokenSep(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n'
}

func firstWord(s string) string {
	s = strings.TrimLeft(s, " \t")
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		return s[:i]
	}
	return s
}

func looksLikePath(token string) bool {
	if token == "" {
		return false
	}
	return strings.Contains(token, "/") || strings.HasPrefix(token, ".") || strings.HasPrefix(token, "~")
}

func prefixMatch(options []string, token string) []string {
	var out []string
	for _, o := range options {
		if strings.HasPrefix(o, token) {
			out = append(out, o)
		}
	}
	return out
}

func commonPrefix(ss []string) string {
	if len(ss) == 0 {
		return ""
	}
	p := ss[0]
	for _, s := range ss[1:] {
		for !strings.HasPrefix(s, p) {
			if p == "" {
				return ""
			}
			p = p[:len(p)-1]
		}
	}
	return p
}

func decorateComplete(match string) string {
	if strings.HasSuffix(match, "/") {
		return match
	}
	return match + " "
}

func completeAttach(token string) []string {
	if strings.Contains(token, "/") || strings.HasPrefix(token, ".") || strings.HasPrefix(token, "~") {
		return completePath(token)
	}
	var out []string
	if strings.HasPrefix("clear", token) {
		out = append(out, "clear")
	}
	out = append(out, completePath(token)...)
	return out
}

func completeSessions(token string) []string {
	metas, err := session.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, m := range metas {
		if strings.HasPrefix(m.ID, token) {
			out = append(out, m.ID)
		}
	}
	return out
}

func completePath(token string) []string {
	token = strings.Trim(token, `"'`)
	searchDir, displayDir, base := splitCompletePath(token)
	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if base != "" && !strings.HasPrefix(name, base) {
			continue
		}
		if base == "" && strings.HasPrefix(name, ".") {
			continue
		}
		p := displayDir + name
		if e.IsDir() {
			p += "/"
		}
		if strings.ContainsAny(p, " \t") {
			p = `"` + p + `"`
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// splitCompletePath turns a typed path prefix into the directory to list,
// the prefix to put back on matches (as the user typed it, including ~/),
// and the filename prefix to filter on.
func splitCompletePath(token string) (searchDir, displayDir, base string) {
	home, _ := os.UserHomeDir()
	if token == "~" || token == "~/" {
		if home == "" {
			return ".", "", token
		}
		return home, "~/", ""
	}
	if strings.HasPrefix(token, "~/") && home != "" {
		rest := token[2:]
		d, b := splitDirBase(rest)
		return filepath.Join(home, d), "~/" + d, b
	}
	d, b := splitDirBase(token)
	search := d
	if search == "" {
		search = "."
	}
	return search, d, b
}

func splitDirBase(token string) (dir, base string) {
	i := strings.LastIndex(token, "/")
	if i < 0 {
		return "", token
	}
	return token[:i+1], token[i+1:]
}

// oddTrailingBackslash reports whether s's last line ends with an unescaped
// backslash, the shell-style "this line continues" marker.
func oddTrailingBackslash(s string) bool {
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		s = s[i+1:]
	}
	n := 0
	for i := len(s) - 1; i >= 0 && s[i] == '\\'; i-- {
		n++
	}
	return n%2 == 1
}
