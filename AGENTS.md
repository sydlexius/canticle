## Project

**mxlrcgo-svc (sydlexius/mxlrcgo-svc)**

A Go CLI tool that fetches synced lyrics from the Musixmatch API and saves them as `.lrc` files. The project is being maintained under its own module path, with global state eliminated and the hardcoded API token externalized.

**Core Value:** The tool fetches synced lyrics reliably and writes correct `.lrc` files. Everything else (project structure, config handling, CI) exists to support that.

### Constraints

- **Binary name**: `mxlrcgo-svc` (matches new module name)
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
- `cmd/mxlrcgo-svc/` - CLI entry point only; no business logic
- `internal/models/` - shared data types; no dependencies on other internal packages
- `internal/musixmatch/` - API client + `Fetcher` interface; depends on `models`
- `internal/lyrics/` - LRC writer + `Writer` interface + `Slugify`; depends on `models`
- `internal/scanner/` - input parsing and directory scan; depends on `app` and `models`
- `internal/app/` - orchestration loop, `InputsQueue`; depends on `musixmatch`, `lyrics`, `models`
- No global mutable variables -- all state is owned by `App` struct
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
- Layered architecture with dependency injection via interfaces (`musixmatch.Fetcher`, `lyrics.Writer`)
- No global mutable state -- all state is owned by the `App` struct
- Sequential processing loop with cooldown timer between API calls
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
- `main()` calls `scanner.ParseInput()` which populates an `*app.InputsQueue`
- `app.NewApp()` accepts the populated queue plus injected `Fetcher` and `Writer` dependencies
- `app.Run(ctx)` processes the queue sequentially; failed items accumulate in a separate `failed` queue
- Context cancellation (Ctrl+C / SIGTERM) exits the loop; `handleFailed()` writes a retry file
- State is not persisted between runs (except the timestamp-named `_failed.txt` retry file)
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
- Triggers: Direct CLI invocation (`mxlrcgo-svc [args]`)
- Responsibilities: Parses args, loads token, creates scanner/fetcher/writer, runs processing loop, handles failures and graceful shutdown
- Location: `Makefile` and `.goreleaser.yml`
- Triggers: `make build` or GoReleaser on tag push
- Responsibilities: Compiles `cmd/mxlrcgo-svc` into `mxlrcgo-svc` binary
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
