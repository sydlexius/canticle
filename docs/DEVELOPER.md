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

`make scan` requires [grype](https://github.com/anchore/grype) at the version pinned in `.github/workflows/ci.yml` (the `grype-version` input on the Image Scan job). That file is the single source of truth -- `scripts/check-tool-versions.sh` parses the pin out of it rather than carrying its own copy, so this page deliberately does not restate the number. Install that version and `make doctor` will verify the local binary matches. CI runs grype with `only-fixed: true` to suppress CVEs that have no released fix, reducing flakes from transient vuln-DB churn that cannot be actioned.

Other useful targets:

```sh
make smoke               # lightweight CLI smoke test
make test                # race tests
make test-shuffle        # race tests with randomized order (-shuffle=on)
make test-cover          # coverage profile + HTML report
make coverage-floor      # enforce the per-package coverage floor
make vulncheck           # govulncheck (pinned)
make scan                # build the Docker image and scan it for HIGH+ CVEs (needs Docker + grype at the ci.yml pin)
make sync-tool-versions  # assert the golangci-lint and grype pins match across CI and local
```

### Live serve smoke (manual)

A manual end-to-end check of a build against the real providers. It is never run in CI: it talks to live third-party APIs and spends a rate-limited token mint.

**1. Generate the fixture library.** Copy `scripts/smoke-fixtures.example.toml` to `smoke-fixtures.local.toml` at the repo root (gitignored) and list a few well-known songs with their real lengths (`duration = 225` or `"3:45"`). Never commit real titles.

```sh
make smoke-fixtures                                   # OUT=/tmp/canticle-smoke-fixtures TRACKS=smoke-fixtures.local.toml
make smoke-fixtures OUT=/tmp/smoke-lib TRACKS=$HOME/my-tracks.toml
make smoke-fixtures CLEAN=1                           # regenerate into a non-empty OUT
```

Each track becomes a silent, ID3-tagged mono MP3 of exactly the listed length. The length matters: the timing guard judges a synced lyric against the audio duration, so a short file demotes a correct `.lrc` to `.txt`. Every duration is re-read with the same reader serve uses, and a mismatch over 1s fails the run. A nonsense-tagged negative control is always added. ffmpeg comes from the checksum-pinned provisioning in `internal/ffmpeg` (override with `go run ./cmd/smokefixtures -ffmpeg <path>`). An `OUT` inside the repo tree is refused, and so is a non-empty one (a stale `.lrc` there would make the scanner skip its track and the smoke pass on old results): `CLEAN=1` removes only the `NN - *.mp3` fixtures the tool wrote and their same-stem `.lrc`/`.txt`/`.lrc.orig` sidecars.

**2. Run serve in an isolated config/data dir.** Keep it away from your real config, database and token. `env -i` drops every exported `MXLRC_*`/`MUSIXMATCH_*` variable (a token, `MXLRC_DB_PATH`, `MXLRC_SECRETS_KEY_FILE`, `MXLRC_PROVIDERS_FALLBACK_ORDER`, ...) so none leaks in; with no token, serve mints one:

```sh
S=/tmp/canticle-smoke
mkdir -p "$S/config/mxlrcgo-svc"
printf '[providers]\nprimary = "musixmatch"\nfallback_order = []\ndisabled = []\n' > "$S/config/mxlrcgo-svc/config.toml"
iso() { env -i HOME="$HOME" PATH="$PATH" XDG_CONFIG_HOME="$S/config" XDG_DATA_HOME="$S/data" "$@"; }
iso go run ./cmd/mxlrcgo-svc library add /tmp/canticle-smoke-fixtures --name smoke
iso go run ./cmd/mxlrcgo-svc serve
```

`primary = "musixmatch"` with `fallback_order = []` pins attribution, and `disabled = []` ensures an inherited `providers.disabled` cannot silently skip the lane: every result comes from the one lane under test. Start serve **exactly once** for the mint: the token endpoint rate-limits per egress IP, so never loop or script restarts around a failed mint. The log should say `bootstrapped a musixmatch token and persisted it`.

**3. Check the results.**

- Each real track gets an `.lrc` next to it carrying `[source:musixmatch]` (a `.txt` for a song with only unsynced lyrics).
- The negative control ends as a miss (no `.lrc`/`.txt`, queue status deferred, `unavailable` once retired). A lyric for it is the decoy-payload failure fixed in #939.
- Stop and restart serve once: it must reuse the stored token (no new bootstrap line in the log).

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
