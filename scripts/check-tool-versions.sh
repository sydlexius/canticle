#!/usr/bin/env bash
# check-tool-versions.sh -- assert tool version pins agree across CI and the
# pre-commit config so local and CI checks cannot silently diverge. Run via
# `make sync-tool-versions` (and `make doctor`). It catches CI-vs-pre-commit pin
# drift, e.g. a golangci-lint version bumped in one file but not the other.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

status=0

# golangci-lint: the version: input on the CI action vs the pre-commit rev.
ci_glci="$(grep -oE 'version: v[0-9]+\.[0-9]+\.[0-9]+' .github/workflows/ci.yml | head -1 | awk '{print $2}')"
pc_glci="$(awk '/golangci\/golangci-lint/{f=1} f&&/rev:/{print $2; exit}' .pre-commit-config.yaml)"
if [ -z "$ci_glci" ] || [ -z "$pc_glci" ]; then
  echo "FAIL: could not parse golangci-lint version (ci='$ci_glci' pre-commit='$pc_glci')." >&2
  status=1
elif [ "$ci_glci" != "$pc_glci" ]; then
  echo "FAIL: golangci-lint pin drift: ci.yml=$ci_glci vs .pre-commit-config.yaml=$pc_glci." >&2
  echo "      Align both pins (and the local install) to the same version." >&2
  status=1
else
  echo "OK: golangci-lint pinned to $ci_glci in ci.yml and .pre-commit-config.yaml."
fi

exit $status
