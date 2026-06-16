# Developer Guide

This page covers building from source, the make targets, the quality gate, contributing, and the project's design decisions.

## Development setup

Requires Go 1.26.2 or newer.

The entrypoint lives in `cmd/mxlrcgo-svc`, so `go run .` does not work. Use:

```sh
go run ./cmd/mxlrcgo-svc [args]
```

`make help` lists every target.

## Quality gate and git hooks

Wire the tracked git hooks once (this sets `core.hooksPath=.githooks`, a relative shared setting, so every worktree -- including any you add later -- inherits them with no extra setup):

```sh
make hooks      # enable the pre-commit + pre-push hooks
make doctor     # verify the hooks are wired and tool-version pins agree
```

`make gate` runs the full pre-push gate (the same chain `.githooks/pre-push` runs): conflict-marker check, gofmt, build, race tests, patch coverage, golangci-lint, actionlint, and govulncheck. The pre-commit hook runs a faster subset on each commit.

`make scan` requires [grype](https://github.com/anchore/grype) v0.114.0 (the version pinned in CI). Install it and `make doctor` will verify the local version matches. CI runs grype with `only-fixed: true` to suppress CVEs that have no released fix, reducing flakes from transient vuln-DB churn that cannot be actioned.

Other useful targets:

```sh
make smoke               # lightweight CLI smoke test
make test                # race tests
make test-shuffle        # race tests with randomized order (-shuffle=on)
make test-cover          # coverage profile + HTML report
make coverage-floor      # enforce the per-package coverage floor
make vulncheck           # govulncheck (pinned)
make scan                # build the Docker image and scan it for HIGH+ CVEs (needs Docker + grype v0.114.0)
make sync-tool-versions  # assert the golangci-lint and grype pins match across CI and local
```

## Documentation site

The documentation site (this site) is built with [ProperDocs](https://github.com/properdocs/properdocs), a maintained drop-in continuation of MkDocs 1.x, using the Material theme. The pages live under `docs/` and the config is `properdocs.yml` at the repo root.

```sh
make docs-deps    # install the Python doc tooling (pip install -r dev-requirements.txt)
make docs-serve   # live-reload preview at http://127.0.0.1:8000
make docs         # strict build into ./site (the same check CI runs)
```

CI publishes the site to GitHub Pages via `.github/workflows/pages.yml`. The build job installs from the hash-pinned `dev-requirements.lock` and runs `properdocs build --strict`; the deploy job runs only on `push`/`workflow_dispatch`.

## Contributing

- Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`, `ci:`, `chore:`, etc.
- Run `make gate` before opening a pull request.
- Use `slog` for structured logs; `fmt.Printf` only for direct user-facing CLI output (timer, counts).
- Wrap errors with `fmt.Errorf("context: %w", err)`.
- Formatting, naming, and file layout are enforced by `gofmt` and `.golangci.yml` -- follow the linter.

See `AGENTS.md` in the repository for a deeper reference on the stack, conventions, architecture, and data flow.

## Design decisions

- [Multilingual lyric output policy](multilingual-output-policy.md) - how the writer handles songs with an original and a translation: a single bilingual `.lrc` where the original and translation lines share one timestamp. Several code comments under `internal/` reference this policy.
- [Multi-provider orchestration](multi-provider-orchestration.md) - how multiple lyrics-provider lanes run together: ordered fallback by default (parallel race opt-in), per-lane circuit breakers, a single-writer dedup guarantee via the `queue.Complete` CAS, and the cross-lane error precedence that backs off rather than recording a false miss.
