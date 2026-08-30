# Technology Stack

**Analysis Date:** 2026-04-10

## Languages

**Primary:**
- Go 1.25 (minimum, per `go.mod`) - All application code

**Secondary:**
- Bash - Pre-commit hooks (`.githooks/pre-commit`), Makefile targets
- YAML - CI/CD workflows (`.github/workflows/`), configuration files

## Runtime

**Environment:**
- Go (compiled binary, no runtime dependency)
- CGO disabled for release builds (`CGO_ENABLED=0` in `.goreleaser.yml`)

**Package Manager:**
- Go Modules (`go.mod` / `go.sum`)
- Lockfile: `go.sum` present

## Frameworks

**Core:**
- None - Pure Go standard library for HTTP, I/O, and CLI orchestration
- `github.com/alexflint/go-arg` v1.6.1 - CLI argument parsing via struct tags (`cmd/mxlrcgo-svc/main.go`)

**Testing:**
- Go standard `testing` package - No third-party test framework

**Build/Dev:**
- Make (`Makefile`) - Build orchestration (build, test, lint, fmt, clean)
- GoReleaser (`.goreleaser.yml`) - Cross-platform release builds
- golangci-lint v2.11.4 (`.golangci.yml`) - Linter aggregator with 12 enabled linters

## Key Dependencies

**Critical (direct):**
- `github.com/alexflint/go-arg` v1.6.1 - CLI argument parsing. Defines the user interface via struct tags on `Args` in `cmd/mxlrcgo-svc/main.go`
- `github.com/dhowden/tag` v0.0.0-20240417053706 - Audio file metadata reading (ID3, MP4, FLAC, OGG, DSF). Used in `internal/scanner/scanner.go` for directory-scan mode
- `github.com/joho/godotenv` v1.5.1 - Optional `.env` file loading for token resolution. Used in `cmd/mxlrcgo-svc/main.go`
- `github.com/valyala/fastjson` v1.6.10 - High-performance JSON parsing for Musixmatch API responses. Used in `internal/musixmatch/client.go`
- `golang.org/x/text` v0.36.0 - Unicode normalization (NFKC) for filename sanitization in `Slugify()` (`internal/lyrics/slugify.go`)

**Indirect:**
- `github.com/alexflint/go-scalar` v1.2.0 - Transitive dependency of go-arg

## Configuration

**Environment:**
- Token resolution: `--token` CLI flag > `MUSIXMATCH_TOKEN` env var > `.env` file (loaded via godotenv)
- No hardcoded token fallback — missing token is a fatal startup error
- Optional `.env` file in working directory for local development

**CLI Arguments** (defined in `cmd/mxlrcgo-svc/main.go` `Args` struct):
- `Song` (positional, required) - Song info as `artist,title` pairs, a `.txt` file path, or a directory path
- `-o/--outdir` (default: `lyrics`) - Output directory for `.lrc` files
- `-c/--cooldown` (default: `15`) - Cooldown between API requests in seconds
- `-d/--depth` (default: `100`) - Max recursion depth for directory scanning
- `-u/--update` - Overwrite existing `.lrc` files in directory mode
- `--bfs` - Use BFS instead of DFS for directory traversal
- `-t/--token` - Musixmatch API token

**Build:**
- `go.mod` - Module definition and Go version constraint
- `.goreleaser.yml` - Cross-compilation targets (linux/darwin/windows, amd64/arm64, excluding windows/arm64)
- `.golangci.yml` - Linter configuration (v2 format)
- `.editorconfig` - Editor formatting (tabs for Go, 2-space for YAML/JSON/MD)
- `.gitattributes` - Line ending normalization (LF everywhere)
- `.typos.toml` - Spell-checker config (excludes `go.sum`)

**Quality Tooling:**
- `.pre-commit-config.yaml` - Pre-commit framework hooks: trailing-whitespace, end-of-file-fixer, check-yaml, check-added-large-files (500KB), check-merge-conflict, typos, gitleaks, golangci-lint, gofmt, conventional-pre-commit
- `.githooks/pre-commit` - Manual pre-commit hook: typos, gofmt, go build, golangci-lint, govulncheck

## Platform Requirements

**Development:**
- Go 1.25+
- golangci-lint v2.11+ (for linting)
- typos-cli (for spell checking)
- govulncheck (for vulnerability scanning)
- goimports (optional, for import formatting)
- pre-commit (optional, for `.pre-commit-config.yaml` hooks)

**Production:**
- Standalone static binary (CGO_ENABLED=0)
- No runtime dependencies
- Supported platforms: linux/amd64, linux/arm64, darwin/amd64, darwin/arm64, windows/amd64

## CI/CD Pipeline

**GitHub Actions Workflows:**
- `ci.yml` - Lint + Test + Build matrix (linux/darwin/windows x amd64/arm64). Uses `dorny/paths-filter` to skip on non-code changes. Build requires lint+test to pass first.
- `release.yml` - GoReleaser on `v*.*.*` tags. Produces cross-platform archives with conventional-commit changelogs.
- `codeql.yml` - GitHub CodeQL security analysis for Go. Runs on push/PR to main and weekly (Monday 04:17 UTC).
- `dependabot-auto-approve.yml` - Auto-approves Dependabot PRs for patch/minor updates.

**Dependabot:**
- Weekly updates (Monday) for `gomod` and `github-actions` ecosystems
- Conventional commit prefixes (`chore(deps)` for Go, `ci` for Actions)

---

*Stack analysis: 2026-04-10*
