#!/usr/bin/env bash
set -euo pipefail

# Updates the cubcoder executable from the public GitHub raw endpoint.
# Redistributable: copy to any Linux or macOS machine (LAN or internet) and run.
# Overridable via env:
#   CUBCODER_BASE_URL  base raw URL (default points at the public GitHub repo)
#   CUBCODER_DEST      install path (default /usr/local/bin/cubcoder)
#   CUBCODER_ARCH      override arch detection (amd64|arm64)

BASE_URL="${CUBCODER_BASE_URL:-https://raw.githubusercontent.com/cubcloudai/cubcoder/main/dist}"
DEST="${CUBCODER_DEST:-/usr/local/bin/cubcoder}"

case "$(uname -s)" in
  Linux)  OS="linux" ;;
  Darwin) OS="darwin" ;;
  *) echo "ERROR: unsupported OS: $(uname -s)" >&2; exit 1 ;;
esac

# Detect arch unless overridden
if [ -n "${CUBCODER_ARCH:-}" ]; then
  ARCH="$CUBCODER_ARCH"
else
  case "$(uname -m)" in
    x86_64|amd64)        ARCH="amd64" ;;
    aarch64|arm64)       ARCH="arm64" ;;
    *) echo "ERROR: unsupported architecture: $(uname -m)" >&2; exit 1 ;;
  esac
fi

NAME="cubcoder-${OS}-${ARCH}"
URL="${BASE_URL}/${NAME}"

# Pick a downloader that's actually installed
if command -v curl >/dev/null 2>&1; then
  DL() { curl -fsSL -o "$1" "$2"; }
elif command -v wget >/dev/null 2>&1; then
  DL() { wget -qO "$1" "$2"; }
else
  echo "ERROR: need curl or wget installed" >&2
  exit 1
fi

TMP="$(mktemp)"
SUMS="$(mktemp)"
trap 'rm -f "$TMP" "$SUMS"' EXIT

echo "Downloading cubcoder (${OS}/${ARCH}) from ${URL}"
if ! DL "$TMP" "$URL"; then
  echo "ERROR: download failed from $URL" >&2
  exit 1
fi

# Sanity check: must be a native executable, not an HTML error page.
# ELF magic on Linux, Mach-O 64-bit magic on macOS.
case "$OS" in
  linux)  MAGIC="7f454c46"; KIND="ELF" ;;
  darwin) MAGIC="cffaedfe"; KIND="Mach-O" ;;
esac
if command -v file >/dev/null 2>&1; then
  if ! file "$TMP" | grep -q "$KIND"; then
    echo "ERROR: download is not a $KIND binary (got: $(file -b "$TMP"))" >&2
    echo "Check the URL — it must point at raw.githubusercontent.com/.../dist, not the GitHub HTML file view." >&2
    exit 1
  fi
else
  # No `file` available: check the magic bytes directly
  if [ "$(head -c 4 "$TMP" | od -An -tx1 | tr -d ' \n')" != "$MAGIC" ]; then
    echo "ERROR: download is not a $KIND binary (bad magic)" >&2
    exit 1
  fi
fi

# Verify against the checksums written by `make release`
if command -v sha256sum >/dev/null 2>&1; then
  SHA() { sha256sum "$1"; }
else
  SHA() { shasum -a 256 "$1"; }
fi
if ! DL "$SUMS" "${BASE_URL}/SHA256SUMS.txt"; then
  echo "ERROR: could not download SHA256SUMS.txt from ${BASE_URL}" >&2
  exit 1
fi
EXPECTED="$(awk -v name="$NAME" '$2 == name {print $1}' "$SUMS")"
if [ -z "$EXPECTED" ]; then
  echo "ERROR: no entry for $NAME in SHA256SUMS.txt" >&2
  exit 1
fi
ACTUAL="$(SHA "$TMP" | awk '{print $1}')"
if [ "$ACTUAL" != "$EXPECTED" ]; then
  echo "ERROR: checksum mismatch for $NAME" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  got:      $ACTUAL" >&2
  exit 1
fi
echo "Checksum OK"

# Remove any stale copy that shadows DEST earlier in PATH
rm -f "$HOME/.local/bin/cubcoder"

# Install, using sudo only when we can't do it ourselves.
# /usr/local/bin may not exist on a fresh Apple Silicon mac.
DESTDIR="$(dirname "$DEST")"
if [ ! -d "$DESTDIR" ]; then
  mkdir -p "$DESTDIR" 2>/dev/null || sudo mkdir -p "$DESTDIR"
fi
if [ -w "$DESTDIR" ]; then
  install -m 0755 "$TMP" "$DEST"
else
  sudo install -m 0755 "$TMP" "$DEST"
fi

# Verify the binary we just installed (not whatever PATH resolves to)
"$DEST" --version
