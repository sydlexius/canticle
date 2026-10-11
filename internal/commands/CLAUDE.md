# internal/commands

- **`revalidate`, `reconcile-editor-tag` and `reconcile-remediated` are AGGREGATE-ONLY on stdout.** No path, artist, title or lyric text is ever printed, since a sidecar path carries the library's private metadata. `--tail` is the one place per-file detail lands.
- The `scan reconcile-*` family is dry-run by default and writes a JSONL backup of what was applied; the confirmation flag is `--yes` (`--apply` for `revalidate`).
- **`runTimingValidationSweep` applies BEFORE it stamps**, so a failed remediation stays in the backlog. Do not reorder.
- **Upgrade-sweep trips write through `LRCWriter.WriteLRCNoDowngrade`, never forced**, even under `serve --update`.
- Each command is thin wiring over its package: logic belongs in the package, not here.
