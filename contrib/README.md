# Canticle contrib scripts

Optional, self-contained helper scripts that integrate Canticle with other tools.
They are not part of the Canticle binary and carry no build-time dependency on it.

## `lidarr-rename-sidecars.sh`

A Lidarr **Custom Script** that keeps `.lrc` / `.txt` lyric sidecars next to their
audio when Lidarr renames a track file. Without it, a rename orphans the sidecar
under the old name and Canticle's next scan re-fetches lyrics that already exist
on disk. This script prevents the orphan at the source; `canticle realign` cleans
up orphans that already exist.

### Install

1. Copy `lidarr-rename-sidecars.sh` somewhere Lidarr can execute it, and make it
   executable (`chmod +x lidarr-rename-sidecars.sh`).
2. In Lidarr: *Settings -> Connect -> Add -> Custom Script*.
3. Set **Path** to the script, and enable the **On Rename** notification trigger.
   Leave the other triggers off (the script exits cleanly for any non-rename
   event, so extra triggers are harmless but unnecessary).

### Contract

On a rename Lidarr invokes the script with these environment variables:

| Variable | Meaning |
|---|---|
| `Lidarr_EventType` | `Rename` for the rename event (the script no-ops otherwise). |
| `Lidarr_TrackFile_PreviousPaths` | Old audio file paths, `\|`-separated. |
| `Lidarr_TrackFile_Paths` | New audio file paths, `\|`-separated, index-aligned with the previous paths. |

For each aligned old/new pair the script moves `${old_stem}.lrc` and
`${old_stem}.txt` to the new stem, **only when the source exists and the
destination does not** (it never overwrites an existing file and never converts
`.lrc` <-> `.txt`). Paths containing spaces are handled correctly.

### Requirements and caveats

- **Same filesystem view.** Lidarr and the script must resolve the same paths; on
  Docker/Unraid this means identical mount points for the media library.
- **Strict bash.** The script runs under `set -euo pipefail`; a missing required
  variable on a rename event is a hard error (exit 1).
- It is a complement to, not a replacement for, `canticle realign`: use the script
  to prevent future orphans and `realign` to fix existing ones.
