#!/usr/bin/env bash
# lidarr-rename-sidecars.sh -- move .lrc/.txt lyric sidecars when Lidarr renames
# a track file, so the sidecar is never orphaned under the old name.
#
# Install as a Lidarr Custom Script:
#   Settings -> Connect -> Add -> Custom Script
#   Path: /path/to/lidarr-rename-sidecars.sh
#   Notification triggers: enable "On Rename" (others are ignored).
#
# On a rename, Lidarr sets:
#   Lidarr_EventType                    = "Rename"
#   Lidarr_TrackFile_PreviousPaths      = old audio paths, '|'-separated
#   Lidarr_TrackFile_Paths              = new audio paths, '|'-separated
# The two lists are aligned by index. For each pair this script moves the
# sidecars ${old_stem}.lrc and ${old_stem}.txt to ${new_stem}.<same ext>, but
# ONLY when the source exists and the destination does not (clobber-safe skip),
# and never converts .lrc<->.txt.
#
# Lidarr and this script must see the same filesystem (identical mount paths).
# It is a source-side complement to `canticle realign`, which cleans up orphans
# after the fact.
#
# Exit status:
#   0  handled (or not a Rename event, or nothing to do)
#   1  a required environment variable was missing on a Rename event
set -euo pipefail

fail() { printf 'lidarr-rename-sidecars: %s\n' "$1" >&2; exit 1; }

# Only act on rename events; exit cleanly for every other trigger (including
# Lidarr's "Test" event, which sets EventType=Test).
if [[ "${Lidarr_EventType:-}" != "Rename" ]]; then
    exit 0
fi

[[ -n "${Lidarr_TrackFile_PreviousPaths:-}" ]] || fail "Lidarr_TrackFile_PreviousPaths is not set"
[[ -n "${Lidarr_TrackFile_Paths:-}" ]] || fail "Lidarr_TrackFile_Paths is not set"

# Split the '|'-separated lists into arrays without splitting on spaces.
IFS='|' read -r -a old_paths <<< "${Lidarr_TrackFile_PreviousPaths}"
IFS='|' read -r -a new_paths <<< "${Lidarr_TrackFile_Paths}"

if [[ "${#old_paths[@]}" -ne "${#new_paths[@]}" ]]; then
    fail "path list length mismatch (${#old_paths[@]} previous vs ${#new_paths[@]} new)"
fi

# strip_ext echoes its argument with the final extension removed.
strip_ext() {
    local path="$1"
    printf '%s' "${path%.*}"
}

moved=0
for i in "${!old_paths[@]}"; do
    old_audio="${old_paths[$i]}"
    new_audio="${new_paths[$i]}"
    [[ -n "$old_audio" && -n "$new_audio" ]] || continue

    old_stem="$(strip_ext "$old_audio")"
    new_stem="$(strip_ext "$new_audio")"
    [[ "$old_stem" != "$new_stem" ]] || continue

    for ext in lrc txt; do
        src="${old_stem}.${ext}"
        dst="${new_stem}.${ext}"
        if [[ -f "$src" && ! -e "$dst" ]]; then
            # Handle each move independently: under `set -e` a single failing mv
            # (permissions, cross-device link) would otherwise abort the whole
            # event and strand the remaining, unrelated sidecar pairs.
            if ! mkdir -p "$(dirname "$dst")"; then
                printf 'lidarr-rename-sidecars: mkdir failed for %s; skipping\n' "$dst" >&2
                continue
            fi
            # mv -n (no-clobber) closes the TOCTTOU window between the -e test
            # above and the move: if another process creates $dst in between, the
            # move is skipped rather than overwriting it.
            if mv -n -- "$src" "$dst"; then
                printf 'lidarr-rename-sidecars: moved %s -> %s\n' "$src" "$dst"
                moved=$((moved + 1))
            else
                printf 'lidarr-rename-sidecars: move failed %s -> %s; skipping\n' "$src" "$dst" >&2
            fi
        fi
    done
done

printf 'lidarr-rename-sidecars: %d sidecar(s) moved\n' "$moved"
exit 0
