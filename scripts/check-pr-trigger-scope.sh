#!/usr/bin/env bash
# check-pr-trigger-scope.sh -- assert workflows run on pull requests to ANY base
# branch.
#
# THE FAILURE: with `pull_request: branches: [main]`, a stacked PR (base is
# another feature branch) runs none of the workflows behind it. A PR with no
# checks reads much like a PR whose checks passed, so an untested PR looks
# mergeable.
#
# WHAT IT CHECKS (default-deny, so a new workflow is covered automatically):
#   1. EVERY workflow (*.yml and *.yaml; Actions loads both) with a
#      `pull_request:` or `pull_request_target:` key under `on:` must carry no
#      `branches:` / `branches-ignore:` under it (block form) and none on the key
#      line (flow form `pull_request: {branches: [main]}`), unless the file is on
#      EXEMPT. Only the 2-space layout is parsed (repo standard).
#   2. MUST_TRIGGER files (the ones producing the required `Protect main`
#      contexts) must still HAVE a pull_request trigger; losing it means no check
#      ever runs.
#
# Exit: 0 = ok, 1 = violation, 2 = setup error.
# Usage: bash scripts/check-pr-trigger-scope.sh [workflow-dir]
set -euo pipefail

DIR="${1:-$(cd "$(dirname "$0")/.." && pwd)/.github/workflows}"
# ci.yml owns Build/Test/Lint/Coverage Floor; codeql.yml owns Analyze Go (the
# ruleset's code_scanning rule also needs an analysis on every PR).
MUST_TRIGGER="ci.yml codeql.yml"
# EXEMPT: files allowed to restrict the base branch, one reason each.
#   dependabot-auto-approve.yml -- approval + auto-merge path; must only act on PRs to main.
#   pages.yml -- docs-site build; not a required check.
EXEMPT=" dependabot-auto-approve.yml pages.yml "

for w in $MUST_TRIGGER; do
  if [ ! -f "$DIR/$w" ]; then
    echo "FAIL: workflow not found: $DIR/$w" >&2
    exit 2
  fi
done

status=0
# An unmatched glob stays literal in bash, so skip non-files.
for f in "$DIR"/*.yml "$DIR"/*.yaml; do
  [ -f "$f" ] || continue
  w=$(basename "$f")
  # A required check needs pull_request itself; pull_request_target is a
  # different event and does not produce it.
  case " $MUST_TRIGGER " in
    *" $w "*)
      if ! grep -qE '^  pull_request:' "$f" && ! grep -qE '^("on"|on):.*pull_request([^_]|$)' "$f"; then
        echo "FAIL: $w has no pull_request trigger but produces a required check"
        status=1
        continue
      fi ;;
  esac
  if ! grep -qE '^  pull_request(_target)?:' "$f" && ! grep -qE '^("on"|on):.*pull_request' "$f"; then
    continue
  fi
  case "$EXEMPT" in *" $w "*) continue ;; esac
  bad=$(awk '
    { line = $0; sub(/[[:space:]]*#.*/, "", line) }
    # An inline on: mapping carries its filters on the same line.
    /^("on"|on):/ && line ~ /pull_request/ && line ~ /branches/ { print NR ": " $0 }
    /^[^[:space:]#]/ { inon = (line ~ /^("on"|on):/); inpr = 0; next }
    !inon { next }
    /^  pull_request(_target)?:/ { inpr = 1; if (line ~ /branches/) print NR ": " $0; next }
    /^  [^[:space:]#]/ { inpr = 0 }
    inpr && line ~ /^[[:space:]]+branches(-ignore)?:/ { print NR ": " $0 }
  ' "$f")
  if [ -n "$bad" ]; then
    echo "FAIL: $w restricts pull_request to specific base branches:"
    printf '%s\n' "$bad" | sed 's/^/  /'
    status=1
  fi
done

if [ "$status" -ne 0 ]; then
  echo "  HINT: remove the branches filter (or add a justified EXEMPT entry in scripts/check-pr-trigger-scope.sh);" >&2
  echo "  a stacked PR would otherwise run none of that workflow." >&2
  exit 1
fi
echo "OK: pull_request workflows run on any base branch (except EXEMPT)"
