#!/usr/bin/env bash
# test-check-pr-trigger-scope.sh -- mutation tests for check-pr-trigger-scope.sh.
# Builds throwaway workflow dirs, mutates one file, asserts the exit code.
# Hermetic and sub-second. Run: bash scripts/test-check-pr-trigger-scope.sh
set -uo pipefail

GUARD="$(cd "$(dirname "$0")" && pwd)/check-pr-trigger-scope.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

passed=0 failed=0
expect() { # expect <want-rc> <name> <dir>
  local got=0 out
  out=$(bash "$GUARD" "$3" 2>&1) || got=$?
  if [ "$got" = "$1" ]; then
    passed=$((passed + 1)); echo "ok   $2"
  else
    failed=$((failed + 1)); echo "FAIL $2: want $1, got $got"; printf '%s\n' "$out" | sed 's/^/      /'
  fi
}

MUST="ci codeql"
PUSH='  push:\n    branches: [main]\n'
fixture() { # compliant tree: push restricted, PR unrestricted
  local w; mkdir -p "$1"
  for w in $MUST; do
    printf 'name: x\non:\n%b  pull_request:\n    types: [opened]\n' "$PUSH" > "$1/$w.yml"
  done
}
put() { printf 'name: x\non:\n%b' "$3" > "$1/$2.yml"; } # put <dir> <name> <on-body>

fixture "$TMP/clean"
expect 0 "compliant fixture passes" "$TMP/clean"

for w in $MUST; do
  fixture "$TMP/b-$w"; put "$TMP/b-$w" "$w" "  pull_request:\n    branches: [main]\n"
  expect 1 "$w: block-form branches filter fails" "$TMP/b-$w"
  fixture "$TMP/l-$w"; put "$TMP/l-$w" "$w" "  pull_request:\n    branches:\n      - main\n"
  expect 1 "$w: block-list branches filter fails" "$TMP/l-$w"
  fixture "$TMP/f-$w"; put "$TMP/f-$w" "$w" "  pull_request: {branches: [main]}\n"
  expect 1 "$w: flow-map branches filter fails" "$TMP/f-$w"
  fixture "$TMP/d-$w"; put "$TMP/d-$w" "$w" "$PUSH"
  expect 1 "$w: dropped pull_request trigger fails" "$TMP/d-$w"
done

fixture "$TMP/ign"; put "$TMP/ign" ci "  pull_request:\n    branches-ignore: [wip]\n"
expect 1 "branches-ignore fails" "$TMP/ign"

fixture "$TMP/new"; put "$TMP/new" brand-new "  pull_request:\n    branches: [main]\n"
expect 1 "unlisted new workflow with a filter fails (default-deny)" "$TMP/new"
fixture "$TMP/tgt"; put "$TMP/tgt" pr-labels "  pull_request_target:\n    types: [opened]\n    branches: [main]\n"
expect 1 "pull_request_target with a filter fails" "$TMP/tgt"
fixture "$TMP/tgt-ok"; put "$TMP/tgt-ok" pr-labels "  pull_request_target:\n    types: [opened]\n"
expect 0 "pull_request_target without a filter passes" "$TMP/tgt-ok"

for w in dependabot-auto-approve pages; do
  fixture "$TMP/ex-$w"; put "$TMP/ex-$w" "$w" "  pull_request:\n    branches: [main]\n"
  expect 0 "EXEMPT $w may filter" "$TMP/ex-$w"
done

fixture "$TMP/yaml-bad"
printf 'name: x\non:\n  pull_request:\n    branches: [main]\n' > "$TMP/yaml-bad/extra.yaml"
expect 1 ".yaml workflow with a filter fails" "$TMP/yaml-bad"
fixture "$TMP/yaml-ok"
printf 'name: x\non:\n  pull_request:\n    types: [opened]\n' > "$TMP/yaml-ok/extra.yaml"
expect 0 ".yaml workflow without a filter passes" "$TMP/yaml-ok"

fixture "$TMP/ok"; put "$TMP/ok" ci "$PUSH  pull_request:\n    # branches: [main] was removed\n    types: [opened]\n"
expect 0 "push filter and comments are ignored" "$TMP/ok"

fixture "$TMP/jobs"
printf 'name: x\non:\n  pull_request:\njobs:\n  a:\n    strategy:\n      branches: [main]\n' > "$TMP/jobs/ci.yml"
expect 0 "a branches key outside on: is ignored" "$TMP/jobs"

for w in ci codeql; do
  fixture "$TMP/tgtonly-$w"; put "$TMP/tgtonly-$w" "$w" "$PUSH  pull_request_target:\n"
  expect 1 "$w with only pull_request_target fails (not the required event)" "$TMP/tgtonly-$w"
done

fixture "$TMP/inline"
printf 'name: x\non: {pull_request: {branches: [main]}}\n' > "$TMP/inline/extra.yml"
expect 1 "inline on: mapping with a branch filter fails" "$TMP/inline"
fixture "$TMP/inline-ok"
printf 'name: x\non: {pull_request: {types: [opened]}}\n' > "$TMP/inline-ok/extra.yml"
expect 0 "inline on: mapping without a filter passes" "$TMP/inline-ok"

fixture "$TMP/missing"; rm "$TMP/missing/codeql.yml"
expect 2 "missing MUST_TRIGGER workflow is a setup error" "$TMP/missing"

expect 0 "live workflows comply" "$(cd "$(dirname "$GUARD")/.." && pwd)/.github/workflows"

echo "=== $passed passed, $failed failed ==="
[ "$failed" -eq 0 ]
