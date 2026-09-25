---
name: verify
description: Build and drive cubcoder end-to-end to verify a change at the REPL surface.
---

# Verifying cubcoder changes

Build: `make build` (binary lands at `./cubcoder`; version stamps from git describe).

Drive the REPL by piping stdin — piped input skips raw mode but the REPL works
normally. One line per prompt or slash command, end with `exit`:

```bash
printf 'Reply with only: pong\n/status\nexit\n' | ./cubcoder 2>&1 | sed 's/\x1b\[[0-9;]*m//g'
```

The `sed` strips ANSI colors for readable captures.

Backend: the default gateway is the LiteLLM tunnel at
`https://ma.cubcloud.ai/v1` (config in `~/.cubcoder/config.json`). Override
the base with `-url <base>/v1` or `CUBCODER_API_URL` — any OpenAI-compatible
endpoint works. Check reachability with `curl <base>/v1/models`
(the gateway needs the API key; a direct vLLM endpoint typically does not).

Gotchas:
- The startup preflight pings `/models` with a short timeout; a down gateway
  prints a warning but the REPL still starts.
- Model turns over vLLM take 5–60s depending on prompt size; give piped
  sessions a generous timeout.
- read_file results are capped at ~24k chars, useful for sizing
  context-growth experiments (a full read of main.go ≈ 8k tokens).
- One-shot mode is `./cubcoder "prompt"`; flags must come before the prompt.
