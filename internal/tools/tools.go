// Package tools implements cubcoder's local coding tools. Unlike cubgraph's
// server-side, workspace-jailed tools, these operate on the user's current
// working directory — cubcoder is a local harness, like Claude Code.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"cubcoder/internal/provider"
)

// Tool is one callable tool.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	Mutating    bool // writes files or runs commands → needs permission
	Quiet       bool // suppress the agent's "running …" spinner — the tool renders its own progress
	// Run executes the tool. ctx is the turn's context: it is cancelled when the
	// user interrupts the turn (Esc), so anything long-running — a command, an
	// HTTP fetch, a blocking wait — must derive its own deadline from it rather
	// than context.Background(), or the interrupt can't reach it.
	Run func(ctx context.Context, args string) (string, error)
	// RunRich, when set, is used instead of Run by the agent loop: it can return
	// images alongside the text result (read_file on a PNG). Run should still
	// be set — as the text-only fallback — for callers that only know Run.
	RunRich func(ctx context.Context, args string) (string, []provider.Image, error)
}

// Validate checks raw tool-call arguments against the tool's JSON-schema
// parameters before the tool runs: the args must parse as a JSON object and
// carry every required property (a null value counts as missing). It returns an
// error phrased for the model to self-correct. A weaker local model emits
// malformed or under-specified tool calls more often than a frontier one, so
// catching them here — with a specific hint and without running the tool or
// prompting the user — beats a downstream failure or a run on garbage.
func (t *Tool) Validate(args string) error {
	args = strings.TrimSpace(args)
	if args == "" {
		args = "{}"
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(args), &obj); err != nil {
		return fmt.Errorf("arguments must be a JSON object; expected %s", t.argHint())
	}
	var missing []string
	for _, name := range requiredNames(t.Parameters) {
		if v, ok := obj[name]; !ok || v == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required argument(s) %s; expected %s",
			strings.Join(quoteAll(missing), ", "), t.argHint())
	}
	return nil
}

// argHint summarizes a tool's parameters for a correction message, e.g.
// "path (required), offset, limit".
func (t *Tool) argHint() string {
	props, _ := t.Parameters["properties"].(map[string]any)
	if len(props) == 0 {
		return "no arguments"
	}
	req := map[string]bool{}
	for _, n := range requiredNames(t.Parameters) {
		req[n] = true
	}
	// Sort for a stable hint, required fields first.
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if req[names[i]] != req[names[j]] {
			return req[names[i]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, len(names))
	for i, n := range names {
		if req[n] {
			parts[i] = n + " (required)"
		} else {
			parts[i] = n
		}
	}
	return strings.Join(parts, ", ")
}

// requiredNames reads a schema's "required" list, tolerating both the in-code
// []string (built by obj()) and the []any a JSON-decoded schema would carry.
func requiredNames(schema map[string]any) []string {
	switch r := schema["required"].(type) {
	case []string:
		return r
	case []any:
		out := make([]string, 0, len(r))
		for _, v := range r {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func quoteAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// Registry holds the available tools.
type Registry struct {
	tools map[string]*Tool
	order []string
}

// Console connects run_command to the user's terminal so commands that prompt
// for input — sudo, ssh, anything that reads a password — can be answered
// interactively. Out is where live command output (including the prompt) is
// mirrored so the user can see it; input is read from the controlling terminal
// (/dev/tty). Set only for the interactive lead session: a Console is nil for
// background worker agents, which run concurrently and must not touch the
// terminal, and for piped/non-tty runs.
type Console struct {
	Out io.Writer
	// Ask poses a yes/no question on the terminal and returns the answer. Used
	// to offer remembering a typed sudo password for the session. Nil disables
	// the offer.
	Ask func(question string) bool
	// SuspendInput, when set, takes any background terminal reader the host
	// runs (the REPL's Esc watcher) off /dev/tty for the duration of an
	// interactive command, so the two never race for keystrokes. It returns
	// the function that puts the reader back.
	SuspendInput func() (resume func())
	// Interrupt, when set, cancels the turn currently in flight. The
	// interactive command's input pump calls it on a bare Escape keypress:
	// the Esc watcher is suspended while a command owns the terminal, so the
	// pump is the only reader that can see the keystroke — without this, Esc
	// during a long interactive command would be swallowed by the command's
	// stdin and the interrupt would silently do nothing.
	Interrupt func()

	mu     sync.Mutex
	sudoPW []byte // session sudo password; memory only, never persisted
}

// rememberSudo caches the sudo password for the rest of the session, so later
// sudo prompts in interactive commands are answered without retyping it.
func (c *Console) rememberSudo(pw []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sudoPW = append(c.sudoPW[:0:0], pw...)
}

// sudoPassword returns a copy of the remembered sudo password, if any.
func (c *Console) sudoPassword() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sudoPW) == 0 {
		return nil, false
	}
	return append([]byte(nil), c.sudoPW...), true
}

// SudoRemembered reports whether a sudo password is cached for this session.
func (c *Console) SudoRemembered() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sudoPW) > 0
}

// ForgetSudo zeroes and drops the remembered sudo password, reporting whether
// one was remembered.
func (c *Console) ForgetSudo() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sudoPW) == 0 {
		return false
	}
	for i := range c.sudoPW {
		c.sudoPW[i] = 0
	}
	c.sudoPW = nil
	return true
}

// New builds a registry whose tools operate on the process working directory
// (the original behavior). Equivalent to NewIn("").
func New() *Registry { return NewIn("") }

// NewIn builds a registry whose tools resolve paths against root and run
// commands with root as their working directory. An empty root means the
// process working directory — file ops use paths as given (absolute paths
// allowed) and commands inherit the process CWD. A non-empty root is a jail:
// file-tool paths that resolve outside it (`..`, an absolute path elsewhere, a
// symlink out) are rejected, which is what keeps a background worker inside
// its git worktree. Commands still run with the worktree as cwd but are not
// themselves path-jailed (the shell can reach anywhere). Use NewInteractive
// to allow terminal input.
func NewIn(root string) *Registry { return newRegistry(root, nil) }

// NewInteractive is NewIn with a Console attached, so commands that prompt for
// input can be answered from the terminal. Used for the lead agent in an
// interactive session on a tty.
func NewInteractive(root string, con *Console) *Registry { return newRegistry(root, con) }

func newRegistry(root string, con *Console) *Registry {
	r := &Registry{tools: map[string]*Tool{}}
	r.register(readFile(root))
	r.register(listDir(root))
	r.register(searchCode(root))
	r.register(gitDiff(root))
	r.register(webSearch())
	r.register(fetchURL())
	r.register(writeFile(root))
	r.register(applyPatch(root))
	r.register(editFile(root))
	r.register(runCommand(root, con))
	return r
}

func (r *Registry) register(t *Tool) {
	if _, exists := r.tools[t.Name]; !exists {
		r.order = append(r.order, t.Name)
	}
	r.tools[t.Name] = t
}

// Register adds a tool to the registry. Used to attach tools built outside this
// package (e.g. the orchestrator's delegation tools) to an agent's registry.
func (r *Registry) Register(t *Tool) { r.register(t) }

func (r *Registry) Get(name string) (*Tool, bool) { t, ok := r.tools[name]; return t, ok }

// Schemas returns the tool definitions to advertise to the model.
func (r *Registry) Schemas() []provider.ToolSchema {
	var out []provider.ToolSchema
	for _, n := range r.order {
		t := r.tools[n]
		out = append(out, provider.ToolSchema{Name: t.Name, Description: t.Description, Parameters: t.Parameters})
	}
	return out
}

// ---- helpers ----

func obj(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}
func str(desc string) map[string]any  { return map[string]any{"type": "string", "description": desc} }
func intp(desc string) map[string]any { return map[string]any{"type": "integer", "description": desc} }

func parse(args string, dst any) error {
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return json.Unmarshal([]byte(args), dst)
}

// resolve maps a tool path argument to a real filesystem path.
//
// An empty root leaves the path as-is (process-CWD behavior for the lead
// agent). A non-empty root is a jail: the path is joined onto root when
// relative, then cleaned and symlink-resolved, and rejected if the result
// is not inside root. Absolute paths are allowed only when they land inside
// the jail. This is what keeps a worker agent from writing `/etc/...` or
// `../../secret` even though it auto-approves.
func resolve(root, path string) (string, error) {
	if path == "" {
		path = "."
	}
	if root == "" {
		return path, nil
	}
	rootAbs, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	var full string
	if filepath.IsAbs(path) {
		full = filepath.Clean(path)
	} else {
		full = filepath.Join(rootAbs, path)
	}
	full = canonicalPath(full)
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || !inRoot(rel) {
		return "", fmt.Errorf("path %q is outside the working directory", path)
	}
	return full, nil
}

// jailRel is resolve for tools that pass a path to a command running with
// Dir=root (rg, git). It returns a path relative to root so the command
// cannot be pointed at an escaped location even if it ignores our cwd.
func jailRel(root, path string) (string, error) {
	full, err := resolve(root, path)
	if err != nil {
		return "", err
	}
	if root == "" {
		return full, nil
	}
	rootAbs, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(rootAbs, full)
	if err != nil || !inRoot(rel) {
		return "", fmt.Errorf("path %q is outside the working directory", path)
	}
	return rel, nil
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		return r, nil
	}
	return filepath.Clean(abs), nil
}

// canonicalPath symlink-resolves path when it exists. For a path that does
// not exist yet (write_file creating a new file), it resolves the longest
// existing prefix and joins the rest, so a symlink-out-of-jail parent is
// still caught.
func canonicalPath(path string) string {
	path = filepath.Clean(path)
	if r, err := filepath.EvalSymlinks(path); err == nil {
		return r
	}
	var rest []string
	cur := path
	for {
		dir := filepath.Dir(cur)
		if dir == cur {
			break
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		if r, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(append([]string{r}, rest...)...)
		}
		cur = dir
	}
	return path
}

func inRoot(rel string) bool {
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

// ---- read_file ----

func readFile(root string) *Tool {
	rich := func(_ context.Context, args string) (string, []provider.Image, error) {
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := parse(args, &a); err != nil {
			return "", nil, err
		}
		full, err := resolve(root, a.Path)
		if err != nil {
			return "", nil, err
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return "", nil, err
		}
		if isImage(data) {
			// The image itself goes to the model as image content; the text
			// names what it is so the transcript (and a model that can't see
			// images) still makes sense.
			img, desc, err := LoadImage(a.Path, data)
			if err != nil {
				return "", nil, err
			}
			return fmt.Sprintf("[image %s (%s) — attached to this result for viewing]", a.Path, desc), []provider.Image{img}, nil
		}
		text := string(data)
		if isPDF(data) {
			if text, err = extractPDFText(full); err != nil {
				return "", nil, fmt.Errorf("%s: %w", a.Path, err)
			}
		}
		lines := strings.Split(text, "\n")
		start := a.Offset
		if start < 1 {
			start = 1
		}
		limit := a.Limit
		if limit <= 0 {
			limit = 2000
		}
		end := start - 1 + limit
		if end > len(lines) {
			end = len(lines)
		}
		var b strings.Builder
		for i := start - 1; i < end; i++ {
			fmt.Fprintf(&b, "%6d\t%s\n", i+1, lines[i])
		}
		if end < len(lines) {
			fmt.Fprintf(&b, "... (%d more lines; call again with offset=%d)\n", len(lines)-end, end+1)
		}
		return b.String(), nil, nil
	}
	return &Tool{
		Name:        "read_file",
		Description: "Read a file from the working directory and return it with line numbers. PDF files are converted to plain text automatically (page markers included; table layout preserved). Image files (png, jpeg, gif, webp) are returned as an image you can look at.",
		Parameters: obj(map[string]any{
			"path":   str("file path, relative to the working directory (or absolute inside it)"),
			"offset": intp("1-indexed line to start from (optional)"),
			"limit":  intp("max lines to return (optional, default 2000)"),
		}, "path"),
		RunRich: rich,
		Run: func(ctx context.Context, args string) (string, error) {
			out, _, err := rich(ctx, args)
			return out, err
		},
	}
}

// ---- list_dir ----

func listDir(root string) *Tool {
	return &Tool{
		Name:        "list_dir",
		Description: "List directory contents. Directories are suffixed with /.",
		Parameters:  obj(map[string]any{"path": str("directory to list; defaults to .")}),
		Run: func(_ context.Context, args string) (string, error) {
			var a struct {
				Path string `json:"path"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				a.Path = "."
			}
			full, err := resolve(root, a.Path)
			if err != nil {
				return "", err
			}
			entries, err := os.ReadDir(full)
			if err != nil {
				return "", err
			}
			var names []string
			for _, e := range entries {
				n := e.Name()
				if e.IsDir() {
					n += "/"
				}
				names = append(names, n)
			}
			sort.Strings(names)
			if len(names) == 0 {
				return "(empty directory)", nil
			}
			return strings.Join(names, "\n"), nil
		},
	}
}

// ---- search_code (ripgrep) ----

func searchCode(root string) *Tool {
	return &Tool{
		Name:        "search_code",
		Description: "Search the working directory with ripgrep. Returns path:line:text matches.",
		Parameters: obj(map[string]any{
			"pattern": str("regex pattern (ripgrep syntax)"),
			"path":    str("directory or file to search; defaults to ."),
		}, "pattern"),
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Pattern string `json:"pattern"`
				Path    string `json:"path"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			if a.Path == "" {
				a.Path = "."
			}
			searchPath, err := jailRel(root, a.Path)
			if err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "rg", "--no-heading", "--line-number", "--color=never", "--max-count=50", "--", a.Pattern, searchPath)
			cmd.Dir = root // relative paths (and ".") resolve inside this agent's directory
			out, err := cmd.CombinedOutput()
			if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 1 {
				return "(no matches)", nil
			}
			if err != nil && len(out) == 0 {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", fmt.Errorf("ripgrep error (is rg installed?): %v", err)
			}
			return strings.TrimSpace(string(out)), nil
		},
	}
}

// ---- git_diff (read-only) ----

func gitDiff(root string) *Tool {
	return &Tool{
		Name:        "git_diff",
		Description: "Show the git diff in the working directory. Read-only.",
		Parameters: obj(map[string]any{
			"path":   str("limit the diff to this path (optional)"),
			"staged": map[string]any{"type": "boolean", "description": "show staged (cached) diff instead of unstaged"},
			"ref":    str("compare the working tree against this ref, e.g. HEAD~1 or main (optional)"),
		}),
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Path   string `json:"path"`
				Staged bool   `json:"staged"`
				Ref    string `json:"ref"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			cmdArgs := []string{"diff"}
			if a.Staged {
				cmdArgs = append(cmdArgs, "--cached")
			}
			if a.Ref != "" {
				cmdArgs = append(cmdArgs, a.Ref)
			}
			if a.Path != "" {
				p, err := jailRel(root, a.Path)
				if err != nil {
					return "", err
				}
				cmdArgs = append(cmdArgs, "--", p)
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "git", cmdArgs...)
			cmd.Dir = root // diff this agent's working directory (its worktree)
			cmd.Env = commandEnv()
			out, err := cmd.CombinedOutput()
			if err != nil && len(out) == 0 {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", fmt.Errorf("git diff failed: %v", err)
			}
			s := strings.TrimSpace(string(out))
			if s == "" {
				return "(no changes)", nil
			}
			if len(s) > 50000 {
				s = s[:50000] + "\n... (diff truncated; narrow with path=)"
			}
			return s, nil
		},
	}
}

// ---- web_search (read-only) ----

// DefaultSearchURL is the SearXNG base web_search queries. It is empty by
// default: the search node is a private CubCloud service with no public
// endpoint, so its address is not shipped in the binary. Point web_search at a
// SearXNG base with the search_url config field / CUBCODER_SEARCH_URL; until
// then web_search reports that it is not configured.
const DefaultSearchURL = ""

// searchURL is the active SearXNG base. SetSearchURL overrides it once at
// startup; web_search reads it per call.
var searchURL = DefaultSearchURL

// SetSearchURL points web_search at a different SearXNG base. An empty or
// blank url is ignored, so callers can pass an unset config field harmlessly.
func SetSearchURL(u string) {
	if u = strings.TrimRight(strings.TrimSpace(u), "/"); u != "" {
		searchURL = u
	}
}

// searchResults is the subset of SearXNG's JSON response web_search renders.
type searchResults struct {
	Results []struct {
		Title   string `json:"title"`
		URL     string `json:"url"`
		Content string `json:"content"`
	} `json:"results"`
}

func webSearch() *Tool {
	return &Tool{
		Name:        "web_search",
		Description: "Search the web via CubCloud's self-hosted search node and return ranked results (title, URL, snippet). Use for current information that is not in the codebase. Read-only; no files change.",
		Parameters: obj(map[string]any{
			"query": str("the search query"),
		}, "query"),
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Query string `json:"query"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			if strings.TrimSpace(a.Query) == "" {
				return "", fmt.Errorf("query is required")
			}
			if searchURL == "" {
				return "", fmt.Errorf("web_search is not configured: set search_url (or CUBCODER_SEARCH_URL) to a SearXNG base with JSON output enabled")
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			endpoint := fmt.Sprintf("%s/search?q=%s&format=json", searchURL, url.QueryEscape(a.Query))
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
			if err != nil {
				return "", err
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("search request failed (is the search node at %s up?): %w", searchURL, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("search node returned %s", resp.Status)
			}
			var payload searchResults
			if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
				return "", fmt.Errorf("decode search results: %w", err)
			}
			if len(payload.Results) == 0 {
				return "(no results)", nil
			}
			const maxResults = 8
			var b strings.Builder
			for i, r := range payload.Results {
				if i >= maxResults {
					break
				}
				fmt.Fprintf(&b, "%d. %s\n   %s\n", i+1, strings.TrimSpace(r.Title), strings.TrimSpace(r.URL))
				if c := strings.TrimSpace(r.Content); c != "" {
					fmt.Fprintf(&b, "   %s\n", c)
				}
			}
			return strings.TrimRight(b.String(), "\n"), nil
		},
	}
}

// ---- fetch_url (read-only) ----

// HTML-to-text regexes, compiled once. htmlToText is a pragmatic stripper, not a
// full parser: it drops script/style/comment content, turns block-closing tags
// into line breaks so structure survives, removes the rest of the markup, and
// decodes entities. Good enough to hand a page's prose to the model.
var (
	reScriptStyle = regexp.MustCompile(`(?is)<(script|style|noscript)[^>]*>.*?</(script|style|noscript)>`)
	reComment     = regexp.MustCompile(`(?s)<!--.*?-->`)
	reBlockBreak  = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article|header|footer|blockquote)>|<br\s*/?>`)
	reTag         = regexp.MustCompile(`(?s)<[^>]+>`)
	reInlineSpace = regexp.MustCompile(`[ \t\f\v]+`)
	reBlankLines  = regexp.MustCompile(`\n{3,}`)
)

func htmlToText(s string) string {
	s = reScriptStyle.ReplaceAllString(s, " ")
	s = reComment.ReplaceAllString(s, " ")
	s = reBlockBreak.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reInlineSpace.ReplaceAllString(s, " ")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimSpace(ln)
	}
	s = reBlankLines.ReplaceAllString(strings.Join(lines, "\n"), "\n\n")
	return strings.TrimSpace(s)
}

func fetchURL() *Tool {
	return &Tool{
		Name:        "fetch_url",
		Description: "Fetch an http(s) web page and return its readable text, with HTML markup, scripts, and styles stripped. Pair with web_search to read a result in full. Read-only; no files change.",
		Parameters: obj(map[string]any{
			"url": str("the http or https URL to fetch"),
		}, "url"),
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				URL string `json:"url"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			parsed, err := url.Parse(strings.TrimSpace(a.URL))
			if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return "", fmt.Errorf("url must be an http or https URL")
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("User-Agent", "cubcoder web fetcher")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", fmt.Errorf("fetch failed: %w", err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return "", fmt.Errorf("fetch returned %s", resp.Status)
			}
			const maxBytes = 5 << 20 // cap the download at 5 MiB
			body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
			if err != nil {
				return "", fmt.Errorf("read body: %w", err)
			}
			text := string(body)
			if strings.Contains(resp.Header.Get("Content-Type"), "html") {
				text = htmlToText(text)
			} else {
				text = strings.TrimSpace(text)
			}
			if text == "" {
				return "(no readable text)", nil
			}
			const maxChars = 20000
			if len(text) > maxChars {
				text = strings.ToValidUTF8(text[:maxChars], "") + "\n... (truncated)"
			}
			return text, nil
		},
	}
}

// ---- write_file ----

func writeFile(root string) *Tool {
	return &Tool{
		Name:        "write_file",
		Description: "Create or overwrite a file. Creates parent directories as needed.",
		Mutating:    true,
		Parameters: obj(map[string]any{
			"path":    str("file path, relative to the working directory (or absolute inside it)"),
			"content": str("full file contents"),
		}, "path", "content"),
		Run: func(_ context.Context, args string) (string, error) {
			var a struct {
				Path    string `json:"path"`
				Content string `json:"content"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			path, err := resolve(root, a.Path)
			if err != nil {
				return "", err
			}
			if dir := filepath.Dir(path); dir != "" {
				_ = os.MkdirAll(dir, 0o755)
			}
			if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

// ---- edit_file (unique search/replace) ----

func editFile(root string) *Tool {
	return &Tool{
		Name:        "edit_file",
		Description: "Replace a snippet in a file. Prefer apply_patch for multi-hunk or multi-file edits. Matching is unique by default and tolerates whitespace, indent, and read_file line-number prefixes. Set replace_all=true to replace every match.",
		Mutating:    true,
		Parameters: obj(map[string]any{
			"path":        str("file path, relative to the working directory (or absolute inside it)"),
			"search":      str("text to find"),
			"replace":     str("replacement text"),
			"replace_all": map[string]any{"type": "boolean", "description": "replace every match instead of requiring a unique one"},
		}, "path", "search", "replace"),
		Run: func(_ context.Context, args string) (string, error) {
			var a struct {
				Path       string `json:"path"`
				Search     string `json:"search"`
				Replace    string `json:"replace"`
				ReplaceAll bool   `json:"replace_all"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			path, err := resolve(root, a.Path)
			if err != nil {
				return "", err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return "", err
			}
			updated, n, err := replaceSpan(string(data), a.Search, a.Replace, a.ReplaceAll)
			if err != nil {
				return "", fmt.Errorf("%s: %w", a.Path, err)
			}
			if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
				return "", err
			}
			if n == 1 {
				return fmt.Sprintf("edited %s", a.Path), nil
			}
			return fmt.Sprintf("edited %s (%d replacements)", a.Path, n), nil
		},
	}
}

// ---- run_command ----

// Timeout tuning, as vars so tests can shrink them. commandWaitDelay bounds how
// long Wait blocks on output pipes still held by children after sh exits or the
// group is killed — without it a spawned server inherits the pipe and
// CombinedOutput blocks until that server dies (observed as a multi-hour
// "running" hang on `node server.js`-style commands).
var (
	commandTimeout   = 120 * time.Second
	commandWaitDelay = 5 * time.Second
)

// commandTimeoutMax caps the per-call timeout argument: a model can stretch a
// long build's deadline, not disable the runaway backstop entirely.
const commandTimeoutMax = 10 * time.Minute

// SetCommandTimeout overrides the default run_command timeout (config field
// command_timeout / CUBCODER_COMMAND_TIMEOUT). Zero or negative is ignored,
// so callers can pass an unset config field harmlessly. Call before building
// registries: the tool's description quotes the default.
func SetCommandTimeout(d time.Duration) {
	if d > 0 {
		commandTimeout = d
	}
}

func runCommand(root string, con *Console) *Tool {
	return &Tool{
		Name: "run_command",
		Description: fmt.Sprintf("Run a shell command in the working directory (sh -c). Default timeout %s — override for a long build with the timeout argument (seconds, max %s); on timeout the whole process group is killed. Commands that prompt for input (sudo, ssh) can be answered from the terminal in an interactive session. Never start a server or other long-running process in the foreground — run it in the background with output to a file (e.g. `nohup npm run dev >/tmp/dev.log 2>&1 &`), then poll it with curl or read the log.",
			commandTimeout, commandTimeoutMax),
		Mutating: true,
		// When interactive, the command's output streams live to the terminal, so
		// the agent's "running …" spinner would fight it for the line — suppress
		// it. runInteractive shows its own elapsed-time line when the command goes
		// quiet (see liveWriter), so long silent commands still signal liveness.
		Quiet: con != nil,
		Parameters: obj(map[string]any{
			"command": str("shell command line"),
			"timeout": intp("timeout in seconds for this command (optional; for long builds/tests)"),
		}, "command"),
		Run: func(ctx context.Context, args string) (string, error) {
			var a struct {
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := parse(args, &a); err != nil {
				return "", err
			}
			timeout := commandTimeout
			if a.Timeout > 0 {
				timeout = min(time.Duration(a.Timeout)*time.Second, commandTimeoutMax)
			}
			// Derive the command's deadline from the turn context: Esc cancels ctx,
			// which fires cmd.Cancel and kills the whole process group, so an
			// interrupt no longer waits out a long-running command.
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			if con != nil {
				return runInteractive(ctx, root, a.Command, con, timeout)
			}
			return runCaptured(ctx, root, a.Command, timeout)
		},
	}
}

// commandEnv is the environment every spawned command (and git_diff) inherits.
// Interactive run_command attaches a real pty, so git/man/systemctl see a
// terminal and launch $PAGER (usually less) — the user then has to press q
// before the tool returns. Force cat (and LESS=FRX as a backstop if less is
// still invoked) so existing-info commands dump and exit. Parent PAGER /
// GIT_PAGER values are stripped so they cannot win.
func commandEnv() []string {
	override := map[string]string{
		"PAGER":         "cat",
		"GIT_PAGER":     "cat",
		"GH_PAGER":      "cat",
		"MANPAGER":      "cat",
		"SYSTEMD_PAGER": "cat",
		"AWS_PAGER":     "",
		"LESS":          "FRX",
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
	keys := make([]string, 0, len(override))
	for k := range override {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+override[k])
	}
	return out
}

// runCaptured runs the command non-interactively (stdin is /dev/null) and
// returns its combined output. This is the default for worker agents and
// non-tty sessions.
func runCaptured(ctx context.Context, root, command string, timeout time.Duration) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = root // run in this agent's directory (its worktree)
	cmd.Env = commandEnv()
	// Run the command in its own process group and kill the whole group
	// on timeout. The default cancel kills only sh, leaving its children
	// (a test runner, a server) alive and holding the output pipes.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = commandWaitDelay
	out, err := cmd.CombinedOutput()
	code := 0
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	_ = err
	return commandResult(ctx, code, string(out), timeout), nil
}

// commandResult formats a finished command's output the way the model sees it:
// an exit_code line, the trimmed output, and a timeout note when the deadline
// fired. timeout is the deadline this run actually used (default or per-call).
func commandResult(ctx context.Context, code int, out string, timeout time.Duration) string {
	res := fmt.Sprintf("exit_code=%d\n%s", code, strings.TrimRight(out, "\n"))
	switch ctx.Err() {
	case context.DeadlineExceeded:
		res += fmt.Sprintf("\n(command timed out after %s and its process group was killed; raise the timeout argument for long builds, and start servers in the background with output redirected to a file)", timeout)
	case context.Canceled:
		res += "\n(command interrupted by the user before it finished; its process group was killed)"
	}
	return res
}
