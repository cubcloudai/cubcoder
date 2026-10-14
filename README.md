# cubcoder

A local coding agent that ships as a single static binary. The agent loop and
tools run on your machine; a CubCloud model does the reasoning over the gateway.
No Python runtime, container, or registry to install.

This repo distributes the prebuilt binary and the updater. The source is private.

## Install and update

Download the updater once, then run it anytime to install or update to the
latest build. It installs to `/usr/local/bin/cubcoder` (uses `sudo` only when the
destination needs it) and verifies the download against a published checksum.

```bash
wget https://raw.githubusercontent.com/cubcloudai/cubcoder/main/cc_update.sh
chmod +x cc_update.sh
./cc_update.sh
```

Linux and macOS are supported on both amd64 and arm64; the updater detects your
platform. Override the arch with `CUBCODER_ARCH`, the install path with
`CUBCODER_DEST`, or the download base with `CUBCODER_BASE_URL`.

### Windows

Use the PowerShell updater. It installs `cubcoder.exe` to `%LOCALAPPDATA%\cubcoder`
(no admin needed) and verifies the download against the same published checksum.

```powershell
iwr https://raw.githubusercontent.com/cubcloudai/cubcoder/main/cc_update.ps1 -OutFile cc_update.ps1
powershell -ExecutionPolicy Bypass -File cc_update.ps1
```

amd64 and arm64 are both detected. The same `CUBCODER_ARCH`, `CUBCODER_DEST`, and
`CUBCODER_BASE_URL` overrides apply. On WSL, use `cc_update.sh` and the Linux binary.

## Setup

Run `login` once per machine to store your API key, and optionally the model and
gateway URL. It writes `~/.cubcoder/config.json` at mode `0600`, so the key
survives reboots and new SSH sessions.

```bash
cubcoder login
```

Non-interactive, for provisioning scripts:

```bash
cubcoder config set api_key [API_KEY]
cubcoder config show                    # prints the saved config, keys masked
cubcoder config path
```

Config resolves in this order: flags, then `CUBCODER_*` env vars, then the config
file, then defaults.

## Use

```bash
cubcoder "add a /health endpoint and a test for it"    # one-shot
cubcoder                                                # interactive REPL
cubcoder -yes "..."                                     # auto-approve writes and commands
cubcoder -attach report.pdf "summarize the findings"   # document in context (repeatable)
cubcoder -continue                                      # resume the last session in this directory
cubcoder sessions                                       # list saved sessions
```

Run `/help` inside the REPL for the full command list.
