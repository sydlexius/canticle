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

echo "==> generate web UI assets (templ + Tailwind)"
# Generated web assets (web/templates/*_templ.go, web/static/css/output.css) are
# no longer committed (issue #364): they are produced on build. web/static/embed.go
# embeds output.css at COMPILE TIME, so generation MUST run BEFORE `go build`
# below. templ is pinned via the go.mod tool directive; Tailwind is the v4
# standalone CLI (resolved from a TAILWIND override or PATH). When tailwindcss is
# absent we still run templ and fall back to the on-disk output.css so the gate
# can pass without a network fetch -- CI regenerates from scratch and is the
# source of truth.
TAILWIND_BIN="${TAILWIND:-}"
if [ -z "$TAILWIND_BIN" ]; then
  TAILWIND_BIN="$(command -v tailwindcss 2>/dev/null || command -v tailwind 2>/dev/null || true)"
fi
if [ -n "$TAILWIND_BIN" ]; then
  make generate TAILWIND="$TAILWIND_BIN" || fail "generate web UI assets"
else
  echo "    tailwindcss not found; running templ only and using the existing output.css."
  echo "    (brew install tailwindcss, or set TAILWIND=/path/to/tailwindcss to regenerate CSS)"
  go tool templ generate || fail "templ generate"
  [ -f web/static/css/output.css ] || \
    fail "web/static/css/output.css missing and tailwindcss unavailable; install tailwindcss and run 'make ui'"
fi

# Validate the generated CSS (sentinel classes + size band). Replaces the old
# committed-drift ui-check: nothing is committed to drift now, but a broken
# Tailwind run (missing @source glob, leaked Go vocabulary) must still fail
# loudly since the CSS is no longer reviewed in a diff. Skipped when CSS was
# not regenerated (no tailwindcss above) -- the on-disk file is whatever the
# last real generation produced.
if [ -n "$TAILWIND_BIN" ]; then
  make ui-validate || fail "ui-validate: generated output.css failed sentinel/size checks"
fi

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
# Overridable so the exit-code branches below can be exercised against a stub,
# following the TAILWIND_BIN convention above. Unset -> the real estimator.
HELPER="${PATCH_COVERAGE_HELPER:-$HOME/.claude/scripts/patch-coverage.sh}"
if [ -x "$HELPER" ]; then
  # BRANCH ON THE EXIT CODE, never a bare `||` (#768). The estimator defines four
  # codes whose remedies DIFFER, and two of them are OPPOSITE: exit 1 means write
  # tests, exit 3 means commit what you already wrote. A bare `||` collapsed all of
  # them into "below threshold", which is false for 2 and 3 and sends the reader to
  # do work that cannot help. Observed three times in one session, costing a cycle
  # each: the message is confident, plausible, and wrong.
  #
  # `rc=0; ... || rc=$?` rather than calling it bare: `set -e` is on, so an
  # unguarded non-zero exit would kill the script before anything could read the
  # code. The estimator prints its own detailed diagnosis to stderr on every path,
  # so the job here is to stop MISLABELING it and name the right remedy -- not to
  # restate what it already said.
  rc=0
  COVER_OUT="$COVER_OUT" bash "$HELPER" || rc=$?
  case "$rc" in
    0) ;;
    1)
      fail "patch coverage below codecov.yml threshold -- add tests for the uncovered lines.\n      (this gate is a conservative lower bound; Codecov typically reads a few points higher)"
      ;;
    2)
      # Setup/config fault, NOT a coverage verdict: a missing profile, an
      # unreadable codecov.yml, a malformed threshold. Nothing was measured.
      fail "patch-coverage could not run (exit 2: setup/config). Read its stderr above -- no coverage figure was produced, so this is not a threshold failure."
      ;;
    3)
      # REFUSED: the validity precondition failed, so the diff scope (committed
      # HEAD) and the profile (working tree) would describe different versions of
      # the same file. Deliberately NOT folded into 2 -- 2 legitimately routes to
      # "this repo has no coverage tooling", and a refusal swallowed there is the
      # silent skip the estimator's guard exists to end.
      #
      # TWO causes with DIFFERENT remedies; the estimator's stderr says which:
      #   - uncommitted Go changes -> commit them, then re-run
      #   - `git status` unreadable -> a repo-access fault; the tree state is
      #     UNDETERMINED, committing fixes nothing, and PATCH_COVERAGE_ALLOW_DIRTY
      #     does not apply (that branch exits before the override is consulted)
      fail "patch-coverage REFUSED to measure (exit 3) -- no figure was produced, so this is NOT a threshold failure.\n      Read its stderr above for which precondition failed: commit your changes, or repair git access."
      ;;
    *)
      fail "patch-coverage exited $rc, which this gate does not recognize. Read its stderr above; do not assume a coverage verdict."
      ;;
  esac
else
  echo "    estimator not found at $HELPER"
  echo "    skipping local patch-coverage; Codecov enforces it in CI."
  echo "    (install claude-kit for the local check)"
fi

echo "==> coverage floor (per-package ratchet -- informational; CI enforces)"
# CI-only enforcement (#399): the *enforced* gate is the required "Coverage Floor"
# CI job, which runs on a clean runner. Locally this is INFORMATIONAL only -- a
# loaded dev machine can transiently flap a timing-sensitive package's coverage
# (e.g. a debounce/event branch missing its window under CPU pressure), and a
# false local failure must not block a push. The check still runs and prints, so a
# genuine whole-package regression is visible here too; it is simply not fatal
# locally. internal/web is absent from the floor JSON, so the extra packages in the
# ./... profile are not evaluated.
if [ -s "$COVER_OUT" ]; then
  # Distinguish coverage-floor.sh exit codes: 1 == below floor (informational
  # only -- CI enforces); anything else (2 == config/parser error, etc.) is a
  # real failure and must still fail the gate, not be silently downgraded.
  floor_status=0
  bash scripts/coverage-floor.sh --cover "$COVER_OUT" || floor_status=$?
  if [ "$floor_status" -eq 1 ]; then
    echo "    NOTE: a package is below its coverage floor locally (informational only)."
    echo "    The CI 'Coverage Floor' job is the enforced gate; if this persists there,"
    echo "    add tests or run 'bash scripts/coverage-floor.sh --bump <pkg>' when intended."
  elif [ "$floor_status" -ne 0 ]; then
    fail "coverage floor: coverage-floor.sh exited $floor_status (config/parser error, not a below-floor result)"
  fi
else
  echo "    coverage profile missing or empty; skipping local floor check."
fi

echo "==> codecov report validation (codecovcli dry-run)"
# OPTIONAL local enhancement: validate that the coverage report parses and would
# upload cleanly BEFORE burning PR/CI wall time. The dry-run runs the same CLI
# the codecov-action wraps, but invokes it directly -- bypassing the action's
# binary-download + GPG-signature bootstrap, a known flaky step that has failed
# required CI in the past. --disable-search restricts to our profile so stray
# *coverage* config files are not picked up. codecovcli is not a repo dependency,
# so this is SKIPPED (not failed) when absent; CI's Upload Coverage job remains
# the source of truth. Quiet on success; the captured log is shown on failure.
if command -v codecovcli >/dev/null 2>&1; then
  CC_LOG="$RUN_DIR/codecovcli.log"
  if codecovcli do-upload --dry-run --disable-search --fail-on-error \
      --file "$COVER_OUT" >"$CC_LOG" 2>&1; then
    echo "    coverage report validated (dry-run, no upload)"
  else
    cat "$CC_LOG"
    fail "codecovcli dry-run: coverage report failed validation"
  fi
else
  echo "    codecovcli not installed; skipping (CI uploads coverage)."
  echo "    (pipx install codecov-cli for the local check)"
fi

echo "==> golangci-lint"
# A REMOVED WORKTREE POISONS THE SHARED LINT CACHE (#669). golangci-lint's cache
# is USER-GLOBAL (~/Library/Caches/golangci-lint on macOS), not per-worktree, so
# deleting one worktree leaves entries keyed to paths that no longer exist. The
# next gate in a SIBLING worktree then reports dozens of findings against files
# it cannot even open -- 107 on one occurrence measured 2026-08-05, every one
# naming a path inside the removed directory and none in the working tree.
#
# The cost is entirely in wasted time and misdirection: the findings are not
# real, but they fail the gate, and the natural response is to go looking for a
# bug in code that is fine. This blocked three gate runs in a single session --
# including a release tag push -- and none of them had changed a line of Go.
#
# So the gate detects the condition itself rather than relying on whoever removed
# the worktree to remember. The roster lives under the COMMON git dir, which every
# worktree of this clone shares, so a removal recorded by one gate run is visible
# to the next run in any sibling.
#
# It cleans only on a DISAPPEARANCE. Adding a worktree is harmless (nothing is
# stale), and cleaning unconditionally would throw away a warm cache on every run
# -- the lint step is one of the slowest in the gate, so that trade matters.
if command -v golangci-lint >/dev/null 2>&1; then
  WT_ROSTER="$(git rev-parse --git-common-dir)/golangci-worktree-roster"
  # STRIP THE PREFIX, do not field-split. `awk '{print $2}'` truncates a path at
  # the first space, and a worktree under a directory with a space in its name is
  # entirely ordinary. A truncated path never matches the live list, so it reads
  # as "removed" on EVERY run and would clean the cache every time -- silently
  # discarding the warm-cache trade this whole block exists to preserve.
  #
  # LC_ALL=C throughout: comm requires both inputs in the SAME collation, and the
  # roster outlives the run that wrote it. A locale change between runs would
  # otherwise make comm reject a roster it had itself produced.
  WT_NOW="$(git worktree list --porcelain | sed -n 's/^worktree //p' | LC_ALL=C sort)"
  if [ -f "$WT_ROSTER" ]; then
    # A path in the recorded roster that is absent from the live list was removed.
    # comm -23 prints lines unique to the first (recorded) side.
    if WT_GONE="$(LC_ALL=C comm -23 "$WT_ROSTER" <(printf '%s\n' "$WT_NOW"))" && [ -n "$WT_GONE" ]; then
      echo "    worktree removed since the last gate run; cleaning the shared lint cache:"
      printf '%s\n' "$WT_GONE" | sed 's/^/      - /'
      golangci-lint cache clean || echo "    WARNING: cache clean failed; phantom findings may follow" >&2
    fi
  fi
  printf '%s\n' "$WT_NOW" > "$WT_ROSTER"
fi
golangci-lint run ./... || fail "lint"

echo "==> actionlint (workflow lint)"
if command -v actionlint >/dev/null 2>&1; then
  actionlint || fail "actionlint"
else
  echo "    actionlint not installed; skipping (CI still lints workflows)"
fi

# The CI test-shard split (issue #662) fails SILENTLY when it drifts: a package
# claimed by no shard is simply never tested, and nothing goes red. Assert here
# that every package in `go list ./...` lands in exactly one shard, and that each
# partitioned package's buckets are complete and disjoint.
# Needs Bash 4+ (declare -A); stock macOS /bin/bash is 3.2, so skip rather than
# fail when only an old shell is available -- CI runs it on ubuntu-latest.
echo "==> ci-shards (test-shard split integrity)"
if bash -c '[ "${BASH_VERSINFO[0]}" -ge 4 ]' 2>/dev/null; then
  bash scripts/ci-shards.sh verify || fail "ci-shards verify"
else
  echo "    bash 4+ not available; skipping (CI still verifies the shard split)"
fi

echo "==> govulncheck"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || fail "govulncheck"
else
  echo "    govulncheck not installed; skipping (CI still enforces it)"
fi

echo "OK: all pre-push checks passed"
