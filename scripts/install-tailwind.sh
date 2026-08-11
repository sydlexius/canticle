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

# Retry transient network failures. Every build path (CI shards, Dockerfile,
# GoReleaser hooks) funnels through this one download, so a single dropped
# connection to the GitHub release CDN fails a whole run. Observed in CI as
# "curl: (35) Recv failure: Connection reset by peer" on an otherwise green PR.
#
# --retry-all-errors is what makes this work rather than merely look like it does:
# plain --retry covers timeouts, 408/429, and 5xx, but a mid-transfer connection
# reset is none of those, so without it the exact failure being fixed here would
# not be retried. A genuine 404 still fails (after burning the retries) because
# -f keeps HTTP errors fatal.
#
# It was added in curl 7.71.0 (2020), which every path this project builds on
# clears -- Alpine 3.24 ships 8.21, ubuntu-latest ships 8.x, a current macOS
# ships 8.7. But older LTS hosts do not (Ubuntu 20.04: 7.68, RHEL 7: 7.29), and
# an unknown option is a HARD curl failure, which would break `make ui` on a
# machine where it used to work. So probe once and degrade to plain --retry:
# still better than no retry, and never worse than before this script retried.
CURL_RETRY=(--retry 3 --retry-delay 2 --connect-timeout 15)
if curl --help all 2>/dev/null | grep -q -- '--retry-all-errors'; then
  CURL_RETRY+=(--retry-all-errors)
else
  echo "install-tailwind: curl lacks --retry-all-errors (needs 7.71.0+); a dropped connection will not be retried" >&2
fi

echo "==> downloading ${ASSET} (v${VERSION})"
curl -fsSL "${CURL_RETRY[@]}" -o "$workdir/$ASSET" "$BASE/$ASSET"

echo "==> verifying sha256"
# Resolve a SHA-256 tool. Linux ships `sha256sum`; macOS (a first-class dev
# platform here) ships `shasum -a 256` instead. Compute-and-compare rather than
# `sha256sum -c` so we do not depend on either tool's checkfile format.
if command -v sha256sum >/dev/null 2>&1; then
  sha256_of() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
  sha256_of() { shasum -a 256 "$1" | awk '{print $1}'; }
else
  echo "install-tailwind: neither sha256sum nor shasum found; cannot verify download" >&2
  exit 1
fi

# Pull the expected hash for our asset from the release's sha256sums.txt. Entries
# look like "<hash>  ./tailwindcss-linux-x64"; match the asset at end-of-line with
# an optional leading "./" so an upstream switch to a bare "filename" form does
# not silently break verification. ASSET is a controlled value (no regex metachars).
expected="$(curl -fsSL "${CURL_RETRY[@]}" "$BASE/sha256sums.txt" \
  | grep -E "[[:space:]]\.?/?${ASSET}\$" \
  | awk '{print $1}' | head -n1)"
if [ -z "$expected" ]; then
  echo "install-tailwind: no sha256 entry for ${ASSET} in sha256sums.txt" >&2
  exit 1
fi
actual="$(sha256_of "$workdir/$ASSET")"
if [ "$expected" != "$actual" ]; then
  echo "install-tailwind: sha256 mismatch for ${ASSET}" >&2
  echo "  expected: $expected" >&2
  echo "  actual:   $actual" >&2
  exit 1
fi

# Atomically place the verified binary at DEST.
mkdir -p "$(dirname "$DEST")"
mv "$workdir/$ASSET" "$DEST"
chmod +x "$DEST"
echo "==> installed Tailwind v${VERSION} to ${DEST}"
