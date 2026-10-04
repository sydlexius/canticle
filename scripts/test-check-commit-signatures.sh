#!/usr/bin/env bash
# test-check-commit-signatures.sh -- hermetic tests for check-commit-signatures.sh.
# Builds throwaway repos signed with a throwaway SSH key; never touches the real
# repo config or keys. Unsigned commits are made with `git commit-tree` (the very
# path that bypasses commit.gpgsign in practice). Run: bash scripts/test-check-commit-signatures.sh
set -uo pipefail

CHECK="$(cd "$(dirname "$0")" && pwd)/check-commit-signatures.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
ssh-keygen -q -t ed25519 -N '' -f "$TMP/key" >/dev/null

passed=0 failed=0
ok() { passed=$((passed + 1)); echo "ok   $1"; }
bad() { failed=$((failed + 1)); echo "FAIL $1"; }
expect() { # expect <want-rc> <name> <must-match-or-empty> <dir> [args...]
  local want=$1 name=$2 pat=$3 dir=$4 got=0 out; shift 4
  out=$(cd "$dir" && bash "$CHECK" "$@" 2>&1) || got=$?
  if [ "$got" = "$want" ] && { [ -z "$pat" ] || printf '%s' "$out" | grep -q -- "$pat"; }; then
    ok "$name"
  else
    bad "$name: want rc=$want /$pat/, got rc=$got"; printf '%s\n' "$out" | sed 's/^/      /'
  fi
}

newrepo() { # newrepo <dir>: main with one signed commit, origin/main at it
  git init -q -b main "$1"
  git -C "$1" config user.name t; git -C "$1" config user.email t@example.com
  git -C "$1" config gpg.format ssh; git -C "$1" config user.signingkey "$TMP/key.pub"
  git -C "$1" config commit.gpgsign true
  git -C "$1" commit -q --allow-empty -m base
  git -C "$1" update-ref refs/remotes/origin/main HEAD
}
signed() { git -C "$1" commit -q --allow-empty -m "$2"; }
unsigned() { # unsigned <dir> <subject>: commit-tree without -S, then advance HEAD
  local c; c=$(git -C "$1" commit-tree "HEAD^{tree}" -p HEAD -m "$2") && git -C "$1" reset -q --hard "$c"
}

newrepo "$TMP/ok"; signed "$TMP/ok" s1; signed "$TMP/ok" s2
expect 0 "all signed passes" "PASS" "$TMP/ok"

newrepo "$TMP/one"; signed "$TMP/one" s1; unsigned "$TMP/one" naked-top
expect 1 "unsigned tip fails and is named" "UNSIGNED: .* naked-top" "$TMP/one"

newrepo "$TMP/mid"; signed "$TMP/mid" s1; unsigned "$TMP/mid" naked-mid; signed "$TMP/mid" s2
expect 1 "unsigned commit mid-range fails and is named" "UNSIGNED: .* naked-mid" "$TMP/mid"

newrepo "$TMP/msg"; unsigned "$TMP/msg" "$(printf 'x\n\ngpgsig fake')"
expect 1 "gpgsig text in the message does not count" "UNSIGNED" "$TMP/msg"

# No origin/main and no remote-tracking refs: the whole of HEAD is checked (fail
# closed), never local main as a boundary.
newrepo "$TMP/nobase"; git -C "$TMP/nobase" update-ref -d refs/remotes/origin/main; git -C "$TMP/nobase" branch -m work
expect 0 "no remote refs: all-signed history passes" "PASS" "$TMP/nobase"
unsigned "$TMP/nobase" naked-nobase
expect 1 "no remote refs: unsigned commit is checked and named" "UNSIGNED: .* naked-nobase" "$TMP/nobase"

# Large signed commit message: must not read as unsigned (SIGPIPE under pipefail).
newrepo "$TMP/big"; git -C "$TMP/big" commit -q --allow-empty -m bigmsg -m "$(head -c 400000 /dev/zero | tr '\0' 'x' | fold -w 70)"
expect 0 "signed commit with a large message passes" "PASS" "$TMP/big"

newrepo "$TMP/empty"
expect 0 "empty range passes" "PASS" "$TMP/empty"

expect 1 "explicit range is honored" "naked-mid" "$TMP/mid" "origin/main..HEAD"
expect 2 "bad explicit range exits 2" "bad range" "$TMP/mid" "nope..HEAD"

# --refs: what the pre-push hook passes. The remote sha bounds an existing branch.
cd "$TMP/mid" || exit 1
tip=$(git rev-parse HEAD); first=$(git rev-parse HEAD~2); zero=0000000000000000000000000000000000000000
refs_rc() { local rc=0; printf '%s\n' "$1" | bash "$CHECK" --refs >/dev/null 2>&1 || rc=$?; echo "$rc"; }
check_refs() { # check_refs <want> <name> <ref line>
  local rc; rc=$(refs_rc "$3"); if [ "$rc" = "$1" ]; then ok "$2"; else bad "$2: want $1, got $rc"; fi
}
check_refs 1 "--refs new branch checks vs merge base" "refs/heads/w $tip refs/heads/w $zero"
check_refs 0 "--refs up-to-date ref sends nothing" "refs/heads/w $tip refs/heads/w $tip"
check_refs 0 "--refs delete is skipped" "refs/heads/w $zero refs/heads/w $first"
check_refs 1 "--refs existing branch checks remote..local" "refs/heads/w $tip refs/heads/w $first"

# New remote ref whose tip IS local main, with no origin/main: local main is no
# evidence the remote has it, so the unsigned commit must be caught.
newrepo "$TMP/newmain"; git -C "$TMP/newmain" update-ref -d refs/remotes/origin/main; unsigned "$TMP/newmain" naked-newmain
nm=$(git -C "$TMP/newmain" rev-parse HEAD)
cd "$TMP/newmain" || exit 1
out=$(printf '%s\n' "refs/heads/main $nm refs/heads/main $zero" | bash "$CHECK" --refs 2>&1); rc=$?
if [ "$rc" = 1 ] && printf '%s' "$out" | grep -q "UNSIGNED: .* naked-newmain"; then ok "--refs new ref at local main, no origin/main, fails and is named"; else bad "--refs new ref at local main: rc=$rc"; fi

echo "passed=$passed failed=$failed"
[ "$failed" -eq 0 ]
