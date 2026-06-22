#!/usr/bin/env bash
# install-tailwind.sh -- download + verify the Tailwind v4 standalone CLI.
#
# The serve-mode web UI's CSS is generated at build time (issue #364), so every
# build path that runs `make ui` / `make tailwind` needs the node-free Tailwind
# standalone binary on disk. This script downloads it from the GitHub release and
# validates its sha256 against the release's sha256sums.txt before installing, so
# a tampered or truncated download fails loudly instead of producing bad CSS.
#
# Reused by the Dockerfile, GoReleaser before-hooks, and CI so the download +
# checksum logic lives in exactly one place.
#
# Usage:
#   scripts/install-tailwind.sh [DEST]
#
#   DEST       install path for the binary (default: ./tailwindcss in CWD).
#              May also be set via the TAILWIND_DEST env var; the positional
#              arg wins when both are given.
#
# Env overrides:
#   TAILWIND_VERSION  release tag without the leading v (default: 4.2.0)
#   TAILWIND_ASSET    release asset name; when unset it is auto-detected from
#                     uname (e.g. tailwindcss-linux-x64, tailwindcss-macos-arm64).
#                     Override for musl libc (Alpine): tailwindcss-linux-x64-musl.
#
# Exit status: 0 on success; non-zero on download or checksum failure.
set -euo pipefail

VERSION="${TAILWIND_VERSION:-4.2.0}"
DEST="${1:-${TAILWIND_DEST:-./tailwindcss}}"

# Auto-detect the release asset from the host OS/arch unless caller pinned one.
# The Dockerfile pins the -musl variant explicitly (Alpine is musl-linked, where
# the default glibc binary will not run); CI pins linux-x64; this default keeps
# the GoReleaser before-hook portable across a Linux runner and a maintainer's
# Mac.
if [ -n "${TAILWIND_ASSET:-}" ]; then
  ASSET="$TAILWIND_ASSET"
else
  case "$(uname -s)" in
    Linux)  os=linux ;;
    Darwin) os=macos ;;
    *) echo "install-tailwind: unsupported OS $(uname -s); set TAILWIND_ASSET" >&2; exit 1 ;;
  esac
  case "$(uname -m)" in
    x86_64|amd64)  arch=x64 ;;
    aarch64|arm64) arch=arm64 ;;
    *) echo "install-tailwind: unsupported arch $(uname -m); set TAILWIND_ASSET" >&2; exit 1 ;;
  esac
  ASSET="tailwindcss-${os}-${arch}"
fi

BASE="https://github.com/tailwindlabs/tailwindcss/releases/download/v${VERSION}"

# Work in a private temp dir so a failed checksum never leaves a half-downloaded
# binary at DEST. Cleaned up on any exit.
workdir="$(mktemp -d)"
trap 'rm -rf "$workdir"' EXIT

echo "==> downloading ${ASSET} (v${VERSION})"
curl -fsSL -o "$workdir/$ASSET" "$BASE/$ASSET"

echo "==> verifying sha256"
# Pull the single line for our asset from the release's sha256sums.txt and
# rewrite its path to point at the downloaded file, then verify. A mismatch or
# a missing line makes sha256sum -c exit non-zero.
curl -fsSL "$BASE/sha256sums.txt" \
  | grep "${ASSET}\$" \
  | sed "s|^\([^ ]*\)  ./|\1  $workdir/|" \
  | sha256sum -c -

# Atomically place the verified binary at DEST.
mkdir -p "$(dirname "$DEST")"
mv "$workdir/$ASSET" "$DEST"
chmod +x "$DEST"
echo "==> installed Tailwind v${VERSION} to ${DEST}"
