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

### CI test sharding

CI runs the test suite across parallel `Test Shard` jobs rather than one `go test ./...` (issue #662). The split lives in `scripts/ci-shards.sh`, which is the single source of truth for **both** the shard-name list (consumed by the workflow matrix) and the package map each shard resolves to. Keeping them in one file is deliberate: if a shard were named in the map but missing from the matrix, its packages would be excluded from the dynamic `rest` remainder and run by nobody -- nothing would fail, those packages would just stop being tested.

```sh
bash scripts/ci-shards.sh matrix          # the matrix JSON the workflow consumes
bash scripts/ci-shards.sh names           # shard names, one per line
bash scripts/ci-shards.sh packages <name> # that shard's package list
bash scripts/ci-shards.sh run <name>      # that shard's -run regex (partitioned shards only)
bash scripts/ci-shards.sh verify          # assert exactly-once package coverage
```

`verify` asserts that every package in `go list ./...` lands in exactly one shard, that each named shard's directories exist, and that a partitioned package's buckets are complete and disjoint. It runs in `make gate` and again in the `Go Cache Primer` job, which every shard depends on, so drift fails the pipeline before any test runs. The script needs Bash 4+ (`declare -A`); stock macOS `/bin/bash` is 3.2, so `make gate` skips it locally when only the old shell is present, and CI enforces it.

`rest` is the **dynamic remainder** -- everything no named shard claims -- so a newly added package automatically lands in a shard.

Two packages are heavy enough that a shard of their own would still be the pole (`internal/commands` at 252s, `internal/queue` at 178s of measured CI time), so they are *partitioned by test name*: a round-robin over sorted test names splits each into buckets (`commands-1..3`, `queue-1..2`) selected with `-run`. Before relying on a new partition, confirm each bucket passes alone, shuffled, under `-race`.

To rebalance, edit the `SHARDS` / `BUCKETS` maps at the top of the script and re-run `verify`; the matrix regenerates from them. Base the split on **CI** timings from the job log, not local ones -- the ratio between the two is not stable.

Each shard uploads its own `coverage-<shard>` artifact. The `Coverage Floor` and `Upload Coverage` jobs merge them with `go tool gocovmerge` (pinned via the `go.mod` tool directive) before use. Do **not** replace that with a `cat`: each shard profile repeats the mode line and blocks, and `coverage-floor.sh` sums `nstmts` per matching line, so concatenation inflates the denominator while the covered numerator stays flat -- producing a plausible-looking wrong number rather than an obvious failure.

### Coverage floor (one-way ratchet)

`make coverage-floor` (`scripts/coverage-floor.sh`) enforces a per-package floor recorded in `scripts/coverage-floor.json`: a PR that drops any `internal/` package below its floor fails the check, even if Codecov's patch coverage passes. It complements patch coverage (which only sees changed lines) by guarding whole-package regressions. The script is pure awk (no `jq`) and reuses the test step's coverage profile via `COVER_OUT` when one is supplied.

Floors move **one way at a time**, per package, never via a bulk overwrite:

```sh
# After adding tests that genuinely raise a package's coverage, ratchet its
# floor up to the new measured value (refuses to lower):
bash scripts/coverage-floor.sh --bump internal/<pkg>

# Only for a PR that removes dead (uncovered) code and so legitimately lowers
# the ratio (refuses if current >= floor; the PR must explain the removal):
bash scripts/coverage-floor.sh --lower internal/<pkg>
```

Ratchet to *current actuals*, not aspirational targets - do not nickel-and-dime coverage on defensive or unreachable branches. `internal/web` is intentionally excluded (its tests need the `make ui` CSS asset, so they can't run in a bare `go test`); Codecov covers it. Commit the `--bump`/`--lower` JSON change in the same PR that earned it, citing the change in the commit message.

## Documentation site

The documentation site (this site) is built with [ProperDocs](https://github.com/properdocs/properdocs), a maintained drop-in continuation of MkDocs 1.x, using the Material theme. The pages live under `docs/` and the config is `properdocs.yml` at the repo root.

```sh
make docs-deps    # install the Python doc tooling (pip install --require-hashes -r dev-requirements.lock)
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

See `CLAUDE.md` (the "Architecture" orientation and "Package catalog" sections) for a deeper reference on the package surface, architecture, and data flow.

## Design decisions

- [Multilingual lyric output policy](multilingual-output-policy.md) - how the writer handles songs with an original and a translation: a single bilingual `.lrc` where the original and translation lines share one timestamp. Several code comments under `internal/` reference this policy.
- [Multi-provider orchestration](multi-provider-orchestration.md) - how multiple lyrics-provider lanes run together: ordered fallback by default (parallel race opt-in), per-lane circuit breakers, a single-writer dedup guarantee via the `queue.Complete` CAS, and the cross-lane error precedence that backs off rather than recording a false miss.
