#!/usr/bin/env bash
# netdata-read-report.sh -- retrospective per-container READ-activity report,
# sourced entirely from netdata's stored history.
#
# MUST BE RUN ON THE DOCKER HOST (e.g. the Unraid box), not from a workstation and
# not inside a container. It queries netdata's local HTTP API at 127.0.0.1:19999 and
# reads per-cgroup charts that exist only where the containers actually run. To point
# it at a remote netdata instead, set NETDATA_URL.
#
# WHY THERE IS NO OVERNIGHT DAEMON. netdata already records per-container disk I/O at
# 1s resolution with roughly two weeks of retention, so ANY past window is queryable
# after the fact. Polling on a schedule would add load to the very host being
# measured, and would record nothing netdata does not already have. Run this in the
# morning against last night; do not run it continuously.
#
# WRITES ARE DELIBERATELY IGNORED. On a cache-backed array, writes land on NVMe and
# are flushed later by the mover, so they do not spin the library disks. Only reads
# wake them.
#
# THE METRIC IS TOUCH EVENTS, NOT BYTES. A one-byte read costs the same spin-up as a
# gigabyte, so this reports how OFTEN reads happen and how long the quiet gaps are,
# rather than a volume total.
#
# Usage (on the host):
#   netdata-read-report.sh                            # last 12h, canticle-stack
#   netdata-read-report.sh 28800                      # last 8h
#   netdata-read-report.sh 43200 music-assistant      # any container
#   netdata-read-report.sh 43200 canticle-stack 10    # 10s buckets instead of 60s
#
# Discover container chart names with:
#   curl -s http://localhost:19999/api/v1/charts | grep -o 'cgroup_[a-z0-9_-]*\.io'
set -euo pipefail

SECONDS_BACK="${1:-43200}"       # default 12h
CONTAINER="${2:-canticle-stack}"
BUCKET="${3:-60}"                # seconds per sample
NETDATA="${NETDATA_URL:-http://localhost:19999}"

points=$(( SECONDS_BACK / BUCKET ))
[ "$points" -lt 1 ] && points=1

url="${NETDATA}/api/v1/data?chart=cgroup_${CONTAINER}.io&after=-${SECONDS_BACK}&points=${points}&format=csv"

if ! csv=$(curl -sf --max-time 30 "$url"); then
  echo "ERROR: could not reach netdata at ${NETDATA} (chart cgroup_${CONTAINER}.io)." >&2
  echo "Run this ON THE DOCKER HOST, or set NETDATA_URL to a reachable instance." >&2
  echo "List available charts:" >&2
  echo "  curl -s ${NETDATA}/api/v1/charts | grep -o 'cgroup_[a-z0-9_-]*\\.io'" >&2
  exit 1
fi

if [ "$(printf '%s\n' "$csv" | wc -l)" -le 1 ]; then
  echo "ERROR: netdata returned no samples for cgroup_${CONTAINER}.io." >&2
  echo "The chart may not exist (wrong container name?) or the window predates retention." >&2
  exit 1
fi

printf '%s\n' "$csv" | BUCKET="$BUCKET" CONTAINER="$CONTAINER" SECONDS_BACK="$SECONDS_BACK" awk -F, '
NR == 1 { next }                       # skip header: time,read,write
{
  # Column 2 is read (KiB/s, averaged over the bucket). Column 3 is write, ignored
  # by design -- see the header comment.
  r = $2 + 0
  if (r < 0) r = -r                    # netdata signs one direction negative
  n++
  ts[n] = $1
  if (r > 0.01) {                      # a bucket carrying any meaningful read
    active++
    if (r > peak) { peak = r; peakat = $1 }
    if (run > longest) { longest = run; longestend = ts[n-1] }
    run = 0
  } else {
    quiet++
    run++
  }
}
END {
  if (n == 0) { print "no samples in window"; exit 1 }
  if (run > longest) { longest = run; longestend = ts[n] }
  b = ENVIRON["BUCKET"] + 0
  printf "=== READ activity: %s, last %d min (%ds buckets, source: netdata) ===\n", \
         ENVIRON["CONTAINER"], ENVIRON["SECONDS_BACK"]/60, b
  printf "  samples            : %d\n", n
  printf "  buckets WITH reads : %d  (%.1f%%)\n", active, 100.0*active/n
  printf "  buckets QUIET      : %d  (%.1f%%)\n", quiet, 100.0*quiet/n
  printf "  peak read          : %.1f KiB/s at %s\n", peak, peakat
  printf "\n"
  printf "  LONGEST CONTIGUOUS QUIET WINDOW: %d min (ended %s)\n", longest*b/60, longestend
  printf "    (issue #684 used a 900s / 15-minute zero-read bar)\n"
  if (longest*b >= 900) printf "    -> MEETS the 15-minute idle bar\n"
  else                  printf "    -> below the 15-minute idle bar\n"
  printf "\n"
  printf "  NOTE: a bucket counts as quiet only if its AVERAGED read rate is ~0, so a\n"
  printf "  single small read inside a %ds bucket can still average near zero. Re-run\n", b
  printf "  with a smaller bucket (3rd arg) to tighten that resolution.\n"
}
'
