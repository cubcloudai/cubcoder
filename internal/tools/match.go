package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// replaceSpan replaces search with replace in content. When all is false the
// match must be unique; when true every non-overlapping match is replaced.
// Matching is exact first, then trailing-whitespace-tolerant, then
// indent-tolerant — local models routinely drift on those. read_file line
// number prefixes copied into search are stripped. Returns the new content
// and how many replacements landed.
func replaceSpan(content, search, replace string, all bool) (string, int, error) {
	crlf := strings.Contains(content, "\r\n")
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
		search = strings.ReplaceAll(search, "\r\n", "\n")
		replace = strings.ReplaceAll(replace, "\r\n", "\n")
	}
	search = stripReadFileLineNumbers(search)
	replace = stripReadFileLineNumbers(replace)
	// A search that ends in a newline addresses whole lines. Matching works on
	// the text before that newline, so drop the newline from both sides or the
	// replacement would gain a blank line. An empty replacement then deletes
	// the line itself, newline included, instead of leaving a blank one.
	lineSearch := strings.HasSuffix(search, "\n")
	if lineSearch {
		search = strings.TrimSuffix(search, "\n")
		replace = strings.TrimSuffix(replace, "\n")
	}
	if search == "" {
		return "", 0, fmt.Errorf("search text is empty")
	}
	spans := findSpans(content, search)
	if len(spans) == 0 {
		return "", 0, fmt.Errorf("search text not found%s", hintNearby(content, search))
	}
	if !all && len(spans) > 1 {
		return "", 0, fmt.Errorf("search text matches %d places; add context to make it unique or set replace_all=true", len(spans))
	}
	out := content
	for i := len(spans) - 1; i >= 0; i-- {
		sp := spans[i]
		end := sp.end
		if lineSearch && replace == "" && end < len(out) && out[end] == '\n' {
			end++
		}
		out = out[:sp.start] + reindent(sp.fileLines, replace) + out[end:]
	}
	if crlf {
		out = strings.ReplaceAll(out, "\n", "\r\n")
	}
	return out, len(spans), nil
}

type span struct {
	start, end int
	fileLines  []string // matched original lines; set on indent-tolerant matches
}

func findSpans(content, search string) []span {
	if sp := findExact(content, search); len(sp) > 0 {
		return sp
	}
	if sp := findLineMatch(content, search, matchRtrim, false); len(sp) > 0 {
		return sp
	}
	return findLineMatch(content, search, matchIndent, true)
}

func findExact(content, search string) []span {
	search = strings.TrimSuffix(search, "\n")
	if search == "" {
		return nil
	}
	var out []span
	from := 0
	for {
		i := strings.Index(content[from:], search)
		if i < 0 {
			return out
		}
		start := from + i
		out = append(out, span{start: start, end: start + len(search)})
		from = start + len(search)
	}
}

type lineEq func(file, search []string) bool

func matchRtrim(file, search []string) bool {
	for i := range search {
		if strings.TrimRight(file[i], " \t") != strings.TrimRight(search[i], " \t") {
			return false
		}
	}
	return true
}

func matchIndent(file, search []string) bool {
	for i := range search {
		if strings.TrimLeft(file[i], " \t") != strings.TrimLeft(search[i], " \t") {
			return false
		}
	}
	return true
}

func findLineMatch(content, search string, eq lineEq, keepLines bool) []span {
	sLines, _ := splitKeepNL(search)
	if len(sLines) == 0 {
		return nil
	}
	cLines, starts, ends := indexLines(content)
	if len(cLines) < len(sLines) {
		return nil
	}
	n := len(sLines)
	var out []span
	for i := 0; i <= len(cLines)-n; {
		if !eq(cLines[i:i+n], sLines) {
			i++
			continue
		}
		sp := span{start: starts[i], end: ends[i+n-1]}
		if keepLines {
			sp.fileLines = append([]string(nil), cLines[i:i+n]...)
		}
		out = append(out, sp)
		i += n
	}
	return out
}

// indexLines returns each line (no newline) and the [start, end) byte span of
// that line's text in content. end does not include a following newline, so a
// replacement keeps the file's newline structure around the match.
func indexLines(content string) (lines []string, starts, ends []int) {
	if content == "" {
		return nil, nil, nil
	}
	from := 0
	for {
		i := strings.IndexByte(content[from:], '\n')
		if i < 0 {
			lines = append(lines, content[from:])
			starts = append(starts, from)
			ends = append(ends, len(content))
			return
		}
		i += from
		lines = append(lines, content[from:i])
		starts = append(starts, from)
		ends = append(ends, i)
		from = i + 1
		if from == len(content) {
			// trailing newline: a final empty line, so a last-line match can
			// still address the empty string after the last \n.
			lines = append(lines, "")
			starts = append(starts, from)
			ends = append(ends, from)
			return
		}
	}
}

func splitKeepNL(s string) ([]string, bool) {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil, false
	}
	return strings.Split(s, "\n"), true
}

func leadingWS(s string) string {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[:i]
}

// reindent copies leading whitespace from the matched file lines onto the
// replacement when the model under-indented it. Only used for indent-tolerant
// matches; an intentional dedent still wins when the replacement is longer.
func reindent(fileLines []string, replace string) string {
	if len(fileLines) == 0 || replace == "" {
		return replace
	}
	rLines := strings.Split(replace, "\n")
	if len(rLines) != len(fileLines) {
		return replace
	}
	for i := range rLines {
		if strings.TrimSpace(rLines[i]) == "" {
			continue
		}
		fi, ni := leadingWS(fileLines[i]), leadingWS(rLines[i])
		if len(ni) < len(fi) {
			rLines[i] = fi + strings.TrimLeft(rLines[i], " \t")
		}
	}
	return strings.Join(rLines, "\n")
}

// read_file formats lines as "%6d\t%s". Models often paste that into search
// or a patch; strip it when most non-empty lines look like that.
var readFilePrefix = regexp.MustCompile(`^[ \t]*\d+\t`)

func stripReadFileLineNumbers(s string) string {
	lines := strings.Split(s, "\n")
	hit, nonempty := 0, 0
	for _, ln := range lines {
		if ln == "" {
			continue
		}
		nonempty++
		if readFilePrefix.MatchString(ln) {
			hit++
		}
	}
	if nonempty == 0 || hit*2 < nonempty {
		return s
	}
	for i, ln := range lines {
		lines[i] = readFilePrefix.ReplaceAllString(ln, "")
	}
	return strings.Join(lines, "\n")
}

func hintNearby(content, search string) string {
	key := ""
	for _, ln := range strings.Split(search, "\n") {
		ln = strings.TrimSpace(stripReadFileLineNumbers(ln))
		if ln != "" {
			key = ln
			break
		}
	}
	if key == "" {
		return ""
	}
	if len(key) > 80 {
		key = key[:80]
	}
	needle := key
	hits := collectHits(content, needle)
	if len(hits) == 0 && len(needle) > 8 {
		hits = collectHits(content, needle[:8])
	}
	if len(hits) == 0 {
		return ""
	}
	return "\nnearby lines:\n" + strings.Join(hits, "\n")
}

func collectHits(content, key string) []string {
	var hits []string
	for i, ln := range strings.Split(content, "\n") {
		if !strings.Contains(ln, key) && !strings.Contains(strings.TrimSpace(ln), strings.TrimSpace(key)) {
			continue
		}
		show := ln
		if len(show) > 80 {
			show = show[:80] + "…"
		}
		hits = append(hits, fmt.Sprintf("%6d\t%s", i+1, show))
		if len(hits) == 3 {
			break
		}
	}
	return hits
}
