# CLAUDE.md

Repo-specific principles and pitfalls only. This file is loaded into every session and every spawned agent, so it stays short.

> **On resume / handoff, read the gitignored `SESSION-STATE.md` at the repo root FIRST.** It is the running orchestration checkpoint (current main SHA, in-flight PRs/worktrees, ordered NEXT ACTIONS, standing directives). Transient session state lives there, never in this file or the auto-memory.

> **Longer-form working notes (problems, methods, fixes, measurements; newest first) live in the tracked `session-management.md` at the repo root; read it on resume as well.** Durable rules stay in this file.

## Where things are

- `docs/ARCHITECTURE.md` -- project overview, architecture orientation, one-line-per-package catalog.
- `docs/DEVELOPER.md` -- gate chain, git hooks, make targets, linter and CI detail, CLI and database conventions.
- `REVIEW.md` -- the defect classes a diff reviewer checks (hard invariants, destructive paths, privacy).
- `internal/<pkg>/CLAUDE.md` -- that package's invariants; loaded only when working in that directory.

**Do NOT add architecture, package descriptions or per-package detail back to this file.** A new package or command gets one line in `docs/ARCHITECTURE.md`; a load-bearing rule for one package goes in its nested `CLAUDE.md` (or a comment at the code site); measurements and history go to `session-management.md`.

## Memory MCP: query it, it will not come to you

Durable lessons live in the `memory-canticle` MCP server (repo tier) and `memory-global` (user tier). The store is PULL-only: it returns nothing unless you call `search_nodes`. An interactive session gets an index at startup, but **spawned subagents do not**, so when dispatching one into an area below, query first and put the answer in its brief, or tell it to query.

QUERY BEFORE, not after, each of these:

- **Releases** - notes formatting, and reading a red Release workflow correctly.
- **Unraid / prod ops** - compose stack paths, picking up a new image, long detached jobs, egress.
- **Debugging a "stuck" or idle-looking pipeline** - queue timestamps, log levels, throttling. There is a standing "do not re-test this" result here; check before re-running an experiment.
- **Review / merge mechanics** - what the merge oracle actually counts, and thread resolution.
- Any "why is it done this way here" question where a prior decision or a refuted hypothesis exists.

Natural phrasing works (word order, plurals, hyphens and camelCase are handled); prefer one broad query over guessing the stored wording. A miss costs one call; a silently-missed constraint has cost real work here more than once. The auto-memory `.md` files remain canonical (the MCP holds a one-line hook plus a `Detail: <file>.md` pointer); do not copy a fact back into the auto-memory index.

## What to work on next

When the user says **"next"**, **"what's next"**, **"keep going"**, or any equivalent lazy prompt with no specific task, inspect the open GitHub issues and milestones before starting, then confirm scope with the user first. Do not assume a fixed backlog order -- the milestones and their dependency chains change as work ships; read the live issue tracker each time.

## Build and test

- The entrypoint is `cmd/mxlrcgo-svc`, so `go run .` does not work: `go run ./cmd/mxlrcgo-svc [args]`.
- After a fresh clone run `make ui` before `go build` (the templ and CSS output is generated and gitignored).
- `make hooks` once to enable the tracked git hooks; `make gate` before pushing; `make help` lists every target.

## Principles and pitfalls

- **Never reintroduce CGO.** SQLite is pure-Go `modernc.org/sqlite`; CGO breaks cross-compilation.
- **No global mutable state.** Inject dependencies through interfaces and mock at the boundary.
- **Integration tests use real SQLite**, not mocks. Open it with `internal/dbtest` (`dbtest.Open(t)` / `dbtest.OpenAt`), not `db.Open`: migrating per test is what made CI slow. Keep `db.Open` only where the test is about the migration, the read-only open, or lock contention.
- **Never `--no-verify`**, on a commit or a push. The only push override is `PUSH_GATE=full`, which is stricter.
- **CI is authoritative** for the race suite and for golangci-lint findings that do not reproduce locally (the `gosec` taint analyzers such as `G704` are prone to version-specific phantoms). Do not chase local reproduction, but read the finding: suppress with a per-site `//nolint:gosec // reason: ...` only after confirming a false positive, and fix a real one. Bumping the CI pin needs a probe PR.
- **Every `//nolint` carries `// reason:`** (enforced by `nolintlint`). Do not add a path to the exclusion block in `.golangci.yml` to silence a new finding.
- **Private library metadata stays off stdout.** A sidecar path, artist, title or lyric line is private; the aggregate-only commands print counts only.
- Conventional commits: `feat:`, `fix:`, `docs:`, `ci:`, `chore:`, etc.
- `slog` for structured logs; `fmt.Printf` only for direct user-facing CLI output (timer, counts).
- Wrap errors with `fmt.Errorf("context: %w", err)`.
- Everything else (formatting, naming, file layout) is enforced by `gofmt` + `.golangci.yml`: follow the linter, not a written rule.

## PR workflow

Use the global slash commands (maintained outside this repo): `/prep-pr` to open a PR, `/handle-review` to triage bot comments, `/merge-pr` to merge + clean up. The catalog and flow live in the user-global instructions, not here. The raw-`gh` history-expansion gotcha is in `docs/DEVELOPER.md`.
