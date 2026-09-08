#!/usr/bin/env bash
# check-hooks.sh -- verify git is wired to the tracked .githooks directory so the
# pre-commit and pre-push gates actually run in every worktree. Invoked by
# `make doctor` and `make hooks`. Exits non-zero with remediation guidance when
# the wiring is missing.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

want=".githooks"

# ASK GIT WHERE THE HOOKS ARE; DO NOT RE-DERIVE IT. `--git-path hooks` returns
# the directory git will ACTUALLY run hooks from, already accounting for
# core.hooksPath, its relative/absolute form, and worktree layout. Re-deriving
# that answer is what made both of this script's historical false failures:
#
#   1. A string compare against the literal ".githooks" rejected a working
#      ABSOLUTE setting (measured: hooks ran on every push while this script
#      said FAIL and advised `make hooks`, which would have changed nothing).
#   2. Resolving ".githooks" against the CURRENT root rejected every git
#      WORKTREE, whose root holds its own copy of the tracked directory while
#      git keeps using the primary clone's. Measured against a real worktree in
#      this repo: `git rev-parse --git-path hooks` named the primary clone's
#      .githooks and hooks ran, while this script exited 1.
#
# Both were the same mistake -- asserting a path git had already computed -- and
# a false failure that prescribes a no-op remedy is worse than no check at all.
# `--type=path` matters for the same reason: git expands a leading `~` in a
# path-typed value, and a plain `--get` returns the unexpanded string.
#
# WHAT THIS DOES NOT ASSERT, stated so the check is not mistaken for more than
# it is: it verifies WHERE git will run hooks from, never that the CONTENT there
# is the repo's tracked code. A `.githooks` SYMLINK pointing elsewhere is
# accepted (measured), because `pwd -P` resolves both sides and git really does
# run whatever the link targets -- so accepting it is honest about execution
# while being silent about provenance. This is a wiring check for a developer's
# own clone, not a tamper check: anyone who can plant that symlink can already
# edit .githooks/pre-push directly, so a content check here would buy nothing
# against the same actor. CI is what actually gates what lands.
#
# UNSET and SET-BUT-EMPTY are reported distinctly. Both fail, and both take the
# same remedy, but conflating them prints a message that contradicts the config
# file a reader is looking at: `--get` exits 1 when the key is absent and 0 when
# it holds an empty string, so the exit STATUS is what separates them, not the
# value. Telling someone a key they can see is "<unset>" sends them looking for
# the wrong problem.
configured_path="$(git config --type=path --get core.hooksPath 2>/dev/null)" || configured_path="__CHH_UNSET__"
if [ "$configured_path" = "__CHH_UNSET__" ]; then
  echo "FAIL: core.hooksPath is <unset>; expected the repo's '$want'." >&2
  echo "      Run: make hooks" >&2
  exit 1
fi
if [ -z "$configured_path" ]; then
  echo "FAIL: core.hooksPath is set but EMPTY; expected the repo's '$want'." >&2
  echo "      Run: make hooks" >&2
  exit 1
fi

hooks_dir="$(git rev-parse --git-path hooks 2>/dev/null || true)"

# Resolution is `cd` + `pwd -P` rather than realpath(1), which is absent on a
# stock macOS. `|| true` keeps an unreadable directory from aborting the script
# with a raw bash `cd: Permission denied` under `set -e`, which would bypass the
# FAIL/remediation contract every other branch honors.
resolve_dir() {
  [ -d "$1" ] || return 0
  (cd "$1" 2>/dev/null && pwd -P) || true
}

hooks_resolved="$(resolve_dir "$hooks_dir")"
if [ -z "$hooks_resolved" ]; then
  echo "FAIL: git's hooks path '$hooks_dir' is not a readable directory." >&2
  echo "      Run: make hooks" >&2
  exit 1
fi

# The tracked directory is valid at EITHER root: a worktree checks out its own
# copy, while git keeps using the primary clone's. Both are the same tracked
# content, so both are accepted -- that is the C1 fix, and it is why this
# compares against a SET rather than a single expected path.
#
# THE SECOND ROOT IS ADDED ONLY FOR AN ACTUAL LINKED WORKTREE, gated on --git-dir
# differing from --git-common-dir. Deriving it unconditionally was wrong in a
# SUBMODULE, where the common dir is <super>/.git/modules/<name> and the parent
# is <super>/.git/modules -- a directory INSIDE .git. Measured: planting
# executable hooks at <super>/.git/modules/.githooks produced a false OK. That is
# unreachable in this repo (no .gitmodules) but it is a wrong answer either way,
# and a checker that can be satisfied by a path inside .git is not checking much.
primary_root=""
if [ "$(git rev-parse --git-dir)" != "$(git rev-parse --git-common-dir)" ]; then
  primary_root="$(resolve_dir "$(git rev-parse --git-common-dir)/..")"
fi

accepted=""
for root in "$REPO_ROOT" "$primary_root"; do
  [ -n "$root" ] || continue
  candidate="$(resolve_dir "$root/$want")"
  if [ -n "$candidate" ] && [ "$candidate" = "$hooks_resolved" ]; then
    accepted="$candidate"
    break
  fi
done

if [ -z "$accepted" ]; then
  # Prints the RESOLVED paths, not just the configured string: the worktree
  # failure above was invisible precisely because the message never showed what
  # the expected path had resolved to.
  # primary_root is empty outside a linked worktree, so the message names one
  # root there rather than printing the same path twice, which reads like a bug
  # in the checker rather than a fact about the config.
  checked="$REPO_ROOT"
  if [ -n "$primary_root" ] && [ "$primary_root" != "$REPO_ROOT" ]; then
    checked="$REPO_ROOT and $primary_root"
  fi
  echo "FAIL: git runs hooks from '$hooks_resolved'," >&2
  echo "      which is not this repo's '$want' (checked $checked)." >&2
  echo "      Run: make hooks" >&2
  exit 1
fi

for hook in pre-commit pre-push; do
  if [ ! -x "$accepted/$hook" ]; then
    echo "FAIL: $accepted/$hook is missing or not executable." >&2
    exit 1
  fi
done

echo "OK: git hooks wired to $want (pre-commit + pre-push)."
