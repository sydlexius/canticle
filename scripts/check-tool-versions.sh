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
# `|| true` keeps `set -euo pipefail` from aborting the script when a pattern or
# file is absent; the `-z` checks below report the parse failure with context.
ci_glci="$(grep -oE 'version: v[0-9]+\.[0-9]+\.[0-9]+' .github/workflows/ci.yml 2>/dev/null | head -1 | awk '{print $2}' || true)"
pc_glci="$(awk '/golangci\/golangci-lint/{f=1} f&&/rev:/{print $2; exit}' .pre-commit-config.yaml 2>/dev/null || true)"
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

# grype: the grype-version input in the CI scan job vs the locally installed binary.
# Graceful skip if grype is not installed; it is optional for non-scan workflows.
if ! command -v grype >/dev/null 2>&1; then
  echo "NOTE: grype not found in PATH; skipping grype version check."
else
  expected_grype="$(grep -oE 'grype-version: v[0-9]+\.[0-9]+\.[0-9]+' .github/workflows/ci.yml 2>/dev/null | head -1 | grep -oE 'v[0-9]+\.[0-9]+\.[0-9]+' || true)"
  if [ -z "$expected_grype" ]; then
    echo "FAIL: could not parse grype-version from .github/workflows/ci.yml." >&2
    status=1
  else
    local_grype="$(grype version 2>/dev/null | grep '^Version:' | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -1 | sed 's/^/v/' || true)"
    if [ -z "$local_grype" ]; then
      echo "FAIL: could not parse local grype version." >&2
      status=1
    elif [ "$local_grype" != "$expected_grype" ]; then
      echo "FAIL: grype pin drift: ci.yml=$expected_grype vs local=$local_grype." >&2
      echo "      Install grype $expected_grype to match the CI pin." >&2
      status=1
    else
      echo "OK: grype pinned to $expected_grype in ci.yml and matches local install."
    fi
  fi
fi

exit $status
