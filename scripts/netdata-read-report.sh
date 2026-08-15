#!/usr/bin/env bash
# netdata-read-report.sh -- retrospective ARRAY-DISK READ-activity report, sourced
# entirely from netdata's stored history.
#
# MUST BE RUN ON THE DOCKER HOST (e.g. the Unraid box), not from a workstation and
# not inside a container. It queries netdata's local HTTP API at 127.0.0.1:19999 and
# reads per-device charts that exist only where the disks actually are. To point it
# at a remote netdata instead, set NETDATA_URL.
#
# WHY PER-DEVICE AND NOT PER-CONTAINER. An earlier version of this script read
# `cgroup_<container>.io`, which CANNOT ANSWER THE QUESTION IT WAS WRITTEN FOR. The
# container chart aggregates every block device, and canticle's ~300 MB SQLite DB
# lives on NVMe appdata -- so a busy cgroup chart is consistent with the array being
# fully asleep, and a quiet one proves nothing either. Only the physical data-disk
# devices observe a platter wake-up.
#
# DO NOT USE THE `mdNp1` CHARTS. They are the md/parity layer's view and report a
# FLAT ZERO for reads even while the underlying member disk is demonstrably busy
# (Unraid serves a read from one member directly; parity is consulted on writes and
# rebuilds). Reading those yields a confident, completely false "the array was silent
# all night". Verified 2026-07-29: all ten md devices read 0 across a 12h window in
# which sdb showed steady traffic.
#
# The disk roster and the ground-truth spin state come from Unraid's own
# /var/local/emhttp/disks.ini (`spundown=0|1`), which is also the known-positive
# control: if this script reports every disk quiet while disks.ini shows drives
# spun up, distrust the script, not the disks.
#
# WHY THERE IS NO OVERNIGHT DAEMON. netdata already records per-DEVICE disk I/O at
# 1s resolution with roughly two weeks of retention, so ANY past window is queryable
# after the fact. Polling on a schedule would add load to the very host being
# measured, and would record nothing netdata does not already have. Run this in the
# morning against last night; do not run it continuously.
#
# (It records per-container charts too, and this script deliberately does NOT use
# them -- see the per-device note above. Retention is the point here, not the chart.)
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
#   netdata-read-report.sh              # last 12h, 60s buckets, all data disks
#   netdata-read-report.sh 28800        # last 8h
#   netdata-read-report.sh 43200 10     # 10s buckets instead of 60s
set -euo pipefail

SECONDS_BACK="${1:-43200}"       # default 12h
BUCKET="${2:-60}"                # seconds per sample
NETDATA="${NETDATA_URL:-http://localhost:19999}"
DISKS_INI="${DISKS_INI:-/var/local/emhttp/disks.ini}"

points=$(( SECONDS_BACK / BUCKET ))
[ "$points" -lt 1 ] && points=1

if [ ! -r "$DISKS_INI" ]; then
  echo "ERROR: cannot read ${DISKS_INI}." >&2
  echo "This script must run on the Unraid host; it needs the disk roster to know" >&2
  echo "which devices are DATA disks (parity is excluded -- it never serves reads)." >&2
  exit 1
fi

# Roster: "diskN sdX spundown" for Data disks only. Parity disks are deliberately
# skipped: a parity drive is not read to satisfy a file read, so counting it would
# attribute unrelated spin-up to the library.
roster=$(awk -F= '
  /^\[/            { name = $0; gsub(/[]["]/, "", name) }
  /^device=/       { dev  = $2 }
  /^spundown=/     { spun = $2 }
  /^type=/         { type = $2
                     gsub(/"/, "", dev); gsub(/"/, "", spun); gsub(/"/, "", type)
                     if (type == "Data") print name, dev, spun }
' "$DISKS_INI")

if [ -z "$roster" ]; then
  echo "ERROR: no Data disks found in ${DISKS_INI}." >&2
  exit 1
fi

printf '=== ARRAY DATA-DISK READ activity, last %d min (%ds buckets, source: netdata) ===\n' \
       "$(( SECONDS_BACK / 60 ))" "$BUCKET"
printf '    parity excluded (never read to serve a file); writes ignored (cache-backed)\n\n'
printf '  %-14s %-22s %-16s %s\n' "DISK" "BUCKETS WITH READS" "LONGEST QUIET" "SPUN UP NOW"

spun_up=0
total_disks=0
worst_quiet=""

while read -r name dev spun; do
  total_disks=$(( total_disks + 1 ))
  [ "$spun" = "0" ] && spun_up=$(( spun_up + 1 ))

  url="${NETDATA}/api/v1/data?chart=disk.${dev}&after=-${SECONDS_BACK}&points=${points}&format=csv"
  if ! csv=$(curl -sf --max-time 30 "$url"); then
    printf '  %-14s %s\n' "${name}(${dev})" "UNAVAILABLE (no chart disk.${dev})"
    continue
  fi
  if [ "$(printf '%s\n' "$csv" | wc -l)" -le 1 ]; then
    printf '  %-14s %s\n' "${name}(${dev})" "no samples (window predates retention?)"
    continue
  fi

  line=$(printf '%s\n' "$csv" | BUCKET="$BUCKET" LABEL="${name}(${dev})" SPUN="$spun" awk -F, '
  NR == 1 { next }                       # header: time,reads,writes
  {
    # Column 2 is reads. Column 3 (writes) is ignored by design: on a cache-backed
    # array writes land on NVMe and are flushed later by the mover, so they never
    # spin the library disks.
    r = $2 + 0
    if (r < 0) r = -r                    # netdata signs one direction negative
    n++
    if (r > 0.01) { active++; if (run > longest) longest = run; run = 0 }
    else          { run++ }
  }
  END {
    if (n == 0) { exit 1 }
    if (run > longest) longest = run
    b = ENVIRON["BUCKET"] + 0
    printf "%-14s %5d/%-5d (%5.1f%%)     %4d min          %s|%d",
           ENVIRON["LABEL"], active, n, 100.0*active/n, longest*b/60,
           (ENVIRON["SPUN"] == "0" ? "YES" : "no"), longest*b
  }') || { printf '  %-14s %s\n' "${name}(${dev})" "no samples in window"; continue; }

  quiet_secs="${line##*|}"
  printf '  %s\n' "${line%|*}"
  if [ -z "$worst_quiet" ] || [ "$quiet_secs" -lt "$worst_quiet" ]; then
    worst_quiet="$quiet_secs"
  fi
done <<EOF
$roster
EOF

printf '\n'
printf '  data disks spun up RIGHT NOW: %d of %d  (source: %s)\n' \
       "$spun_up" "$total_disks" "$DISKS_INI"
if [ -n "$worst_quiet" ]; then
  printf '  shortest longest-quiet across disks: %d min\n' "$(( worst_quiet / 60 ))"
  printf '    (issue #684 used a 900s / 15-minute zero-read bar)\n'
  if [ "$worst_quiet" -ge 900 ]; then
    printf '    -> every data disk MEETS the 15-minute idle bar\n'
  else
    printf '    -> at least one data disk is below the 15-minute idle bar\n'
    printf '       NOT automatically a canticle regression. This bar is a HOST\n'
    printf '       property: every tenant sharing the array counts against it, and\n'
    printf '       #684 was CLOSED on exactly that split -- canticle read at a small\n'
    printf '       fraction of the busiest neighbor, so the canticle-attributable\n'
    printf '       criterion was met while the host-wide bar stayed out of reach.\n'
    printf '       Attribute per-container before blaming this service.\n'
  fi
fi
printf '\n'
printf '  SANITY CHECK: if every disk above reads quiet but "spun up RIGHT NOW" is\n'
printf '  non-zero, distrust this report -- that is the fingerprint of a dead chart\n'
printf '  (the mdNp1 devices fail exactly this way), not of a sleeping array.\n'
printf '\n'
printf '  NOTE: a bucket counts as quiet only if its AVERAGED read rate is ~0, so a\n'
printf '  single small read inside a %ds bucket can still average near zero. Re-run\n' "$BUCKET"
printf '  with a smaller bucket (2nd arg) to tighten that resolution.\n'
