# Changelog

All notable changes to cubcoder are recorded here. Versions follow the git
tags; each tagged release ships rebuilt `dist/` binaries with SHA256 checksums.

## [0.1.22] - 2026-09-04

### Added
- REPL: Tab completes `/commands`, `/attach` paths, `/resume` session ids,
  and path-shaped tokens. Ctrl-R reverse-searches prompt history. Ctrl-J
  (or `\` then Enter, or Shift-Enter) inserts a newline without sending.
  A turn longer than 8s rings the bell and sends an OSC 9 notification
  when it finishes.

## [0.1.21] - 2026-09-04

### Added
- `todo_write` tool and a visible session plan. Multi-step tasks get a
  checklist (`[ ]` pending, `[~]` in progress, `[x]` done). `/plan <task>`
  is a read-only turn that writes the plan without touching files;
  `/go` implements it. `/plan` with no argument prints the list; `/status`
  shows `2/5 done`. The plan is stored with the session so it survives
  resume and compaction. At most one item is in progress.
- Session checkpoints. Each user turn in a git repo snapshots HEAD and the
  working tree (tracked + untracked; gitignored files skipped). `/undo`
  restores the last snapshot — files, HEAD, conversation, and plan —
  after a confirm; `/rewind` lists them and `/rewind <n>` restores an
  earlier one. Newest 20 are kept. A merge in progress is refused.

## [0.1.20] - 2026-09-03

### Added
- `apply_patch` tool. One call updates several places or several files, in the
  `*** Begin Patch` / `*** Update File:` / `*** Add File:` / `*** Delete File:`
  format or a standard unified diff. Hunk matching is whitespace- and
  indent-tolerant, strips `read_file` line-number prefixes and code fences,
  and refuses ambiguous hunks. Planning happens before any write, so a failed
  hunk leaves every file untouched; a hunk whose result is already in the file
  is reported as already applied. The approval preview renders each file's
  diff. Worker paths are jailed like the other file tools. The system prompt
  now steers models to `apply_patch` for edits and `edit_file` for a single
  snippet.
- `edit_file` gains `replace_all` and the same tolerant matching as patches:
  exact first, then trailing-whitespace, then indent (the replacement is
  re-indented to match the file). A miss reports up to three nearby lines
  containing the first search line.
- Worker logs persist with the session. `/agents`, `/agent <id>`, and
  `read_agent_log` work after `-continue` / `-resume`; workers that were still
  running when the session ended show as stopped. Logs are capped at 64 KiB
  each (middle elided).
- Running workers show on the lead's spinner line (`w1 → run_command go test`)
  and the prompt carries a `2w` count while workers exist.

### Changed
- Spawned commands and `git_diff` run with `PAGER`, `GIT_PAGER`, `GH_PAGER`,
  `MANPAGER`, and `SYSTEMD_PAGER` set to `cat` and `LESS=FRX`, so `git log`
  or `man` in an interactive `run_command` no longer waits on `q`.
- Worker log tool-call lines show the path, command, or pattern instead of
  truncated JSON.

## [0.1.19] - 2026-08-30

### Added
- REPL prompt history and cursor movement. Up/down recalls previous
  submissions (saved to `~/.cubcoder/history`, mode 0600, newest 1000 kept;
  `$CUBCODER_HISTORY` relocates the file). Left/right move the cursor,
  Ctrl-A/E jump to the start/end of the line, Ctrl-W deletes the previous
  word, and Delete removes the character under the cursor. Arrow keys were
  previously ignored. History is recorded at the main prompt only, so login
  and approval answers are not saved; `exit`/`quit` and consecutive
  duplicates are skipped.
- Turn footer and a hot prompt. After each turn cubcoder prints a one-line
  summary (`3 tools · 14s · 12,481 → 14,002 tokens`). Once context use
  crosses 75% the prompt carries a trailing `76%`; at 85% it turns yellow,
  matching the headroom warning.

### Changed
- Worker path jail. A rooted tool registry (background workers) rejects
  `read_file` / `write_file` / `edit_file` / `list_dir` / `search_code` /
  `git_diff` paths that resolve outside the worktree: `..`, an absolute path
  elsewhere, or a symlink that leaves the jail. The lead agent is unchanged
  (empty root, user-approved). Commands still run with the worktree as cwd
  and are not themselves path-jailed.
- Tool call lines show a path or command, not truncated JSON
  (`→ write_file  internal/tools/tools.go`). Results sit on the next line,
  dim on success and red on `Error:` or a non-zero `exit_code`. A blank
  line separates tool batches from the model's text.
- Approval preview for `write_file` and `edit_file` is a unified diff
  against the file on disk (20 lines, with context). New files are marked
  as such. `run_command` shows the cwd it will run in.

## [0.1.18] - 2026-08-26

### Added
- Session persistence and resume. Every completed turn is written to
  `~/.cubcoder/sessions/<id>.json` (mode 0600, atomic write, newest 100 kept),
  so a conversation survives exit, a gateway hiccup, or a closed laptop.
  `cubcoder -continue` resumes the latest session started in the current
  directory, `-resume <id>` (unique prefix accepted) resumes any session,
  `cubcoder sessions` / `/sessions` list them, and `/resume [id]` switches
  mid-REPL, saving the session being left. `/reset` starts a new session id.
  A resumed session runs under the current system prompt so a changed
  agents.md applies, warns when recorded in another directory, and re-anchors
  token accounting from the server on its first request. `-no-save` /
  `CUBCODER_NO_SAVE=1` keeps a session off disk; `CUBCODER_SESSIONS` relocates
  the directory. `/status` shows the session id. Worker agents are not saved.
- Image input. `read_file` on a PNG, JPEG, GIF, or WebP (detected by magic
  bytes, up to 5 MiB) returns the picture as image content the model can see,
  with a one-line description (format, dimensions, size) as the text result;
  `/attach` and `-attach` accept images the same way, fenced in the message as
  numbered attachments. The OpenAI wire format carries user images as
  multimodal parts and delivers tool-result images in one user message right
  after that turn's tool results (the tool role is string-only); the
  Anthropic format puts them inside the tool_result block. Images count 1600
  tokens each in the estimator, and stale-read eviction drops superseded image
  bytes so screenshots don't accumulate in context.

## [0.1.15] - 2026-07-29

### Added
- Esc interrupts the running turn without exiting. Pressing Escape while the
  model is thinking or streaming cancels the in-flight request and returns to
  the prompt with the conversation intact — Ctrl-C remains "quit the app". A
  watcher on /dev/tty listens only while a turn runs; it steps aside for the
  tool-approval prompt and interactive commands (sudo/ssh), keystrokes typed
  during a run still reach the next prompt, and arrow keys are recognized as
  escape sequences rather than cancels. A tool already executing finishes
  (interrupt takes effect between tools); every recorded tool call still gets
  a result so the history stays valid for the next request. Raw-mode REPL
  only — one-shot and piped runs are unchanged.

## [0.1.14] - 2026-07-28

### Added
- Project instructions from agents.md. At startup cubcoder looks for an
  agents.md file in the working directory (any casing, AGENTS.md preferred)
  and appends its contents to the system prompt, so repo-specific guidance —
  build commands, conventions, layout — is in context from the first turn.
  The instructions survive `/reset` and compaction, worker agents load the
  copy checked out in their own worktree, and a startup line notes which file
  was loaded. Oversized files are truncated at 32 KiB with a marker.

## [0.1.12] - 2026-07-11

### Added
- Big-document ingestion. A document too large to attach verbatim (past ~60%
  of the context window, where a single message can never be compacted away)
  can now be digested instead of refused: the text is split into chunks on
  line boundaries — snapping page ranges to the PDF extractor's page markers —
  and the model summarizes each chunk into a compact digest that rides the
  next message. Every section of the digest is labeled with the line and page
  range it covers, and the digest names the file path, so the model can pull
  any section verbatim with read_file when detail matters. In the REPL,
  `/attach` asks before spending the model calls (a digest can take several
  minutes); `-attach` at launch proceeds with a notice. Chunks grow past the
  16k-token target to keep very large documents under 64 model calls, and a
  document too big even for that is refused with a clear error.

## [0.1.11] - 2026-07-11

### Added
- `/compact` command. Forces a compaction pass now instead of waiting for the
  automatic 75% threshold, and reports the tokens reclaimed. Useful before
  starting a new large task in a long-lived session.
- Stale-read eviction. Before (and instead of) lossy summary compaction, tool
  results from `read_file` that a later call superseded — the same path and
  range re-read, or the file replaced outright by `write_file` — are elided in
  place to a one-line stub naming the file. The newest copy of each read stays
  verbatim, tool-call/result pairing stays wire-valid for both backends, and
  the model can always re-read the file, so unlike summarization nothing is
  irrecoverably lost. In long sessions where the same files are read repeatedly
  as they evolve, this often frees enough space to skip summarization entirely.
- Exact token accounting. Both backends now report the server's real token
  usage per request (`stream_options.include_usage` on the OpenAI path; usage
  events, summed across cache fields, on the Anthropic path). Context tracking
  anchors on the server's exact reading of the last request and estimates only
  the messages appended since, so `/status`, the headroom warning, and the
  compaction threshold work from real numbers instead of the deliberately
  conservative 3-bytes-per-token guess — the window no longer compacts early
  on systematic overestimation.

## [0.1.10] - 2026-07-11

### Added
- Document attachments. `/attach <path>` in the REPL (or a repeatable
  `-attach` flag at launch) stages a document whose text is injected directly
  into the next user message, fenced with named markers — no tool call needed,
  and the file can live anywhere, not just the working directory. PDFs go
  through the same extraction pipeline as `read_file` (page markers, table
  layout preserved); text files pass through unchanged; binary and zip-based
  office formats (docx/xlsx) are refused with guidance. Staged attachments
  show in `/attach` and `/status`, count toward the context estimate, and are
  consumed by the message they ride; `/attach clear` drops them. A document
  larger than ~60% of the context window is refused with token counts, since a
  single oversized message can never be compacted away.

## [0.1.9] - 2026-07-07

### Fixed
- Spinner frames switched from braille to ASCII. Braille frames render as
  identical tofu boxes on fonts missing U+28xx coverage (DejaVu Sans Mono,
  Konsole default), making the spinner read as a hang. The first frame now
  draws immediately, and the frame and elapsed counter stay at normal
  intensity so the liveness line remains visible on dark color schemes.

## [0.1.8] - 2026-07-07

### Added
- Window labels. `/label <text>` (or `cubcoder -label "..."` at launch) sets
  the terminal window/tab title, so several concurrent sessions can be told
  apart and kept ordered in the title bar, tab strip, taskbar, and Alt-Tab
  switcher. Uses the standard xterm OSC 0 title sequence — honored by every
  mainstream terminal emulator, no per-window-manager code — with the original
  title saved on the xterm title stack and restored on exit or `/label clear`.
  `/label` alone shows the current label.
- PDF reading. `read_file` now extracts text from PDF files instead of
  returning binary garbage, so the agent can work with financial reports,
  invoices, and other PDF documents. Files are detected by magic bytes (any
  extension works) and returned with `--- page N ---` markers. Extraction
  prefers poppler's `pdftotext -layout` when installed, which keeps table
  columns aligned; otherwise a pure-Go extractor compiled into the binary
  (github.com/ledongthuc/pdf, MIT) reconstructs lines and column spacing from
  glyph coordinates — no new runtime dependency, still a single static binary.
  Scanned image-only PDFs (no text layer) report a clear error; OCR is out of
  scope. Note: the Go module now requires Go 1.24 (was 1.22) for the PDF
  library; cross-compilation of all four dist targets verified.
- Spinner elapsed time. The progress spinner now shows how long the current
  wait has been running (`⠹ thinking… 47s`), so a slow model call or long tool
  run is distinguishable from a hang. The counter resets per phase (thinking,
  compacting, running a tool), not per turn.

## [0.1.7] - 2026-07-07

### Added
- Session sudo password memory. When an interactive command hits sudo's
  password prompt and you type the password, cubcoder now offers to remember it
  for the rest of the session; later sudo prompts are answered automatically
  (with a `(using remembered sudo password)` note). The password lives in
  memory only — never written to disk, never included in the output the model
  sees (sudo's echo-off entry already kept it out of the capture) — and a
  rejected password is dropped so the next prompt falls back to manual entry.
  `/sudo` shows whether one is remembered; `/sudo forget` clears it. Detection
  matches sudo's default `[sudo] password for <user>:` prompt only; ssh
  passwords are deliberately not cached.

## [0.1.6] - 2026-06-14

### Added
- Startup splash. On an interactive terminal, cubcoder now opens with the
  CyberBear mascot rendered in truecolor half-block ANSI art, with the app name
  and version centered beneath it. The art is embedded in the binary (no asset
  directory to ship) and shows once at launch. A terminal guard keeps it clean
  everywhere else: off a tty, in one-shot mode, or on a bare console that can't
  render truecolor (TERM=linux, dumb, vt100, and the like), startup falls back to
  the existing single-line banner.

## [0.1.5] - 2026-06-13

### Added
- Web search and page fetch, both answered from CubCloud infrastructure. The new
  `web_search` tool queries a self-hosted SearXNG node (no third-party search
  API, no key) and returns ranked results as title / URL / snippet; `fetch_url`
  retrieves an http(s) page and strips it to readable text (scripts, styles, and
  markup removed) so the model can read a result in full. Both tools are
  read-only, prompt-free, and available to the lead agent and to delegated
  workers. The search endpoint defaults to the in-fleet node and is overridable
  with the `search_url` config field, the `CUBCODER_SEARCH_URL` env var, or
  `cubcoder config set search_url <url>`; `config show` reports it. No new build
  dependency: HTML-to-text extraction uses the standard library.

## [0.1.4] - 2026-06-10

### Added
- Interactive command input. In an interactive session (stdin and stdout are a
  terminal), `run_command` runs on a pseudo-terminal connected to your terminal,
  so a command that prompts for input now works: `sudo`, `ssh`, or anything that
  reads a password from `/dev/tty` shows its prompt live and lets you type the
  answer. Command output is mirrored to the screen and captured for the model at
  the same time. Off a tty (pipes, one-shot with redirected I/O) and for
  background worker agents, commands stay captured and non-interactive as before.

### Changed
- Interactive sessions now stream command output live to the terminal; the model
  still receives the captured result. The pseudo-terminal bridge adds a
  dependency on `github.com/creack/pty`, a pure-Go library, so the build stays a
  static, CGO-free binary.

## [0.1.3] - 2026-06-10

### Added
- Persistent credentials. `cubcoder login` prompts for the API key, model, and
  gateway URL and writes them to `~/.cubcoder/config.json` (mode `0600`), so the
  key survives reboots and new shells instead of living in a session-only
  `export`. `/login` does the same from inside the REPL and applies the new
  credentials to the running session without a restart. Non-interactive
  `cubcoder config set <field> <value>`, `config show` (keys masked), and
  `config path` cover provisioning scripts. Resolution order is unchanged:
  flags > `CUBCODER_*` env > config file > defaults.

## [0.1.2] - 2026-06-05

### Fixed
- Multi-line paste no longer leaves literal `^[[200~` / `^[[201~` markers around
  pasted text. v0.1.1 enabled bracketed paste while stdin stayed in canonical
  mode, so the kernel echoed the markers. Input now runs in a partial raw mode
  (canonical line buffering and echo off; output processing and signal handling
  left on) and the reader consumes the paste markers itself.

### Changed
- The REPL reads input through a small raw-mode line reader that echoes
  keystrokes and supports backspace, Ctrl-U, Ctrl-D, and Enter; arrow keys and
  other escape sequences are ignored. A pasted block keeps its internal newlines
  and arrives as a single submission. Non-tty stdin (pipes, one-shot mode) keeps
  line-buffered reads. Terminal state is restored on exit and on SIGINT. termios
  control is hand-rolled per platform (linux, darwin) with a no-op fallback, so
  the build stays dependency-free.

## [0.1.1] - 2026-06-05

### Added
- Multi-line paste support in the REPL via bracketed paste, so a pasted block
  submits as one request instead of splitting at the first newline. (Superseded
  by the raw-mode reader in 0.1.2, which removes the echoed-marker regression.)

## [0.1.0] - 2026-06-03

First tagged release. Local coding agent driving the CubCloud local model
through the LiteLLM gateway with a Claude-Code-style tool loop. Single static
binary.

### Added
- Multi-agent orchestrator: the lead agent delegates subtasks to background
  worker agents, each in its own git worktree, with `/agents`, `/agent <id>`,
  and `/stop <id>` for visibility and control.
- Startup gateway preflight, `/status`, and tool-call argument validation.
- Configurable tool-loop iteration cap (`--max-iters`, default 150).
- `make release`: cross-build all targets (linux and darwin, amd64 and arm64)
  with SHA256 checksums, embedding a clean tagged version.

### Changed
- Default context window raised to the 262k served window, with provider
  defaults matched to the gateway.
- Tool output is capped and the conversation compacts inside long agentic tasks
  to stay within the context window.

### Fixed
- Retry chat POSTs dropped by stale gateway keep-alive connections.
- Kill the whole process group when a command times out, so no orphans survive.
