#!/usr/bin/env bash
# Verify deploy/aligner's generated hash locks are exactly what their sources
# resolve to: regenerate each lock into a temp dir with the recipe in
# deploy/aligner/README.md and compare against the committed file.
#
# Why this exists: the per-arch/test locks can be edited without touching
# their sources (Dependabot did exactly that in #1064/#1065, bumping
# transitives past what their parents allow), and no other check installs
# them. Resolution only -- nothing is installed -- so this is fast and needs
# no Docker: uv cross-resolves each target via --python-platform.
#
# The --exclude-newer date is read from each lock's OWN header line
#   # Generated: --exclude-newer <RFC3339>
# which the README already requires be kept in sync on every regeneration, so
# bumping the date stays a header edit with no second place (here) to change.
#
# Header rule: the hand-written header is the leading run of lines that are
# empty or start with `#`; uv's --no-header output starts directly at the
# first requirement line. Both sides are compared from their first line that
# is neither empty nor a comment through EOF, so uv's own `# via` annotations
# (always after the first requirement) are still compared.
#
# Usage: scripts/check-aligner-locks-fresh.sh [aligner-dir]
#   UV=/path/to/uv overrides the uv binary (default: uv on PATH; the recipe
#   and CI pin uv==0.9.7 -- another version may format output differently).
set -euo pipefail

ALIGNER_DIR="${1:-deploy/aligner}"
UV="${UV:-uv}"
CPU_INDEX="https://download.pytorch.org/whl/cpu"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

strip_header() {
  awk 'started || ($0 != "" && $0 !~ /^#/) { started = 1; print }' "$1"
}

pins() {
  { grep -E '^[A-Za-z0-9][A-Za-z0-9._-]*==' "$1" || true; } | sed -E 's/ .*//' | sort -u
}

failed=0
check() {
  local lock="$1" src="$2"
  shift 2
  local committed="$ALIGNER_DIR/$lock"
  local date
  date="$(sed -n -E 's/^# Generated: --exclude-newer ([^ ]+).*/\1/p' "$committed")"
  if [ "$(printf '%s\n' "$date" | grep -c .)" -ne 1 ]; then
    echo "::error file=$committed::expected exactly one '# Generated: --exclude-newer <date>' header line in $lock; cannot reproduce its resolution"
    failed=1
    return
  fi

  if ! (cd "$ALIGNER_DIR" && "$UV" pip compile --quiet --generate-hashes \
      --python-version 3.13 --no-header --no-emit-index-url \
      --exclude-newer "$date" "$@" "$src" -o "$tmp/$lock"); then
    echo "::error file=$committed::uv could not resolve $src (exclude-newer $date) -- see output above"
    failed=1
    return
  fi

  strip_header "$committed" > "$tmp/$lock.committed"
  strip_header "$tmp/$lock" > "$tmp/$lock.fresh"
  if [ ! -s "$tmp/$lock.committed" ] || [ ! -s "$tmp/$lock.fresh" ]; then
    echo "::error file=$committed::no requirement lines found after stripping the header of $lock"
    failed=1
    return
  fi

  if cmp -s "$tmp/$lock.committed" "$tmp/$lock.fresh"; then
    echo "$lock: fresh (matches $src resolved at $date)"
    return
  fi

  pins "$tmp/$lock.committed" > "$tmp/$lock.cpins"
  pins "$tmp/$lock.fresh" > "$tmp/$lock.fpins"
  local only_committed only_fresh
  only_committed="$(comm -23 "$tmp/$lock.cpins" "$tmp/$lock.fpins" | paste -sd' ' -)"
  only_fresh="$(comm -13 "$tmp/$lock.cpins" "$tmp/$lock.fpins" | paste -sd' ' -)"
  diff -u --label "$lock (committed)" --label "$lock (regenerated)" \
    "$tmp/$lock.committed" "$tmp/$lock.fresh" | head -n 200 || true
  echo "::error file=$committed::$lock does not match a fresh resolution of $src at exclude-newer $date. Committed-only pins: ${only_committed:-none}. Resolver's pins: ${only_fresh:-none} (if both are none, only hashes/annotations differ). Never edit this lock by hand: regenerate it per deploy/aligner/README.md (Dependency lock section) and re-add its header block."
  failed=1
}

check requirements-linux-amd64.txt requirements.in \
  --python-platform x86_64-manylinux_2_28 \
  --extra-index-url "$CPU_INDEX" --index-strategy unsafe-best-match
check requirements-linux-arm64.txt requirements.in \
  --python-platform aarch64-manylinux_2_28 \
  --extra-index-url "$CPU_INDEX" --index-strategy unsafe-best-match
check requirements-test.txt requirements-test.in

exit "$failed"
