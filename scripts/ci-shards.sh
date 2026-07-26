#!/usr/bin/env bash
# ci-shards.sh -- single source of truth for the CI test-shard split (issue #662).
#
# The `Test Shard` matrix in .github/workflows/ci.yml splits `go test ./...`
# across parallel runners. This script owns BOTH halves of that split so they
# cannot drift apart:
#
#   * the shard NAME list consumed by the workflow matrix, and
#   * the package list (plus optional -run bucket regex) each shard resolves to.
#
# Keeping them in one file matters because the failure mode is silent: if a
# named shard existed in the package map but not in the matrix, its packages
# would be excluded from `rest` and run by NOBODY. Nothing would fail; those
# packages would simply stop being tested. The `verify` subcommand below turns
# that into a hard error.
#
# Usage:
#   ci-shards.sh matrix            Print the matrix JSON: {"shard":[...]}
#   ci-shards.sh names             Print shard names, one per line
#   ci-shards.sh packages <shard>  Print the shard's space-separated package list
#   ci-shards.sh run <shard>       Print the shard's -run regex (empty if none)
#   ci-shards.sh verify            Assert every package lands in exactly one shard
#
# Exit codes: 0 ok, 1 verification failure, 2 usage/config error.
#
# Requires Bash 4+ (`declare -A`). CI runs ubuntu-latest (Bash 5); locally on
# macOS use Homebrew bash, which `env bash` picks up ahead of /bin/bash 3.2.

set -euo pipefail

if [ -z "${BASH_VERSINFO:-}" ] || [ "${BASH_VERSINFO[0]}" -lt 4 ]; then
  echo "ci-shards: needs Bash 4+ (found ${BASH_VERSION:-unknown})." >&2
  echo "  macOS system bash is 3.2; install a newer one (brew install bash)." >&2
  exit 2
fi

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# ---------------------------------------------------------------------------
# The shard map.
#
# Balanced against MEASURED CI per-package times from the `Test (go)` job log of
# run 30222557570 on main: 936.6s summed across 56 packages, which the unsharded
# job ran in a 315s step (~3x in-runner parallelism). Job wall time after
# sharding is max(shard) + runner overhead, so the goal is a flat maximum, not a
# large shard count.
#
#   internal/commands  252.2s -> PARTITIONED 3 ways (~84s each)
#   internal/queue     178.5s -> PARTITIONED 2 ways (~89s each)
#   internal/web       111.5s -> its own shard; the projected pole
#   auth               webauth 59.5 + auth 16.4 + secrets 12.7 + server 4.8 = 93.4s
#   sqlite             instrumentalrecalib 35.6 + purgeprovenance 33.9 + prune 25.6 = 95.1s
#   scanpath           cache 24.2 + scan 23.9 + db 21.2 + reports 21.2 + identityrepair 17.9 = 108.4s
#   rest               dynamic remainder, ~98s (detectorbackfill 16.5, library 12.2,
#                      audiometa 10.0, cmd/mxlrcgo-svc 9.1, audiodur 8.1, scanfail 5.7,
#                      detector 2.1, plus a ~35-package sub-2s tail)
#
# internal/commands and internal/queue are PARTITIONED rather than merely given
# their own shards because each alone exceeded the target pole: a shard cannot
# be faster than its slowest single package, so 252s and 178s had to be split by
# test name to come down at all.
#
# internal/web is NOT partitioned. It could be, but one test (TestSameOriginGuard,
# 27.8s) is a quarter of the package, so a name round-robin would leave a ~70s
# bucket next to a ~40s one -- little gain for an extra runner. Revisit only if
# web becomes the pole by a wide margin.
# ---------------------------------------------------------------------------
declare -A SHARDS=(
  [commands]="internal/commands"
  [queue]="internal/queue"
  [web]="internal/web"
  [auth]="internal/webauth internal/auth internal/secrets internal/server"
  [sqlite]="internal/instrumentalrecalib internal/purgeprovenance internal/prune"
  [scanpath]="internal/cache internal/scan internal/db internal/reports internal/identityrepair"
)

# Bucket count for each PARTITIONED base. A base listed here is split into
# "<base>-1".."<base>-N" by a round-robin over its sorted test names; a base
# absent from this map runs as a single named shard.
declare -A BUCKETS=(
  [commands]=3
  [queue]=2
)

# ---------------------------------------------------------------------------
# Derive the full shard-name list from the two maps above, so the matrix can
# never disagree with the package map.
# ---------------------------------------------------------------------------
shard_names() {
  local k n i
  # Sorted for a stable, reviewable matrix ordering.
  for k in $(printf '%s\n' "${!SHARDS[@]}" | sort); do
    n="${BUCKETS[$k]:-1}"
    if [ "$n" -eq 1 ]; then
      echo "$k"
    else
      for ((i = 1; i <= n; i++)); do
        echo "${k}-${i}"
      done
    fi
  done
  # The dynamic remainder always comes last.
  echo "rest"
}

matrix_json() {
  local first=1 out='{"shard":['
  while IFS= read -r s; do
    [ -z "$s" ] && continue
    if [ "$first" -eq 1 ]; then first=0; else out+=','; fi
    out+="\"${s}\""
  done < <(shard_names)
  out+=']}'
  printf '%s\n' "$out"
}

# Packages belonging to any NAMED shard, one per line (relative paths).
named_packages() {
  local k p
  for k in "${!SHARDS[@]}"; do
    for p in ${SHARDS[$k]}; do
      echo "$p"
    done
  done
}

# The `rest` shard: everything in `go list ./...` not claimed by a named shard.
# Derived dynamically so a NEWLY ADDED package automatically lands in a shard
# and can never be silently untested.
rest_packages() {
  local module excludes p
  module=$(go list -m)
  excludes=""
  while IFS= read -r p; do
    excludes="${excludes:+${excludes}|}${p}"
  done < <(named_packages)
  go list ./... | grep -v -E "^${module}/(${excludes})(/|$)"
}

# Test names for a partition bucket. Names are grepped out of *_test.go, so no
# compile is needed. Round-robin over SORTED names keeps the buckets balanced
# regardless of name clustering; an alphabetic split would be lopsided because
# most names in a package share a prefix. A grepped name that is not a real test
# is a harmless no-op in -run.
bucket_run_regex() {
  local base="$1" idx="$2" buckets="$3" names sel="" i=0 t
  # shellcheck disable=SC2086 # reason: SHARDS values are space-separated
  # directory lists that grep must receive as separate arguments.
  names=$(grep -rhoE '^func (Test|Example|Fuzz)[A-Za-z0-9_]+' \
            --include='*_test.go' ${SHARDS[$base]} \
          | sed -E 's/^func //' | grep -vx 'TestMain' | sort -u)
  while IFS= read -r t; do
    [ -z "$t" ] && continue
    if [ $((i % buckets)) -eq "$idx" ]; then
      sel="${sel:+${sel}|}${t}"
    fi
    i=$((i + 1))
  done < <(printf '%s\n' "$names")
  if [ -z "$sel" ]; then
    echo "ci-shards: empty partition for ${base}-$((idx + 1))" >&2
    return 1
  fi
  printf '^(%s)$\n' "$sel"
}

# Resolve a shard name to "<packages>" (space separated, ./pkg/... form).
shard_packages() {
  local shard="$1" base p pkgs=""
  case "$shard" in
    rest)
      # rest_packages emits full import paths, which `go test` accepts directly.
      while IFS= read -r p; do
        pkgs+="${p} "
      done < <(rest_packages)
      ;;
    *-[0-9])
      base="${shard%-*}"
      if [ -z "${SHARDS[$base]+x}" ]; then
        echo "ci-shards: unknown partition base: $base" >&2
        return 2
      fi
      for p in ${SHARDS[$base]}; do
        pkgs+="./${p}/... "
      done
      ;;
    *)
      if [ -z "${SHARDS[$shard]+x}" ]; then
        echo "ci-shards: unknown shard: $shard" >&2
        return 2
      fi
      for p in ${SHARDS[$shard]}; do
        pkgs+="./${p}/... "
      done
      ;;
  esac
  if [ -z "${pkgs// /}" ]; then
    echo "ci-shards: empty package list for shard $shard" >&2
    return 1
  fi
  printf '%s\n' "$pkgs"
}

shard_run() {
  local shard="$1" base idx buckets
  case "$shard" in
    *-[0-9])
      base="${shard%-*}"
      idx=$((${shard##*-} - 1))
      buckets="${BUCKETS[$base]:-0}"
      if [ "$buckets" -lt 2 ]; then
        echo "ci-shards: $shard looks partitioned but base $base has no bucket count" >&2
        return 2
      fi
      if [ "$idx" -ge "$buckets" ]; then
        echo "ci-shards: $shard index exceeds bucket count $buckets for $base" >&2
        return 2
      fi
      bucket_run_regex "$base" "$idx" "$buckets"
      ;;
    *)
      printf '\n'
      ;;
  esac
}

# ---------------------------------------------------------------------------
# verify: the guard that makes the silent failure loud.
#
# Checks, in order:
#   1. Every package in `go list ./...` is claimed by exactly one shard.
#   2. Every named shard's directories actually exist (a rename would otherwise
#      quietly empty a shard while `rest` silently absorbed its packages).
#   3. Every partitioned base's buckets are non-empty AND their union is the
#      package's complete test-name set, with no test in two buckets.
# ---------------------------------------------------------------------------
verify() {
  local rc=0 module p count all_named
  module=$(go list -m)

  # (2) named directories exist
  while IFS= read -r p; do
    if [ ! -d "$p" ]; then
      echo "FAIL: shard directory does not exist: $p" >&2
      rc=1
    fi
  done < <(named_packages)

  # (1) exactly-once coverage of go list ./...
  all_named=$(named_packages)
  while IFS= read -r pkg; do
    count=0
    while IFS= read -r p; do
      case "$pkg" in
        "${module}/${p}" | "${module}/${p}/"*) count=$((count + 1)) ;;
      esac
    done < <(printf '%s\n' "$all_named")
    # rest claims anything no named shard claimed
    if [ "$count" -eq 0 ]; then count=1; fi
    if [ "$count" -ne 1 ]; then
      echo "FAIL: package claimed by $count shards: $pkg" >&2
      rc=1
    fi
  done < <(go list ./...)

  # (3) partition buckets are complete and disjoint
  local base buckets i total_names union_names dup
  for base in "${!BUCKETS[@]}"; do
    buckets="${BUCKETS[$base]}"
    # shellcheck disable=SC2086 # reason: SHARDS values are space-separated
    # directory lists that grep must receive as separate arguments.
    total_names=$(grep -rhoE '^func (Test|Example|Fuzz)[A-Za-z0-9_]+' \
                    --include='*_test.go' ${SHARDS[$base]} \
                  | sed -E 's/^func //' | grep -vx 'TestMain' | sort -u)
    union_names=""
    for ((i = 0; i < buckets; i++)); do
      local re
      if ! re=$(bucket_run_regex "$base" "$i" "$buckets"); then
        echo "FAIL: bucket ${base}-$((i + 1)) is empty" >&2
        rc=1
        continue
      fi
      # Strip ^( )$ and split on |
      re="${re#^(}"
      re="${re%)$}"
      union_names+=$(printf '%s\n' "${re//|/$'\n'}")
      union_names+=$'\n'
    done
    # `|| true`: under `set -e`, grep -v exits 1 on empty input -- which happens
    # exactly when a bucket regex failed above and `continue`d. Without this the
    # assignment aborts verify before the remaining bases are checked and before
    # the accumulated rc is reported, so the guard stays silent in the one case
    # it exists to catch.
    dup=$(printf '%s' "$union_names" | grep -v '^$' | sort | uniq -d || true)
    if [ -n "$dup" ]; then
      echo "FAIL: test names in more than one ${base} bucket:" >&2
      printf '%s\n' "$dup" >&2
      rc=1
    fi
    if ! diff <(printf '%s' "$union_names" | grep -v '^$' | sort -u) \
              <(printf '%s\n' "$total_names" | sort -u) >/dev/null; then
      echo "FAIL: ${base} buckets do not cover exactly the package's tests" >&2
      diff <(printf '%s' "$union_names" | grep -v '^$' | sort -u) \
           <(printf '%s\n' "$total_names" | sort -u) >&2 || true
      rc=1
    fi
  done

  if [ "$rc" -eq 0 ]; then
    echo "ci-shards: OK -- $(go list ./... | wc -l | tr -d ' ') packages across $(shard_names | wc -l | tr -d ' ') shards, each in exactly one."
  fi
  return "$rc"
}

case "${1:-}" in
  matrix) matrix_json ;;
  names) shard_names ;;
  packages)
    [ $# -ge 2 ] || { echo "ci-shards: packages needs a shard name" >&2; exit 2; }
    shard_packages "$2"
    ;;
  run)
    [ $# -ge 2 ] || { echo "ci-shards: run needs a shard name" >&2; exit 2; }
    shard_run "$2"
    ;;
  verify) verify ;;
  *)
    echo "Usage: $0 {matrix|names|packages <shard>|run <shard>|verify}" >&2
    exit 2
    ;;
esac
