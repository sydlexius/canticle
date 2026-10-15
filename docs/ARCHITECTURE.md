# Architecture

This page is the map of the code: what the project is, how the two run modes fit together, and one line per package. Build, gate and CI mechanics are in the [Developer Guide](DEVELOPER.md).

## Project overview

`canticle` (module `github.com/sydlexius/canticle`; the `cmd/mxlrcgo-svc` directory, config paths, and systemd unit names retain the historical `mxlrcgo-svc` string) is a Go tool for fetching synced lyrics. It has two faces: a one-shot `fetch` CLI that writes `.lrc` / `.txt` files, and a stateful `serve` mode -- an HTTP server with a durable SQLite work queue, a background worker, a library scan scheduler (+ optional filesystem watcher), multi-provider orchestration, encrypted-at-rest secrets, and a browser-authenticated web UI. Global state is eliminated; the API token is externalized; config is TOML.

Deeper stack and convention detail is discoverable from `go.mod`, the `Makefile` (`make help`), `.golangci.yml`, and the [Developer Guide](DEVELOPER.md).

## Architecture (one-paragraph orientation)

Cmd/internal layout. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary and owns no business logic; it parses the subcommand tree, loads config + DB, builds the dependency graph, and dispatches. The command tree lives in `internal/commands` (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `revalidate`, `timing-accuracy`, `completion`). Two principal paths run under `internal/`: **fetch mode** -- `scanner` parses CLI/text-file/directory input into an in-memory `queue.InputsQueue`, `app` drains it sequentially, `musixmatch` fetches (a `Fetcher` interface), and `lyrics` writes `.lrc` / `.txt` / instrumental output (a `Writer` interface); and **serve mode** -- a `scan` scheduler over `library` roots enqueues work into the durable SQLite `queue.DBQueue`, a `worker` drains it through the multi-provider `orchestrator` (Musixmatch + petitlyrics + `innertube` `providers`, each behind a `circuit` breaker with `backoff` retry), consulting `cache`, gated by optional `verification` / `detector` sidecars (via `ffmpeg`) and `langguard`, swept in the background by the timing-revalidation pass (`revalidate`) and the instrumental backfill, fronted by the `server` HTTP handler (`auth` API keys, `trustnet` IP gating, optional `servetls`) and the `web` browser UI (`webauth` sessions). Shared infra: `config` (TOML, XDG paths, token precedence CLI > env > file), `db` (pure-Go SQLite `modernc.org/sqlite`, no CGO, goose migrations in `internal/db/migrations/`), `secrets` (AES-256-GCM at rest), `normalize` (NFKC cache keys), `models` (shared types, depends on nothing else internal). Dependencies are injected through interfaces -- mock at the boundary; there is no global mutable state.

## Package catalog

Every package with a one-line purpose. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary (`cmd/genlib` is an internal test-data generator and `cmd/smokefixtures` builds the live serve-smoke library for `make smoke-fixtures`; neither is shipped); everything else lives under `internal/` (the directory matches the package name) except the embedded web assets under `web/`.

**Rule for edits here:** a catalog change accompanies a NEW package or a NEW command, in one line; keep the catalog current when the package surface changes. Behavior detail, measurements and review history do not go on this page: an invariant a future editor could break belongs in a comment at the code site or in that package's `CLAUDE.md` (`internal/<pkg>/CLAUDE.md`), and measurements and review history go to `session-management.md` at the repo root.

### Core fetch/write path

- `models` -- shared data types (`Track`, `Song`, `Lyrics`, `Synced`, `Inputs`, `Library`, `ScanResult`, ...); depends on nothing else internal.
- `musixmatch` -- Musixmatch desktop API client + `Fetcher` interface; parses the nested JSON into `models`.
- `petitlyrics` -- petitlyrics.com provider adapter, used as a fallback lane.
- `innertube` -- YouTube Music's unauthenticated internal ("innertube") API adapter, tokenless like petitlyrics; owns the three-call flow (search, next, browse), the client-string selection that picks a timed-cue response, its own pacer with a policy floor, and search-result verification. It multiplexes per track between upstream licensors: `[source:innertube]` stays constant while `[upstream:]` carries the licensor -- see [Provider Attribution](provider-attribution.md) and [Provider Terms](provider-terms.md).
- `providers` -- provider abstraction (`LyricsProvider`, `Fetcher`, `AdaptivePacer`) plus provider-generation/version invalidation that retires stale cache entries when the provider set changes.
- `orchestrator` -- multi-lane orchestration (`Lane`, `Orchestrator`, parallel-race + suitability scoring); composes `providers` with per-lane `circuit` breakers.
- `circuit` -- concurrency-safe per-lane circuit breaker modeling a provider's rate-limit/throttle response.
- `backoff` -- shared retry-delay formula (1m, 2m, 4m, ..., capped at 1h) used by the worker, durable queue, and fetch loop.
- `lyrics` -- LRC/TXT/instrumental writer (`Writer`, `LRCWriter`), `Slugify`, an `.lrc` parser, provenance-tag embedding, and fsync helpers; owns the accept-time timing guard (`DecidePromotion`) and the no-downgrade guard (#553).
- `lrcnormalize` -- pure transform (`ParseBody`, `Expand`) that expands compressed multi-timestamp LRC lines (`[t1][t2]text`) into one cue per timestamp and classifies `[key:value]` ID-tag lines distinctly from cues; no I/O.
- `normalize` -- NFKC cache-key normalization, duration bucketing, fuzzy-match confidence, album-artist resolution.
- `langguard` -- Unicode-script classification/filtering of lyric text against a configured language allowlist.
- `scanner` -- parses CLI/text-file/directory input into the in-memory queue; skips files that consistently fail metadata read (via the injected `MetadataFailureStore`); serves and banks an unsettled file's identity through the injected `IdentityStore`.
- `app` -- one-shot `fetch`-mode orchestration loop over the in-memory `InputsQueue`; depends on the `Fetcher`/`Writer` interfaces.
- `lrcbackfill` -- rewrites existing `.lrc` sidecars that use compressed multi-timestamp lines into the expanded one-cue-per-line form.
- `respdrift` -- detects a provider that answers many distinct queries with the same track.

### Persistence and stateful services

- `db` -- pure-Go SQLite (`modernc.org/sqlite`) open/migrate (goose), WAL, foreign keys, busy-retry, and a read-only open path; migrations in `internal/db/migrations/`.
- `cache` -- lyrics cache repository (`CacheRepo`) over SQLite; `Lookup` falls back to the bucket-0 unknown-duration row.
- `scanfail` -- `Store` recording files that consistently fail metadata read, so the scanner skips them until mtime/size changes; satisfies `scanner.MetadataFailureStore`.
- `audiodur` -- exact per-file audio duration cache (`Store`) keyed by path and validated by (mtime, size).
- `revalidate` -- re-judges `.lrc` files ALREADY on disk against their companion audio's exact duration and plans remediation: demote a `MisSynced` lyric's words to `.txt`, quarantine a `Categorical` one.
- `queue` -- the in-memory `InputsQueue` (fetch mode) and the durable SQLite `DBQueue` (serve/worker mode) with priority tiers and randomized within-tier dequeue.
- `library` -- library-root CRUD repository (`Add`/`List`/`Get`/`GetByName`/`Update`/`Remove`).
- `scan` -- library scanning: `Enqueuer`, the `scan_results` `Repo`, and the periodic scheduler that enqueues missing lyrics. After each scan the `Adopter` gives every settled (`done`, lyric file beside the audio) index row a `done` `work_queue` row, or links it to the existing row for the song, so queue-only readers see it; budgeted per scan, backed up to `adopt-settled-backup.jsonl` beside the database.
- `worker` -- durable-queue `Worker` that drains work items through the providers/orchestrator and cache; a row with `word_timing_state='queued'` takes the word-recheck path (`runWordRecheck`).
- `reports` -- read-only, run-on-demand reports over existing SQLite data; no write paths.
- `secrets` -- encrypted-at-rest (AES-256-GCM) store for recoverable runtime secrets, persisted as opaque BLOBs.
- `watcher` -- optional filesystem watcher that triggers targeted library scans on change; complements, never replaces, the periodic scheduler.
- `selfwrite` -- a TTL'd, concurrency-safe set of paths this process just wrote, shared between the `lyrics` writer (records) and the `watcher` (suppresses); a leaf package.
- `prune` -- reconciles `work_queue`/`scan_results` against the filesystem: rows whose source audio file has vanished are deleted; a row gone inside a surviving directory is deleted only by age-out (`prune_gone_since`).
- `purgeprovenance` -- bulk-deletes `.lrc`/`.txt` sidecars matching a provenance filter (`--source <name>` or `--no-source`) and resets the coupled `work_queue`/`scan_results` rows so the next scan re-fetches; driven by the dry-run-by-default `scan purge-provenance`.
- `instrumentalmark` -- marks a track instrumental by hand through `LRCWriter.WriteManualMarker`.
- `identityrepair` -- re-reads each `scan_results` file's tags (via the injected `IdentityReader` seam) to correct run-together multi-value artist rows, re-keying the coupled `work_queue` row; driven by the dry-run-by-default `scan reconcile-identity`.
- `lyricblock` -- exact-words fingerprints of lyric bodies an operator marked wrong, in a `lyric_blocks` store keyed by normalized artist/title identity; driven by `scan mark-wrong`, `scan list-blocks` and `scan unblock`.
- `titlerepair` -- finds rows that settled with lyrics but whose title the instrumental title rule would now settle as instrumental, and repairs the ones whose lyric file is provably the settle's own; driven by the dry-run-by-default `scan reconcile-instrumental-titles`.
- `audiometa` -- store of per-file audio metadata read from the library's audio files; `Identity` is the scanner's read of it, `Record` the one write. See "Tag cache reads" below.
- `detectorbackfill` -- attributes historical instrumental detections to the detector lane for reporting.
- `instrumentalbackfill` -- runs the audio detector over rows it never scored.
- `instrumentalrecalib` -- re-decides earlier detector rejections from stored detector scores.
- `schedule` -- the periodic library scan's schedule: frequency, time of day and weekday.
- `repair` -- the contract an operator fix implements so the CLI and the web UI start the same code (`Repair`: `Info`, a database-only `Pending`, `Run` with `Options`), a `Result` of counts keyed by fixed identifiers, and a `Registry` keyed by slug that its owner injects; imports nothing internal.
- `tierrepair` -- the sync-tier classification and guarded apply loop of `scan reconcile-sync-tier`, over an injected queue interface and backup sink; it never opens a database or prints.
- `wordsyncrepair` -- the selection and batched write-ahead queuing of `scan reconcile-word-sync`, over an injected queue interface and backup; it never opens a database or prints.

### Tag cache reads

A scan of an unchanged library opens no file contents, apart from the cases the User Guide lists (an embedded-lyrics mode, a file whose duration parse fails, and the lyric files settled-file adoption reads to classify them). Three readers share the `audio_metadata` tag cache; this is what each does and the rules that keep them consistent. The operator view is "What a scan reads" in [User Guide](USER_GUIDE.md#what-a-scan-reads).

- **Version stamps.** A row is current for a file when its path, mtime (nanoseconds), size and duration-reader version match. `scanner.TagReaderVersion` (migration 072, column `tag_reader_version`) is a second stamp for the code that extracts artist, album artist, title, album, ISRC and recording ID. Only the identity read checks it. A bump makes every existing row a miss for that read until the file is read and recorded again. A change to tag extraction that alters those values for an unchanged file must bump it, because mtime and size do not change when the reader does and artist + title key `scan_results` and `work_queue`.
- **The identity read.** `audiometa.Store.Identity` returns the identity fields only when mtime, size, duration-reader version and tag-reader version all match. `scanner.cachedIdentity` uses it for an unsettled file, after the metadata-failure skip, and only when a store is wired, embedded lyrics are off and the file stat succeeds. A lookup error is a miss, never a failure. `Facts` (the worker's read) and `Lookup` do not compare the tag-reader stamp.
- **Writers record the full read.** `Store.Record` replaces every column and stamps the row current, so a writer passes the complete `scanner.AudioFacts`, never a subset. The scan builds it with `audioFactsFromHandle`, the same builder `scan index-metadata` uses. The scan (`bankFacts`) and the worker (`readAndBankMetadata`) write only when the stamp from the read's own open handle equals the stat the lookup missed on, the duration parse did not fail and `AudioFacts.MultiValueErr` is nil. The scan also requires recording enrichment on. The worker requires the global enrichment default on.
- **Key spelling per writer.** The scan keys by `RebaseUnderCanonicalRoot(absRoot, canonRoot, path)` and `scan index-metadata` by the path it walks under the canonical root: both resolve the library root once and leave a symlink inside the library alone. The worker keys by `pathutil.CanonicalPath`, which resolves the file itself. For a file reached through a symlink inside the library the worker's key is the target and the others' key is the link, so the same file can hold two rows. Readers validate by the caller's current mtime and size, so neither row serves a stale hit.
- **Open counts.** On a four-file fixture with enrichment on, embedded lyrics off and an empty cache, three consecutive scans open `4, 4, 4` files before the scan banked its read and `4, 0, 0` after (`TestScanBank_OpenCountsBeforeAndAfter`). The counter covers the unsettled-file tag read and the skipped-file duration probe. It does not count the settled-file index read, and the fixture has no settled files. The method and the cases still opened are in `session-management.md`.

### Serve-mode HTTP surface and web UI

- `server` -- serve-mode HTTP `Handler` plus its seams (`Authenticator`, `WorkQueue`, `Readiness`, `StatusReporter`, `Inventory`, `MetricsReporter`) and metrics.
- `auth` -- stateless API-key authentication (in-memory and SQL `Store`, `Scope`, `Key`) for the HTTP API.
- `webauth` -- browser auth for the web UI: Argon2id password hashing, an admin user store, and a server-side session store (tokens hashed at rest); kept separate from `auth` (different storage/lifecycle/threat model).
- `trustnet` -- client-IP resolution and a trusted-network allowlist, without trusting spoofable headers.
- `servetls` -- optional TLS for the serve listener behind a `CertManager` seam: bring-your-own PEM or a self-signed bootstrap.
- `pathutil` -- path-containment checks confining filesystem targets to configured roots; shared by `server`, `watcher`, `scan`.
- `web` -- serves the web UI from embedded templ templates and `go:embed`'d static assets; `flacfallback.go` (opt-in `server.preview_flac_fallback`) serves `/preview/{id}/audio.flac`.
- `web/static` -- compiled CSS and self-hosted fonts embedded into the binary so the UI serves offline.
- `web/templates` -- templ source for the UI shell; generated `*_templ.go` are built on demand and gitignored (run `make ui` after a fresh clone before `go build`).
- `tablesort` -- shared column-sort component for the web UI's tables.

### Sidecars, config, cross-cutting

- `verification` -- optional acoustic verification of fetched lyrics (`Verifier`, `HTTPVerifier`) against an external service, using a short audio sample.
- `detector` -- optional audio-based instrumental detection sidecar (external AudioSet/YAMNet classifier, vendored at `deploy/yamnet-detector/`); a three-gated decision (music / sung-vocal / speech) over short windows.
- `ffmpeg` -- resolves an ffmpeg executable for the sidecars, auto-provisioning a checksum-pinned static build when none is configured or on PATH.
- `timing` -- the shared pure predicate classifying synced-lyric timing against audio duration; sole owner of `TimingOutcome`/`Evaluate`, `IsDecorative`, and the calibrated `Tolerance`/`CategoricalRatio` constants. No I/O.
- `instrumentaltitle` -- pure classifier (`Classify(title, album) Verdict{Tier, Family, Position}`, tiers `None < Weak < Strong`) for instrumental title and album qualifiers; a cappella, karaoke and sing-along titles are excluded (`None`), and the verdict never carries any part of the title. No I/O.
- `config` -- TOML config resolution (XDG paths, registry-driven keys, token precedence CLI > env > file) plus redaction, validation, render/write; owns `[timing_validation]`.
- `logging` -- `slog` logger setup and secret redaction.
- `realign` -- confidence resolver (`Realigner`, `Move`, `Apply`) that re-attaches orphaned `.lrc`/`.txt` sidecars to renamed or moved audio: exact ISRC/MBID provenance, a filesystem heuristic (title-only name guard plus a `min_margin` runner-up rule), and an opt-in N:M matcher (`heuristic-nm`). Backup-first and clobber-safe.
- `commands` -- the CLI command tree: top-level `Args` and every subcommand (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `revalidate`, `timing-accuracy`, `completion`), each thin CLI wiring over its package, including the `scan reconcile-*` family; the serve-mode sweeps live here too.
- `timingacc` -- pure line-start accuracy measurement for `timing-accuracy`: a monotone LCS `matchLines` over `normalize.NormalizeKey` text and `LineStats`; no I/O, so it imports neither `lyrics` nor `models`.
- `version` -- build-time `Version`/`Commit`/`Date` (GoReleaser ldflags) and `VersionString()`.
- `testutil` -- generates synthetic ID3-tagged audio for load/concurrency tests and the genlib tool.
- `dbtest` -- test-only helper that opens fully migrated SQLite databases from a per-binary template (`Open`, `OpenAt`, `Path`) instead of re-running every migration per test.
- `aligner` -- client for the forced-alignment sidecar, which returns per-word timestamps for given lyric lines.
- `failsig` -- normalizes a work item's error text into a stable signature for grouping failures.
- `identity` -- shared matching that re-attaches a moved sidecar or row to its audio file; used by `realign` and `prune`.
- `sidecar` -- the lyric sidecar file extensions canticle writes and the kind each carries.
- `vocalcalib` -- computes a sung-vocal gate threshold from stored per-track detector scores.
