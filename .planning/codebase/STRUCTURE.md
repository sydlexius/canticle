# Codebase Structure

**Analysis Date:** 2026-04-10 (updated post-M0 restructure)

## Directory Layout

```
mxlrc-go/
├── cmd/
│   └── mxlrcgo-svc/
│       └── main.go             # CLI entry point, token resolution, dependency wiring
├── internal/
│   ├── app/
│   │   ├── app.go              # App struct, Run(ctx), timer, handleFailed
│   │   └── queue.go            # InputsQueue FIFO implementation
│   ├── lyrics/
│   │   ├── writer.go           # LRCWriter, WriteLRC, write{Synced,Unsynced,Instrumental}LRC
│   │   ├── slugify.go          # Slugify() filename sanitization
│   │   └── slugify_test.go     # Tests for Slugify
│   ├── models/
│   │   └── models.go           # Track, Song, Lyrics, Synced, Lines, Time, Inputs
│   ├── musixmatch/
│   │   ├── client.go           # Client struct, FindLyrics(ctx, Track)
│   │   └── fetcher.go          # Fetcher interface
│   └── scanner/
│       └── scanner.go          # Scanner, ParseInput, GetSong{Multi,Text,Dir}, AssertInput
├── .claude/                    # Claude Code configuration
│   ├── commands/               # Custom slash commands
│   ├── settings.json
│   └── settings.local.json     # (gitignored)
├── .githooks/
│   └── pre-commit              # Pre-commit quality checks
├── .github/
│   ├── workflows/
│   │   ├── ci.yml
│   │   ├── codeql.yml
│   │   ├── release.yml
│   │   ├── dependabot-auto-approve.yml
│   │   └── dependabot-merge.yml
│   └── dependabot.yml
├── .planning/                  # Planning documents
│   └── codebase/               # Codebase analysis docs
├── go.mod                      # Module: github.com/doxazo-net/canticle, Go 1.25
├── go.sum
├── Makefile
├── README.md
├── LICENSE
├── .golangci.yml
├── .goreleaser.yml
├── .pre-commit-config.yaml
├── .editorconfig
├── .gitattributes
├── .gitignore
├── .typos.toml
└── .coderabbit.yml
```

## Directory Purposes

**`cmd/mxlrcgo-svc/`:**
- Purpose: Binary entry point — the only `package main`
- Contains: `main.go` (token resolution, signal context, dependency wiring, `App.Run`)
- Key file: `main.go`

**`internal/app/`:**
- Purpose: Processing orchestration and queue management
- Contains: `App` struct (owns all state), `InputsQueue` FIFO
- Key files: `app.go`, `queue.go`

**`internal/musixmatch/`:**
- Purpose: Musixmatch API client
- Contains: `Client` implementation, `Fetcher` interface
- Key files: `client.go`, `fetcher.go`

**`internal/lyrics/`:**
- Purpose: LRC file formatting and writing
- Contains: `LRCWriter`, `Writer` interface, `Slugify()`
- Key files: `writer.go`, `slugify.go`, `slugify_test.go`

**`internal/models/`:**
- Purpose: Shared data types (leaf package — no internal imports)
- Contains: All structs used across packages
- Key file: `models.go`

**`internal/scanner/`:**
- Purpose: Input parsing and directory scanning
- Contains: `Scanner`, mode detection, audio metadata reading
- Key file: `scanner.go`

**`.github/workflows/`:**
- Purpose: CI/CD pipeline definitions
- Contains: 5 workflow YAML files
- Key files: `ci.yml` (lint/test/build), `release.yml` (GoReleaser)

**`.githooks/`:**
- Purpose: Local git hook scripts (installed via `make hooks`)
- Contains: `pre-commit` hook running typos, gofmt, go build, golangci-lint, govulncheck

**`.planning/codebase/`:**
- Purpose: Codebase analysis documents consumed by planning/execution agents
- Generated: Yes (by codebase mapping)
- Committed: Yes

## Key File Locations

**Entry Points:**
- `cmd/mxlrcgo-svc/main.go`: CLI entry point. `func main()` — token resolution, wires all dependencies, calls `App.Run(ctx)`.

**Configuration:**
- `go.mod`: Module path `github.com/doxazo-net/canticle`, Go 1.25
- `.golangci.yml`: Linter rules (errcheck, govet, staticcheck, gosec, revive, etc.)
- `.goreleaser.yml`: Cross-platform build matrix — binary name `mxlrcgo-svc`, build path `./cmd/mxlrcgo-svc`
- `Makefile`: Build/test/lint/format commands
- `.pre-commit-config.yaml`: Pre-commit framework hooks

**Core Logic:**
- `internal/musixmatch/client.go`: API client. `Client.FindLyrics(ctx, Track) (Song, error)`.
- `internal/lyrics/writer.go`: LRC output. `LRCWriter.WriteLRC(Song, string, string) error`.
- `internal/scanner/scanner.go`: Input parsing. `Scanner.ParseInput(...)`.
- `internal/models/models.go`: All shared types. `Track`, `Song`, `Lyrics`, `Synced`, `Lines`, `Time`, `Inputs`.
- `internal/app/queue.go`: Queue. `InputsQueue` with safe `Next()`/`Pop()` returning `(models.Inputs, error)`.

**Testing:**
- `internal/lyrics/slugify_test.go`: Unit tests for `Slugify()`. Only test file in the project.

## Naming Conventions

**Files:**
- Go source: lowercase, single-word or compound-word names: `main.go`, `client.go`, `writer.go`, `slugify.go`
- Test files: `{source}_test.go` pattern: `slugify_test.go`
- Config files: dotfiles with standard names: `.golangci.yml`, `.goreleaser.yml`, `.editorconfig`

**Functions:**
- PascalCase for all exported: `FindLyrics`, `WriteLRC`, `ParseInput`, `GetSongDir`, `Slugify`, `AssertInput`
- camelCase for unexported helpers: `writeSyncedLRC`, `writeUnsyncedLRC`, `writeInstrumentalLRC`
- Method receivers: short names matching struct initial: `c *Client`, `w *LRCWriter`, `q *InputsQueue`, `sc *Scanner`

**Variables:**
- camelCase throughout new code
- Package-level globals eliminated — state lives in `App` struct

**Types:**
- PascalCase structs: `Track`, `Song`, `Lyrics`, `Synced`, `Lines`, `Time`, `Inputs`, `InputsQueue`, `App`, `Client`, `LRCWriter`, `Scanner`
- JSON tags use Musixmatch API field names: `json:"track_name,omitempty"`

**Constants:**
- `apiURL` in `internal/musixmatch/client.go` (unexported, package-level)

## Where to Add New Code

**New Feature (e.g., new input mode, new output format):**
- Musixmatch API client: `internal/musixmatch/`
- Output formatting: `internal/lyrics/`
- Input parsing/scanning: `internal/scanner/`
- New types: `internal/models/models.go`
- CLI flags/orchestration: `cmd/mxlrcgo-svc/main.go`
- Tests: `{package}_test.go` alongside source in same `internal/` package

**New Internal Package:**
- Create `internal/<name>/` with focused responsibility
- Define an interface in the consuming package (not the implementing package)
- Depend only on `internal/models` and Go stdlib where possible

**New External Integration:**
- Add dependency: `go get <package>` (updates `go.mod` and `go.sum`)
- Wrap in a new `internal/` package

**New CI/CD Step:**
- Workflow files: `.github/workflows/`
- Local hooks: `.githooks/pre-commit`

## Special Directories

**`.planning/`:**
- Purpose: Codebase analysis and planning documents for AI-assisted development
- Generated: Yes (by GSD codebase mapper)
- Committed: Yes

**`.claude/`:**
- Purpose: Claude Code configuration and custom commands
- Generated: Partially (settings are manual, some commands are generated)
- Committed: Yes (except `settings.local.json`)

**`.opencode/`:**
- Purpose: OpenCode/GSD (Get Shit Done) framework configuration, agents, workflows
- Generated: Yes (framework installation)
- Committed: Yes

**`.serena/`:**
- Purpose: Serena MCP configuration
- Generated: Yes
- Committed: No (gitignored)

**`dist/` (when present):**
- Purpose: GoReleaser build output
- Generated: Yes (by `goreleaser build`)
- Committed: No (gitignored)

---

*Structure analysis: 2026-04-10 | Updated post-M0: 2026-04-11*
