# #643 verification report -- audio_durations key canonicalization

## Status

Verified honest, no defects found that block committing. All claims in the pre-existing
(uncommitted) diff checked against real test runs, not just reading. Committed locally.

## Design as actually implemented

`audio_durations.file_path` IS: the absolute, symlink-resolved (canonical) path of the
audio file, as produced by `filepath.Abs` + `filepath.EvalSymlinks`.

- `internal/pathutil.CanonicalRoot(root) (absRoot, canonRoot)` -- resolves a library root
  ONCE per scan (best-effort: any resolve failure degrades to the absolute-but-unresolved
  form rather than erroring).
- `internal/pathutil.RebaseUnderCanonicalRoot(absRoot, canonRoot, p)` -- rewrites a
  per-file path (built by walking under `absRoot`) onto `canonRoot` with a pure string
  join, so a whole scan pays exactly one `EvalSymlinks` for the root, not one per file
  (preserves the "disks stay spun down" property #441 was built for). A path that doesn't
  fall under `absRoot` falls back to a full per-path resolve.
- `internal/pathutil.CanonicalPath(path)` -- full `Abs` + `EvalSymlinks` for a single path,
  used at the worker's fetch-time write (the file is already open for the metadata read
  that call piggybacks on, so the extra resolve is free).
- Scanner (`internal/scanner/scanner.go`): `ScanLibrary` computes `absRoot, canonRoot`
  once, threads them through `scanDir`'s recursion, and `recordDuration` is called with
  `pathutil.RebaseUnderCanonicalRoot(absRoot, canonRoot, filePath)` instead of the raw
  joined path. `models.ScanResult.FilePath` (the enqueued source path) is UNCHANGED --
  still the raw, unresolved join -- so scan-enqueued work_queue rows still carry the
  as-walked spelling; only the audio_durations cache key is canonicalized at the scanner.
- Worker (`internal/worker/worker.go`): `recordDuration` now derives the cache key via
  `pathutil.CanonicalPath(path)` before calling `w.durations.Record`, so a webhook item
  (already `ResolveWithinRoot`-resolved) and a scan-enqueued item (raw spelling,
  canonicalized here) land on the identical key for the same inode.
- `internal/library.validate` now rejects a non-absolute library root outright
  (`filepath.IsAbs` check), rather than silently `Abs()`-ing it against the process cwd.
  This is a deliberate choice: an implicit cwd-relative resolution would itself be
  ambiguous/surprising for a `canticle library add`/`update` CLI invocation, so reject is
  more honest than a silent guess. (CodeRabbit's issue-plan chose auto-`Abs()`+`Clean()`
  instead; per repo policy CR plans are advisory only, and reject is a defensible, arguably
  safer alternative -- it forces the operator to be explicit rather than trusting whatever
  directory the CLI happened to be invoked from.)
- Migration 035's header (unreleased, so edited in place) gained a "KEY CANONICALIZATION
  (#643)" section stating the above, a "SCOPE LIMIT" note (only the library ROOT is
  resolved; an individually-symlinked file/directory deeper in an otherwise-real tree is
  NOT separately resolved -- that would cost one EvalSymlinks per file, defeating the
  amortization), and an "EXISTING ROWS: none" note (table ships in this same migration,
  unreleased, zero callers of Lookup -- nothing to discard).

## Teeth proof (mutation testing, real output)

Three independent mutations, each reverting one capture site back to its pre-fix (raw,
uncanonicalized) behavior, run against the UNCHANGED test files, then restored.

### 1. Scanner: `RebaseUnderCanonicalRoot(...)` -> raw `filePath`

```
=== RUN   TestScanLibrary_SymlinkedRootCollapsesToOneCanonicalKey
    scanner_test.go:806: scan wrote key(s) [.../001/music/song.flac];
        want the canonical key ".../001/real-array-music/song.flac" present
--- FAIL: TestScanLibrary_SymlinkedRootCollapsesToOneCanonicalKey (0.00s)

=== RUN   TestScanRecordsDurationWhenEnriching
    scanner_test.go:578: recorded path ".../T/.../001/song.flac",
        want canonical "/private/var/folders/.../001/song.flac"
--- FAIL: TestScanRecordsDurationWhenEnriching (0.00s)
```
(macOS `/var` -> `/private/var` symlink is what surfaces this even without an
explicit test fixture symlink -- the fix is exercised on every run, not just the
dedicated symlink test.)

### 2. Worker: `pathutil.CanonicalPath(path)` -> raw `path`

```
=== RUN   TestWorkerRecordsRefreshedDuration
    duration_capture_test.go:64: recorded paths = [.../001/song.flac],
        want exactly ["/private/var/folders/.../001/song.flac"]
--- FAIL: TestWorkerRecordsRefreshedDuration (0.00s)

=== RUN   TestWorkerRecordsDurationUnderSymlinkedSourcePath
    duration_capture_test.go:253: recorded key ".../001/music/song.flac" for a
        symlinked SourcePath; want the canonical (symlink-resolved) key
        ".../001/real-array-music/song.flac" -- this must match what the
        scanner records for the same inode
--- FAIL: TestWorkerRecordsDurationUnderSymlinkedSourcePath (0.00s)
```

### 3. Library: removed the `filepath.IsAbs` rejection in `validate`

```
=== RUN   TestAdd_ValidatesRequiredFields/relative_path
    repository_test.go:107: Add returned nil error; want validation error
=== RUN   TestAdd_ValidatesRequiredFields/relative_path_with_dot
    repository_test.go:107: Add returned nil error; want validation error
--- FAIL: TestAdd_ValidatesRequiredFields (0.01s)
```

All three mutations were reverted immediately after confirming failure; `git diff --stat`
after restoration matched the pre-mutation diff exactly (441 insertions across the same
10 files) before committing.

## Two-capture-sites-agree proof

`internal/scanner/scanner_test.go:TestScanLibrary_SymlinkedRootCollapsesToOneCanonicalKey`
builds a REAL symlinked root (`base/music -> base/real-array-music`), scans through the
symlink alias, and then:
1. Asserts the scanner's own write lands under the canonical (`EvalSymlinks`-resolved)
   key, not the symlink spelling.
2. Derives what a webhook consumer's SourcePath would look like
   (`pathutil.ResolveWithinRoot(symlinkRoot, ...)`, mirroring
   `internal/server.confinedPayloadPath`) and what a scan-enqueued consumer's SourcePath
   would look like (the raw `results[0].FilePath`), canonicalizes each via
   `pathutil.CanonicalPath` (mirroring exactly what `worker.recordDuration` now does), and
   asserts BOTH hit the row the scan wrote, at the correct duration (210s).

`internal/worker/duration_capture_test.go:TestWorkerRecordsDurationUnderSymlinkedSourcePath`
is the worker-side half: an item whose `SourcePath` is spelled through a symlinked root
records under the same canonical key `filepath.EvalSymlinks` would produce for the real
file -- i.e. the exact key the scanner test above proves the scanner writes.

Together these two tests prove convergence end-to-end: same inode, same key, from both
capture sites, via a filesystem-level symlink (not a mocked path string).

## Caller audit (step 6 -- shared-surface scope check)

Delegated to a read-only subagent; findings corroborated by direct grep.

- `internal/pathutil.CanonicalRoot`, `RebaseUnderCanonicalRoot`, `CanonicalPath` are pure
  additions: called only from `internal/scanner/scanner.go`, `internal/worker/worker.go`,
  and `internal/pathutil/pathutil_test.go`. Zero other callers today.
- `pathutil.WithinRoot` / `pathutil.ResolveWithinRoot` are byte-for-byte unmodified in this
  diff (confirmed via `git diff`). Their existing callers (`internal/lyrics/writer.go`,
  `internal/watcher/watcher.go`, `internal/server/server.go`, `internal/scan/scheduler.go`,
  `internal/realign/realign.go`, `internal/commands/realign.go`, `internal/prune/prune.go`)
  are unaffected -- no signature or behavior change.
- `internal/library.validate`'s new `filepath.IsAbs` rejection: the only two callers are
  `Repo.Add` and `Repo.Update`. No HTTP/web handler adds or updates a library root (library
  management is CLI-only in this codebase). The two CLI call sites
  (`internal/commands/commands.go` `runLibrary`'s `library add` / `library update`) pass
  the operator's `--path` argument straight through with no prior `filepath.Abs` --
  meaning a relative `--path` that previously succeeded (silently cwd-dependent, the exact
  bug #643 identifies) now fails loudly. This is a real, INTENDED user-facing behavior
  change per the issue's AC ("A relative library root cannot produce a
  working-directory-dependent key") -- not a regression. No untouched existing test breaks:
  every pre-existing `library`/`commands`/`scan`/`prune` test that calls `Add`/`Update`
  already uses `t.TempDir()` or an absolute literal path.

Conclusion: `internal/pathutil` and `internal/library` changes are minimal, additive, and
do not alter any existing caller's behavior except the one intended, in-scope tightening.

## Coverage-floor justification

`scripts/coverage-floor.json`: `internal/pathutil` changed from **92 -> 90** (a LOWER
number). This LOOKS like the exact "silent regression" pattern this step exists to catch,
but it is legitimate here, verified two ways:

1. Measured coverage on this branch is 90.7% (`go test -cover ./internal/pathutil/...`),
   below the pre-existing 92% floor recorded against the code BEFORE this diff (verified by
   stashing the diff and re-measuring: 92.0% on the base). The floor bump tool
   (`coverage-floor.sh --lower`) only permits lowering to the CURRENT measured value and
   refuses if current >= existing floor, so this is a real, tool-verified drop, not an
   arbitrary edit.
2. Why it dropped: three new functions were added (`CanonicalRoot`, `RebaseUnderCanonicalRoot`,
   `CanonicalPath`), each with a best-effort degrade branch that is not exercised by the new
   tests -- `go tool cover -func` shows `CanonicalRoot` and `CanonicalPath` both at 85.7%.

   CORRECTION (hostile-review pass, 2026-07-24): the paragraph above, as originally
   written, misidentified WHICH branch is uncovered -- it claimed the `EvalSymlinks` error
   path. That is factually wrong. Re-measured directly with
   `go test -covermode=count -coverprofile=/tmp/pathutil.cov ./internal/pathutil/...` then
   `grep pathutil.go /tmp/pathutil.cov`: the `EvalSymlinks` error branches (lines 105-107 in
   `CanonicalRoot`, 146-148 in `CanonicalPath`) both show count=1, i.e. covered (by
   `TestCanonicalRootDegradesOnNonexistentRoot` / `TestCanonicalPathDegradesOnNonexistent`,
   which exercise a nonexistent path -- `EvalSymlinks` fails on those, `Abs` does not). The
   genuinely uncovered lines are the earlier `filepath.Abs` error fallbacks (101-103 in
   `CanonicalRoot`, 142-144 in `CanonicalPath`), count=0. `Abs` only fails when
   `os.Getwd` fails (a relative input whose cwd is unreadable/deleted), which is why no
   test reaches it -- effectively unreachable in a normal test process, not "awkward to
   construct". Adding three new functions with this one genuinely-unreachable defensive
   branch each mechanically pulls the package average down even though every property the
   issue's ACs require IS tested; the coverage-floor drop 92 -> 90 stands as correct, this
   just fixes the stated reason. Not chasing this with a contrived test, per repo policy.
3. This is NOT a hidden regression: the new symlink-fixture tests
   (`TestCanonicalRootAndRebaseCollapseSymlinkedRoot`,
   `TestCanonicalPathResolvesSymlinkedComponent`) are net-new, real coverage of the
   happy-path canonicalization logic; the floor number simply reflects that the new
   defensive-only lines outnumber what a reasonable unit test would chase (per this repo's
   "do not chase the last unreachable defensive lines" policy).

Verdict: legitimate, tool-verified `--lower`, not a masked regression. Recommend the
maintainer spot-check this reasoning since a floor decrease is unusual, but it is correct
here.

## Migration header vs code cross-check (step 7)

- Header claims scanner resolves root "ONCE PER SCAN" -- confirmed:
  `ScanLibrary` calls `pathutil.CanonicalRoot(root)` exactly once before recursing.
- Header claims worker resolves "Abs then EvalSymlinks... immediately before the cache
  write" -- confirmed: `CanonicalPath` is called inline in `recordDuration`, one call per
  fetched item, right before `w.durations.Record`.
- Header claims a relative library root "can no longer produce a working-directory-
  dependent key" via `library.validate` rejecting non-absolute paths -- confirmed (and
  mutation-tested above).
- Header's "SCOPE LIMIT" (only the root is resolved, not every intermediate symlink) is
  accurate to the code: `RebaseUnderCanonicalRoot` does a pure string join for anything
  under `absRoot`, with no per-file `EvalSymlinks`.
- Header's "EXISTING ROWS: none" is accurate: migration 035 is unreleased on this stacked
  branch, so there is genuinely no prior release with rows to discard.
- Cross-checked against `recordDuration`'s doc comment in `internal/scanner/scanner.go`
  (unchanged prose about handle-derived stamps vs path re-stat) -- no contradiction; that
  comment describes the STAMP, this diff only changes the KEY, and the two remain
  independent as the issue's problem-2 analysis requires.

## `make gate` -- full output

All stages passed. Full logs saved at `/tmp/gate_final.log` (this session) and the
pre-existing `.superpowers/643-gate.log` / `.superpowers/643-gate2.log` from the prior
agent's earlier runs (both also green after the fixes were already in place). Key results
from this session's run:

- Race tests: all packages `ok`, including `internal/scanner`, `internal/worker`,
  `internal/pathutil`, `internal/library`, `internal/db` (also re-run standalone with
  `-race` explicitly: all `ok`).
- Patch coverage: 82.28% (65/79 executable lines) against a 70% threshold -- `OK`.
- Coverage floor: all packages `ok`, including `internal/pathutil` at 90% (== floor, +0%
  delta) and `internal/scanner` at 78% (floor 68%, +10%) and `internal/worker` at 87%
  (floor 83%, +4%).
- golangci-lint: `0 issues`.
- actionlint: clean.
- govulncheck: `0 vulnerabilities` in code/imports (1 in a required-but-uncalled module,
  pre-existing, unrelated).
- Final line: `OK: all pre-push checks passed`.

## Overrule candidates for the maintainer

1. **`internal/pathutil` coverage floor dropped 92 -> 90.** Verified legitimate above
   (three new defensive branches, not a masked regression), but a floor decrease is
   unusual enough to flag explicitly rather than bury in the diff.
2. **`library.validate` now hard-rejects a relative `--path`** on `canticle library
   add`/`update`, where it previously silently succeeded (cwd-dependent). This is the
   correct fix per the issue's AC, but it is a user-facing CLI behavior change with no
   accompanying CLI-level test or help-text update (the diff's new test coverage is at the
   repository/storage layer only, per the issue's own task-3 instructions). Worth a
   one-line note in the eventual PR description so it isn't a silent surprise for existing
   scripts that configure libraries with relative paths.
3. Scope is otherwise clean: no `Lookup` consumer work, no #640 identity-key work, no
   `normalize`/lyrics-cache changes -- confirmed by caller audit.
