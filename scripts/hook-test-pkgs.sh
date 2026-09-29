#!/usr/bin/env bash
# hook-test-pkgs.sh -- map changed paths to the Go packages `pre-push-gate.sh
# --hook` must test. Reads repo-relative paths on STDIN (one per line, as
# `git diff --name-only --no-renames` prints them) and writes `go test`
# patterns on STDOUT, one per line. Run from the repo root. Empty output means
# no Go-relevant path changed.
#
# FAIL CLOSED: anything that can change Go behavior without naming a package
# directly widens to `./...` rather than to nothing:
#   - go.mod / go.sum
#   - any non-.go path under internal/, web/ or cmd/ (.templ sources, whose
#     *_templ.go output is gitignored; go:embed'd migrations and web/static
#     assets; any other embedded or read-at-test-time file)
#   - a .go path (or a testdata/ path) whose package directory no longer holds a
#     .go file, e.g. the last file of a package deleted or moved away
# A testdata/ path maps to the package that owns the testdata directory. A
# deleted .go file maps to its directory, so the survivors are still tested.
set -uo pipefail

has_go() { compgen -G "$1/*.go" >/dev/null; }

pkgs=()
wide=0
while IFS= read -r p; do
  [ -n "$p" ] || continue
  case "$p" in
    go.mod | go.sum) wide=1; break ;;
  esac
  dir=""
  if [[ "/$p" == */testdata/* ]]; then
    pre="/$p"
    pre="${pre%%/testdata/*}"
    dir="${pre#/}"
    [ -n "$dir" ] || dir="."
  elif [[ "$p" == *.go ]]; then
    dir="$(dirname "$p")"
  else
    case "$p" in
      internal/* | web/* | cmd/*) wide=1; break ;;
    esac
    continue # docs, scripts, workflows, top-level config: not a Go input
  fi
  if ! has_go "$dir"; then
    wide=1
    break
  fi
  pkgs+=("./$dir")
done

if [ "$wide" -eq 1 ]; then
  echo "./..."
elif [ "${#pkgs[@]}" -gt 0 ]; then
  printf '%s\n' "${pkgs[@]}" | LC_ALL=C sort -u
fi
