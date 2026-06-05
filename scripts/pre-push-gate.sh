#!/usr/bin/env bash
# pre-push-gate.sh -- deterministic pre-push checks for mxlrcgo-svc.
#
# Runs the same quality chain as the pre-commit hook (gofmt, build, lint,
# govulncheck) plus the full test suite and a patch-coverage gate that
# mirrors Codecov's patch check. Run this before opening or updating a PR
# so a coverage regression is caught locally instead of on the next push.
#
# Exit status:
#   0  all checks passed
#   1  a check failed (build, test, lint, vuln, or patch coverage)
#   2  setup error (cannot resolve BASE, missing helper, etc.)
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_ROOT"

fail() { printf 'FAIL: %b\n' "$1" >&2; exit 1; }

# Per-worktree run lock + artifact dir. The mkdir is atomic, so two gate runs in
# the SAME worktree cannot clobber each other's coverage profile; the key is the
# worktree path, so gates in DIFFERENT worktrees still run concurrently.
RUN_KEY="$(printf '%s' "$REPO_ROOT" | cksum | cut -d' ' -f1)"
RUN_DIR="${TMPDIR:-/tmp}/mxlrc-gate-$RUN_KEY"
if ! mkdir "$RUN_DIR" 2>/dev/null; then
  fail "another gate run is active for this worktree ($RUN_DIR); wait for it to finish, or 'rm -rf $RUN_DIR' if it is stale"
fi
trap 'rm -rf "$RUN_DIR"' EXIT

echo "==> conflict markers"
# Reject unresolved merge-conflict markers in tracked files. Only the
# unambiguous opening/closing markers (7 '<' or '>' at column 0) are matched: a
# real conflict always carries both, and neither appears in normal content, so
# the middle '=======' marker is skipped to avoid flagging markdown setext
# headings. The .githooks dir is excluded so this very pattern does not self-trip.
CONFLICT_RE='^(<{7}|>{7})( |$)'
if git grep -nIE "$CONFLICT_RE" -- ':!.githooks/*' >/dev/null 2>&1; then
  echo "Unresolved conflict markers:" >&2
  git grep -nIE "$CONFLICT_RE" -- ':!.githooks/*' >&2 || true
  fail "resolve conflict markers before pushing"
fi

echo "==> gofmt"
unformatted=$(gofmt -l . | grep -v '^vendor/' || true)
[ -n "$unformatted" ] && fail "gofmt needed:\n$unformatted"

echo "==> go build"
go build ./... || fail "build"

echo "==> go test (race + coverage)"
# Coverage profile lives inside the locked per-worktree run dir (cleaned by the
# EXIT trap set above), so concurrent runs never share a path.
COVER_OUT="$RUN_DIR/coverage.out"
go test -race -count=1 -coverprofile="$COVER_OUT" ./... || fail "tests"

echo "==> patch coverage (Codecov parity, conservative lower bound)"
# OPTIONAL local enhancement. The estimator lives in claude-kit
# (~/.claude/scripts), not in this repo, so this step is not a hard dependency:
# when the estimator is present it reads this repo's codecov.yml for both the
# threshold and the file excludes (single source of truth) and gates on the
# result; when absent it is SKIPPED, not failed. This gate is a dev-only
# convenience -- CI enforces patch coverage via Codecov directly
# (.github/workflows/ci.yml), so nothing is lost when the estimator is missing.
HELPER="$HOME/.claude/scripts/patch-coverage.sh"
if [ -x "$HELPER" ]; then
  COVER_OUT="$COVER_OUT" \
    bash "$HELPER" || fail "patch coverage below codecov.yml threshold (note: this gate is a conservative lower bound; Codecov typically reads a few points higher)"
else
  echo "    estimator not found at $HELPER"
  echo "    skipping local patch-coverage; Codecov enforces it in CI."
  echo "    (install claude-kit for the local check)"
fi

echo "==> golangci-lint"
golangci-lint run ./... || fail "lint"

echo "==> actionlint (workflow lint)"
if command -v actionlint >/dev/null 2>&1; then
  actionlint || fail "actionlint"
else
  echo "    actionlint not installed; skipping (CI still lints workflows)"
fi

echo "==> govulncheck"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || fail "govulncheck"
else
  echo "    govulncheck not installed; skipping (CI still enforces it)"
fi

echo "OK: all pre-push checks passed"
