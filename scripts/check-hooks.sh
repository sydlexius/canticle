#!/usr/bin/env bash
# check-hooks.sh -- verify git is wired to the tracked .githooks directory so the
# pre-commit and pre-push gates actually run in every worktree. Invoked by
# `make doctor` and `make hooks`. Exits non-zero with remediation guidance when
# the wiring is missing.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

want=".githooks"
got="$(git config --get core.hooksPath || true)"
if [ "$got" != "$want" ]; then
  echo "FAIL: core.hooksPath is '${got:-<unset>}'; expected '$want'." >&2
  echo "      Run: make hooks" >&2
  exit 1
fi

for hook in pre-commit pre-push; do
  if [ ! -x "$want/$hook" ]; then
    echo "FAIL: $want/$hook is missing or not executable." >&2
    exit 1
  fi
done

echo "OK: git hooks wired to $want (pre-commit + pre-push)."
