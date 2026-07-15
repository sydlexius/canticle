# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

> **On resume / handoff, read the gitignored `SESSION-STATE.md` at the repo root FIRST.** It is the running orchestration checkpoint (current main SHA, in-flight PRs/worktrees, ordered NEXT ACTIONS, standing directives). Transient session state lives there, never in this file or the auto-memory.

## Project Overview

`canticle` (module `github.com/doxazo-net/canticle`, matching the repo after the migration to the doxazo-net org; the `cmd/mxlrcgo-svc` directory, config paths, and systemd unit names retain the historical `mxlrcgo-svc` string) is a Go tool for fetching synced lyrics. It has two faces: a one-shot `fetch` CLI that writes `.lrc` / `.txt` files, and a stateful `serve` mode -- an HTTP server with a durable SQLite work queue, a background worker, a library scan scheduler (+ optional filesystem watcher), multi-provider orchestration, encrypted-at-rest secrets, and a browser-authenticated web UI. Global state is eliminated; the API token is externalized; config is TOML.

For the full per-package reference, see the "Package catalog" section below. Deeper stack and convention detail is discoverable from `go.mod`, the `Makefile` (`make help`), `.golangci.yml`, and `docs/DEVELOPER.md` -- keep the catalog current when the package surface changes.

## What to work on next

When the user says **"next"**, **"what's next"**, **"keep going"**, or any equivalent lazy prompt with no specific task, inspect the open GitHub issues and milestones before starting, then confirm scope with the user first. Do not assume a fixed backlog order -- the milestones and their dependency chains change as work ships; read the live issue tracker each time.

## Build & Test

`make help` lists every target. Two non-obvious points worth knowing up front:

- The entrypoint lives in `cmd/mxlrcgo-svc`, so `go run .` does not work. Use `go run ./cmd/mxlrcgo-svc [args]`.
- A single test: `go test -run TestFoo ./internal/<pkg>` (tests live next to the code they cover under `internal/`).

Run `make hooks` once to enable the tracked git hooks, and `make gate` before pushing. See "Quality gating and CI" below for the full target list.

## Architecture (one-paragraph orientation)

Cmd/internal layout. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary and owns no business logic; it parses the subcommand tree, loads config + DB, builds the dependency graph, and dispatches. The command tree lives in `internal/commands` (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `completion`). Two principal paths run under `internal/`: **fetch mode** -- `scanner` parses CLI/text-file/directory input into an in-memory `queue.InputsQueue`, `app` drains it sequentially, `musixmatch` fetches (a `Fetcher` interface), and `lyrics` writes `.lrc` / `.txt` / instrumental output (a `Writer` interface); and **serve mode** -- a `scan` scheduler over `library` roots enqueues work into the durable SQLite `queue.DBQueue`, a `worker` drains it through the multi-provider `orchestrator` (Musixmatch + petitlyrics `providers`, each behind a `circuit` breaker with `backoff` retry), consulting `cache`, gated by optional `verification` / `detector` sidecars (via `ffmpeg`) and `langguard`, fronted by the `server` HTTP handler (`auth` API keys, `trustnet` IP gating, optional `servetls`) and the `web` browser UI (`webauth` sessions). Shared infra: `config` (TOML, XDG paths, token precedence CLI > env > file), `db` (pure-Go SQLite `modernc.org/sqlite`, no CGO, goose migrations in `internal/db/migrations/`), `secrets` (AES-256-GCM at rest), `normalize` (NFKC cache keys), `models` (shared types, depends on nothing else internal). Dependencies are injected through interfaces -- mock at the boundary; there is no global mutable state. See the "Package catalog" below for the full `internal/`/`web/` surface.

## Package catalog

Every package with a one-line purpose. `cmd/mxlrcgo-svc/main.go` is the entry point for the released `canticle` binary (`cmd/genlib` is an internal test-data generator, not shipped); everything else lives under `internal/` (the directory matches the package name) except the embedded web assets under `web/`.

**Core fetch/write path**
- `models` -- shared data types (`Track`, `Song`, `Lyrics`, `Synced`, `Inputs`, `Library`, `ScanResult`, ...); depends on nothing else internal.
- `musixmatch` -- Musixmatch desktop API client + `Fetcher` interface; parses the nested JSON into `models`.
- `petitlyrics` -- petitlyrics.com provider adapter, used as a fallback lane.
- `providers` -- provider abstraction (`LyricsProvider`, `Fetcher`, `AdaptivePacer`) plus provider-generation/version invalidation that retires stale cache entries when the provider set changes.
- `orchestrator` -- multi-lane orchestration (`Lane`, `Orchestrator`, parallel-race + suitability scoring); composes `providers` with per-lane `circuit` breakers.
- `circuit` -- concurrency-safe per-lane circuit breaker modeling a provider's rate-limit/throttle response.
- `backoff` -- shared retry-delay formula (1m, 2m, 4m, ..., capped at 1h) used by the worker, durable queue, and fetch loop.
- `lyrics` -- LRC/TXT/instrumental writer (`Writer`, `LRCWriter`), `Slugify`, an `.lrc` parser, provenance-tag embedding, and fsync helpers.
- `normalize` -- NFKC cache-key normalization, duration bucketing, fuzzy-match confidence, album-artist resolution.
- `langguard` -- Unicode-script classification/filtering of lyric text against a configured language allowlist.
- `scanner` -- parses CLI/text-file/directory input into the in-memory queue; skips files that consistently fail metadata read (via the injected `MetadataFailureStore`).
- `app` -- one-shot `fetch`-mode orchestration loop over the in-memory `InputsQueue`; depends on the `Fetcher`/`Writer` interfaces.

**Persistence and stateful services**
- `db` -- pure-Go SQLite (`modernc.org/sqlite`) open/migrate (goose), WAL, foreign keys, busy-retry, and a read-only open path; migrations in `internal/db/migrations/`.
- `cache` -- lyrics cache repository (`CacheRepo`) over SQLite.
- `scanfail` -- `Store` recording files that consistently fail metadata read, so the scanner skips them until mtime/size changes; satisfies `scanner.MetadataFailureStore`.
- `queue` -- the in-memory `InputsQueue` (fetch mode) and the durable SQLite `DBQueue` (serve/worker mode) with priority tiers and randomized within-tier dequeue.
- `library` -- library-root CRUD repository (`Add`/`List`/`Get`/`GetByName`/`Update`/`Remove`).
- `scan` -- library scanning: `Enqueuer`, the `scan_results` `Repo`, and the periodic scheduler that enqueues missing lyrics.
- `worker` -- durable-queue `Worker` that drains work items through the providers/orchestrator and cache.
- `reports` -- read-only, run-on-demand reports over existing SQLite data; no write paths.
- `secrets` -- encrypted-at-rest (AES-256-GCM) store for recoverable runtime secrets, persisted as opaque BLOBs.
- `watcher` -- optional filesystem watcher that triggers targeted library scans on change; complements, never replaces, the periodic scheduler.
- `prune` -- reconciles `work_queue`/`scan_results` against the filesystem: rows whose source audio file has vanished are deleted (`os.Stat` is the sole authority; `Directory` granularity for the periodic sweep, `Exact` for the reactive prune and `scan reconcile-paths`; an in-flight guard defers rows whose linked work is still `processing`).
- `identityrepair` -- re-reads each `scan_results` file's tags (via the injected `IdentityReader` seam) to correct run-together multi-value artist rows ingested before the ID3v2.4 fix (issue #466); updates `scan_results` in place and re-keys the coupled `work_queue` row, merging on the `(artist_key, title_key)` unique conflict and skipping `processing` rows. Each correction's backup record is written and fsynced inside the transaction before the row commits (backup-first / write-ahead: a report failure rolls the change back), so an applied correction always has its restorable record. Shared by the dry-run-by-default `scan reconcile-identity` CLI command and a one-shot, marker-gated serve-mode startup backfill.

**Serve-mode HTTP surface and web UI**
- `server` -- serve-mode HTTP `Handler` plus its seams (`Authenticator`, `WorkQueue`, `Readiness`, `StatusReporter`, `Inventory`, `MetricsReporter`) and metrics.
- `auth` -- stateless API-key authentication (in-memory and SQL `Store`, `Scope`, `Key`) for the HTTP API.
- `webauth` -- browser auth for the web UI: Argon2id password hashing, an admin user store, and a server-side session store (tokens hashed at rest); kept separate from `auth` (different storage/lifecycle/threat model).
- `trustnet` -- client-IP resolution and a trusted-network allowlist, without trusting spoofable headers.
- `servetls` -- optional TLS for the serve listener behind a `CertManager` seam: bring-your-own PEM or a self-signed bootstrap.
- `pathutil` -- path-containment checks confining filesystem targets to configured roots; shared by `server`, `watcher`, `scan`.
- `web` -- serves the web UI (fixed-sidebar shell, Reports placeholder, read-only Config view) from embedded templ templates and `go:embed`'d static assets.
- `web/static` -- compiled CSS and self-hosted fonts embedded into the binary so the UI serves offline.
- `web/templates` -- templ source for the UI shell; generated `*_templ.go` are built on demand and gitignored (run `make ui` after a fresh clone before `go build`).

**Sidecars, config, cross-cutting**
- `verification` -- optional acoustic verification of fetched lyrics (`Verifier`, `HTTPVerifier`) against an external service, using a short audio sample.
- `detector` -- optional audio-based instrumental detection sidecar (external AudioSet/YAMNet classifier, vendored at `deploy/yamnet-detector/`); a three-gated decision (music / sung-vocal / speech gates) sampling short windows across the track in one inference call; a legacy mean-only sidecar degrades safely to never-instrumental.
- `ffmpeg` -- resolves an ffmpeg executable for the sidecars, auto-provisioning a checksum-pinned static build when none is configured or on PATH.
- `config` -- TOML config resolution (XDG paths, registry-driven keys, token precedence CLI > env > file) plus redaction, validation, render/write.
- `logging` -- `slog` logger setup and secret redaction.
- `realign` -- four-tier resolver (`Realigner`, `Move`, `Apply`) that re-attaches orphaned `.lrc`/`.txt` sidecars to renamed or moved audio: exact ISRC/MBID provenance, filesystem heuristic with a Jaro-Winkler name guard, ambiguous, conflict. Backup-first and clobber-safe; shared by the `realign` CLI command and serve-mode reactive realign (watcher / scan / Lidarr-webhook).
- `commands` -- the CLI command tree: top-level `Args` and every subcommand (`fetch`, `serve`, `scan`, `library`, `keys`, `secrets`, `config`, `queue`, `provenance`, `realign`, `completion`). The `realign` subcommand is thin CLI wiring over the `realign` package (above); `scan reconcile-paths` drives `prune` on demand and `scan reconcile-identity` drives `identityrepair` on demand. All share the dry-run/`--yes`/JSONL-backup ergonomics of `scan reconcile`.
- `version` -- build-time `Version`/`Commit`/`Date` (GoReleaser ldflags) and `VersionString()`.
- `testutil` -- generates synthetic ID3-tagged audio for load/concurrency tests and the genlib tool.

## CLI usage and input modes

See `README.md` for flags and examples. Worth flagging: directory mode overrides `--outdir` (writes the output next to the audio file; the extension depends on lyric type - `.lrc` when synced lyrics are found, `.txt` when only unsynced lyrics or an instrumental marker is written), and `--upgrade` re-fetches songs that previously got `.txt` (unsynced) to promote them when synced lyrics become available.

## Quality gating and CI

- Local gate: `make gate` (`scripts/pre-push-gate.sh`) runs the full pre-push chain behind a per-worktree run-lock: conflict-marker check, gofmt, build, race tests, patch coverage (Codecov parity; skipped if the estimator is absent), codecov report validation (`codecovcli do-upload --dry-run`; skipped if `codecovcli` is absent), golangci-lint, actionlint, govulncheck.
- Git hooks: `make hooks` sets `core.hooksPath=.githooks` (a relative, shared git setting), so every worktree -- including new ones -- inherits the hooks with no per-worktree setup. `.githooks/pre-commit` runs a conflict-marker check then typos -> gofmt -> build -> golangci-lint -> govulncheck; `.githooks/pre-push` runs the full gate. Verify the wiring with `make doctor` (or `scripts/check-hooks.sh`).
- Make targets (`make help` lists all): `gate` (full pre-push gate), `doctor` (verify hook wiring + tool-version pins), `scan` (build the Docker image and grype it for HIGH+ CVEs), `test-shuffle` (`go test -race -shuffle=on`), `sync-tool-versions` (assert the golangci-lint pin agrees across CI and pre-commit, via `scripts/check-tool-versions.sh`), `vulncheck` (pinned `govulncheck@v1.1.4`), `coverage-floor` (one-way per-package coverage ratchet over `internal/` via `--bump`/`--lower`, jq-free; `scripts/coverage-floor.sh` + `scripts/coverage-floor.json`; policy in `docs/DEVELOPER.md`).
- Linter config: `.golangci.yml`. Always include a `// reason` comment after any `//nolint:linter` directive. Keep the golangci-lint pin aligned across `ci.yml` and `.pre-commit-config.yaml` (`make sync-tool-versions` enforces it).
- golangci-lint version policy: the version pinned in CI (`ci.yml`) is the source of truth. `make sync-tool-versions` only aligns the config-file pins; it does not pin the binary you have installed locally, so `make gate` / the pre-commit hook can pass locally on a different golangci-lint version while CI flags issues your version does not (and vice versa). The `gosec` taint analyzers (e.g. `G704` SSRF on `httpClient.Do`) are especially prone to version-specific phantom findings that do not reproduce locally. When CI flags one of these, treat CI as authoritative: apply a per-site `//nolint:gosec // reason` (the reason is required) rather than chasing local reproduction. Bumping the CI pin needs a probe PR -- run against a clean cache and confirm no analyzer-regression findings before merging. (Pattern from sydlexius/stillwater `lint-config.instructions.md`.)
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
