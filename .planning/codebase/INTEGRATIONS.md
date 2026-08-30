# External Integrations

**Analysis Date:** 2026-04-10

## APIs & External Services

**Musixmatch Lyrics API:**
- Purpose: Fetch synced lyrics, unsynced lyrics, and track metadata for songs
- Base URL: `https://apic-desktop.musixmatch.com/ws/1.1/macro.subtitles.get`
- SDK/Client: Direct HTTP via Go `net/http` standard library (`internal/musixmatch/client.go`)
- Auth: User token passed as `usertoken` query parameter. Resolved with precedence: `--token` CLI flag > `MUSIXMATCH_TOKEN` env var > `.env` file (via godotenv). No hardcoded fallback.
- HTTP method: GET
- Request timeout: 30 seconds (`musixmatch.go` line 48)
- Response parsing: `github.com/valyala/fastjson` for initial navigation, then `encoding/json` Unmarshal for typed structs
- Response size limit: 2 MiB (`musixmatch.go` line 77)
- Rate limiting: Client-side cooldown timer between requests (default 15 seconds, configurable via `-c/--cooldown`)
- Custom headers: `authority: apic-desktop.musixmatch.com`, `cookie: x-mxm-token-guid=`
- Query parameters: `format`, `namespace`, `subtitle_format`, `app_id`, `usertoken`, `q_album`, `q_artist`, `q_artists`, `q_track`, `track_spotify_id`, `q_duration`, `f_subtitle_length`

**API Response Structure:**
- Deeply nested JSON: `message.body.macro_calls.{matcher.track.get|track.lyrics.get|track.subtitles.get}.message`
- Track data: Artist, title, album, track length, instrumental flag, has_lyrics, has_subtitles
- Synced lyrics: Array of `{text, time: {total, minutes, seconds, hundredths}}` objects
- Unsynced lyrics: Plain text body
- Error codes: 401 (rate limited / invalid token), 404 (not found)

**API Error Handling** (`musixmatch.go`):
- HTTP 401: "too many requests: increase the cooldown time and try again in a few minutes"
- HTTP 404: "no results found"
- Other HTTP errors: Reads up to 8KB of error body for diagnostics
- JSON-level 401 with `hint: "renew"`: "invalid token"
- JSON-level 404: "no results found"

## Data Storage

**Databases:**
- None currently in use
- CLAUDE.md documents a planned pattern for future stateful features: `modernc.org/sqlite` (pure Go, no CGO), WAL mode, goose migrations, repository pattern

**File Storage:**
- Local filesystem only
- Output: `.lrc` files written to `--outdir` (default `lyrics/`) or alongside audio files in directory mode
- Failed items: `{timestamp}_failed.txt` written to current directory on partial failure (not in directory mode)
- File creation: `os.Create` with buffered writes via `bufio.Writer`

**Caching:**
- None

## Authentication & Identity

**Auth Provider:**
- None (CLI tool, no user authentication)
- Musixmatch API token is the only credential, supplied via CLI flag, environment variable, or `.env` file (no compiled default)

## Monitoring & Observability

**Error Tracking:**
- None (CLI tool)

**Logs:**
- Go standard `log` package
- Logs to stderr (Go default)
- Log messages: search progress, API errors, file write success/failure, skipped files in directory mode

## CI/CD & Deployment

**Hosting:**
- GitHub Releases (binary distribution)
- No server deployment (standalone CLI tool)

**CI Pipeline:**
- GitHub Actions
- Workflows: `ci.yml` (lint/test/build), `release.yml` (GoReleaser), `codeql.yml` (security), `dependabot-auto-approve.yml`
- Runner: `ubuntu-latest` for all workflows

**Release Process:**
- Tag `v*.*.*` triggers GoReleaser
- Produces archives: `.tar.gz` (linux/darwin), `.zip` (windows)
- Changelog generated from conventional commits (groups: Features, Bug Fixes, Performance, Refactoring, Other)
- GitHub Release created with `GITHUB_TOKEN`

## Environment Configuration

**Required env vars:**
- None if `--token` CLI flag is provided; otherwise `MUSIXMATCH_TOKEN` must be set (or present in `.env`)

**Optional:**
- `.env` file in working directory — loaded via `godotenv` before token resolution

**CI/CD secrets:**
- `GITHUB_TOKEN` - Used by GoReleaser for release creation and by Dependabot workflows for PR management

**Secrets location:**
- GitHub Actions secrets (repository settings)
- Optional local `.env` file for token loading (gitignored)

## Webhooks & Callbacks

**Incoming:**
- None (CLI tool, no server)

**Outgoing:**
- None

## Audio File Metadata Reading

**Library:** `github.com/dhowden/tag`
- Purpose: Read artist/title metadata from audio files in directory-scan mode
- Used in: `internal/scanner/scanner.go` `GetSongDir()`
- Supported formats: `.mp3`, `.m4a`, `.m4b`, `.m4p`, `.alac`, `.flac`, `.ogg`, `.dsf` (defined in `supportedFType()`)
- Metadata fields used: `Artist()`, `Title()`

---

*Integration audit: 2026-04-10*
