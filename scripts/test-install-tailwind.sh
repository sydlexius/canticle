#!/usr/bin/env bash
# test-install-tailwind.sh -- hermetic tests for install-tailwind.sh.
# Stubs curl (no network), asserts the retry window and the fail-fast paths.
# Run: bash scripts/test-install-tailwind.sh
set -uo pipefail

SCRIPT="$(cd "$(dirname "$0")" && pwd)/install-tailwind.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

# Stub curl: logs argv, answers `--help all`, serves a fixed binary and a
# sha256sums.txt whose hash is $STUB_SUM. STUB_FAIL=<rc> makes every fetch fail.
cat > "$TMP/bin/curl" <<'STUB'
#!/usr/bin/env bash
if [ "$1" = "--help" ]; then echo "     --retry-all-errors"; exit 0; fi
printf '%s\n' "$*" >> "$STUB_LOG"
[ -n "${STUB_FAIL:-}" ] && exit "$STUB_FAIL"
out=""
while [ $# -gt 0 ]; do [ "$1" = "-o" ] && out="$2"; shift; done
if [ -n "$out" ]; then printf 'payload' > "$out"; else echo "$STUB_SUM  ./tailwindcss-linux-x64"; fi
STUB
chmod +x "$TMP/bin/curl"

GOOD="$(printf 'payload' | { sha256sum 2>/dev/null || shasum -a 256; } | awk '{print $1}')"
passed=0 failed=0
check() { # check <name> <condition-rc>
  if [ "$2" = 0 ]; then passed=$((passed + 1)); echo "ok   $1"
  else failed=$((failed + 1)); echo "FAIL $1"; fi
}
run() { # run <sum> [env...]; sets rc, log
  : > "$TMP/log"; rm -f "$TMP/dest"
  env PATH="$TMP/bin:$PATH" STUB_LOG="$TMP/log" STUB_SUM="$1" TAILWIND_ASSET=tailwindcss-linux-x64 \
    "${@:2}" bash "$SCRIPT" "$TMP/dest" >/dev/null 2>&1
  rc=$?
}

run "$GOOD"
check "success installs the binary" "$([ "$rc" = 0 ] && [ -x "$TMP/dest" ]; echo $?)"
check "retry window is at least 5 attempts" "$(grep -q -- '--retry 5' "$TMP/log"; echo $?)"
check "no fixed retry delay (exponential backoff)" "$(! grep -q -- '--retry-delay' "$TMP/log"; echo $?)"
check "total retry time is bounded at 90s on both downloads" "$([ "$(grep -c -- '--retry-max-time 90' "$TMP/log")" = 2 ]; echo $?)"
check "both downloads use the retry flags" "$([ "$(grep -c -- '--retry 5' "$TMP/log")" = 2 ]; echo $?)"

run "deadbeef"
check "checksum mismatch fails fast" "$([ "$rc" != 0 ] && [ ! -e "$TMP/dest" ]; echo $?)"

run "$GOOD" STUB_FAIL=22
check "download failure is fatal" "$([ "$rc" != 0 ] && [ ! -e "$TMP/dest" ]; echo $?)"

echo "passed=$passed failed=$failed"
[ "$failed" = 0 ]
