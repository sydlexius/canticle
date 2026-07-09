## Project

**Canticle (doxazo-net/canticle)**

A Go CLI tool that fetches synced lyrics from the Musixmatch API (and fallback providers) and saves them as `.lrc` files. The project is maintained under its own module path with global state eliminated and the API token externalized. Beyond the original one-shot `fetch` mode it now ships stateful, long-running features: a `serve` mode (HTTP server + durable SQLite work queue + background worker + library scan scheduler + optional filesystem watcher + browser-authenticated web UI), TOML config, multi-provider orchestration with per-lane circuit breakers, optional acoustic verification and instrumental detection sidecars, and encrypted-at-rest secrets.

**Core Value:** The tool fetches synced lyrics reliably and writes correct `.lrc` files. Everything else (project structure, config handling, CI) exists to support that.

### Constraints

- **Binary name**: `canticle` (the Go module path is `github.com/doxazo-net/canticle`, matching the repo. The `cmd/mxlrcgo-svc` directory, config paths under `/etc/mxlrcgo-svc` and `~/.config/mxlrcgo-svc`, and systemd unit names retain the historical `mxlrcgo-svc` string and are intentionally left unchanged)
- **No CGO**: Must remain CGO_ENABLED=0 for cross-compilation
- **Go 1.26.2+**: Minimum Go version per go.mod
- **Behavior preservation**: All existing CLI flags and behaviors must work identically after restructuring
- **Token precedence**: CLI flag > environment variable (`MUSIXMATCH_TOKEN`) > `.env` file

## Technology Stack

## Languages
- Go 1.26.2 (minimum, per `go.mod`) - All application code
- Bash - Pre-commit hooks (`.githooks/pre-commit`), Makefile targets
- YAML - CI/CD workflows (`.github/workflows/`), configuration files
## Runtime
- Go (compiled binary, no runtime dependency)
- CGO disabled for release builds (`CGO_ENABLED=0` in `.goreleaser.yml`)
- Go Modules (`go.mod` / `go.sum`)
- Lockfile: `go.sum` present
## Frameworks
- None - Pure Go standard library for HTTP, I/O, and CLI orchestration
- `github.com/alexflint/go-arg` v1.6.1 - CLI argument parsing via struct tags (`Args` in `cmd/mxlrcgo-svc/main.go`)
- Go standard `testing` package - No third-party test framework
- Make (`Makefile`) - Build orchestration and the quality gate (build, run, test, test-shuffle, test-cover, patch-cover, gate, scan, vulncheck, coverage-floor, lint, fmt, hooks, doctor, sync-tool-versions, smoke, clean). `make help` lists every target with a one-liner
- GoReleaser (`.goreleaser.yml`) - Cross-platform release builds
- golangci-lint v2.11.4 (`.golangci.yml`) - Linter aggregator with 12 enabled linters
## Key Dependencies
- `github.com/alexflint/go-arg` v1.6.1 - CLI argument parsing. Defines the entire user interface via struct tags on `Args` in `cmd/mxlrcgo-svc/main.go`
- `github.com/dhowden/tag` v0.0.0-20240417053706-3d75831295e8 - Audio file metadata reading (ID3, MP4, FLAC, OGG, DSF). Used in `internal/scanner/scanner.go` for directory-scan mode
- `github.com/joho/godotenv` v1.5.1 - `.env` file loading for token configuration. Used in `cmd/mxlrcgo-svc/main.go`
- `github.com/valyala/fastjson` v1.6.10 - High-performance JSON parsing for Musixmatch API responses. Used in `internal/musixmatch/client.go` to navigate deeply nested JSON
- `golang.org/x/text` v0.36.0 - Unicode normalization (NFKC) for filename sanitization in `Slugify()` (`internal/lyrics/slugify.go`)
- `github.com/alexflint/go-scalar` v1.2.0 - Transitive dependency of go-arg
## Configuration
- Token precedence: CLI flag (`-t/--token`) > environment variable (`MUSIXMATCH_TOKEN`) > `.env` file (loaded via godotenv)
- No fallback/hardcoded token -- token is required; binary exits with error if none provided
- `Song` (positional, required) - Song info as `artist,title` pairs, a `.txt` file path, or a directory path
- `-o/--outdir` (default: `lyrics`) - Output directory for `.lrc` files
- `-c/--cooldown` (default: `15`) - Cooldown between API requests in seconds
- `-d/--depth` (default: `100`) - Max recursion depth for directory scanning
- `-u/--update` - Overwrite existing `.lrc` files in directory mode
- `--bfs` - Use BFS instead of DFS for directory traversal
- `-t/--token` - Musixmatch API token
- `go.mod` - Module definition and Go version constraint
- `.goreleaser.yml` - Cross-compilation targets (linux/darwin/windows, amd64/arm64, excluding windows/arm64)
- `.golangci.yml` - Linter configuration (v2 format)
- `.editorconfig` - Editor formatting (tabs for Go, 2-space for YAML/JSON/MD)
- `.gitattributes` - Line ending normalization (LF everywhere)
- `.typos.toml` - Spell-checker config (excludes `go.sum`)
- `.pre-commit-config.yaml` - Pre-commit framework hooks: trailing-whitespace, end-of-file-fixer, check-yaml, check-added-large-files (500KB), check-merge-conflict, typos, gitleaks, golangci-lint, gofmt, conventional-pre-commit
- `.githooks/pre-commit` - Tracked pre-commit hook: conflict-marker check, typos, gofmt, go build, golangci-lint, govulncheck
- `.githooks/pre-push` - Tracked pre-push hook: runs the full gate (`scripts/pre-push-gate.sh`). Wired via `make hooks` (`core.hooksPath=.githooks`, a relative shared setting so every worktree inherits both hooks); `scripts/check-hooks.sh` / `make doctor` verify the wiring
- `scripts/pre-push-gate.sh` - Deterministic gate: conflict markers, gofmt, build, race tests, patch coverage, golangci-lint, actionlint, govulncheck, behind a per-worktree run-lock
- `scripts/check-tool-versions.sh` - Asserts the golangci-lint pin agrees across `ci.yml` and `.pre-commit-config.yaml` (`make sync-tool-versions`)
- `scripts/coverage-floor.sh` + `scripts/coverage-floor.json` - One-way per-package coverage floor (`make coverage-floor`)
## Platform Requirements
- Go 1.26.2+
- golangci-lint v2.11+ (for linting)
- typos-cli (for spell checking)
- govulncheck (for vulnerability scanning; the gate/`make vulncheck` pin `v1.1.4`)
- actionlint (for workflow linting in the gate)
- goimports (optional, for import formatting)
- pre-commit (optional, for `.pre-commit-config.yaml` hooks)
- grype + Docker (optional, for `make scan` image CVE scanning; CI runs it regardless)
- jq (optional, for `make coverage-floor`)
- Standalone static binary (CGO_ENABLED=0)
- No runtime dependencies
- Supported platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64
## CI/CD Pipeline
- `ci.yml` - Lint + Test + image CVE Scan (grype via `anchore/scan-action`, fail on HIGH+) + Build matrix (linux/darwin/windows x amd64/arm64). Uses `dorny/paths-filter` to skip on non-code changes. Build requires lint+test to pass first.
- `release.yml` - GoReleaser on `v*.*.*` tags. Produces cross-platform archives with conventional-commit changelogs.
- `nightly.yml` - Nightly Docker image build/push to GHCR from `main`.
- `codeql.yml` - GitHub CodeQL security analysis for Go. Runs on push/PR to main and weekly (Monday 04:17 UTC).
- `claude.yml` / `claude-code-review.yml` - Claude bot (issue/PR assist + auto review). All actions SHA-pinned with `persist-credentials: false`.
- `dependabot-auto-approve.yml` - Auto-approves Dependabot PRs for patch/minor updates.
- `dependabot-merge.yml` - Auto-merges approved Dependabot PRs after CI passes (squash merge, delete branch).
- All workflow actions are SHA-pinned (`# vX` comment) with `persist-credentials: false` on checkouts; job-level `permissions` include `contents: read`.
- Weekly updates (Monday) for `gomod`, `github-actions`, and `docker` ecosystems
- Conventional commit prefixes (`chore(deps)` for Go/Docker, `ci` for Actions)

## Conventions

## Naming Patterns
- `cmd/mxlrcgo-svc/main.go` - CLI entry point (single file)
- `internal/<package>/<file>.go` - lowercase single-word filenames per package: `client.go`, `fetcher.go`, `writer.go`, `slugify.go`, `scanner.go`, `app.go`, `queue.go`, `models.go`
- Test files use Go's standard `_test.go` suffix: `slugify_test.go`
- PascalCase for all exported identifiers: `NewClient()`, `FindLyrics()`, `WriteLRC()`, `InputsQueue`, `Track`, `Song`
- camelCase for unexported identifiers: `writeSyncedLRC()`, `writeUnsyncedLRC()`, `writeInstrumental()`
- Method receivers are short abbreviations: `c` for `Client`, `w` for `LRCWriter`, `q` for `InputsQueue`, `sc` for `Scanner`
- Short, abbreviated names preferred: `fn` (filename), `fp` (filepath), `cur` (current), `res` (response)
- Loop variables are single-letter: `i`, `f`, `m`, `v`
- Error variables are always `err` (shadowed freely within nested scopes)
- Struct fields use PascalCase with JSON tags: `TrackName string \`json:"track_name,omitempty"\``
- All shared types defined in `internal/models/models.go`
- lowerCamelCase for unexported package-level constants: `const apiURL = "https://..."`
## Code Style
- `gofmt` is the canonical formatter (enforced by pre-commit hook and CI)
- `goimports` for import grouping (run via `make fmt`)
- Tab indentation for `.go` files, 2-space indentation for config files (`.editorconfig`)
- `golangci-lint` v2 with `.golangci.yml` config
- Enabled linters: `errcheck`, `govet`, `staticcheck`, `unused`, `bodyclose`, `gosec`, `noctx`, `unconvert`, `unparam`, `wastedassign`, `misspell`, `revive`
- Test files excluded from: `gosec`, `errcheck`, `noctx`
- US English locale enforced via `misspell`
- `revive` configured with `disableStutteringCheck` and `unexported-return` warnings
- Always include a justification comment after `//nolint:linter`:
- Used sparingly -- only for `gosec` false positives on file operations with user-provided paths
- LF enforced everywhere via `.gitattributes` (`* text=auto eol=lf`)
## Import Organization
- None used. All imports are direct package paths.
## Error Handling
- Functions return `error` (not `bool`) for fallible operations
- Wrap errors with `fmt.Errorf("context: %w", err)` for call-stack context
- Explicitly discard close error with `_ =` only when already returning a real error
- Named return `retErr error` pattern in `WriteLRC()` enables deferred close error capture
## Logging
- `slog.Info("verb noun", "key", value)` for informational progress messages
- `slog.Error("message", "error", err)` for non-fatal error reporting
- `slog.Warn(...)` for skipped/degraded items
- `slog.Debug(...)` for verbose scan details
- `os.Exit(1)` after `slog.Error` for unrecoverable startup errors in `main()`
- `fmt.Printf()` used for user-facing output (timer countdown, result counts)
## Comments
- `nolint` directives always get a `// reason` suffix
- Inline comments for non-obvious constants: `// 2 MiB`
- Exported functions have doc comments per Go convention
- Unexported helpers may omit doc comments
## Function Design
- Functions are generally short (10-50 lines)
- Longest function: `GetSongDir()` (~60 lines), `FindLyrics()` (~115 lines)
- Pointer receivers on all stateful types: `func (q *InputsQueue) Pop() (models.Inputs, error)`
- Interface types for dependencies: `musixmatch.Fetcher`, `lyrics.Writer` -- enables testing via mocks
- `(value, error)` tuple for all fallible operations: `FindLyrics(ctx, track)`, `Next()`, `Pop()`
- `error` return for write operations: `WriteLRC()`, `writeSyncedLRC()`, etc.
- Pointer-or-nil for validation: `AssertInput(song string) *models.Track`
## Module Design
- `cmd/mxlrcgo-svc/` - CLI entry point only; parses args, loads config + DB, wires the dependency graph, dispatches to the selected subcommand. No business logic.
- No global mutable variables -- state is owned by the constructed structs (`App`, `Worker`, server `Handler`, etc.); dependencies are injected through interfaces (`Fetcher`, `Writer`, `Store`, ...) so each layer is mockable at its boundary.
- See the package catalogue below for the full `internal/` and `web/` surface.

## Packages
Every package is listed with a one-line purpose and its location. `cmd/mxlrcgo-svc/main.go` is the sole entry point; everything else lives under `internal/` except the embedded web assets under `web/`.

### Core fetch/write path
- `internal/models` - Shared data types (`Track`, `Song`, `Lyrics`, `Synced`, `Inputs`, `Library`, `ScanResult`, `LaneAttempt`, ...); depends on nothing else internal. Location: `internal/models/models.go`.
- `internal/musixmatch` - Musixmatch desktop API client + `Fetcher` interface; parses the nested JSON into `models`. Location: `internal/musixmatch/client.go`, `internal/musixmatch/fetcher.go`.
- `internal/petitlyrics` - Lyrics provider adapter for petitlyrics.com, used as a fallback lane. Location: `internal/petitlyrics/`.
- `internal/providers` - Provider abstraction (`LyricsProvider`, `Fetcher`, `AdaptivePacer`) and provider-generation/version invalidation that retires stale cache entries when the provider set changes. Location: `internal/providers/`.
- `internal/orchestrator` - Multi-provider/lane orchestration: `Lane`, `Orchestrator`, parallel-race + suitability scoring; composes `providers` with per-lane `circuit` breakers. Location: `internal/orchestrator/`.
- `internal/circuit` - Concurrency-safe per-lane circuit breaker modelling a single provider's rate-limit/throttle response. Location: `internal/circuit/`.
- `internal/backoff` - Shared retry-delay formula (1m, 2m, 4m, ..., capped at 1h) used by the worker, durable queue, and legacy fetch loop. Location: `internal/backoff/`.
- `internal/lyrics` - LRC/TXT/instrumental writer (`Writer` interface, `LRCWriter`), `Slugify`, an `.lrc` parser, provenance-tag embedding, and fsync helpers. Location: `internal/lyrics/`.
- `internal/normalize` - NFKC cache-key normalization, duration bucketing, fuzzy match confidence, and album-artist resolution. Location: `internal/normalize/`.
- `internal/langguard` - Unicode-script classification/filtering of lyric text against a configured language allowlist. Location: `internal/langguard/`.
- `internal/scanner` - Parses CLI/text-file/directory input into the in-memory queue (`Scanner`, `ScanOptions`). Skips files that consistently fail metadata read via the injected `MetadataFailureStore` (issue #376). Location: `internal/scanner/scanner.go`.
- `internal/app` - One-shot `fetch`-mode orchestration loop over the in-memory `InputsQueue`; depends on the `Fetcher` and `Writer` interfaces. Location: `internal/app/app.go`.

### Persistence and stateful services
- `internal/db` - Pure-Go SQLite (`modernc.org/sqlite`) open/migrate (goose), WAL mode, foreign keys, busy-retry, and a read-only open path. Migrations live in `internal/db/migrations/`. Location: `internal/db/`.
- `internal/cache` - Lyrics cache repository (`CacheRepo`) over the SQLite DB. Location: `internal/cache/cache.go`.
- `internal/scanfail` - `Store` over the SQLite DB recording files that consistently fail metadata read, so the scanner skips re-reading them until their mtime/size changes (issue #376). Satisfies `scanner.MetadataFailureStore`. Location: `internal/scanfail/scanfail.go`.
- `internal/queue` - Two queues: the in-memory `InputsQueue` (fetch mode) and the durable SQLite `DBQueue` (serve/worker mode) with priority tiers and randomized within-tier dequeue. Location: `internal/queue/`.
- `internal/library` - Library-root CRUD repository (`Repo`: `Add`/`List`/`Get`/`GetByName`/`Update`/`Remove`). Location: `internal/library/repository.go`.
- `internal/scan` - Library scanning: `Enqueuer`, the `scan_results` `Repo`, and the periodic scheduler that enqueues missing lyrics. Location: `internal/scan/`.
- `internal/worker` - Durable-queue `Worker` that dequeues work items and processes them via the providers/orchestrator and cache. Location: `internal/worker/worker.go`.
- `internal/reports` - Read-only, run-on-demand reports over existing SQLite data (`work_queue`, `scan_results`, `provider_outcomes`); no write paths. Location: `internal/reports/`.
- `internal/secrets` - Encrypted-at-rest (AES-256-GCM) store for recoverable runtime secrets (Musixmatch token, serve-mode webhook key), persisted as opaque BLOBs in the DB. Location: `internal/secrets/`.
- `internal/watcher` - Optional filesystem watcher that triggers targeted library scans on change; complements, never replaces, the periodic scheduler. Location: `internal/watcher/`.

### Serve-mode HTTP surface and web UI
- `internal/server` - Serve-mode HTTP `Handler` plus its seams (`Authenticator`, `WorkQueue`, `Readiness`, `StatusReporter`, `Inventory`, `MetricsReporter`) and metrics. Location: `internal/server/`.
- `internal/auth` - Stateless API-key authentication (in-memory and SQL `Store`, `Scope`, `Key`) for the HTTP API. Location: `internal/auth/`.
- `internal/webauth` - Browser authentication for the web UI: Argon2id password hashing, an admin user store, and a server-side session store (tokens hashed at rest). Kept separate from `auth` (different storage/lifecycle/threat model). Location: `internal/webauth/`.
- `internal/trustnet` - Client-IP resolution and trusted-network allowlist for the HTTP surface, without trusting spoofable headers. Location: `internal/trustnet/`.
- `internal/servetls` - Optional TLS for the serve listener behind a `CertManager` seam: bring-your-own PEM or a self-signed bootstrap. Location: `internal/servetls/`.
- `internal/pathutil` - Path-containment checks confining filesystem targets to configured roots; shared by `server`, `watcher`, and `scan`. Location: `internal/pathutil/`.
- `internal/prune` - Reconciles `work_queue`/`scan_results` against the filesystem: rows whose source audio file has vanished (renamed/merged/deleted) are deleted so a moved track cannot leave a permanently-failing or wedged row behind (#453). `os.Stat` of the source is the sole authority for "gone"; `Directory` granularity (cheap, dir-level) backs the periodic sweep and `Exact` (per-file) backs the reactive watcher prune and the `scan reconcile-paths` CLI. An in-flight guard defers any source whose linked `work_queue` row is still `processing`. Location: `internal/prune/`.
- `internal/web` - Serves the serve-mode web UI (fixed-sidebar shell, Reports placeholder, read-only Config view) from embedded templ templates and `go:embed`'d static assets. Location: `internal/web/`.
- `web/static` - Embeds the compiled CSS and self-hosted fonts into the binary so the UI serves offline. Location: `web/static/`.
- `web/templates` - templ source for the web UI shell; generated `*_templ.go` files are generated on build and gitignored, not committed (issue #364 -- run `make ui` after a fresh clone before `go build`). Location: `web/templates/`.

### Sidecars, config, and cross-cutting
- `internal/verification` - Optional acoustic verification of fetched lyrics (`Verifier`, `HTTPVerifier`) against an external service, using a short audio sample. Location: `internal/verification/`.
- `internal/detector` - Optional audio-based instrumental detection sidecar that queries an external AudioSet classifier (YAMNet; vendored at `deploy/yamnet-detector/`). The sidecar's `/classify` returns `{"mean": {...}, "max": {...}}` per-class maps. The decision is three-gated: the music gate (summed `instrumental_classes` mean >= `min_confidence`), the sung-vocal gate (#384; no `vocal_classes` peak `max`-over-frames >= `vocal_max_confidence`), AND the speech gate (#403; the frame mean of `speech_classes` stays below `speech_max_confidence` -- `Speech` moved out of `vocal_classes` into its own mean-based gate so sustained speech blocks but brief incidental speech does not). To catch late-entering vocals it samples `spread_samples` short windows across the whole track (via ffprobe duration + an ffmpeg `aselect` filter) in one inference call; a legacy mean-only sidecar degrades safely to never-instrumental. Location: `internal/detector/`.
- `internal/ffmpeg` - Resolves an ffmpeg executable for the verification/detection sidecars, auto-provisioning a checksum-pinned static build when none is configured or on PATH. Location: `internal/ffmpeg/`.
- `internal/config` - TOML config resolution (XDG paths, registry-driven keys, token precedence CLI > env > file) plus redaction, validation, and render/write. Location: `internal/config/`.
- `internal/logging` - `slog` logger setup and secret redaction. Location: `internal/logging/`.
- `internal/commands` - The CLI command tree: top-level `Args` and every subcommand (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `completion`) with their sub-subcommands. `realign` (in `realign.go`) re-attaches orphaned `.lrc`/`.txt` sidecars to renamed audio via a four-tier confidence resolver (exact ISRC/MBID provenance, filesystem heuristic with a Jaro-Winkler name guard, ambiguous, conflict), gated by the `[realign]` config section, with the same dry-run/`--yes`/JSONL-backup ergonomics as `scan reconcile`. `scan reconcile-paths` (in `reconcile_paths.go`) drives `internal/prune` on demand to delete queue/scan rows whose source audio file has vanished, at `Exact` granularity, with the same dry-run/`--yes`/JSONL-backup ergonomics. Location: `internal/commands/`.
- `internal/version` - Build-time `Version`/`Commit`/`Date` metadata (overridden by GoReleaser ldflags) and `VersionString()` for `--version`. Location: `internal/version/version.go`.
- `internal/testutil` - Generates synthetic ID3-tagged audio files for load/concurrency tests and the genlib tool. Location: `internal/testutil/`.
## Commit Conventions
- Prefixes: `feat:`, `fix:`, `docs:`, `style:`, `refactor:`, `perf:`, `test:`, `build:`, `ci:`, `chore:`, `revert:`
- No emoji in commits, code, comments, or documentation
## Spell Checking
- `go.sum` is excluded from spell checking
- Runs as both pre-commit hook check and pre-commit framework hook
## Secret Scanning
- Scans for accidentally committed secrets/credentials

## Architecture

## Pattern Overview
- `cmd/mxlrcgo-svc/` contains the CLI entry point; all business logic lives under `internal/`
- A subcommand tree (`internal/commands`) selects between a stateless one-shot `fetch` mode and stateful, long-running `serve`/`scan` modes
- Layered architecture with dependency injection via interfaces (`musixmatch.Fetcher`, `lyrics.Writer`, `auth.Store`, provider/orchestrator seams) -- mock at the boundary
- No global mutable state -- state is owned by the constructed structs (`App`, `Worker`, server `Handler`)
- Fetch mode: sequential processing loop with a cooldown timer between API calls
- Serve mode: durable SQLite work queue drained by a background worker, fed by a library scan scheduler (+ optional filesystem watcher), with an HTTP API and browser UI in front
- Context propagation throughout for graceful shutdown on Ctrl+C / SIGTERM
## Layers
- Purpose: Parse CLI arguments, resolve token, orchestrate startup
- Location: `cmd/mxlrcgo-svc/main.go`
- Contains: `main()`, `Args` struct
- Depends on: `go-arg`, `godotenv`, `internal/app`, `internal/lyrics`, `internal/musixmatch`, `internal/scanner`
- Used by: Nothing (top-level entry point)
- Purpose: Orchestrate the main processing loop, manage work queues
- Location: `internal/app/app.go`, `internal/app/queue.go`
- Contains: `App` struct, `NewApp()`, `Run()`, `timer()`, `handleFailed()`, `InputsQueue`, `NewInputsQueue()`
- Depends on: `musixmatch.Fetcher`, `lyrics.Writer`, `internal/models`
- Used by: `cmd/mxlrcgo-svc/main.go`
- Purpose: Communicate with the Musixmatch desktop API, parse responses, return structured song data
- Location: `internal/musixmatch/client.go`, `internal/musixmatch/fetcher.go`
- Contains: `Client` struct, `FindLyrics(ctx, track) (Song, error)`, `Fetcher` interface
- Depends on: `net/http`, `fastjson`, `encoding/json`, `internal/models`
- Used by: `internal/app` (via `Fetcher` interface)
- Purpose: Format song data into LRC format and write to disk
- Location: `internal/lyrics/writer.go`, `internal/lyrics/slugify.go`
- Contains: `LRCWriter`, `WriteLRC()`, `writeSyncedLRC()`, `writeUnsyncedLRC()`, `writeInstrumental()`, `Slugify()`, `Writer` interface
- Depends on: `internal/models`, `golang.org/x/text/unicode/norm`
- Used by: `internal/app` (via `Writer` interface)
- Purpose: Input parsing and directory scanning
- Location: `internal/scanner/scanner.go`
- Contains: `Scanner`, `ParseInput()`, `GetSongMulti()`, `GetSongText()`, `GetSongDir()`, `AssertInput()`
- Depends on: `dhowden/tag`, `internal/app`, `internal/models`
- Used by: `cmd/mxlrcgo-svc/main.go`
- Purpose: Define all shared data structures
- Location: `internal/models/models.go`
- Contains: `Track`, `Song`, `Lyrics`, `Synced`, `Lines`, `Time`, `Inputs`
- Depends on: Nothing (pure data definitions)
- Used by: Every other layer
## Data Flow
`main()` parses the command tree (`internal/commands`), resolves config (`internal/config`) and, for stateful subcommands, opens the SQLite DB (`internal/db`), then dispatches to the selected subcommand. Two principal flows exist:

### Fetch mode (`fetch`, one-shot, stateless)
- `scanner.ParseInput()` populates an in-memory `*queue.InputsQueue`
- `app.NewApp()` accepts the populated queue plus injected `Fetcher` and `Writer` dependencies
- `app.Run(ctx)` processes the queue sequentially with a cooldown between API calls; failed items accumulate in a separate `failed` queue
- Context cancellation (Ctrl+C / SIGTERM) exits the loop; `handleFailed()` writes a timestamp-named `_failed.txt` retry file
- No state is persisted between runs (except that retry file)

### Serve mode (`serve` / `scan`, stateful, long-running)
- `scan` (or the in-process scheduler under `serve`) walks configured library roots (`internal/library`, `internal/scan`), records `scan_results`, and enqueues missing-lyric work items into the durable SQLite `queue.DBQueue`
- An optional `internal/watcher` lowers scan latency by triggering targeted re-scans on filesystem events; the periodic scheduler remains the source of truth
- The `internal/worker` dequeues work items and resolves lyrics through the multi-provider `internal/orchestrator` (Musixmatch + petitlyrics lanes, each behind an `internal/circuit` breaker with `internal/backoff` retry cadence), consulting/updating the `internal/cache`, then writes output via `internal/lyrics`
- Optional sidecars (`internal/verification`, `internal/detector` via `internal/ffmpeg`) gate or classify results; `internal/langguard` filters by language
- The `internal/server` HTTP handler exposes API endpoints (authenticated via `internal/auth` API keys, IP-gated by `internal/trustnet`, optionally TLS via `internal/servetls`), and `internal/web` serves the browser UI behind `internal/webauth` sessions
- Secrets are read from the encrypted `internal/secrets` store; `internal/reports` answers read-only queries over the accumulated DB state
- All durable state lives in the WAL-mode SQLite DB; the process is restart-safe and resumes from the queue
## Key Abstractions
- Purpose: FIFO queue holding items to process (or that failed)
- Location: `internal/app/queue.go`
- Pattern: Slice-backed queue; `Next()` peeks, `Pop()` removes and returns `(models.Inputs, error)`, `Push()` appends
- Purpose: API client for the Musixmatch desktop endpoint
- Location: `internal/musixmatch/client.go`
- Pattern: Struct with `Token` and shared `*http.Client`; implements `Fetcher` interface
- Purpose: Represent the full result from a lyrics lookup
- Location: `internal/models/models.go`
- Pattern: Nested value types. `Song` composes `Track`, `Lyrics`, and `Synced`; JSON tags map to Musixmatch API fields
- Purpose: Represent a single work item in the processing queue
- Location: `internal/models/models.go`
- Pattern: Bundles a `Track` (what to search for) with `Outdir` and `Filename` (where to write output)
- Purpose: Durable SQLite-backed work queue for serve/worker mode
- Location: `internal/queue/queue.go`
- Pattern: `DBQueue` struct with `Enqueue`, `Dequeue`, `Complete`, `Fail`, `Cleanup`, plus inspection/maintenance methods `List(ctx, ListFilter)`, `Retry(ctx, id)` (rejects non-failed rows with `ErrNotRetryable`), `ClearDone(ctx)`, and `CountDone(ctx)`. Used by the `queue list / failed / retry / clear` CLI subcommands and the worker.
- Dequeue ordering: `Dequeue` shuffles within each priority tier (`ORDER BY priority DESC, RANDOM()`) by default to avoid a strictly-alphabetical scraping fingerprint. The unexported `randomized` field (true by default) selects between two complete SQL constants; `SetRandomized(bool)` applies the `[queue] randomize` config key / `MXLRC_QUEUE_RANDOMIZE` env var (default true). `List` output stays deterministic.
- Purpose: Persistence for library scan_results rows
- Location: `internal/scan/repository.go`
- Pattern: `Repo` struct with `Upsert`, `ListByLibrary`, `ListPendingByLibrary`, `SetStatus`, plus inspection/maintenance methods `List(ctx, Filter)` (filters by optional `LibraryID` and `Status`), `ClearByLibrary(ctx, libraryID)`, and `CountByLibrary(ctx, libraryID)`. Used by the `scan results` and `scan clear` CLI subcommands and the scheduler.
- Purpose: Library root CRUD (with name lookup for CLI ergonomics)
- Location: `internal/library/repository.go`
- Pattern: `Repo` struct with `Add`, `List`, `Get`, `GetByName`, `Update`, `Remove`. `GetByName` lets `scan` and other commands accept either a numeric library id or a human-readable name from `--library`.
## Entry Points
- Location: `cmd/mxlrcgo-svc/main.go` (`func main()`)
- Triggers: Direct CLI invocation (`canticle [args]`)
- Responsibilities: Parses args, loads token, creates scanner/fetcher/writer, runs processing loop, handles failures and graceful shutdown
- Location: `Makefile` and `.goreleaser.yml`
- Triggers: `make build` or GoReleaser on tag push
- Responsibilities: Compiles `cmd/mxlrcgo-svc` into the `canticle` binary
## Runtime Error Handling
- `FindLyrics()` returns `(Song, error)` -- caller logs the error and pushes to the `failed` queue
- `WriteLRC()` returns `error` -- uses named return `retErr` to capture deferred close errors
- `Next()` / `Pop()` return `(models.Inputs, error)` -- error on empty queue prevents nil-deref panics
- File I/O errors during input parsing return `error` up to `main()`, which calls `os.Exit(1)`
- HTTP status codes mapped to specific error messages: 401 = "too many requests", 404 = "no results found"
- Response body size is capped at 2 MiB to prevent memory exhaustion
- `defer` pattern used for closing `http.Response.Body` and output files
## Cross-Cutting Concerns

## Project Skills

No project skills found. Add skills to any of: `.claude/skills/`, `.agents/skills/`, `.cursor/skills/`, or `.github/skills/` with a `SKILL.md` index file.
