# cubcoder

A local coding agent — Claude Code-style — that drives the CubCloud local model
through the LiteLLM gateway. Single static Go binary, so it ships to client VMs
with no Python runtime, container, or registry. The agent loop and tools run
**locally** (on the dev's files); the model only provides reasoning.

## Status (MVP)

Local model only (LiteLLM). Working: model-driven tool loop, local file
read/search/write/edit, shell command execution, permission prompts, one-shot
and interactive modes, history compaction, and a multi-agent orchestrator that
runs several coders in parallel (see below). Claude provider, streaming output,
and richer UX are next.

## Build

```bash
go build -o cubcoder .
```

## Setup

Run `login` once per machine to store the API key (and, optionally, the model
and gateway URL). It writes `~/.cubcoder/config.json` (mode `0600`), so the key
**survives reboots and new SSH sessions** — unlike `export`, which lives for one
shell only.

```bash
cubcoder login          # prompts for API key, model (default pre-filled), gateway URL
```

Non-interactive equivalents, handy for provisioning scripts:

```bash
cubcoder config set api_key sk-...    # set one field
cubcoder config show                  # print the saved config (keys masked)
cubcoder config path                  # print the config file location
```

Only the API key is required; `model` defaults to `Qwen3.6-35B-A3B-FP8` and
`base_url` to the LiteLLM gateway. You can also `/login` from inside the REPL.

## Use

```bash
cubcoder "add a /health endpoint and a test for it"   # one-shot
cubcoder                                               # interactive REPL
cubcoder -yes "..."                                    # auto-approve writes/commands
cubcoder -attach report.pdf "summarize the findings"   # document in context (repeatable)
cubcoder -attach shot.png "why is this dialog clipped?" # images work too
cubcoder -continue                                     # pick up the last session in this directory
cubcoder -resume 20260826-15                           # resume a specific session (id or unique prefix)
cubcoder sessions                                      # list saved sessions
```

Config resolves in this order: **flags > `CUBCODER_*` env vars > config file >
defaults**. So `-key`/`CUBCODER_API_KEY`, `-url`/`CUBCODER_API_URL`, and
`-model`/`CUBCODER_MODEL` still override the saved file for a one-off run.
`-max-iters` (`CUBCODER_MAX_ITERS`, default 150) caps the tool-loop iterations
per request; raise it for long orchestration-plus-fixup tasks that hit the cap
mid-run.

### REPL commands

| Command          | Effect                                              |
|------------------|-----------------------------------------------------|
| `/reset`         | clear the conversation                              |
| `/compact`       | shrink the conversation now: evict stale reads, summarize old turns |
| `/plan [task]`   | show the plan, or outline steps without editing files |
| `/go`            | implement the current plan                          |
| `/undo`          | restore files and conversation to the last checkpoint |
| `/rewind [n]`    | list checkpoints, or restore checkpoint n           |
| `/model [name]`  | show or switch the model mid-session                |
| `/login`         | set API key + model, saved to `~/.cubcoder/config.json`, applied live |
| `/auto [on\|off]`| toggle confirmation prompts for writes/commands     |
| `/attach <path>` | attach a document (text, PDF, or image) to your next message (`/attach` lists, `/attach clear` drops) |
| `/sessions`      | list saved sessions (this directory marked `*`)     |
| `/resume [id]`   | load a saved session (latest for this directory when no id) |
| `/sudo [forget]` | show or forget the remembered sudo password         |
| `/label <text>`  | label this window: sets the terminal title (`/label clear` to undo) |
| `/agents`        | list worker agents and their status                 |
| `/agent <id>`    | show one worker's full log                          |
| `/stop <id>`     | stop a running worker                               |
| `/agents clean`  | remove all worker worktrees and branches            |
| `/help`, `exit`  | help / quit                                         |

At the prompt: ↑/↓ recalls previous submissions (persisted in
`~/.cubcoder/history`, or `$CUBCODER_HISTORY`); ←/→ move the cursor, Ctrl-A/E
jump to the start/end, Ctrl-W deletes the previous word. Tab completes
`/commands`, `/attach` paths, `/resume` session ids, and path-shaped tokens
(`internal/ag…`). Ctrl-R searches history; Ctrl-J (or a line ending in `\`
then Enter, or Shift-Enter where the terminal sends it) inserts a newline
without sending. A turn that runs longer than 8s rings the terminal bell
and sends an OSC 9 notification when it finishes. Once context is 75% full
the prompt shows the percentage. At any confirmation prompt the change is
shown as a unified diff (or the command and cwd); `y` allow once, `N` deny
(default), `a` allow and stop asking for the rest of the session (same as
`/auto on`).

`/attach <path>` (or `-attach` at launch) puts a document straight into the
prompt context: the file's text rides inside your next message, fenced with
named markers, so the model reads it without needing a tool call — and the
file doesn't have to live in the working directory. PDFs are extracted the
same way `read_file` does (page markers, table layout preserved); any text
file passes through as-is. Attachments are staged until you send a message
(`/attach` lists them, `/status` shows them, `/attach clear` drops them) and
are consumed by that message. A document too large for the model's context
window (a single oversized message can never be compacted away) is offered as
a digest instead: the model summarizes it section by section, and the compact
digest rides your next message with line and page pointers back into the file,
so the model can read any section verbatim with read_file when detail matters.
The REPL asks before digesting, since it costs one model call per section and
can take several minutes; `-attach` at launch digests automatically with a
notice.

At the start of each user turn in a git repo, cubcoder snapshots HEAD plus
the working tree (tracked and untracked, gitignored files skipped) onto a
ref under `refs/cubcoder/cp/`. `/undo` restores the last snapshot — files,
HEAD, the conversation, and the plan — after a confirm; `/rewind` lists
them and `/rewind 3` restores an earlier one. The newest 20 snapshots are
kept. A merge in progress is refused so a half-merged worker branch is not
clobbered. Checkpoints are per session and survive `-continue` / `/resume`
as long as git still has the objects.

`/plan <task>` is a read-only turn: mutating tools (`write_file`,
`edit_file`, `apply_patch`, `run_command`, `delegate_task`, `wait_for_agents`)
are hidden, and the model writes a checklist with `todo_write` then stops.
`/go` (or any following message) implements that plan. `/plan` with no
argument prints the current checklist; `/status` shows `2/5 done`.

Every completed turn is saved to `~/.cubcoder/sessions/<id>.json` (mode
`0600`, since transcripts contain your code), so a session survives exit, a
gateway hiccup, or a closed laptop. `cubcoder -continue` resumes the most recent
session started in the current directory; `-resume <id>` (a unique prefix is
enough) resumes any session, and `cubcoder sessions` or `/sessions` lists them.
`/resume` switches sessions mid-REPL, saving the one you leave. `/reset` starts
a new session id and leaves the old file in place. A resumed session runs under
the current system prompt (so a changed `agents.md` applies) and warns when it
was recorded in a different directory. The newest 100 sessions are kept; pass
`-no-save` (or `CUBCODER_NO_SAVE=1`) to keep a session off disk, or set
`CUBCODER_SESSIONS` to move the directory.

Images go to the model as pictures, not text. `read_file` on a PNG, JPEG, GIF,
or WebP (detected by content, up to 5 MiB) returns the image itself for a
vision-capable model to look at, with a one-line description in the transcript;
`/attach shot.png` puts a screenshot in your next message the same way. The
served Qwen 3.6 model reads images. A stale image read is
evicted like a stale text read, so screenshots don't pile up in context.

Context is managed automatically: token counts come from the server's exact
accounting per request (not a guess), and when the conversation passes 75% of
the model's window, cubcoder first elides stale `read_file` results that a
later read or write superseded — near-lossless, since the model can always
re-read — and only then summarizes the oldest turns. `/compact` runs the same
pass on demand and reports the tokens reclaimed; `/status` shows the current
usage and headroom.

When a command prompts for a sudo password and you type it, cubcoder offers to
remember it for the rest of the session; later sudo prompts are then answered
automatically. The password is held in memory only — never written to disk,
never shown to the model — and a rejected password is dropped so you can retype
it. `/sudo forget` clears it early.

Running several sessions at once? `/label Coding Project 1` puts that text in the
terminal's window/tab title, so each session is identifiable in the title bar,
tab strip, taskbar, and Alt-Tab switcher. `cubcoder -label "..."` does the same
at launch for scripted window setups. The label uses the standard xterm title
sequence, which every mainstream terminal honors (inside tmux, turn on
`set-titles` to pass it through); the original title is restored on exit.
`/label clear` removes it early.

## Parallel agents (orchestrator)

The agent you talk to is a **lead** that can split a goal into independent
subtasks and run them at the same time. It delegates each subtask to a
background **worker** agent. Each worker runs in its own **git worktree** on its
own branch (`cubcoder/<name>`), so two coders never touch the same files. When a
worker finishes, its changes are committed to its branch; the lead reviews the
branches and merges the good work back.

Requires the project to be a git repository with at least one commit.

```bash
cubcoder -yes "build a static frontend in web/ and a SQLite schema in db/"
```

The lead drives this through four lead-only tools: `delegate_task(name, task)`
starts a worker, `wait_for_agents(ids?)` blocks for results, `check_agents()`
polls status, and `read_agent_log(id)` dumps a worker's log. Workers run silent
to the terminal (their output goes to per-agent logs); watch them with
`/agents` and `/agent <id>` between turns. While workers run, their last action
rides the lead's spinner line and the prompt shows a `2w` count. Worker logs are
saved with the session, so `/agents` and `read_agent_log` still work after a
resume (workers cut off mid-run show as stopped).

Worktrees live in a temp dir outside the repo and are **not** deleted on exit,
so work survives on its branch. Run `/agents clean` to remove them. Background
workers auto-approve their own file writes and commands since they cannot prompt
interactively; the worktree is their sandbox. File tools (`read_file`,
`write_file`, `apply_patch`, `edit_file`, `list_dir`, `search_code`, `git_diff`)
refuse any path that resolves outside that worktree — `..`, an absolute path elsewhere, or
a symlink that leaves the jail. The lead agent (empty root, user-approved) is
not jailed.

## Tools

`read_file`, `list_dir`, `search_code` (ripgrep), `git_diff` (read-only),
`write_file`, `apply_patch` (multi-hunk, multi-file), `edit_file` (single
search/replace, `replace_all` optional), `todo_write` (session plan),
`run_command` (sh -c, 120s).
Writes and commands prompt for confirmation unless `-yes`. Patch hunks and
`edit_file` searches match whole lines with tolerance for trailing whitespace,
indent drift, and pasted `read_file` line numbers; a patch is planned in full
before anything is written, so one bad hunk changes nothing. Commands run with
`PAGER=cat` (and the git/gh/man/systemd equivalents) so pagers never block a
tool call. The lead also gets
the orchestration tools above. Every tool resolves paths against, and runs in,
its own agent's working directory; a worker's paths are jailed to its worktree
so auto-approved writes cannot walk out with `..` or an absolute path.

`read_file` returns images (png/jpeg/gif/webp, by magic bytes) as image
content the model can see; the text result names the file, format, dimensions
and size.

`read_file` handles PDFs: it detects them by content (magic bytes, not
extension) and returns extracted text with `--- page N ---` markers. When
poppler's `pdftotext` is installed it is used with `-layout`, which keeps table
columns aligned — the best mode for financial reports and invoices. Without
poppler, a pure-Go extractor built into the binary reconstructs lines and rough
column spacing from glyph coordinates, so PDFs work on a bare client VM too.
Scanned image-only PDFs have no text layer and report an error (OCR is out of
scope).

## Layout

| Path                       | Purpose                                   |
|----------------------------|-------------------------------------------|
| `main.go`                  | CLI, REPL, permission prompts, output     |
| `internal/agent`           | the model-driven tool loop                |
| `internal/orchestrator`    | lead/worker coordination, git worktrees   |
| `internal/provider`        | model backends (OpenAI-compatible; Claude next) |
| `internal/tools`           | local coding tools                        |
| `internal/config`          | flag/env config resolution                |
| `internal/session`         | saved sessions (~/.cubcoder/sessions)     |
| `internal/checkpoint`      | git snapshots for `/undo` / `/rewind`     |

## Design notes

- Talks to **LiteLLM**, not cubgraph: a local coding tool must edit the dev's
  local files, which cubgraph's server-side workspace tools can't do.
- Provider is an interface (`internal/provider`); the Anthropic/Claude client
  drops in behind it as a fallback or preference without touching the loop.
- No external Go dependencies — stdlib only — for a clean static build.
- Parallelism reuses the same agent loop: a worker is just another agent with
  its tools rooted at a git worktree. The core loop is unchanged; isolation
  comes from the working directory, not from special-casing concurrency.
