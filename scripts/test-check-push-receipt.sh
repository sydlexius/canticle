#!/usr/bin/env bash
# test-check-push-receipt.sh -- hermetic tests for check-push-receipt.sh. Builds a
# throwaway repo, crafts receipts and pre-push stdin lines, and asserts the exit
# code of each case. Sub-second; no network, no Go toolchain.
set -uo pipefail

CHK="$(cd "$(dirname "$0")" && pwd)/check-push-receipt.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
cd "$T" || exit 2
git init -q -b main . && git config user.email t@example.invalid && git config user.name t
echo a > f && git add f && git commit -qm one
C1=$(git rev-parse HEAD)
echo b > f && git commit -qam two
C2=$(git rev-parse HEAD)
T2=$(git rev-parse 'HEAD^{tree}')
git commit -q --amend -m reworded
C2R=$(git rev-parse HEAD)
Z=0000000000000000000000000000000000000000
R="$(git rev-parse --git-dir)/prep-pr-receipt.json"

mk() { # mk <result> <tree_sha> [producer] [schema]
  printf '{"schema":"%s","producer":"%s","result":"%s","tree_sha":"%s","commit_sha":"%s","worktree":"x","steps":[]}\n' \
    "${4:-gate-receipt/v1}" "${3:-gate-runner}" "$1" "$2" "$C2" > "$R"
}
L() { echo "refs/heads/b $1 refs/heads/b $Z"; }

passed=0 failed=0
t() { # t <name> <want-rc> <stdin>
  local got=0 out
  out=$(printf '%b' "$3" | bash "$CHK" 2>&1) || got=$?
  if [ "$got" = "$2" ]; then
    passed=$((passed + 1)); echo "ok   $1 -> $got | $out"
  else
    failed=$((failed + 1)); echo "FAIL $1: want $2, got $got | $out"
  fi
}

mk pass "$T2"
t "matching receipt"           0 "$(L "$C2")\n"
t "reworded commit, same tree" 0 "$(L "$C2R")\n"
t "stale tree"                 1 "$(L "$C1")\n"
t "multi-ref, all match"       0 "$(L "$C2")\n$(L "$C2R")\n"
t "multi-ref, one stale"       1 "$(L "$C2")\n$(L "$C1")\n"
t "multi-ref, stale first"     1 "$(L "$C1")\n$(L "$C2")\n"
# A real (non-delete) sha that merely STARTS with 0 must never read as a delete.
t "0-prefixed sha, not a delete" 1 "$(L 0123456789012345678901234567890123456789)\n"
t "0-prefixed + matching"      1 "$(L 0123456789012345678901234567890123456789)\n$(L "$C2")\n"
t "delete ref only"            0 "(delete) $Z refs/heads/old $C1\n"
t "delete + matching"          0 "(delete) $Z refs/heads/old $C1\n$(L "$C2")\n"
t "delete + stale"             1 "(delete) $Z refs/heads/old $C1\n$(L "$C1")\n"
t "empty stdin"                1 ""
t "garbage stdin"              1 "refs/heads/b notasha refs/heads/b $Z\n"
t "unknown sha"                1 "$(L 1234567890123456789012345678901234567890)\n"
git tag -a -m annotated vtag "$C2"; TAGO=$(git rev-parse vtag)
t "annotated tag, same tree"   0 "refs/tags/vtag $TAGO refs/tags/vtag $Z\n"
git tag -a -m old vold "$C1"; TOLD=$(git rev-parse vold)
t "annotated tag, stale tree"  1 "refs/tags/vold $TOLD refs/tags/vold $Z\n"
mk fail "$T2";                         t "result=fail"     1 "$(L "$C2")\n"
mk pass nothex;                        t "bad tree_sha"    1 "$(L "$C2")\n"
mk pass "$T2" hand;                    t "wrong producer"  1 "$(L "$C2")\n"
mk pass "$T2" gate-runner gate-receipt/v2; t "wrong schema" 1 "$(L "$C2")\n"
echo '{not json' > "$R";               t "malformed JSON"  1 "$(L "$C2")\n"
echo '[1,2]' > "$R";                   t "JSON array"      1 "$(L "$C2")\n"
rm -f "$R";                            t "missing receipt" 1 "$(L "$C2")\n"
mk pass "$T2"
echo dirty > f;                        t "dirty, unstaged" 1 "$(L "$C2")\n"
git add f;                             t "dirty, staged"   1 "$(L "$C2")\n"
git checkout -q HEAD -- f; echo u > untracked
t "dirty, untracked" 1 "$(L "$C2")\n"
rm -f untracked;                       t "clean again"     0 "$(L "$C2")\n"
mk pass "$(echo "$T2" | tr 'a-f' 'A-F')"; t "uppercase tree_sha" 0 "$(L "$C2")\n"
mk pass "$T2"; ALT="$(dirname "$R")/alt.json"; mv "$R" "$ALT"
PREP_PR_RECEIPT="$ALT" t "PREP_PR_RECEIPT override" 0 "$(L "$C2")\n"

# .githooks/pre-push itself, against a stub gate. A second throwaway repo holds
# the hook, the real receipt checker, and a stub pre-push-gate.sh that records
# its argv, so each case asserts both the exit code and whether (and how) the
# gate ran.
HOOK="$(cd "$(dirname "$CHK")/.." && pwd)/.githooks/pre-push"
H="$T/hookrepo"
mkdir -p "$H/scripts" && cd "$H" || exit 2
git init -q -b main . && git config user.email t@example.invalid && git config user.name t
cp "$CHK" scripts/check-push-receipt.sh
printf '#!/usr/bin/env bash\nexit "${SIG_RC:-0}"\n' > scripts/check-commit-signatures.sh # stub; real one is tested separately
printf '#!/usr/bin/env bash\necho "gate:$*" >> "%s/gate.log"\nexit 0\n' "$T" > scripts/pre-push-gate.sh
git add scripts && git commit -qm hook
HC=$(git rev-parse HEAD)
R="$(git rev-parse --git-dir)/prep-pr-receipt.json"
printf '{"schema":"gate-receipt/v1","producer":"gate-runner","result":"pass","tree_sha":"%s"}\n' \
  "$(git rev-parse 'HEAD^{tree}')" > "$R"
h() { # h <name> <want-rc> <want-gate-log> [PUSH_GATE value]
  local got=0 out log
  rm -f "$T/gate.log"
  if [ "$#" -ge 4 ]; then
    out=$(printf '%s\n' "$(L "$HC")" | PUSH_GATE="$4" bash "$HOOK" 2>&1) || got=$?
  else
    out=$(printf '%s\n' "$(L "$HC")" | env -u PUSH_GATE bash "$HOOK" 2>&1) || got=$?
  fi
  log=$(cat "$T/gate.log" 2>/dev/null | tr '\n' ' ')
  if [ "$got" = "$2" ] && [ "$log" = "$3" ]; then
    passed=$((passed + 1)); echo "ok   hook: $1 -> $got gate=[$log] | $out"
  else
    failed=$((failed + 1)); echo "FAIL hook: $1: want $2 gate=[$3], got $got gate=[$log] | $out"
  fi
}
h "receipt pass skips the gate"   0 ""
# An unsigned commit must be refused BEFORE the receipt fast path (tree-keyed).
SIG_RC=1 h "unsigned commit beats a passing receipt" 1 ""
h "PUSH_GATE=full runs full gate" 0 "gate: "      full
# The override must not bypass the signature check (full gate only checks base..HEAD).
SIG_RC=1 h "unsigned commit beats PUSH_GATE=full" 1 "" full
h "PUSH_GATE=skip is rejected"    2 ""            skip
h "unknown PUSH_GATE is rejected" 2 ""            bogus
rm -f "$R"
h "no receipt runs --hook gate"   0 "gate:--hook "

# Widening: multi-ref pushes and non-HEAD refs must fail closed to --hook-all.
hs() { # hs <name> <want-gate-log> <stdin lines...>
  local name="$1" want="$2" log out got=0; shift 2
  rm -f "$T/gate.log"
  out=$(printf '%s\n' "$@" | env -u PUSH_GATE bash "$HOOK" 2>&1) || got=$?
  log=$(cat "$T/gate.log" 2>/dev/null | tr '\n' ' ')
  if [ "$got" = 0 ] && [ "$log" = "$want" ]; then
    passed=$((passed + 1)); echo "ok   hook: $name -> gate=[$log]"
  else
    failed=$((failed + 1)); echo "FAIL hook: $name: want gate=[$want], got $got gate=[$log] | $out"
  fi
}
PREV=$C1 # a valid sha that is not the hook repo HEAD
hs "single HEAD ref stays scoped"     "gate:--hook "     "$(L "$HC")"
hs "two refs widen"                   "gate:--hook-all " "$(L "$HC")" "$(L "$HC")"
hs "non-HEAD ref widens"              "gate:--hook-all " "$(L "$PREV")"
hs "delete + HEAD stays scoped"       "gate:--hook "     "$(L "$HC")" "refs/heads/x 0000000000000000000000000000000000000000 refs/heads/x $HC"

echo "passed=$passed failed=$failed"
[ "$failed" -eq 0 ]
