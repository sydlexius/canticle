# internal/config

`[timing_validation]` rules:

- `enabled` AND `revalidate_existing` are both required before the sweep touches files already on disk.
- The `on_mis_synced` / `on_categorical` enums differ by exactly one value and it is load-bearing: a categorical lyric is another song's words, so `demote` is illegal there.
- An unrecognized action ALWAYS resets to the conservative default.

Do not add a key, flag or env var for the timing thresholds; they are constants by design (see `internal/timing/CLAUDE.md`).
