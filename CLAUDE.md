# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> **On resume / handoff, read the gitignored `SESSION-STATE.md` at the repo root FIRST.** It is the running orchestration checkpoint (current main SHA, in-flight PRs/worktrees, ordered NEXT ACTIONS, standing directives). Transient session state lives there, never in this file or the auto-memory.

> **Longer-form working notes (problems, methods, fixes, measurements; newest first) live in the tracked `session-management.md` at the repo root; read it on resume as well.** Durable rules stay in this file.

## Memory MCP: query it, it will not come to you

Durable, hard-won lessons for this repo live in the `memory-canticle` MCP server (repo tier) and
`memory-global` (user tier). That store is PULL-only: it returns nothing unless you call
`search_nodes`. An interactive session gets an index of its contents at startup, but **spawned
subagents do not**, so an agent that never queries never sees any of it. If you dispatch a subagent
into one of the areas below, either query first and put the answer in its brief, or tell it to query.

QUERY BEFORE, not after, each of these:

- **Releases** - notes formatting, and reading a red Release workflow correctly.
- **Unraid / prod ops** - compose stack paths, picking up a new image, long detached jobs, egress.
- **Debugging a "stuck" or idle-looking pipeline** - queue timestamps, log levels, throttling.
  There is a standing "do not re-test this" result here; check before re-running an experiment.
- **Review / merge mechanics** - what the merge oracle actually counts, and thread resolution.
- Any "why is it done this way here" question where a prior decision or a refuted hypothesis exists.

Natural phrasing works: word order, plurals, hyphens, and camelCase names are all handled. Prefer one
broad query over guessing the stored wording. A miss costs one call; a silently-missed constraint has
already cost real work here more than once.

Detail lives in the auto-memory `.md` files, which remain canonical; the MCP holds a one-line hook
plus a `Detail: <file>.md` pointer. Do not copy a fact back into the auto-memory index - one
canonical home per fact.

## Project Overview

`canticle` (module `github.com/sydlexius/canticle`; the `cmd/mxlrcgo-svc` directory, config paths, and systemd unit names retain the historical `mxlrcgo-svc` string) is a Go tool for fetching synced lyrics. It has two faces: a one-shot `fetch` CLI that writes `.lrc` / `.txt` files, and a stateful `serve` mode -- an HTTP server with a durable SQLite work queue, a background worker, a library scan scheduler (+ optional filesystem watcher), multi-provider orchestration, encrypted-at-rest secrets, and a browser-authenticated web UI. Global state is eliminated; the API token is externalized; config is TOML.

For the full per-package reference, see the "Package catalog" section below. Deeper stack and convention detail is discoverable from `go.mod`, the `Makefile` (`make help`), `.golangci.yml`, and `docs/DEVELOPER.md` -- keep the catalog current when the package surface changes.

## What to work on next

When the user says **"next"**, **"what's next"**, **"keep going"**, or any equivalent lazy prompt with no specific task, inspect the open GitHub issues and milestones before starting, then confirm scope with the user first. Do not assume a fixed backlog order -- the milestones and their dependency chains change as work ships; read the live issue tracker each time.

## Build & Test

`make help` lists every target. Two non-obvious points worth knowing up front:

- The entrypoint lives in `cmd/mxlrcgo-svc`, so `go run .` does not work. Use `go run ./cmd/mxlrcgo-svc [args]`.
- A single test: `go test -run TestFoo ./internal/<pkg>` (tests live next to the code they cover under `internal/`).

Run `make hooks` once to enable the tracked git hooks, and `make gate` before pushing. See "Quality gating and CI" below for the full target list.

## Architecture (one-paragraph orientation)

Cmd/internal layout. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary and owns no business logic; it parses the subcommand tree, loads config + DB, builds the dependency graph, and dispatches. The command tree lives in `internal/commands` (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `revalidate`, `timing-accuracy`, `completion`). Two principal paths run under `internal/`: **fetch mode** -- `scanner` parses CLI/text-file/directory input into an in-memory `queue.InputsQueue`, `app` drains it sequentially, `musixmatch` fetches (a `Fetcher` interface), and `lyrics` writes `.lrc` / `.txt` / instrumental output (a `Writer` interface); and **serve mode** -- a `scan` scheduler over `library` roots enqueues work into the durable SQLite `queue.DBQueue`, a `worker` drains it through the multi-provider `orchestrator` (Musixmatch + petitlyrics + `innertube` `providers`, each behind a `circuit` breaker with `backoff` retry), consulting `cache`, gated by optional `verification` / `detector` sidecars (via `ffmpeg`) and `langguard`, swept in the background by the timing-revalidation pass (`revalidate`) and the instrumental backfill, fronted by the `server` HTTP handler (`auth` API keys, `trustnet` IP gating, optional `servetls`) and the `web` browser UI (`webauth` sessions). Shared infra: `config` (TOML, XDG paths, token precedence CLI > env > file), `db` (pure-Go SQLite `modernc.org/sqlite`, no CGO, goose migrations in `internal/db/migrations/`), `secrets` (AES-256-GCM at rest), `normalize` (NFKC cache keys), `models` (shared types, depends on nothing else internal). Dependencies are injected through interfaces -- mock at the boundary; there is no global mutable state. See the "Package catalog" below for the full `internal/`/`web/` surface.

## Package catalog

Every package with a one-line purpose. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary (`cmd/genlib` is an internal test-data generator and `cmd/smokefixtures` builds the live serve-smoke library for `make smoke-fixtures`; neither is shipped); everything else lives under `internal/` (the directory matches the package name) except the embedded web assets under `web/`. **Rule for edits here:** a catalog change accompanies a NEW package or a NEW command, in one line. Behavior detail, measurements and review history go to `docs/` or `session-management.md`, not here.

**Core fetch/write path**
- `models` -- shared data types (`Track`, `Song`, `Lyrics`, `Synced`, `Inputs`, `Library`, `ScanResult`, ...); depends on nothing else internal.
- `musixmatch` -- Musixmatch desktop API client + `Fetcher` interface; parses the nested JSON into `models`.
- `petitlyrics` -- petitlyrics.com provider adapter, used as a fallback lane.
- `innertube` -- YouTube Music's unauthenticated internal ("innertube") API adapter, tokenless like petitlyrics; owns the three-call flow (search, next, browse), the client-string selection that picks a timed-cue response (`ANDROID_MUSIC`/`IOS_MUSIC`, never `WEB_REMIX`), its own pacer with a 2s policy floor (`MinAllowedInterval`), and search-result verification. It MULTIPLEXES per track between upstream licensors: `[source:innertube]` stays constant while `[upstream:]` carries the licensor -- see `docs/provider-attribution.md` and `docs/provider-terms.md`.
- `providers` -- provider abstraction (`LyricsProvider`, `Fetcher`, `AdaptivePacer`) plus provider-generation/version invalidation that retires stale cache entries when the provider set changes.
- `orchestrator` -- multi-lane orchestration (`Lane`, `Orchestrator`, parallel-race + suitability scoring); composes `providers` with per-lane `circuit` breakers. A result ends the dispatch only if it is suitable AND `lyrics.DecidePromotion` would promote it as-is; a categorical result falls through to the other lanes. No threshold lives here.
- `circuit` -- concurrency-safe per-lane circuit breaker modeling a provider's rate-limit/throttle response.
- `backoff` -- shared retry-delay formula (1m, 2m, 4m, ..., capped at 1h) used by the worker, durable queue, and fetch loop.
- `lyrics` -- LRC/TXT/instrumental writer (`Writer`, `LRCWriter`), `Slugify`, an `.lrc` parser, provenance-tag embedding, and fsync helpers. Owns the accept-time timing guard (`DecidePromotion`): the duration judged against is `Song.AudioDurationSeconds`, which callers stamp from the AUDIO FILE, never `Track.TrackLength`; unknown duration always fails open. Also owns the no-downgrade guard (#553): a candidate whose `Rung` is strictly below `RungOnDisk` (the files themselves, never a provenance header) returns `ErrKeptBetter`, unless `SetForceOverwrite` (wired from `--update` only) is set.
- `lrcnormalize` -- pure transform (`ParseBody`, `Expand`) that expands compressed multi-timestamp LRC lines (`[t1][t2]text`) into one cue per timestamp and classifies `[key:value]` ID-tag lines distinctly from cues; no I/O.
- `normalize` -- NFKC cache-key normalization, duration bucketing, fuzzy-match confidence, album-artist resolution.
- `langguard` -- Unicode-script classification/filtering of lyric text against a configured language allowlist.
- `scanner` -- parses CLI/text-file/directory input into the in-memory queue; skips files that consistently fail metadata read (via the injected `MetadataFailureStore`).
- `app` -- one-shot `fetch`-mode orchestration loop over the in-memory `InputsQueue`; depends on the `Fetcher`/`Writer` interfaces.

**Persistence and stateful services**
- `db` -- pure-Go SQLite (`modernc.org/sqlite`) open/migrate (goose), WAL, foreign keys, busy-retry, and a read-only open path; migrations in `internal/db/migrations/`.
- `cache` -- lyrics cache repository (`CacheRepo`) over SQLite. `Lookup` falls back to the bucket-0 unknown-duration row, so `Invalidate` deletes every duration bucket for a key.
- `scanfail` -- `Store` recording files that consistently fail metadata read, so the scanner skips them until mtime/size changes; satisfies `scanner.MetadataFailureStore`.
- `audiodur` -- exact per-file audio duration cache (`Store`) keyed by path and validated by (mtime, size). A miss is `unknown_duration`, never an error, and `revalidate` fails open on one rather than remediating.
- `revalidate` -- re-judges `.lrc` files ALREADY on disk against their companion audio's exact duration and plans remediation: demote a `MisSynced` lyric's words to `.txt`, quarantine a `Categorical` one. Owns no predicate and no filesystem machinery: the verdict is `timing.Evaluate`, the duration is `audiodur`'s, and every mutation goes through `realign.Apply`.
- `queue` -- the in-memory `InputsQueue` (fetch mode) and the durable SQLite `DBQueue` (serve/worker mode) with priority tiers and randomized within-tier dequeue. A row that records a file or a hand edit is never reopened. `work_queue.upstream` is written in the SAME statement as `provider_lane` and follows the lane, NOT the file.
- `library` -- library-root CRUD repository (`Add`/`List`/`Get`/`GetByName`/`Update`/`Remove`).
- `scan` -- library scanning: `Enqueuer`, the `scan_results` `Repo`, and the periodic scheduler that enqueues missing lyrics.
- `worker` -- durable-queue `Worker` that drains work items through the providers/orchestrator and cache. A row with `word_timing_state='queued'` takes the word-recheck path (`runWordRecheck`): word-capable lanes only, never the cache, and it writes ONLY a result that passes `lyrics.HasQualifyingWords` and promotes as-is, so a recheck can never downgrade a settled `.lrc`.
- `reports` -- read-only, run-on-demand reports over existing SQLite data; no write paths. The `LibraryID` predicate is an `EXISTS` (never a JOIN, so the keyset cursor cannot break) and its unary plus (`+scan_result_id IN (...)`) is load-bearing (it keeps the `work_queue_id=?` prefix-only plan).
- `secrets` -- encrypted-at-rest (AES-256-GCM) store for recoverable runtime secrets, persisted as opaque BLOBs.
- `watcher` -- optional filesystem watcher that triggers targeted library scans on change; complements, never replaces, the periodic scheduler.
- `selfwrite` -- a TTL'd, concurrency-safe set of paths this process just wrote, shared between the `lyrics` writer (records) and the `watcher` (suppresses); a leaf package. Suppression keys on the FILE path, never its directory. Entries expire, so a crash can never leave a path permanently deaf to external change.
- `prune` -- reconciles `work_queue`/`scan_results` against the filesystem: rows whose source audio file has vanished are deleted (`os.Stat` is the sole authority; an in-flight guard defers `processing` rows). A row gone inside a surviving directory is deleted only by AGE-OUT (`prune_gone_since`, confirmed by a second sweep). Age-out is BACKUP-FIRST: the report runs before the delete transaction and a row whose report failed is not deleted. More than `ageOutMaxPerSweep` (50) rows due at once is a hard stop for the unattended sweep; the exit is attended (`scan reconcile-paths --yes`).
- `purgeprovenance` -- bulk-deletes `.lrc`/`.txt` sidecars matching a provenance filter (`--source <name>` or `--no-source`) and resets the coupled `work_queue`/`scan_results` rows so the next scan re-fetches. Backup-first (the caller's `Report` fsyncs a restorable JSONL record first), symlinks never followed, in-flight rows skipped. The row reset and the `cache.Invalidate` commit in ONE transaction and BOTH commit before the unlink, or the re-scan would be satisfied from cache. Driven by the dry-run-by-default `scan purge-provenance`.
- `identityrepair` -- re-reads each `scan_results` file's tags (via the injected `IdentityReader` seam) to correct run-together multi-value artist rows, re-keying the coupled `work_queue` row. Write-ahead: each correction's backup record is fsynced inside the transaction before the row commits. Driven by the dry-run-by-default `scan reconcile-identity`.
- `lyricblock` -- exact-words fingerprints of lyric bodies an operator marked wrong, in a `lyric_blocks` store keyed by normalized artist/title identity with no `work_queue` foreign key; `AnyBlocked` fails open and logs.

**Serve-mode HTTP surface and web UI**
- `server` -- serve-mode HTTP `Handler` plus its seams (`Authenticator`, `WorkQueue`, `Readiness`, `StatusReporter`, `Inventory`, `MetricsReporter`) and metrics.
- `auth` -- stateless API-key authentication (in-memory and SQL `Store`, `Scope`, `Key`) for the HTTP API.
- `webauth` -- browser auth for the web UI: Argon2id password hashing, an admin user store, and a server-side session store (tokens hashed at rest); kept separate from `auth` (different storage/lifecycle/threat model).
- `trustnet` -- client-IP resolution and a trusted-network allowlist, without trusting spoofable headers.
- `servetls` -- optional TLS for the serve listener behind a `CertManager` seam: bring-your-own PEM or a self-signed bootstrap.
- `pathutil` -- path-containment checks confining filesystem targets to configured roots; shared by `server`, `watcher`, `scan`.
- `web` -- serves the web UI from embedded templ templates and `go:embed`'d static assets. `flacfallback.go` (opt-in `server.preview_flac_fallback`) serves `/preview/{id}/audio.flac` through the same session guard and `os.Root` confinement as `/preview/{id}/audio`; ffmpeg is handed the confined handle, never the library path.
- `web/static` -- compiled CSS and self-hosted fonts embedded into the binary so the UI serves offline.
- `web/templates` -- templ source for the UI shell; generated `*_templ.go` are built on demand and gitignored (run `make ui` after a fresh clone before `go build`).

**Sidecars, config, cross-cutting**
- `verification` -- optional acoustic verification of fetched lyrics (`Verifier`, `HTTPVerifier`) against an external service, using a short audio sample.
- `detector` -- optional audio-based instrumental detection sidecar (external AudioSet/YAMNet classifier, vendored at `deploy/yamnet-detector/`); a three-gated decision (music / sung-vocal / speech) over short windows.
- `ffmpeg` -- resolves an ffmpeg executable for the sidecars, auto-provisioning a checksum-pinned static build when none is configured or on PATH.
- `timing` -- the shared pure predicate classifying synced-lyric timing against audio duration; sole owner of `TimingOutcome`/`Evaluate`, `IsDecorative`, and the calibrated `Tolerance`/`CategoricalRatio` constants. **The thresholds are NOT configurable and that omission is the design**: they are a co-calibrated pair, and one predicate serving every caller (the `lyrics` guard, the worker stamp, `revalidate`, the serve-mode sweep) keeps a `timing_outcome` comparable across a deployment's history. A threshold worth tuning belongs here. No I/O.
- `config` -- TOML config resolution (XDG paths, registry-driven keys, token precedence CLI > env > file) plus redaction, validation, render/write. Owns `[timing_validation]`: `enabled` AND `revalidate_existing` are both required. The `on_mis_synced` / `on_categorical` enums differ by exactly one value and it is load-bearing -- a categorical lyric is another song's words, so `demote` is illegal there; an unrecognized action ALWAYS resets to the conservative default.
- `logging` -- `slog` logger setup and secret redaction.
- `realign` -- confidence resolver (`Realigner`, `Move`, `Apply`) that re-attaches orphaned `.lrc`/`.txt` sidecars to renamed or moved audio: exact ISRC/MBID provenance, a filesystem heuristic (title-only name guard plus a `min_margin` runner-up rule), and an opt-in N:M matcher (`heuristic-nm`). Backup-first and clobber-safe.
- `commands` -- the CLI command tree: top-level `Args` and every subcommand (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `revalidate`, `timing-accuracy`, `completion`). Each is thin CLI wiring over its package, including the `scan reconcile-*` family: dry-run by default, a JSONL backup of what was applied, confirmation flag `--yes` (`--apply` for `revalidate`). `revalidate`, `reconcile-editor-tag` and `reconcile-remediated` are AGGREGATE-ONLY on stdout -- no path, artist, title, or lyric text is ever printed, since a sidecar path carries the library's private metadata; `--tail` is the one place per-file detail lands. The serve-mode sweeps live here too; `runTimingValidationSweep` applies BEFORE it stamps so a failed remediation stays in the backlog, and upgrade-sweep trips write through `LRCWriter.WriteLRCNoDowngrade`, never forced, even under `serve --update`.
- `timingacc` -- pure line-start accuracy measurement for `timing-accuracy`: a monotone LCS `matchLines` over `normalize.NormalizeKey` text and `LineStats`; no I/O, so it imports neither `lyrics` nor `models`.
- `version` -- build-time `Version`/`Commit`/`Date` (GoReleaser ldflags) and `VersionString()`.
- `testutil` -- generates synthetic ID3-tagged audio for load/concurrency tests and the genlib tool.

## CLI usage and input modes

See `README.md` for flags and examples. Worth flagging: directory mode overrides `--outdir` (writes the output next to the audio file; the extension depends on lyric type - `.lrc` when synced lyrics are found, `.txt` when only unsynced lyrics or an instrumental marker is written), and `--upgrade` re-fetches songs that previously got `.txt` (unsynced lyrics or an instrumental marker, any provenance) to promote them when better lyrics become available, at a lookup per `.txt` per run (a provider request or more on a cache miss).

## Quality gating and CI

- Local gate: `make gate` (`scripts/pre-push-gate.sh`) runs the FULL chain behind a per-worktree run-lock: conflict markers, commit signatures (main requires signed commits), product name, gofmt, generate + build, non-race tests of the changed packages, patch coverage (skipped if the estimator is absent), golangci-lint, actionlint, PR-trigger scope guard (`scripts/check-pr-trigger-scope.sh`; no `branches:` filter on any PR workflow, so stacked PRs run CI), shard-split verify, govulncheck. The race suite is CI-authoritative; `RUN_RACE=1 make gate` opts in to the full `go test -race ./...` plus the local coverage floor and codecov dry-run (both need its whole-module profile; skipped otherwise, CI Coverage Floor / Upload Coverage own them). `.gates.toml` delegates `/prep-pr` to it, and gate-runner writes a `gate-receipt/v1` to `<git-dir>/prep-pr-receipt.json`.
- Git hooks: `make hooks` sets `core.hooksPath=.githooks` (a relative, shared git setting), so every worktree -- including new ones -- inherits the hooks with no per-worktree setup. `.githooks/pre-commit` runs conflict markers, typos, product name, gofmt, build, golangci-lint, govulncheck on staged content. `.githooks/pre-push` first runs `scripts/check-commit-signatures.sh --refs` on the pushed refs, BEFORE the receipt fast path and the `PUSH_GATE=full` override (the receipt is tree-keyed, and a `git commit-tree` re-creation keeps the tree), then does NOT re-run the full gate: it exits at once when a passing receipt's `tree_sha` matches every pushed ref on a clean tree (`scripts/check-push-receipt.sh`, fail-closed, tests in `make hooks-test`), else runs `pre-push-gate.sh --hook` (non-race tests of the changed packages only, mapped by `scripts/hook-test-pkgs.sh`, which fails closed to `./...` on go.mod/go.sum, any non-`.go` change under `internal/`/`web/`/`cmd/`, or a package left with no `.go` file; govulncheck only when go.mod/go.sum changed, since pre-commit misses merges, rebases and `--no-verify` commits and no CI job runs it; actionlint only when a workflow changed; no coverage floor/codecov dry-run/shard verify). The race suite, coverage floor, and shard split are left to the required CI checks (Test, Coverage Floor, Lint; `scripts/ci-shards.sh` is in the `code` paths filter so a shard-map change runs Lint). The per-check table lives in `docs/DEVELOPER.md`. The only override is `PUSH_GATE=full git push` (full gate, ignore the receipt); there is no skip value and any other value exits 2. Never `--no-verify`. Verify the wiring with `make doctor` (or `scripts/check-hooks.sh`).
- Make targets (`make help` lists all): `gate` (full local gate), `hooks-test` (hermetic tests for the pre-push hook, receipt check and package mapping), `doctor` (verify hook wiring + tool-version pins), `scan` (build the Docker image and grype it for HIGH+ CVEs), `test-shuffle` (`go test -race -shuffle=on`), `sync-tool-versions` (assert the golangci-lint pin agrees across CI and pre-commit, via `scripts/check-tool-versions.sh`), `vulncheck` (pinned `govulncheck@v1.1.4`), `coverage-floor` (one-way per-package coverage ratchet over `internal/` via `--bump`/`--lower`, jq-free; `scripts/coverage-floor.sh` + `scripts/coverage-floor.json`; policy in `docs/DEVELOPER.md`).
- Linter config: `.golangci.yml`. Always include a `// reason:` comment after any `//nolint:linter` directive -- this is now **enforced** by `nolintlint` (`require-explanation: true`), not just a convention, so a directive without the token fails the pre-commit hook, `make gate`, and CI alike. Pre-existing packages are excluded by path while their backlog is backfilled; each backfill PR deletes its own exclusion line, and the block disappears when the last one lands. Do not add a path there to silence a new finding. Keep the golangci-lint pin aligned across `ci.yml` and `.pre-commit-config.yaml` (`make sync-tool-versions` enforces it).
- golangci-lint version policy: the version pinned in CI (`ci.yml`) is the source of truth. `make sync-tool-versions` only aligns the config-file pins; it does not pin the binary you have installed locally, so `make gate` / the pre-commit hook can pass locally on a different golangci-lint version while CI flags issues your version does not (and vice versa). The `gosec` taint analyzers (e.g. `G704` SSRF on `httpClient.Do`) are especially prone to version-specific phantom findings that do not reproduce locally. When CI flags one of these, treat CI as authoritative: apply a per-site `//nolint:gosec // reason` (the reason is required) rather than chasing local reproduction. Bumping the CI pin needs a probe PR -- run against a clean cache and confirm no analyzer-regression findings before merging.
- CI workflows live in `.github/workflows/` (`ci.yml` -- incl. an image CVE `scan` job, `release.yml`, `nightly.yml`, `codeql.yml`). (Action SHA-pinning + `persist-credentials: false` are user-global CI/CD rules.)
- Releases: `git tag vX.Y.Z && git push --tags` triggers GoReleaser.

## Style (non-discoverable rules)

- Conventional commits: `feat:`, `fix:`, `docs:`, `ci:`, `chore:`, etc.
- `slog` for structured logs; `fmt.Printf` only for direct user-facing CLI output (timer, counts).
- Wrap errors with `fmt.Errorf("context: %w", err)`.

Everything else (formatting, naming, file layout) is enforced by `gofmt` + `.golangci.yml` -- follow the linter, not a written rule.

## Database (when adding stateful features)

- Pure-Go SQLite via `modernc.org/sqlite`. **Never reintroduce CGO** -- it breaks cross-compilation.
- WAL mode; goose-managed migrations in `internal/db/migrations/`.
- Repository pattern over interfaces (see `internal/cache/`) so storage stays swappable.
- Integration tests use real SQLite (in-memory `file::memory:?cache=shared` or temp file), not mocks.

## PR Workflow

Use the global slash commands (maintained outside this repo) for the full workflow: `/prep-pr` to open a PR, `/handle-review` to triage bot comments, `/merge-pr` to merge + clean up. The full command catalog and typical flow live in the user-global instructions, not here.

### Reading PR comments (gh API gotcha)

If you fall back to raw `gh` instead of `/handle-review`: the `!` character triggers bash history expansion even inside double quotes, which breaks `--jq` filters using `!=`. Always use `select(.field == "value" | not)` instead:

```bash
gh api "repos/{owner}/{repo}/pulls/{number}/comments" --paginate \
  --jq '[.[] | select(.user.login == "some-bot" | not) | {id, user: .user.login, body}]'
```
