#!/usr/bin/env bash
# coverage-floor.sh -- one-way per-package coverage ratchet.
#
# Runs the suite with per-package coverage and fails if any package drops below
# the floor recorded in scripts/coverage-floor.json. Floors are never lowered
# automatically: raise them intentionally with
#   bash scripts/coverage-floor.sh --update
# after a deliberate coverage improvement. This complements Codecov's patch
# coverage (which only sees the diff) by guarding whole-package regressions.
#
# jq is required; when it is absent the check is skipped (CI/Codecov still
# enforce coverage), mirroring the optional-tool pattern of the patch-cover gate.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

SEED="scripts/coverage-floor.json"
MODE="check"
[ "${1:-}" = "--update" ] && MODE="update"

if ! command -v jq >/dev/null 2>&1; then
  echo "coverage-floor: jq not found; skipping (Codecov still enforces coverage)." >&2
  exit 0
fi

# Collect "pkg pct" lines from the per-package coverage summary. Packages with
# no test files print no "coverage:" token and are skipped.
current="$(go test -count=1 -cover ./... \
  | awk '/coverage: [0-9.]+% of statements/ {
      pkg=""; pct="";
      for (i = 1; i <= NF; i++) {
        if ($i ~ /^github\.com\//) pkg = $i;
        if ($i == "coverage:") { pct = $(i + 1); gsub(/%/, "", pct); }
      }
      if (pkg != "" && pct != "") print pkg, pct;
    }')"

if [ "$MODE" = "update" ]; then
  printf '%s\n' "$current" \
    | jq -R -s 'split("\n") | map(select(length > 0) | split(" "))
                | map({(.[0]): (.[1] | tonumber)}) | add // {}' \
    | jq -S '.' > "$SEED"
  echo "coverage-floor: wrote per-package floors to $SEED"
  exit 0
fi

if [ ! -f "$SEED" ]; then
  echo "coverage-floor: no seed at $SEED; create it with 'bash scripts/coverage-floor.sh --update'." >&2
  exit 0
fi

status=0
while read -r pkg pct; do
  [ -z "$pkg" ] && continue
  floor="$(jq -r --arg p "$pkg" '.[$p] // empty' "$SEED")"
  [ -z "$floor" ] && continue
  if awk "BEGIN { exit !($pct < $floor) }"; then
    echo "FAIL: $pkg coverage ${pct}% < floor ${floor}%" >&2
    status=1
  fi
done <<< "$current"

if [ "$status" -eq 0 ]; then
  echo "OK: all packages meet their recorded coverage floor."
fi
exit $status
