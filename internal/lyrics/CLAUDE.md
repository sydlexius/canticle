# internal/lyrics

- **Accept-time timing guard (`DecidePromotion`).** The duration judged against is `Song.AudioDurationSeconds`, which callers stamp from the AUDIO FILE, never from the provider-returned `Song.Track.TrackLength` (the provider's catalog length makes the comparison near-circular). When it is zero the guard deliberately falls back to `Song.Track.TrackLength` (`guardDurationSeconds`) and fails open only when BOTH are unknown; do not remove or short-circuit the fallback.
- **No-downgrade guard (#553).** A candidate whose `Rung` is strictly below `RungOnDisk` returns `ErrKeptBetter`. `RungOnDisk` reads the files themselves, never a provenance header. The only bypass is `SetForceOverwrite`, wired from `--update` only; do not wire it from anything else.
