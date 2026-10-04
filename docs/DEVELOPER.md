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

`make gate` runs the full local gate: conflict markers, product name, gofmt, web asset generation, build, non-race tests of the changed packages, patch coverage, golangci-lint, actionlint, the PR-trigger scope check, the test-shard split check, and govulncheck. The race suite, coverage floor and codecov dry-run are CI-authoritative and run locally only under `RUN_RACE=1 make gate` (full `go test -race ./...`). `/prep-pr` runs this same chain through `.gates.toml` and records a gate receipt. Run it on demand before a PR. The git hooks do not run all of it.

`.githooks/pre-push` is the fast push path:

1. **Receipt reuse.** If `$(git rev-parse --git-dir)/prep-pr-receipt.json` is a passing `gate-receipt/v1`, its `tree_sha` matches the tree of every ref being pushed, and the working tree is clean, the hook prints one PASS line and exits (`scripts/check-push-receipt.sh`). Anything else (missing, malformed, failing or stale receipt, or a dirty tree) falls through to step 2. A push that only deletes refs needs no gate.
2. **`scripts/pre-push-gate.sh --hook`**, the fast gate. It keeps the cheap checks and the ones CI does not require, and drops the ones a required CI check already enforces:

| Check | pre-commit | pre-push (`--hook`) | `make gate` | CI (required?) |
| --- | --- | --- | --- | --- |
| conflict markers | staged | yes | yes | none |
| typos | staged | no | no | none |
| product name | staged docs | yes (HEAD) | yes | none |
| gofmt | staged | yes | yes | Lint (required, golangci formatters) |
| generate + ui-validate + build | build | yes | yes | Lint, Build (required) |
| go test | no | non-race, changed packages only (fails closed to `./...`) | same; `RUN_RACE=1`: race, `./...` | Test (required, race, sharded) |
| patch coverage | no | yes, from the changed-package profile | yes, same | Codecov patch status (not required) |
| coverage floor | no | no | only with `RUN_RACE=1` (informational) | Coverage Floor (required) |
| codecov dry-run | no | no | only with `RUN_RACE=1` | Upload Coverage (not required) |
| golangci-lint | yes | yes | yes | Lint (required) |
| actionlint | no | only when a workflow changed | yes | none |
| PR-trigger scope | no | yes | yes | Lint (required; runs on any workflow change) |
| test-shard split | no | no | bash 4+ | Lint (required; runs whenever `scripts/ci-shards.sh` changes) |
| govulncheck | yes | only when `go.mod`/`go.sum` changed | yes | none (`make vulncheck` on demand) |

The hook lints the whole module again even though pre-commit already did, because a commit made with `--no-verify` would otherwise reach the remote unlinted. With a warm cache the second run is cheap.

The hook's test list comes from `scripts/hook-test-pkgs.sh`, which maps every path changed since the merge base (deletes and both sides of a rename included) to its package, and a `testdata/` path to the package that owns it. It fails closed to `./...` when the change can alter Go behavior without naming a package: `go.mod`/`go.sum`, any non-`.go` path under `internal/`, `web/` or `cmd/` (a `.templ` source, whose generated `*_templ.go` is gitignored; an embedded migration or `web/static` asset), a package left with no `.go` file, or a missing merge base. It skips tests only when no Go-relevant path changed.

govulncheck runs in the hook when `go.mod` or `go.sum` changed since the merge base. Pre-commit does run it, but pre-commit never runs for a merge, rebase, cherry-pick or `git commit --no-verify`, so a dependency bump arriving that way would otherwise reach the remote unchecked, and no CI job runs govulncheck. A vulnerability-database update against unchanged dependencies is caught only by `make gate` or `make vulncheck`.

`PUSH_GATE=full git push ...` runs the full `make gate` chain and ignores any receipt. It is the only override, and it only makes the push stricter: there is no skip value, and any other `PUSH_GATE` value fails the push with exit 2. Never use `git push --no-verify`. `make hooks-test` runs the hermetic tests for the hook, the receipt check, the package mapping and the PR-trigger guard.

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

Each track becomes a silent, ID3-tagged mono MP3 of exactly the listed length. The length matters: the timing guard judges a synced lyric against the audio duration, so a short file demotes a correct `.lrc` to `.txt`. Every duration is re-read with the same reader serve uses, and a mismatch over 1s fails the run. A nonsense-tagged negative control is always added. ffmpeg comes from the checksum-pinned provisioning in `internal/ffmpeg` (override with `go run ./cmd/smokefixtures -ffmpeg <path>`). An `OUT` inside the repo tree is refused, and so is a non-empty one (a stale `.lrc` there would make the scanner skip its track and the smoke pass on old results): `CLEAN=1` (exactly `1`) replaces only the fixtures listed in the tool's manifest (`.smokefixtures-manifest.json`, written into `OUT` as each file is generated): each listed `.mp3` and its same-stem `.lrc`/`.txt`/`.lrc.orig` sidecars, including a sidecar whose `.mp3` is already gone. Nothing unlisted is touched, and a non-empty `OUT` with no manifest is refused rather than guessed at. The control's artist/title is reserved: a track list entry reusing it is rejected.

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

### Stacked PRs and workflow triggers

Every workflow with a `pull_request` or `pull_request_target` trigger runs with no `branches:` filter, so a PR whose base is another feature branch (a stacked PR) runs the same checks, labels and milestone automation as a PR to `main`; `push` triggers stay restricted to `main`. A filtered trigger would skip the workflow on a stacked PR, and a PR with no checks reads much like a PR whose checks passed. `scripts/check-pr-trigger-scope.sh` enforces this default-deny: it scans every workflow (`.yml` and `.yaml`) and fails on a `branches`/`branches-ignore` filter (block or flow form) unless the file is on its `EXEMPT` list (`dependabot-auto-approve.yml`, an approval and auto-merge path that must only act on PRs to `main`; `pages.yml`, a docs-site build that is not a required check), and fails if `ci.yml` or `codeql.yml` (the required checks) loses its `pull_request` trigger. It runs in `make gate`, in the push hook, and in the required `Lint` CI job (which now also runs on any workflow change), with mutation tests in `make hooks-test`. When a stacked PR's base merges and GitHub retargets it to `main`, trigger a NEW CI run before merging (push a commit, e.g. by updating the branch from `main`) and confirm that run tested the new merge commit: the retarget alone fires no run, and re-running an existing run replays its original commit and payload, so either way the green on the PR was earned against the old base.

### CI test sharding

CI runs the test suite across parallel `Test Shard` jobs rather than one `go test ./...` (issue #662). The split lives in `scripts/ci-shards.sh`, which is the single source of truth for **both** the shard-name list (consumed by the workflow matrix) and the package map each shard resolves to. Keeping them in one file is deliberate: if a shard were named in the map but missing from the matrix, its packages would be excluded from the dynamic `rest` remainder and run by nobody -- nothing would fail, those packages would just stop being tested.

```sh
bash scripts/ci-shards.sh matrix          # the matrix JSON the workflow consumes
bash scripts/ci-shards.sh names           # shard names, one per line
bash scripts/ci-shards.sh packages <name> # that shard's package list
bash scripts/ci-shards.sh run <name>      # that shard's -run regex (partitioned shards only)
bash scripts/ci-shards.sh verify          # assert exactly-once package coverage
```

`verify` asserts that every package in `go list ./...` lands in exactly one shard, that each named shard's directories exist, and that a partitioned package's buckets are complete and disjoint. It runs in `make gate` and in the required `Lint` CI job, so drift blocks the merge (the pre-push hook leaves it to CI). `scripts/ci-shards.sh` is in the `code` paths filter in `ci.yml`, so a change to the shard map alone still runs `Lint`. The script needs Bash 4+ (`declare -A`); stock macOS `/bin/bash` is 3.2, so `make gate` skips it locally when only the old shell is present, and CI enforces it.

`rest` is the **dynamic remainder** -- everything no named shard claims -- so a newly added package automatically lands in a shard.

Three packages are heavy enough that a shard of their own would still be the pole (`internal/commands` at 252s, `internal/web` at 248s, `internal/queue` at 178s of measured CI time), so they are *partitioned by test name*: a round-robin over sorted test names splits each into buckets (`commands-1..3`, `web-1..3`, `queue-1..2`) selected with `-run`. Before relying on a new partition, confirm each bucket passes alone, shuffled, under `-race`.

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
- Run `make gate` (or `/prep-pr`) before opening a pull request; the pre-push hook is the fast subset described above.
- Use `slog` for structured logs; `fmt.Printf` only for direct user-facing CLI output (timer, counts).
- Wrap errors with `fmt.Errorf("context: %w", err)`.
- Web UI track tables list the identity columns in the order Artist, Album, Title; the album is the library file's own, shown as a dash when empty (`templates.AlbumText`).
- Formatting, naming, and file layout are enforced by `gofmt` and `.golangci.yml` -- follow the linter.

See `CLAUDE.md` (the "Architecture" orientation and "Package catalog" sections) for a deeper reference on the package surface, architecture, and data flow.

## Design decisions

- [Multilingual lyric output policy](multilingual-output-policy.md) - how the writer handles songs with an original and a translation: a single bilingual `.lrc` where the original and translation lines share one timestamp. Several code comments under `internal/` reference this policy.
- [Multi-provider orchestration](multi-provider-orchestration.md) - how multiple lyrics-provider lanes run together: ordered fallback by default (parallel race opt-in), per-lane circuit breakers, a single-writer dedup guarantee via the `queue.Complete` CAS, and the cross-lane error precedence that backs off rather than recording a false miss.
