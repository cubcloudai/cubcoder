// cubcoder — a local coding agent that drives the CubCloud local model (via
// the LiteLLM gateway) with a Claude-Code-style tool loop. Single static
// binary; ships on client VMs. MVP: local model only.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"cubcoder/internal/agent"
	"cubcoder/internal/banner"
	"cubcoder/internal/checkpoint"
	"cubcoder/internal/config"
	"cubcoder/internal/orchestrator"
	"cubcoder/internal/provider"
	"cubcoder/internal/session"
	"cubcoder/internal/tools"
)

// spinner is an in-place progress indicator. Start/Stop are called from a
// single goroutine (the agent loop), so they need no locking between them; the
// only concurrency is the ticker goroutine, which Stop joins before returning.
type spinner struct {
	enabled bool
	stop    chan struct{}
	done    chan struct{}
	// workers, if set, is polled each frame so background agent last-lines
	// ride the thinking/running line instead of being invisible until /agents.
	workers func() []orchestrator.Snapshot
}

func (s *spinner) Start(label string) {
	if !s.enabled {
		return
	}
	s.Stop() // clear any previous
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	go func() {
		// ASCII frames only: braille spinners render as identical tofu boxes on
		// fonts without U+28xx coverage (e.g. DejaVu Sans Mono, Konsole's
		// default), which reads as a frozen solid block.
		frames := `|/-\`
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		start := time.Now()
		// Elapsed time distinguishes "slow but alive" from "hung" while waiting
		// on the model; it resets per wait, not per turn. Frame and counter stay
		// at normal intensity — dim can be near-invisible on dark schemes, and
		// this line is the only liveness signal.
		draw := func(i int) {
			line := fmt.Sprintf("%c %s%s…%s %s", frames[i%len(frames)], dim, label, reset, time.Since(start).Round(time.Second))
			if s.workers != nil {
				if extra := workerSpinnerSuffix(s.workers(), termWidth(os.Stdout.Fd())); extra != "" {
					line += dim + extra + reset
				}
			}
			fmt.Printf("\r\033[K%s", line)
		}
		draw(0) // show immediately; a blank first 100ms looks like a hang
		for i := 1; ; i++ {
			select {
			case <-s.stop:
				fmt.Print("\r\033[K") // clear the line
				close(s.done)
				return
			case <-t.C:
				draw(i)
			}
		}
	}()
}

func (s *spinner) Stop() {
	if s.stop == nil {
		return
	}
	close(s.stop)
	<-s.done
	s.stop = nil
}

// version is set at build time via -ldflags "-X main.version=...".
var version = "dev"

const (
	dim    = "\033[2m"
	cyan   = "\033[36m"
	green  = "\033[32m"
	red    = "\033[31m"
	yellow = "\033[33m"
	reset  = "\033[0m"
)

func main() {
	// Subcommands handled before flag parsing. login and config write the
	// persistent config file (~/.cubcoder/config.json) so the API key survives
	// reboots and new shells, instead of living in a session-only env var that
	// `export` clears on logout.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "login", "--login", "-login":
			if err := runLogin(); err != nil {
				fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
				os.Exit(1)
			}
			return
		case "config":
			if err := runConfig(os.Args[2:]); err != nil {
				fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
				os.Exit(1)
			}
			return
		case "sessions":
			if err := printSessions(""); err != nil {
				fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
				os.Exit(1)
			}
			return
		}
	}

	var (
		url         = flag.String("url", "", "LiteLLM/OpenAI-compatible base URL (env CUBCODER_API_URL)")
		key         = flag.String("key", "", "API key (env CUBCODER_API_KEY)")
		model       = flag.String("model", "", "model name (env CUBCODER_MODEL); a claude-* name uses the Claude backend")
		anthKey     = flag.String("anthropic-key", "", "Anthropic API key (env CUBCODER_ANTHROPIC_KEY)")
		maxTokens   = flag.Int("max-tokens", 0, "output token cap per turn (env CUBCODER_MAX_TOKENS; default 32768)")
		ctxLimit    = flag.Int("context-limit", 0, "total context window; caps output to fit (env CUBCODER_CONTEXT_LIMIT; default per backend)")
		maxIters    = flag.Int("max-iters", 0, "max tool-loop iterations per request (env CUBCODER_MAX_ITERS; default 150)")
		cmdTimeout  = flag.Int("command-timeout", 0, "default run_command timeout in seconds (env CUBCODER_COMMAND_TIMEOUT; default 120)")
		yes         = flag.Bool("yes", false, "auto-approve file writes and commands (no prompts)")
		label       = flag.String("label", "", "label this window: set the terminal title (also /label in the REPL)")
		contFlag    = flag.Bool("continue", false, "resume the most recent session started in this directory")
		resumeID    = flag.String("resume", "", "resume a saved session by id (or unique prefix; see `cubcoder sessions`)")
		noSave      = flag.Bool("no-save", false, "don't save this session to ~/.cubcoder/sessions (env CUBCODER_NO_SAVE=1)")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	var attachPaths stringList
	flag.Var(&attachPaths, "attach", "attach a document (text, PDF, or image) to the first prompt; repeatable (also /attach in the REPL)")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `cubcoder — local coding agent

usage:
  cubcoder [flags] [prompt]      start a session (or run a one-shot prompt)
  cubcoder login                 set the API key and model, saved across reboots
  cubcoder config set <k> <v>    set one config field
  cubcoder config show           print the saved config (keys masked)
  cubcoder config path           print the config file location
  cubcoder sessions              list saved sessions (resume with -continue or -resume <id>)

flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println("cubcoder", version)
		return
	}

	cfg := config.Load(*url, *key, *model, *anthKey, *maxTokens, *ctxLimit, *maxIters, *cmdTimeout, *yes)
	tools.SetSearchURL(cfg.SearchURL) // empty keeps the built-in default
	// Before any registry is built: the run_command description quotes the default.
	tools.SetCommandTimeout(time.Duration(cfg.CommandTimeout) * time.Second)

	// Window label: the terminal title is the highest, most persistent spot we
	// can put text — it survives scrollback and shows in the window manager.
	title := &windowTitle{enabled: isTerminal(os.Stdout)}
	defer title.Clear()
	if *label != "" {
		title.Set(*label)
	}

	p, err := buildProvider(cfg, cfg.Model)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
		os.Exit(1)
	}
	if cfg.APIKey == "" && !strings.HasPrefix(strings.ToLower(cfg.Model), "claude") {
		fmt.Fprintln(os.Stderr, dim+"warning: no LiteLLM key set (use -key or CUBCODER_API_KEY)"+reset)
	}

	// Orchestrator: the lead agent (this process) can delegate subtasks to
	// background worker agents, each in its own git worktree. A worker is a
	// plain agent rooted at its worktree, auto-approving (it can't prompt), with
	// its own provider; the coordinator attaches logging hooks.
	cwd, _ := os.Getwd()
	// An agents.md in the working directory carries repo-specific instructions.
	// It rides the system prompt so it survives /reset and compaction. Workers
	// load it from their own worktree: a tracked agents.md is checked out there.
	agentsName, agentsBlock := agent.ProjectInstructions(cwd)
	makeWorker := func(worktree string) (*agent.Agent, error) {
		wp, err := buildProvider(cfg, cfg.Model)
		if err != nil {
			return nil, err
		}
		_, wBlock := agent.ProjectInstructions(worktree)
		w := agent.NewWithSystem(wp, tools.NewIn(worktree), agent.SystemPrompt+wBlock)
		if cfg.MaxIters > 0 {
			w.MaxIters = cfg.MaxIters
		}
		w.Confirm = func(string, string) bool { return true }
		return w, nil
	}
	coord := orchestrator.New(cwd, makeWorker)
	coord.OnProgress = (&liveStatus{enabled: isTerminal(os.Stdout)}).render

	// The lead runs commands interactively when attached to a terminal, so a
	// command that prompts (sudo, ssh) can read the user's input. Off a tty
	// (piped stdin, output redirected) commands stay captured and
	// non-interactive. Worker agents are never interactive — they run
	// concurrently and must not touch the terminal.
	var leadTools *tools.Registry
	var con *tools.Console // non-nil only for the interactive lead session
	if isTerminal(os.Stdin) && isTerminal(os.Stdout) {
		con = &tools.Console{Out: os.Stdout}
		leadTools = tools.NewInteractive("", con)
	} else {
		leadTools = tools.New()
	}
	for _, t := range orchestrator.Tools(coord) {
		leadTools.Register(t)
	}
	ag := agent.NewWithSystem(p, leadTools, orchestrator.LeadSystemPrompt+agentsBlock)
	if cfg.MaxIters > 0 {
		ag.MaxIters = cfg.MaxIters
	}
	ag.Spinner = &spinner{
		enabled: isTerminal(os.Stdout),
		workers: coord.List,
	}
	stdin := &lineReader{in: bufio.NewReader(os.Stdin), history: loadHistory()}

	// Let interactive commands ask the user a yes/no question through the
	// REPL's reader — used to offer remembering a typed sudo password for the
	// session (see the sudo watch in internal/tools).
	if con != nil {
		con.Ask = func(q string) bool {
			stdin.inject(activeEsc.Pause()) // the prompt owns the terminal now
			defer activeEsc.Resume()
			line, _ := stdin.ReadLine(fmt.Sprintf("%s%s [y/N] %s", green, q, reset))
			s := strings.ToLower(strings.TrimSpace(line))
			return s == "y" || s == "yes"
		}
		// Interactive commands (sudo, ssh) pump /dev/tty into the child; the
		// Esc watcher must not race them for keystrokes.
		con.SuspendInput = func() func() {
			stdin.inject(activeEsc.Pause())
			return func() { activeEsc.Resume() }
		}
		// While a command owns the terminal the Esc watcher is paused, so the
		// command's input pump detects the bare Esc itself and cancels the turn
		// through here.
		con.Interrupt = func() {
			if activeCancel != nil {
				activeCancel()
			}
		}
	}

	ui := &turnUI{}
	ag.OnText = ui.onText
	ag.OnToolCall = ui.onToolCall
	ag.OnToolResult = ui.onToolResult
	ag.Confirm = func(name, args string) bool {
		if cfg.AutoApprove {
			return true
		}
		stdin.inject(activeEsc.Pause()) // the prompt owns the terminal now
		defer activeEsc.Resume()
		fmt.Print(preview(name, args))
		line, err := stdin.ReadLine(fmt.Sprintf("%sallow %s? [y/N, a=always] %s", green, name, reset))
		// Esc at the approval prompt denies AND interrupts the turn — a plain
		// "n" tells the model no and lets it try something else; Esc means stop.
		if errors.Is(err, errEsc) {
			if activeCancel != nil {
				activeCancel()
			}
			return false
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "a", "always":
			cfg.AutoApprove = true // stop asking for the rest of the session
			fmt.Println(dim + "(auto-approve on — /auto off to re-enable prompts)" + reset)
			return true
		case "y", "yes":
			return true
		default:
			return false
		}
	}

	// On an interactive, truecolor-capable terminal, show the CyberBear splash
	// once at startup (it carries the name and version). Off a tty, in one-shot
	// mode, or on a bare console that can't render the art, keep a single plain
	// line so piped/redirected output and limited terminals stay clean.
	if len(flag.Args()) == 0 && isTerminal(os.Stdout) && banner.Supported() {
		fmt.Println(banner.Splash("cubcoder", version))
		fmt.Printf("%s%s (%s)%s\n", dim, p.Name(), cfg.BaseURL, reset)
	} else {
		fmt.Printf("%scubcoder %s%s  %s%s (%s)%s\n", cyan, version, reset, dim, p.Name(), cfg.BaseURL, reset)
	}
	if agentsName != "" {
		fmt.Printf("%sproject instructions loaded from %s%s\n", dim, agentsName, reset)
	}
	preflight(p)

	// Session persistence: every completed turn is written to
	// ~/.cubcoder/sessions/<id>.json so the conversation survives exit and can
	// be picked up with -continue / -resume. Worker logs, the plan, and git
	// checkpoint metadata ride the session too; worker work lives on its
	// branches, file rewind uses the git refs under refs/cubcoder/cp/.
	saving := !*noSave && os.Getenv("CUBCODER_NO_SAVE") == ""
	sess := session.New(cwd, cfg.Model)
	if *contFlag || *resumeID != "" {
		var loaded *session.Session
		if *resumeID != "" {
			loaded, err = session.Load(*resumeID)
		} else {
			loaded, err = session.Latest(cwd)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
			os.Exit(1)
		}
		ag.Restore(loaded.Messages)
		ag.SetTodos(loaded.Todos)
		sess = loaded
		if n := coord.Restore(loaded.Workers); n > 0 {
			fmt.Printf("%srestored %d worker log(s) — /agents to list, /agent <id> for a log%s\n", dim, n, reset)
		}
		if loaded.Cwd != cwd {
			fmt.Fprintf(os.Stderr, "%swarning: session %s was recorded in %s; relative paths in it may not match this directory%s\n", yellow, loaded.ID, loaded.Cwd, reset)
		}
		fmt.Printf("%sresumed session %s — %s (%d messages, last active %s)%s\n", dim, loaded.ID, loaded.Title, len(loaded.Messages)-1, ago(loaded.Updated), reset)
	}
	persist := func() {
		if !saving {
			return
		}
		sess.Model = cfg.Model
		sess.Messages = ag.Messages()
		sess.Workers = coord.Records()
		sess.Todos = ag.Todos()
		if err := sess.Save(); err != nil {
			fmt.Fprintf(os.Stderr, "%swarning: session not saved: %v%s\n", yellow, err, reset)
		}
	}

	// Documents attached at launch ride the first prompt. A failed attach exits:
	// running the prompt without a document the user explicitly asked for would
	// silently answer from the wrong context, worst in scripted one-shot runs.
	for _, docPath := range attachPaths {
		if err := attachDoc(ag, docPath, nil); err != nil {
			fmt.Fprintf(os.Stderr, "%serror: attach %s: %v%s\n", red, docPath, err, reset)
			os.Exit(1)
		}
	}

	// One-shot mode: prompt passed as args.
	if args := flag.Args(); len(args) > 0 {
		prompt := strings.Join(args, " ")
		snapshotTurn(sess, cwd, prompt, ag)
		run(ag, stdin, ui, prompt)
		persist()
		reportExit(coord)
		return
	}

	// Interactive REPL. On a tty, switch input to raw mode so we can read a
	// pasted multi-line block as one submission (bracketed paste) and echo
	// keystrokes ourselves, instead of the kernel splitting input at each
	// newline and echoing the paste markers as literal text. A signal handler
	// and a defer both restore the terminal so we never leave it raw.
	fmt.Println(dim + "interactive — type a request; ↑↓ history, Tab complete, Ctrl-R search, Ctrl-J newline, Esc interrupts a running turn, /help, Ctrl-D to exit" + reset)
	if isTerminal(os.Stdin) {
		if restore, ok := makeRawInput(os.Stdin.Fd()); ok {
			stdin.raw = true
			fmt.Print(pasteOn)
			cleanup := func() { fmt.Print(pasteOff); restore(); title.Clear() }
			defer cleanup()
			sigs := make(chan os.Signal, 1)
			signal.Notify(sigs, os.Interrupt)
			go func() { <-sigs; cleanup(); os.Exit(130) }()
		}
	}
	hot := false // tracks whether we've already warned about a near-full context
	for {
		used, window := ag.ContextUsage()
		line, err := stdin.ReadLine(replPrompt(used, window, coord.Count()))
		if errors.Is(err, errEsc) {
			continue // Esc at an empty prompt: nothing to interrupt, just reprompt
		}
		if err != nil { // EOF
			fmt.Println()
			reportExit(coord)
			return
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		stdin.addHistory(line)
		switch {
		case line == "exit" || line == "quit":
			reportExit(coord)
			return
		case line == "/reset":
			// The old session stays on disk as it was; a fresh id starts here so
			// -continue picks up whichever was active last.
			ag.Reset()
			sess = session.New(cwd, cfg.Model)
			fmt.Println(dim + "(conversation reset — new session " + sess.ID + ")" + reset)
			continue
		case line == "/sessions":
			if err := printSessions(cwd); err != nil {
				fmt.Printf("%s%v%s\n", red, err, reset)
			}
			continue
		case strings.HasPrefix(line, "/resume"):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/resume"))
			var loaded *session.Session
			var err error
			if id == "" {
				loaded, err = session.Latest(cwd)
			} else {
				loaded, err = session.Load(id)
			}
			if err != nil {
				fmt.Printf("%s%v%s\n", red, err, reset)
				continue
			}
			persist() // don't lose the conversation we're leaving
			ag.Restore(loaded.Messages)
			ag.SetTodos(loaded.Todos)
			sess = loaded
			if n := coord.Restore(loaded.Workers); n > 0 {
				fmt.Printf("%srestored %d worker log(s)%s\n", dim, n, reset)
			}
			fmt.Printf("%sresumed session %s — %s (%d messages, last active %s)%s\n", dim, loaded.ID, loaded.Title, len(loaded.Messages)-1, ago(loaded.Updated), reset)
			continue
		case line == "/compact":
			if ag.MessageCount() <= 1 {
				fmt.Println(dim + "nothing to compact yet" + reset)
				continue
			}
			before, after := ag.Compact(context.Background())
			if after < before {
				fmt.Printf("%scompacted: ~%s → ~%s tokens%s\n", dim, commas(before), commas(after), reset)
				persist()
			} else {
				fmt.Println(dim + "nothing to compact (history is already recent work)" + reset)
			}
			continue
		case line == "/model":
			fmt.Println(dim + "current model: " + cfg.Model + reset)
			continue
		case strings.HasPrefix(line, "/model "):
			want := strings.TrimSpace(strings.TrimPrefix(line, "/model "))
			np, err := buildProvider(cfg, want)
			if err != nil {
				fmt.Printf("%scan't switch: %v%s\n", red, err, reset)
				continue
			}
			cfg.Model = want
			ag.Provider = np
			fmt.Println(dim + "switched → " + np.Name() + reset)
			continue
		case line == "/login":
			// Same flow as `cubcoder login`, reading through the REPL's raw-mode
			// reader. The saved key/model/URL are applied to the live session and
			// the provider is rebuilt, so the new credentials take effect now
			// without a restart.
			fc, err := login(stdin.ReadLine)
			if err != nil {
				fmt.Printf("%slogin failed: %v%s\n", red, err, reset)
				continue
			}
			if fc.APIKey != "" {
				cfg.APIKey = fc.APIKey
			}
			if fc.BaseURL != "" {
				cfg.BaseURL = fc.BaseURL
			}
			want := cfg.Model
			if fc.Model != "" {
				want = fc.Model
			}
			np, err := buildProvider(cfg, want)
			if err != nil {
				fmt.Printf("%ssaved, but can't use it yet: %v%s\n", red, err, reset)
				continue
			}
			cfg.Model = want
			ag.Provider = np
			fmt.Println(dim + "credentials updated → " + np.Name() + reset)
			continue
		case line == "/auto":
			fmt.Println(dim + "auto-approve: " + onoff(cfg.AutoApprove) + reset)
			continue
		case line == "/auto on":
			cfg.AutoApprove = true
			fmt.Println(dim + "auto-approve ON — no confirmation prompts" + reset)
			continue
		case line == "/auto off":
			cfg.AutoApprove = false
			fmt.Println(dim + "auto-approve OFF — will confirm writes and commands" + reset)
			continue
		case line == "/sudo":
			if con != nil && con.SudoRemembered() {
				fmt.Println(dim + "sudo password: remembered for this session (/sudo forget to clear)" + reset)
			} else {
				fmt.Println(dim + "sudo password: not remembered" + reset)
			}
			continue
		case line == "/sudo forget":
			if con != nil && con.ForgetSudo() {
				fmt.Println(dim + "(sudo password forgotten)" + reset)
			} else {
				fmt.Println(dim + "(no sudo password was remembered)" + reset)
			}
			continue
		case line == "/label":
			if title.label != "" {
				fmt.Println(dim + "window label: " + title.label + reset)
			} else {
				fmt.Println(dim + "no window label (/label <text> to set one)" + reset)
			}
			continue
		case line == "/label clear":
			title.Clear()
			fmt.Println(dim + "(window label cleared)" + reset)
			continue
		case strings.HasPrefix(line, "/label "):
			if !title.enabled {
				fmt.Println(dim + "not a terminal — can't set a window title" + reset)
				continue
			}
			l := strings.TrimSpace(strings.TrimPrefix(line, "/label "))
			title.Set(l)
			fmt.Println(dim + "window labeled " + strconv.Quote(l) + reset)
			continue
		case line == "/attach":
			printAttachments(ag)
			continue
		case line == "/attach clear":
			ag.ClearAttachments()
			fmt.Println(dim + "(staged attachments cleared)" + reset)
			continue
		case strings.HasPrefix(line, "/attach "):
			docPath := strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "/attach ")), `"'`)
			ask := func(prompt string) bool {
				ans, err := stdin.ReadLine(fmt.Sprintf("%s%s [y/N] %s", green, prompt, reset))
				if err != nil {
					return false
				}
				s := strings.ToLower(strings.TrimSpace(ans))
				return s == "y" || s == "yes"
			}
			if err := attachDoc(ag, docPath, ask); err != nil {
				fmt.Printf("%scan't attach: %v%s\n", red, err, reset)
			}
			continue
		case line == "/agents":
			printAgents(coord)
			continue
		case line == "/agents clean":
			for _, m := range coord.Cleanup() {
				fmt.Println(dim + "  " + m + reset)
			}
			continue
		case strings.HasPrefix(line, "/agent "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/agent "))
			if log, ok := coord.Log(id); ok {
				if s, ok := coord.Get(id); ok {
					fmt.Printf("%s%s %q — %s (%s)%s\n", cyan, s.ID, s.Name, s.Status, s.Branch, reset)
				}
				fmt.Println(strings.TrimRight(log, "\n"))
			} else {
				fmt.Printf("%sno agent %q (try /agents)%s\n", red, id, reset)
			}
			continue
		case strings.HasPrefix(line, "/stop "):
			id := strings.TrimSpace(strings.TrimPrefix(line, "/stop "))
			if err := coord.Stop(id); err != nil {
				fmt.Printf("%s%v%s\n", red, err, reset)
			} else {
				fmt.Println(dim + "stopping " + id + "…" + reset)
			}
			continue
		case line == "/plan":
			fmt.Print(formatPlan(ag.Todos()))
			continue
		case strings.HasPrefix(line, "/plan "):
			task := strings.TrimSpace(strings.TrimPrefix(line, "/plan "))
			if task == "" {
				fmt.Print(formatPlan(ag.Todos()))
				continue
			}
			snapshotTurn(sess, cwd, task, ag)
			runTurn(ag, stdin, ui, func(ctx context.Context) error {
				return ag.RunPlan(ctx, task)
			})
			persist()
			fmt.Println(dim + "plan mode — files unchanged. /go to implement, or send a message." + reset)
			hot = warnHeadroom(ag, hot)
			continue
		case line == "/go":
			if len(ag.Todos()) == 0 {
				fmt.Println(dim + "no plan yet — /plan <task> to write one" + reset)
				continue
			}
			snapshotTurn(sess, cwd, "implement the plan", ag)
			run(ag, stdin, ui, agent.GoPrompt)
			persist()
			hot = warnHeadroom(ag, hot)
			continue
		case line == "/undo":
			if len(sess.Checkpoints) == 0 {
				fmt.Println(dim + "no checkpoints yet — they are taken at the start of each turn in a git repo" + reset)
				continue
			}
			n := sess.Checkpoints[len(sess.Checkpoints)-1].N
			if err := rewindTo(stdin, sess, ag, cwd, n); err != nil {
				fmt.Printf("%s%v%s\n", red, err, reset)
			} else {
				persist()
			}
			continue
		case line == "/rewind":
			fmt.Print(formatCheckpoints(sess.Checkpoints))
			continue
		case strings.HasPrefix(line, "/rewind "):
			arg := strings.TrimSpace(strings.TrimPrefix(line, "/rewind "))
			n, err := strconv.Atoi(arg)
			if err != nil {
				fmt.Printf("%s/rewind needs a checkpoint number (see /rewind)%s\n", red, reset)
				continue
			}
			if err := rewindTo(stdin, sess, ag, cwd, n); err != nil {
				fmt.Printf("%s%v%s\n", red, err, reset)
			} else {
				persist()
			}
			continue
		case line == "/status":
			printStatus(ag, cfg, coord)
			if saving {
				fmt.Printf("%ssession  %s%s\n", dim, sess.ID, reset)
			} else {
				fmt.Printf("%ssession  not saved (-no-save)%s\n", dim, reset)
			}
			continue
		case line == "/help":
			fmt.Println(dim + "  /reset         clear the conversation\n  /compact       shrink the conversation now (evict stale reads, summarize old turns)\n  /plan [task]   show the plan, or outline steps without editing files\n  /go            implement the current plan\n  /undo          restore files and conversation to the last checkpoint\n  /rewind [n]    list checkpoints, or restore checkpoint n\n  /status        model, context usage, session id, plan, and workers\n  /sessions      list saved sessions (this directory marked *)\n  /resume [id]   load a saved session (latest for this directory when no id)\n  /model [name]  show or switch model\n  /login         set API key + model, saved to ~/.cubcoder/config.json\n  /auto [on|off] toggle confirmation prompts\n  /attach <path> attach a document (text, PDF, or image) to your next message\n  /attach        list staged attachments (/attach clear to drop them)\n  /sudo [forget] show or forget the remembered sudo password\n  /label <text>  label this window (sets the terminal title; /label clear to undo)\n  /agents        list worker agents and their status\n  /agent <id>    show one worker's full log\n  /stop <id>     stop a running worker\n  /agents clean  remove all worker worktrees and branches\n  /help          this help\n  exit           quit\n  Esc            interrupt the running turn (conversation kept)\n  Tab            complete /commands and file paths\n  Ctrl-R         search prompt history\n  Ctrl-J         insert a newline (or end a line with \\ and Enter)\n  ↑ / ↓          previous / next prompt (saved to ~/.cubcoder/history)\n  ← / →          move cursor (Ctrl-A/E start/end, Ctrl-W delete word)\n  Ctrl-C         quit immediately" + reset)
			continue
		}
		snapshotTurn(sess, cwd, line, ag)
		run(ag, stdin, ui, line)
		persist()
		hot = warnHeadroom(ag, hot)
	}
}

// printSessions lists saved sessions, newest first. Sessions from cwd (when
// given) are marked so the one -continue would pick is obvious.
func printSessions(cwd string) error {
	metas, err := session.List()
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		fmt.Println(dim + "no saved sessions yet" + reset)
		return nil
	}
	for _, m := range metas {
		mark := "  "
		if cwd != "" && m.Cwd == cwd {
			mark = cyan + "* " + reset
		}
		title := m.Title
		if title == "" {
			title = "(untitled)"
		}
		fmt.Printf("%s%s  %s%-9s %3d msgs  %s%s\n", mark, m.ID, dim, ago(m.Updated), m.Messages, m.Cwd, reset)
		fmt.Printf("    %s\n", title)
	}
	if cwd != "" {
		fmt.Println(dim + "* = this directory; /resume <id> to load one, /resume for the latest here" + reset)
	} else {
		fmt.Println(dim + "resume with: cubcoder -resume <id>   (or -continue for the latest in the current directory)" + reset)
	}
	return nil
}

// ago renders a time as a coarse relative age.
func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// runLogin is the standalone `cubcoder login` subcommand: it reads from stdin
// with a plain line reader (the terminal is not in raw mode here) and writes the
// persistent config file. This is the reboot-safe alternative to
// `export CUBCODER_API_KEY=…`, which only lives for one shell session.
func runLogin() error {
	in := bufio.NewReader(os.Stdin)
	_, err := login(func(prompt string) (string, error) {
		fmt.Print(prompt)
		return in.ReadString('\n')
	})
	return err
}

// login prompts for the API key, model, and gateway URL and writes them to the
// persistent config file. Input lines come from readLine — which prints the
// prompt itself, a plain bufio read for the standalone subcommand or the
// REPL's raw-mode reader for /login — so the same flow works in both. Empty
// input keeps the current value (or the shown default), so pressing Enter
// through the model prompt accepts the built-in default. Returns the saved
// config so a live session can apply it.
func login(readLine func(prompt string) (string, error)) (config.FileConfig, error) {
	fc, err := config.LoadFile()
	if err != nil {
		return fc, err
	}
	// secret masks the shown current value (API keys); non-secret fields like the
	// model and URL display in clear text so the user can see what they'll keep.
	prompt := func(label, cur, def string, secret bool) string {
		shown := cur
		if secret {
			shown = mask(cur)
		}
		var p string
		switch {
		case cur != "":
			p = fmt.Sprintf("%s%s%s %s[%s]%s: ", cyan, label, reset, dim, shown, reset)
		case def != "":
			p = fmt.Sprintf("%s%s%s %s[%s]%s: ", cyan, label, reset, dim, def, reset)
		default:
			p = fmt.Sprintf("%s%s%s: ", cyan, label, reset)
		}
		line, _ := readLine(p)
		return strings.TrimSpace(line)
	}

	if v := prompt("API key", fc.APIKey, "", true); v != "" {
		fc.APIKey = v
	}
	if v := prompt("model", fc.Model, config.DefaultModel, false); v != "" {
		fc.Model = v
	}
	if v := prompt("gateway URL", fc.BaseURL, config.DefaultBaseURL, false); v != "" {
		fc.BaseURL = v
	}

	path, err := fc.Save()
	if err != nil {
		return fc, err
	}
	fmt.Printf("%ssaved → %s%s\n", green, path, reset)
	if fc.APIKey == "" {
		fmt.Fprintln(os.Stderr, yellow+"warning: no API key set — the gateway will return 401"+reset)
	}
	return fc, nil
}

// runConfig handles the non-interactive config subcommand:
//
//	cubcoder config set <field> <value>   write one field
//	cubcoder config show                  print the saved config (keys masked)
//	cubcoder config path                  print the config file location
func runConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: cubcoder config <set|show|path> …")
	}
	switch args[0] {
	case "path":
		p, err := config.Path()
		if err != nil {
			return err
		}
		fmt.Println(p)
		return nil
	case "show":
		fc, err := config.LoadFile()
		if err != nil {
			return err
		}
		p, _ := config.Path()
		fmt.Printf("%s# %s%s\n", dim, p, reset)
		fmt.Printf("api_key            %s\n", maskOrUnset(fc.APIKey))
		fmt.Printf("model              %s\n", orDefault(fc.Model, config.DefaultModel+" (default)"))
		fmt.Printf("base_url           %s\n", orDefault(fc.BaseURL, config.DefaultBaseURL+" (default)"))
		fmt.Printf("anthropic_api_key  %s\n", maskOrUnset(fc.AnthropicKey))
		fmt.Printf("search_url         %s\n", orDefault(fc.SearchURL, tools.DefaultSearchURL+" (default)"))
		return nil
	case "set":
		if len(args) < 3 {
			return fmt.Errorf("usage: cubcoder config set <field> <value>")
		}
		fc, err := config.LoadFile()
		if err != nil {
			return err
		}
		if err := fc.SetField(args[1], args[2]); err != nil {
			return err
		}
		path, err := fc.Save()
		if err != nil {
			return err
		}
		fmt.Printf("%sset %s → %s%s\n", green, args[1], path, reset)
		return nil
	default:
		return fmt.Errorf("unknown config command %q (set|show|path)", args[0])
	}
}

// mask hides all but the last 4 characters of a secret for display.
func mask(s string) string {
	if len(s) <= 4 {
		return strings.Repeat("•", len(s))
	}
	return strings.Repeat("•", len(s)-4) + s[len(s)-4:]
}

// maskOrUnset masks a secret, or shows "(unset)" when empty.
func maskOrUnset(s string) string {
	if s == "" {
		return dim + "(unset)" + reset
	}
	return mask(s)
}

// orDefault returns v, or def when v is empty.
func orDefault(v, def string) string {
	if v == "" {
		return dim + def + reset
	}
	return v
}

// buildProvider selects the backend by model name: a "claude*" model uses the
// Anthropic backend (needs an Anthropic key); anything else uses the
// OpenAI-compatible LiteLLM backend.
func buildProvider(cfg config.Config, model string) (provider.Provider, error) {
	if strings.HasPrefix(strings.ToLower(model), "claude") {
		if cfg.AnthropicKey == "" {
			return nil, fmt.Errorf("model %q needs an Anthropic key (set -anthropic-key, CUBCODER_ANTHROPIC_KEY, or anthropic_api_key in config)", model)
		}
		c := provider.NewAnthropic(cfg.AnthropicBaseURL, cfg.AnthropicKey, model)
		if cfg.MaxTokens > 0 {
			c.MaxTokens = cfg.MaxTokens
		}
		if cfg.ContextLimit > 0 {
			c.ContextLimit = cfg.ContextLimit
		}
		return c, nil
	}
	o := provider.NewOpenAI(cfg.BaseURL, cfg.APIKey, model)
	if cfg.MaxTokens > 0 {
		o.MaxTokens = cfg.MaxTokens
	}
	if cfg.ContextLimit > 0 {
		o.ContextLimit = cfg.ContextLimit
	}
	return o, nil
}

// preflight runs cheap startup checks and prints actionable warnings to stderr
// without blocking the session: ripgrep missing (search_code will fail), and —
// for backends that support it — an unreachable gateway or an unregistered
// model. Warnings only; the user may still have a working setup the checks
// can't see (e.g. a gateway that rejects /models but serves chat).
func preflight(p provider.Provider) {
	if _, err := exec.LookPath("rg"); err != nil {
		fmt.Fprintln(os.Stderr, yellow+"warning: ripgrep (rg) not on PATH — the search_code tool will fail until it is installed"+reset)
	}
	pinger, ok := p.(provider.Pinger)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pinger.Ping(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "%swarning: %v%s\n", yellow, err, reset)
	}
}

// printStatus shows the model, context usage and headroom, history size, the
// auto-approve state, and a one-line worker summary.
func printStatus(ag *agent.Agent, cfg config.Config, c *orchestrator.Coordinator) {
	used, window := ag.ContextUsage()
	fmt.Printf("%smodel    %s%s\n", dim, ag.Provider.Name(), reset)
	if window > 0 {
		fmt.Printf("%scontext  ~%s / %s tokens (%d%%), %s headroom%s\n",
			dim, commas(used), commas(window), used*100/window, commas(window-used), reset)
	} else {
		fmt.Printf("%scontext  ~%s tokens (window unknown)%s\n", dim, commas(used), reset)
	}
	fmt.Printf("%shistory  %d messages%s\n", dim, ag.MessageCount(), reset)
	if todos := ag.Todos(); len(todos) == 0 {
		fmt.Printf("%splan     none%s\n", dim, reset)
	} else {
		done := 0
		for _, t := range todos {
			if t.Status == agent.TodoCompleted {
				done++
			}
		}
		fmt.Printf("%splan     %d/%d done  (/plan to view)%s\n", dim, done, len(todos), reset)
	}
	if atts := ag.Attachments(); len(atts) > 0 {
		names := make([]string, len(atts))
		for i, at := range atts {
			names[i] = at.Name
		}
		fmt.Printf("%sattached %s (staged for the next message)%s\n", dim, strings.Join(names, ", "), reset)
	}
	fmt.Printf("%sauto     %s%s\n", dim, onoff(cfg.AutoApprove), reset)
	snaps := c.List()
	if len(snaps) == 0 {
		fmt.Printf("%sworkers  none%s\n", dim, reset)
		return
	}
	n := map[orchestrator.Status]int{}
	for _, s := range snaps {
		n[s.Status]++
	}
	fmt.Printf("%sworkers  %d running, %d done, %d failed, %d stopped%s\n", dim,
		n[orchestrator.StatusRunning], n[orchestrator.StatusDone], n[orchestrator.StatusFailed], n[orchestrator.StatusStopped], reset)
}

// Context-headroom warning thresholds. Compaction triggers at 0.75, so a turn
// that ends above warnHot is one compaction couldn't shrink below it (typically
// a single oversized unit) — exactly when the user should know. Edge-triggered
// with hysteresis at warnCool so it warns once per crossing, not every turn.
const (
	warnHot  = 0.85
	warnCool = 0.70
)

// warnHeadroom prints a one-line warning the first time the conversation crosses
// warnHot. prev is the previous "hot" state; the updated state is returned so
// the REPL can thread it across turns.
func warnHeadroom(ag *agent.Agent, prev bool) bool {
	used, window := ag.ContextUsage()
	if window <= 0 {
		return false
	}
	frac := float64(used) / float64(window)
	switch {
	case frac >= warnHot && !prev:
		fmt.Fprintf(os.Stderr, "%scontext ~%d%% full (%s/%s tokens) — /compact to shrink it now, /reset to clear; older history compacts automatically%s\n",
			yellow, int(frac*100), commas(used), commas(window), reset)
		return true
	case frac < warnCool:
		return false
	}
	return prev
}

// stringList is a repeatable string flag (-attach a.pdf -attach b.md).
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ", ") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

// attachLimitFraction is the largest share of the context window one document
// may take verbatim: compaction can never shrink a single message below
// itself, so a bigger one would leave the session permanently over budget.
const attachLimitFraction = 0.6

// attachDoc extracts a document and stages it on the agent, to be included
// with the next user message. A document past the verbatim limit is offered
// as a digest instead: the model summarizes it chunk by chunk and the compact
// result rides the message, with line/page pointers back into the file.
// confirm gates that digest (it costs model calls and time); nil means
// proceed with a printed notice — the launch path, where the user already
// asked for the attachment and there is no prompt loop yet. Documents that
// fit but crowd the window get a warning.
func attachDoc(ag *agent.Agent, path string, confirm func(prompt string) bool) error {
	full := expandHome(path)
	name := filepath.Base(full)
	if tools.IsImageFile(full) {
		img, desc, err := tools.LoadImageFile(full)
		if err != nil {
			return err
		}
		ag.AttachImage(name, desc, img)
		fmt.Printf("%sattached image %s (%s) — included with your next message%s\n", dim, name, desc, reset)
		return nil
	}
	text, err := tools.ExtractDocument(full)
	if err != nil {
		return err
	}
	est := provider.EstimateInputTokens([]provider.Message{{Role: "user", Content: text}}, nil)
	window := ag.Provider.ContextWindow()

	if window > 0 && est > int(attachLimitFraction*float64(window)) {
		if confirm != nil {
			if !confirm(fmt.Sprintf("%s is ~%s tokens — too large to attach verbatim. Digest it instead? The model summarizes it section by section, which can take several minutes", name, commas(est))) {
				return fmt.Errorf("not attached — ask about the file by path instead and the model will read it in pages with read_file")
			}
		} else {
			fmt.Fprintf(os.Stderr, "%s%s is ~%s tokens — too large to attach verbatim; digesting it section by section (this can take several minutes)%s\n",
				yellow, name, commas(est), reset)
		}
		digest, err := ag.DigestDocument(context.Background(), name, full, text)
		if err != nil {
			return err
		}
		dEst := provider.EstimateInputTokens([]provider.Message{{Role: "user", Content: digest}}, nil)
		if dEst > int(attachLimitFraction*float64(window)) {
			return fmt.Errorf("the digest of %s still doesn't fit (~%s tokens); ask about the file by path instead", name, commas(dEst))
		}
		ag.Attach(name+" (digest)", digest)
		fmt.Printf("%sattached digest of %s (~%s tokens, down from ~%s) — included with your next message%s\n",
			dim, name, commas(dEst), commas(est), reset)
		return nil
	}

	if window > 0 {
		used, _ := ag.ContextUsage()
		if used+est > int(warnHot*float64(window)) {
			fmt.Fprintf(os.Stderr, "%swarning: with this attachment the context is ~%d%% full — consider /reset before sending%s\n",
				yellow, (used+est)*100/window, reset)
		}
	}
	ag.Attach(name, text)
	fmt.Printf("%sattached %s (~%s tokens) — included with your next message%s\n", dim, name, commas(est), reset)
	return nil
}

// printAttachments lists the staged documents for /attach.
func printAttachments(ag *agent.Agent) {
	atts := ag.Attachments()
	if len(atts) == 0 {
		fmt.Println(dim + "no staged attachments (/attach <path> to add one)" + reset)
		return
	}
	for _, at := range atts {
		est := provider.EstimateInputTokens([]provider.Message{{Role: "user", Content: at.Content}}, nil)
		fmt.Printf("%s  %s (~%s tokens)%s\n", dim, at.Name, commas(est), reset)
	}
	fmt.Println(dim + "included with your next message (/attach clear to drop)" + reset)
}

// expandHome resolves a leading ~ to the user's home directory, so /attach
// accepts shell-style paths even though no shell expands them here.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// commas formats a non-negative count with thousands separators: 262144 →
// "262,144".
func commas(n int) string {
	s := strconv.Itoa(n)
	pre := len(s) % 3
	if pre == 0 {
		pre = 3
	}
	var b strings.Builder
	b.WriteString(s[:pre])
	for i := pre; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// printAgents renders the worker roster for /agents.
func printAgents(c *orchestrator.Coordinator) {
	snaps := c.List()
	if len(snaps) == 0 {
		fmt.Println(dim + "no worker agents — the lead spawns them with delegate_task" + reset)
		return
	}
	for _, s := range snaps {
		fmt.Printf("%s%-4s %-12s %-8s%s %s%s%s", statusColor(s.Status), s.ID, s.Name, string(s.Status), reset, dim, s.Elapsed, reset)
		if s.LastLine != "" {
			fmt.Printf(" %s%s%s", dim, truncate(oneLine(s.LastLine), 60), reset)
		}
		fmt.Println()
	}
}

// statusColor maps a worker status to its display color.
func statusColor(s orchestrator.Status) string {
	switch s {
	case orchestrator.StatusRunning:
		return cyan
	case orchestrator.StatusDone:
		return green
	case orchestrator.StatusFailed:
		return red
	case orchestrator.StatusStopped:
		return yellow
	default:
		return dim
	}
}

// liveStatus repaints the worker roster in place while the lead blocks in
// wait_for_agents, so the user watches progress instead of a frozen spinner.
// Its render method is the coordinator's OnProgress callback; the coordinator
// calls it from a single goroutine and joins that goroutine before the wait
// returns, so render owns the terminal for the duration with no interleaving.
type liveStatus struct {
	enabled bool
	lines   int // rows drawn last paint, so the next paint can step back over them
}

func (l *liveStatus) render(snaps []orchestrator.Snapshot) {
	if !l.enabled || len(snaps) == 0 {
		return
	}
	if l.lines > 0 {
		fmt.Printf("\033[%dA", l.lines) // cursor up to the top of the block
	}
	for _, s := range snaps {
		fmt.Print("\033[2K") // clear the whole row before redrawing it
		fmt.Printf("%s%-4s %-12s %-8s%s %s%s%s", statusColor(s.Status), s.ID, s.Name, string(s.Status), reset, dim, s.Elapsed, reset)
		if s.LastLine != "" {
			fmt.Printf(" %s%s%s", dim, truncate(oneLine(s.LastLine), 50), reset)
		}
		fmt.Println()
	}
	// A shorter roster than last time would leave stale rows below; blank them.
	for i := len(snaps); i < l.lines; i++ {
		fmt.Print("\033[2K\n")
	}
	l.lines = len(snaps)
}

// reportExit notes any worktrees left on disk so the user can review or clean.
func reportExit(c *orchestrator.Coordinator) {
	snaps := c.List()
	if len(snaps) == 0 {
		return
	}
	fmt.Println(dim + "worker worktrees still on disk (work preserved on their branches):" + reset)
	for _, s := range snaps {
		fmt.Printf("%s  %s  ·  branch %s  ·  %s%s\n", dim, s.Worktree, s.Branch, string(s.Status), reset)
	}
	fmt.Println(dim + "remove them with /agents clean, or keep them to merge by hand." + reset)
}

// activeEsc is the Esc watcher for the turn currently in flight, nil between
// turns. It is package-level because the tool-approval prompt and interactive
// commands — both of which only ever fire while a turn is running, on the same
// goroutine — must pause it before they read the terminal themselves.
var activeEsc *escWatcher

// activeCancel cancels the turn currently in flight, nil between turns. It
// backs Console.Interrupt: while an interactive command owns the terminal the
// Esc watcher is paused, and the command's input pump reports the bare Esc it
// sees through this instead. Set and cleared in run(); only called from the
// pump goroutine, which the command joins before its tool returns.
var activeCancel context.CancelFunc

func run(ag *agent.Agent, in *lineReader, ui *turnUI, input string) {
	runTurn(ag, in, ui, func(ctx context.Context) error {
		return ag.Run(ctx, input)
	})
}

func runTurn(ag *agent.Agent, in *lineReader, ui *turnUI, fn func(context.Context) error) {
	ctx := context.Background()
	// Esc-to-interrupt needs the raw-mode REPL: off a tty (one-shot, piped)
	// there is no live keystroke stream to watch, and Ctrl-C already covers
	// aborting a script.
	if in != nil && in.raw {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		activeCancel = cancel
		defer func() { activeCancel = nil }()
		if w := startEscWatcher(cancel); w != nil {
			activeEsc = w
			defer func() {
				activeEsc = nil
				in.inject(w.Stop()) // hand type-ahead to the next prompt
			}()
		}
	}
	if ui != nil {
		ui.reset()
	}
	before, window := ag.ContextUsage()
	start := time.Now()
	err := fn(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println(dim + "(interrupted — conversation kept; send a new message to continue)" + reset)
		} else {
			fmt.Fprintf(os.Stderr, "%serror: %v%s\n", red, err, reset)
		}
	}
	elapsed := time.Since(start)
	if ui != nil {
		after, _ := ag.ContextUsage()
		ui.printFooter(elapsed, before, after, window)
	}
	notifyTurnDone(elapsed)
}

func snapshotTurn(sess *session.Session, cwd, prompt string, ag *agent.Agent) {
	if sess == nil || !checkpoint.IsRepo(cwd) {
		return
	}
	n := sess.NextCheckpointN()
	snap, err := checkpoint.Take(cwd, checkpoint.Ref(sess.ID, n), "cubcoder checkpoint")
	if err != nil {
		if !errors.Is(err, checkpoint.ErrNotRepo) {
			fmt.Fprintf(os.Stderr, "%swarning: checkpoint not saved: %v%s\n", yellow, err, reset)
		}
		return
	}
	sess.Checkpoints = append(sess.Checkpoints, session.Checkpoint{
		N: n, Created: time.Now(),
		Head: snap.Head, Commit: snap.Commit, Branch: snap.Branch, Repo: snap.Repo,
		Prompt:   clipPrompt(prompt),
		Messages: ag.MessageCount(),
		Todos:    ag.Todos(),
	})
	for len(sess.Checkpoints) > session.CheckpointKeep {
		old := sess.Checkpoints[0]
		checkpoint.DeleteRef(cwd, checkpoint.Ref(sess.ID, old.N))
		sess.Checkpoints = sess.Checkpoints[1:]
	}
}

func rewindTo(in *lineReader, sess *session.Session, ag *agent.Agent, cwd string, n int) error {
	var cp *session.Checkpoint
	for i := range sess.Checkpoints {
		if sess.Checkpoints[i].N == n {
			cp = &sess.Checkpoints[i]
			break
		}
	}
	if cp == nil {
		return fmt.Errorf("no checkpoint %d (see /rewind)", n)
	}
	if top, err := checkpoint.Toplevel(cwd); err == nil && cp.Repo != "" && top != cp.Repo {
		return fmt.Errorf("checkpoint was taken in %s, not this directory", cp.Repo)
	}
	head := cp.Head
	if len(head) > 7 {
		head = head[:7]
	}
	prompt := cp.Prompt
	if prompt == "" {
		prompt = "(untitled)"
	}
	q := fmt.Sprintf("restore checkpoint %d (%s)? resets HEAD to %s (%s) and drops later turns",
		cp.N, prompt, head, cp.Branch)
	if !askYes(in, q) {
		fmt.Println(dim + "(rewind cancelled)" + reset)
		return nil
	}
	if err := checkpoint.Restore(cwd, cp.Head, cp.Commit); err != nil {
		return err
	}
	ag.TruncateMessages(cp.Messages)
	ag.SetTodos(cp.Todos)
	var kept []session.Checkpoint
	for _, c := range sess.Checkpoints {
		if c.N > n {
			checkpoint.DeleteRef(cwd, checkpoint.Ref(sess.ID, c.N))
			continue
		}
		kept = append(kept, c)
	}
	sess.Checkpoints = kept
	fmt.Printf("%srestored checkpoint %d — %s%s\n", dim, n, prompt, reset)
	if todos := ag.Todos(); len(todos) > 0 {
		fmt.Print(formatPlan(todos))
	}
	return nil
}

func askYes(in *lineReader, question string) bool {
	if in == nil {
		return false
	}
	line, err := in.ReadLine(fmt.Sprintf("%s%s [y/N] %s", green, question, reset))
	if err != nil {
		return false
	}
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "y" || s == "yes"
}

func clipPrompt(s string) string {
	s = strings.TrimSpace(strings.Join(strings.Fields(s), " "))
	if r := []rune(s); len(r) > 72 {
		return string(r[:72]) + "…"
	}
	return s
}

// Bracketed-paste control sequences. pasteOn/pasteOff toggle the terminal's
// bracketed-paste mode; when on, the terminal wraps pasted text in
// pasteStart … pasteEnd. In raw mode (no kernel echo) the reader consumes these
// markers itself, so a pasted block's internal newlines never submit the line.
const (
	pasteOn    = "\033[?2004h"
	pasteOff   = "\033[?2004l"
	pasteStart = "[200~" // CSI body, with the leading ESC already consumed
	pasteEnd   = "[201~"
)

// lineReader reads one submission at a time. On a tty it runs in raw mode and
// echoes keystrokes itself: a bracketed paste is treated as literal text whose
// internal newlines are kept rather than submitting the line, which is what
// lets multi-line pastes arrive as a single request. Off a tty (pipe / one-shot
// stdin) it falls back to plain line-buffered reads.
//
// In raw mode the editor supports left/right cursor movement, Ctrl-A/E, Ctrl-W,
// up/down through previous prompts, Tab completion, Ctrl-R history search,
// and Ctrl-J / trailing-\ newlines. History is loaded from
// ~/.cubcoder/history (or $CUBCODER_HISTORY) at startup and rewritten on each
// new entry so it survives a restart.
type lineReader struct {
	in      *bufio.Reader
	raw     bool
	pending []byte // type-ahead handed back by the Esc watcher; drained first
	history []string
}

const historyKeep = 1000

func historyPath() (string, error) {
	if p := os.Getenv("CUBCODER_HISTORY"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cubcoder", "history"), nil
}

func loadHistory() []string {
	p, err := historyPath()
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, `"`) {
			var s string
			if json.Unmarshal([]byte(line), &s) == nil {
				out = append(out, s)
				continue
			}
		}
		out = append(out, line)
	}
	return out
}

func saveHistory(entries []string) error {
	p, err := historyPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if len(entries) > historyKeep {
		entries = entries[len(entries)-historyKeep:]
	}
	var b strings.Builder
	for _, e := range entries {
		enc, err := json.Marshal(e)
		if err != nil {
			continue
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	dir := filepath.Dir(p)
	tmp, err := os.CreateTemp(dir, ".history.*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(b.String()); err != nil {
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
	return os.Rename(tmpName, p)
}

// addHistory records a submitted prompt. Empty lines, consecutive duplicates,
// and exit/quit are skipped so the file stays useful. Persistence is
// best-effort: a failed write does not fail the prompt.
func (lr *lineReader) addHistory(s string) {
	s = strings.TrimRight(s, "\n")
	if strings.TrimSpace(s) == "" || s == "exit" || s == "quit" {
		return
	}
	if n := len(lr.history); n > 0 && lr.history[n-1] == s {
		return
	}
	lr.history = append(lr.history, s)
	if len(lr.history) > historyKeep {
		lr.history = append([]string(nil), lr.history[len(lr.history)-historyKeep:]...)
	}
	_ = saveHistory(lr.history)
}

// inject queues bytes to be read before the live terminal — how keystrokes the
// Esc watcher consumed during a run reach the prompt they were meant for.
func (lr *lineReader) inject(b []byte) {
	if len(b) > 0 {
		lr.pending = append(lr.pending, b...)
	}
}

// errEsc reports that the user pressed a bare Escape at a ReadLine prompt.
// Callers decide what it means: the REPL prompt ignores it, the tool-approval
// prompt treats it as deny-and-interrupt, y/N prompts fall through to their
// "no" default via the empty line.
var errEsc = errors.New("escape pressed")

// bareEsc reports whether an ESC byte just read stands alone (the Escape key)
// rather than starting a terminal sequence. A sequence's remaining bytes
// arrive in the same burst as the ESC, so they are almost always already in
// the type-ahead stash or bufio's buffer; when neither has anything, wait the
// watcher's grace period and check once more. os.Stdin carries no read
// deadline (it isn't opened for polling), so this uses Buffered() instead of
// the deadline probe the Esc watcher runs on its own /dev/tty descriptor —
// the rare burst split across two kernel reads can misread as bare, which
// costs one ignored keypress at a prompt, not data.
func (lr *lineReader) bareEsc() bool {
	if len(lr.pending) > 0 || lr.in.Buffered() > 0 {
		return false
	}
	time.Sleep(escSeqWait)
	return lr.in.Buffered() == 0
}

// readRune pops injected type-ahead before touching the live reader.
func (lr *lineReader) readRune() (rune, error) {
	if len(lr.pending) > 0 {
		r, size := utf8.DecodeRune(lr.pending)
		lr.pending = lr.pending[size:]
		return r, nil
	}
	r, _, err := lr.in.ReadRune()
	return r, err
}

// ReadLine prints prompt and reads one submission. The editor prints the
// prompt itself (rather than the caller) because erasing needs the column
// input starts at: backspace repositions the cursor by replaying the buffer's
// layout, which is what lets it cross soft-wrapped rows and pasted newlines
// instead of dying at column 0 like a bare "\b \b". Runes are assumed one
// column wide (no wcwidth), the same assumption the echo path has always made.
func (lr *lineReader) ReadLine(prompt string) (string, error) {
	fmt.Print(prompt)
	if !lr.raw {
		return lr.in.ReadString('\n')
	}
	width := termWidth(os.Stdin.Fd())
	if width <= 0 {
		// Unknown width: input never hard-wraps, so wrap-crossing can't be
		// tracked, but erasing across pasted newlines still works.
		width = 1 << 30
	}
	start := visibleWidth(prompt) % width
	var buf []rune
	pos := 0
	paste := false
	sawCR := false       // paste CRLF: swallow the LF after a CR we already turned into \n
	row, col := 0, start // screen cursor, relative to the row input starts on
	histIdx := len(lr.history)
	draft := ""

	// echo prints r and advances the tracked cursor. A rune that fills the
	// last column is followed by an explicit newline so the cursor's position
	// stays deterministic instead of depending on the terminal's pending-wrap
	// behavior; the screen then matches layout() rune for rune.
	echo := func(r rune) {
		if r == '\n' {
			fmt.Print("\n") // ONLCR (OPOST kept on) adds the carriage return
			row, col = row+1, 0
			return
		}
		fmt.Print(string(r))
		col++
		if col >= width {
			fmt.Print("\n")
			row, col = row+1, 0
		}
	}
	// layout is the screen position of the cursor after n runes of buf,
	// using the same advance rules as echo. n may be len(buf) (the end).
	layout := func(n int) (r, c int) {
		r, c = 0, start
		if n > len(buf) {
			n = len(buf)
		}
		for i := 0; i < n; i++ {
			if buf[i] == '\n' {
				r, c = r+1, 0
				continue
			}
			c++
			if c >= width {
				r, c = r+1, 0
			}
		}
		return r, c
	}
	moveTo := func(nr, nc int) {
		if nr < row {
			fmt.Printf("\033[%dA", row-nr)
		} else if nr > row {
			fmt.Printf("\033[%dB", nr-row)
		}
		fmt.Printf("\033[%dG", nc+1) // CHA is 1-based
		row, col = nr, nc
	}
	// redrawFrom reprints buf[from:] and clears leftover glyphs from a
	// previously longer line, then parks the cursor at pos.
	redrawFrom := func(from int) {
		if from < 0 {
			from = 0
		}
		moveTo(layout(from))
		for i := from; i < len(buf); i++ {
			echo(buf[i])
		}
		fmt.Print("\033[J")
		moveTo(layout(pos))
	}
	setBuf := func(s string) {
		moveTo(0, start)
		buf = []rune(s)
		pos = len(buf)
		row, col = 0, start
		for _, x := range buf {
			echo(x)
		}
		fmt.Print("\033[J")
	}
	insert := func(r rune) {
		if pos == len(buf) {
			buf = append(buf, r)
			pos++
			echo(r)
			return
		}
		buf = append(buf, 0)
		copy(buf[pos+1:], buf[pos:])
		buf[pos] = r
		pos++
		redrawFrom(pos - 1)
	}
	goHistory := func(delta int) {
		if len(lr.history) == 0 {
			return
		}
		if histIdx == len(lr.history) {
			draft = string(buf)
		}
		next := histIdx + delta
		if next < 0 {
			next = 0
		}
		if next > len(lr.history) {
			next = len(lr.history)
		}
		if next == histIdx {
			return
		}
		histIdx = next
		if histIdx == len(lr.history) {
			setBuf(draft)
			return
		}
		setBuf(lr.history[histIdx])
	}
	deleteWord := func() {
		if pos == 0 {
			return
		}
		i := pos
		for i > 0 && (buf[i-1] == ' ' || buf[i-1] == '\t') {
			i--
		}
		for i > 0 && buf[i-1] != ' ' && buf[i-1] != '\t' && buf[i-1] != '\n' {
			i--
		}
		buf = append(buf[:i], buf[pos:]...)
		pos = i
		redrawFrom(pos)
	}

	runISearch := func() (accepted string, ok bool) {
		if len(lr.history) == 0 {
			fmt.Print("\a")
			return "", false
		}
		query := ""
		from := len(lr.history)
		saved := string(buf)
		lastMatch := saved
		idx := -1

		find := func(q string, before int) (string, int) {
			if before > len(lr.history) {
				before = len(lr.history)
			}
			q = strings.ToLower(q)
			for i := before - 1; i >= 0; i-- {
				if q == "" || strings.Contains(strings.ToLower(lr.history[i]), q) {
					return lr.history[i], i
				}
			}
			return "", -1
		}
		paint := func(failed bool, match string) {
			moveTo(0, start)
			fmt.Print("\r\033[J")
			label := "reverse-i-search"
			if failed {
				label = "failed i-search"
			}
			fmt.Printf("%s(%s)`%s':%s %s", dim, label, query, reset, match)
		}
		restore := func() {
			fmt.Print("\r\033[J")
			fmt.Print(prompt)
			row, col = 0, start
			buf = []rune(saved)
			pos = len(buf)
			for _, x := range buf {
				echo(x)
			}
			moveTo(layout(pos))
		}

		match, i := find(query, from)
		if match != "" {
			lastMatch, idx = match, i
		}
		paint(false, lastMatch)

		for {
			r, err := lr.readRune()
			if err != nil {
				restore()
				return "", false
			}
			switch {
			case r == 0x1b:
				if lr.bareEsc() {
					restore()
					return "", false
				}
				_ = lr.readEscape()
			case r == '\r':
				fmt.Print("\n")
				return lastMatch, true
			case r == 0x07: // Ctrl-G cancel
				restore()
				return "", false
			case r == 0x12: // Ctrl-R: older match
				if idx < 0 {
					fmt.Print("\a")
					continue
				}
				m, j := find(query, idx)
				if m == "" {
					paint(true, lastMatch)
					fmt.Print("\a")
					continue
				}
				lastMatch, idx = m, j
				paint(false, lastMatch)
			case r == 0x7f || r == '\b':
				if query == "" {
					restore()
					return "", false
				}
				qr := []rune(query)
				query = string(qr[:len(qr)-1])
				m, j := find(query, len(lr.history))
				if m == "" {
					paint(true, lastMatch)
				} else {
					lastMatch, idx = m, j
					paint(false, lastMatch)
				}
			case r < 0x20:
			default:
				query += string(r)
				m, j := find(query, len(lr.history))
				if m == "" {
					paint(true, lastMatch)
					fmt.Print("\a")
				} else {
					lastMatch, idx = m, j
					paint(false, lastMatch)
				}
			}
		}
	}

	for {
		r, err := lr.readRune()
		if err != nil {
			if len(buf) > 0 {
				return string(buf), nil
			}
			return "", err
		}
		switch {
		case r == 0x1b: // ESC: the Escape key, a paste marker, or another sequence
			// A bare Escape returns errEsc so callers can act on it. Before this,
			// readEscape blocked on the next byte and silently swallowed the
			// user's following keystroke as a phantom "sequence body".
			if lr.bareEsc() {
				moveTo(layout(len(buf)))
				fmt.Print("\n")
				return "", errEsc
			}
			seq := lr.readEscape()
			switch seq {
			case pasteStart:
				paste = true
			case pasteEnd:
				paste = false
			default:
				if paste {
					break
				}
				switch seq {
				case "[A", "OA": // up
					goHistory(-1)
				case "[B", "OB": // down
					goHistory(1)
				case "[C", "OC": // right
					if pos < len(buf) {
						pos++
						moveTo(layout(pos))
					}
				case "[D", "OD": // left
					if pos > 0 {
						pos--
						moveTo(layout(pos))
					}
				case "[H", "[1~", "[7~": // home
					pos = 0
					moveTo(layout(0))
				case "[F", "[4~", "[8~": // end
					pos = len(buf)
					moveTo(layout(pos))
				case "[3~": // delete
					if pos < len(buf) {
						buf = append(buf[:pos], buf[pos+1:]...)
						redrawFrom(pos)
					}
				case "[13;2~", "[13;2u", "[27;2;13~": // Shift-Enter (xterm/kitty)
					insert('\n')
				}
			}
		case r == '\r' || r == '\n':
			if paste {
				// CRLF in a paste is one newline; a lone LF is a newline too.
				if r == '\n' && sawCR {
					sawCR = false
					continue
				}
				sawCR = r == '\r'
				insert('\n')
				continue
			}
			if r == '\n' { // Ctrl-J: insert a newline without submitting
				insert('\n')
				continue
			}
			if oddTrailingBackslash(string(buf)) {
				buf = buf[:len(buf)-1]
				pos = len(buf)
				insert('\n')
				continue
			}
			moveTo(layout(len(buf)))
			fmt.Print("\n")
			return string(buf), nil
		case r == '\t':
			if paste {
				insert('\t')
				continue
			}
			from, repl, matches := completeAt(buf, pos)
			if len(matches) == 0 {
				fmt.Print("\a")
				continue
			}
			token := string(buf[from:pos])
			if repl != token {
				rest := append([]rune(nil), buf[pos:]...)
				buf = append(append([]rune{}, buf[:from]...), []rune(repl)...)
				buf = append(buf, rest...)
				pos = from + len([]rune(repl))
				redrawFrom(from)
				continue
			}
			if len(matches) == 1 {
				fmt.Print("\a")
				continue
			}
			moveTo(layout(len(buf)))
			fmt.Print("\n")
			shown := matches
			if len(shown) > 40 {
				shown = shown[:40]
			}
			for _, m := range shown {
				fmt.Println(dim + "  " + m + reset)
			}
			if len(matches) > 40 {
				fmt.Printf("%s  … %d more%s\n", dim, len(matches)-40, reset)
			}
			fmt.Print(prompt)
			row, col = 0, start
			for _, x := range buf {
				echo(x)
			}
			moveTo(layout(pos))
		case r == 0x12: // Ctrl-R: reverse history search
			if paste {
				continue
			}
			if s, ok := runISearch(); ok {
				return s, nil
			}
		case r == 0x7f || r == '\b': // backspace
			if !paste && pos > 0 {
				buf = append(buf[:pos-1], buf[pos:]...)
				pos--
				redrawFrom(pos)
			}
		case r == 0x01: // Ctrl-A: start of line
			if !paste {
				pos = 0
				moveTo(layout(0))
			}
		case r == 0x05: // Ctrl-E: end of line
			if !paste {
				pos = len(buf)
				moveTo(layout(pos))
			}
		case r == 0x17: // Ctrl-W: delete previous word
			if !paste {
				deleteWord()
			}
		case r == 0x04: // Ctrl-D
			if len(buf) == 0 {
				return "", io.EOF
			}
		case r == 0x15: // Ctrl-U: clear the whole submission
			if !paste {
				setBuf("")
				draft = ""
				histIdx = len(lr.history)
			}
		case r < 0x20: // ignore other control bytes
		default:
			insert(r)
		}
	}
}

// visibleWidth counts the columns a prompt occupies: ANSI escape sequences are
// skipped and every remaining rune counts as one column (the editor's usual
// width assumption).
func visibleWidth(s string) int {
	w := 0
	const (
		text = iota
		esc  // just saw ESC
		csi  // inside ESC [ … , runs to its final byte
	)
	state := text
	for _, r := range s {
		switch state {
		case esc:
			if r == '[' {
				state = csi
			} else {
				state = text // two-byte escape (ESC c etc.) — done
			}
		case csi:
			if r >= 0x40 && r <= 0x7e { // final byte
				state = text
			}
		default:
			if r == 0x1b {
				state = esc
			} else {
				w++
			}
		}
	}
	return w
}

// readEscape consumes the rest of an escape sequence after the ESC byte and
// returns its body (e.g. "[200~" for a bracketed-paste start). It reads a CSI
// (ESC [) or SS3 (ESC O) sequence up to its final byte; anything else is
// returned as the single following rune so it can be ignored.
func (lr *lineReader) readEscape() string {
	b, err := lr.readRune()
	if err != nil {
		return ""
	}
	if b != '[' && b != 'O' {
		return string(b)
	}
	var sb strings.Builder
	sb.WriteRune(b)
	for {
		c, err := lr.readRune()
		if err != nil {
			break
		}
		sb.WriteRune(c)
		if c >= 0x40 && c <= 0x7e { // final byte of a CSI/SS3 sequence
			break
		}
	}
	return sb.String()
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

func onoff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func oneLine(s string) string { return strings.ReplaceAll(s, "\n", " ") }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
