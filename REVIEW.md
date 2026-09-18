# Review guidance

Read by automated PR reviewers. Architecture and the package catalog live in `CLAUDE.md`; this file lists only the repo-specific defect classes a diff reviewer should look for. Generic style is enforced by `gofmt` and `.golangci.yml`, so do not comment on it.

## Hard invariants (flag any violation)

- **No CGO.** SQLite is pure-Go `modernc.org/sqlite`, and the release and cross-compile builds set `CGO_ENABLED=0` (the local gate's bare `go build` / `go test` do not, so they will not catch a CGO dependency). Any import or build tag that needs CGO breaks cross-compilation.
- **`//nolint` needs a reason.** Every NEW or TOUCHED directive carries `// reason: ...`. Do not flag untouched directives in the packages listed in the `nolintlint` exclusion block of `.golangci.yml`, and do not suggest adding a path there to silence a new finding.
- **No runtime global state.** Dependencies are injected through interfaces. Flag a new package-level `var` that production code WRITES at runtime (a cache, counter, registry, or singleton). Read-only lookup tables and the test-seam pattern (`var removeFile = os.Remove`, reassigned only by tests) are fine.
- **Timing thresholds are not configurable.** `timing.Tolerance` and `timing.CategoricalRatio` are a co-calibrated pair of constants owned by `internal/timing`. Flag a config key, flag, or per-caller override for either, and flag a second timing predicate outside `timing.Evaluate`.
- **Never change an existing config key's type.** A TOML type mismatch fails decode before any re-default runs, so every deployment that sets the key fails to boot. The correct pattern adds a new key, keeps the old one decoding with a `slog.Warn`, tests precedence with `md.IsDefined` (a blankness check alone cannot tell "set to zero/false" from "omitted"), and maps the deprecated key on the file, env, and `config set` tiers.
- **Never change the SQL of an existing migration** in `internal/db/migrations/`; add a new numbered one. Comment-only annotations to an old migration are allowed.

## Destructive filesystem paths

Code that moves, rewrites, or deletes a user's sidecar (for example `realign`, `revalidate`, `purgeprovenance`, `lrcbackfill`, and the `scan reconcile` family) must:

- Be **dry-run by default** in the CLI: `--yes` for `realign` and the reconcile family, `--apply` for `revalidate`.
- **Preserve a restorable copy before the mutation** (an fsynced JSONL record, or `lrcbackfill`'s `.lrc.orig`), and abort that file's change if the backup fails.
- **Never follow a symlinked sidecar** (`Lstat` / `DirEntry.Type`, not `Stat`) and act only under the configured library roots.
- Keep **coupled database/cache updates in one transaction**. Example: `purgeprovenance` invalidates the cache in the same transaction as the row reset, and both commit before the unlink; otherwise a re-scan is satisfied from cache and the sidecar is lost permanently. A filesystem move cannot join a SQLite transaction, so where a row records the outcome of a move, the move comes FIRST and the row is stamped only on success (the timing sweep leaves a row unstamped when its remediation fails, so it is retried).

Code that mutates `work_queue` / `scan_results` rows (`purgeprovenance`, `identityrepair`, `prune`) must guard on `status != 'processing'` in the UPDATE or DELETE itself, not in an earlier read. (The timing sweep instead excludes `processing` rows when it selects its backlog, and only stamps a column the worker does not write.) Backup timing is per-package and documented at the call site (`prune` reports AFTER its delete commits, by design, so a record never describes a row a race skipped); read that comment before flagging a different ordering.

## Privacy

A sidecar path, artist, title, or lyric line is private library metadata. Flag new code that logs one from an UNATTENDED path (serve-mode sweeps, startup checks, reports) at a level above Debug; flag any HTTP response that returns one to a caller not authorized for library detail, whatever the log level; and flag any per-file output from `revalidate` on stdout (it is aggregate-only; per-file detail goes only to its `--tail` file). Operator-invoked dry-run/apply CLIs that list the files they change are allowed. Lyric text never belongs in any log.

## Queue and worker

- Completion stamps (`SetOutcomeType`, `SetCompletionProvenance`, `SetTimingOutcome`) are written **before** `Complete`, while the row is still `processing`, and are non-fatal.
- A stamp that retires a row from a backlog is one-way. Do not stamp on a transient failure such as an unreadable file or a missing mount; leave the row to retry.
- `timing.Evaluate` returns `UnknownDuration` for a non-positive duration, and nothing is refused on that verdict. The accept-time guard deliberately falls back to the provider's `TrackLength` when the audio duration is missing, so it fails open only when BOTH are unknown; do not flag that fallback. `revalidate` never remediates on an unknown duration.

## Tests

- **Reachability.** A new exported function, endpoint, report, or config key needs a caller outside `_test.go` files. Code with no production caller passes its own tests and ships dead.
- **Log-only arms.** If a branch's only effect is a log line, its test must capture `slog` output. Otherwise the branch is untested, however high the coverage number.
- **Coincidental assertions.** A `strings.Contains` that matches for an unrelated reason (such as a weekday name containing "day") proves nothing. Check that the asserted substring comes from the path under test.
- Integration tests use real SQLite (in-memory or a temp file), not a mocked database. Unit tests may fake a repository interface at the boundary (for example the worker's queue seam).

## CI and supply chain

- External actions and reusable workflows are pinned to a commit SHA with a `# vX` comment. Local `uses: ./...` references resolve at the running commit and cannot take a ref; do not ask to pin them. Checkout steps set `persist-credentials: false`. Job-level `permissions:` replaces the workflow-level block, so include `contents: read` where needed.
- No `paths` or `paths-ignore` trigger filters on workflows that produce required status checks (a check that never runs reads as failed).
- A PR that edits `.github/**`, `REVIEW.md`, `CLAUDE.md`, `AGENTS.md`, build scripts, hooks, or `go.sum` beyond its stated scope deserves explicit scrutiny. These files steer tooling, including this review. Because they are read from the PR's own head, an automated review of a PR that edits them is NOT review of those edits: say so explicitly and leave them to a maintainer.

## Out of scope

- Generated `web/templates/*_templ.go` files are gitignored; never request changes to them.
- Do not propose making provider pacing floors (`petitlyrics` 10s, `innertube` 2s) configurable or lower. They are policy.
