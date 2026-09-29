#!/usr/bin/env bash
# test-hook-test-pkgs.sh -- hermetic tests for hook-test-pkgs.sh, the changed-path
# -> package mapping behind `pre-push-gate.sh --hook`. Builds a throwaway tree
# shaped like this repo (no git, no Go toolchain) and asserts the package list
# each changed-path set produces. The invariant under test: a Go-relevant change
# never maps to NOTHING.
set -uo pipefail

MAP="$(cd "$(dirname "$0")" && pwd)/hook-test-pkgs.sh"
T="$(mktemp -d)"
trap 'rm -rf "$T"' EXIT
cd "$T" || exit 2
mkdir -p internal/a/testdata/golden internal/b internal/db/migrations web/templates web/static/css cmd/tool
touch internal/a/a.go internal/b/b.go internal/db/db.go web/static/embed.go cmd/tool/main.go

passed=0 failed=0
t() { # t <name> <want (space-joined)> <changed paths, \n-separated>
  local got
  got=$(printf '%b' "$3" | bash "$MAP" | tr '\n' ' ' | sed 's/ $//')
  if [ "$got" = "$2" ]; then
    passed=$((passed + 1)); echo "ok   $1 -> [$got]"
  else
    failed=$((failed + 1)); echo "FAIL $1: want [$2], got [$got]"
  fi
}

t "no change"                  ""                        ""
t "docs/scripts only"          ""                        "docs/x.md\nscripts/y.sh\n.github/workflows/ci.yml\n"
t "one .go file"               "./internal/a"            "internal/a/a.go\n"
t "two packages, deduped"      "./internal/a ./internal/b" "internal/b/b.go\ninternal/a/a.go\ninternal/a/a_test.go\n"
t "testdata maps to parent"    "./internal/a"            "internal/a/testdata/golden/x.json\n"
t "deleted .go, pkg survives"  "./internal/b"            "internal/b/gone.go\n"
t "last .go of pkg deleted"    "./..."                   "internal/c/c.go\n"
t "rename source + dest"       "./..."                   "internal/c/moved.go\ninternal/a/moved.go\n"
t ".templ edit"                "./..."                   "web/templates/page.templ\n"
t "migration edit"             "./..."                   "internal/db/migrations/0001_x.sql\n"
t "embedded static asset"      "./..."                   "web/static/css/input.css\n"
t "non-.go under cmd/"         "./..."                   "cmd/tool/README\n"
t "go.mod"                     "./..."                   "go.mod\n"
t "go.sum beside a .go"        "./..."                   "internal/a/a.go\ngo.sum\n"

echo "passed=$passed failed=$failed"
[ "$failed" -eq 0 ]
