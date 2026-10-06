package commands

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/revalidate"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/timing"
)

// revalidateFixture builds a config + database + library root holding one audio
// file and one .lrc, and returns (configPath, root, lrcPath).
//
// The audio_durations cache is primed for the audio file through the real
// audiodur store, so the command reaches a verdict through the same lookup it
// uses in production (#441). Without that priming every file reads as
// unknown-duration and fails open, which is the correct behavior but tests
// nothing about remediation.
func revalidateFixture(t *testing.T, lrcBody string) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "library", secretishAlbumDir)
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	audio := filepath.Join(root, secretishTrackName+".mp3")
	if err := os.WriteFile(audio, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	lrc := filepath.Join(root, secretishTrackName+".lrc")
	if err := os.WriteFile(lrc, []byte(lrcBody), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}

	dbPath := filepath.Join(dir, "canticle.db")
	cfgPath := filepath.Join(dir, "config.toml")
	cfg := "[db]\npath = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	primeDuration(t, dbPath, audio, fixtureDurationSeconds)
	return cfgPath, filepath.Join(dir, "library"), lrc
}

// fixtureDurationSeconds is the duration every fixture audio file is recorded
// at. The .lrc bodies in these tests are written against it.
const fixtureDurationSeconds = 120

// primeDuration records seconds as the cached exact duration of audio, through
// the same audiodur store the command reads from.
func primeDuration(t *testing.T, dbPath, audio string, seconds int) {
	t.Helper()
	sqlDB, err := db.Open(t.Context(), dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	fi, err := os.Stat(audio)
	if err != nil {
		t.Fatalf("stat audio: %v", err)
	}
	// Seeded under the PRODUCTION reader identity on purpose: RevalidateCmd
	// builds its store with scanner.DurationReaderVersion, so a seed stamped with
	// anything else would miss and the command under test would silently take the
	// UnknownDuration fail-open path instead of the one being asserted (#711).
	if err := audiodur.New(sqlDB, scanner.DurationReaderVersion).Record(t.Context(), audio, fi.ModTime().UnixNano(), fi.Size(), seconds); err != nil {
		t.Fatalf("record duration: %v", err)
	}
}

// secretishAlbumDir and secretishTrackName stand in for the private library
// metadata a real run walks. They are deliberately distinctive strings so the
// stdout-privacy test can assert their ABSENCE from the report: a directory
// tree carries artist/album/title, which must never reach stdout.
const (
	secretishAlbumDir  = "PrivateAlbumName"
	secretishTrackName = "PrivateTrackTitle"
)

// TestRevalidateDryRunWritesNothingAndReportsCounts is the CLI-level dry-run
// rail: no --apply means the .lrc survives, no quarantine directory appears, and
// the report is a count line.
func TestRevalidateDryRunWritesNothingAndReportsCounts(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath,
	})
	if code != 0 {
		t.Fatalf("exit = %d, want 0: %s", code, out.String())
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("a dry run removed the .lrc: %v", err)
	}
	if !strings.Contains(out.String(), "scanned=") {
		t.Errorf("no count line in the report: %s", out.String())
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("the report does not say it was a dry run: %s", out.String())
	}
}

// TestRevalidateStdoutIsAggregateOnly is the PRIVACY rail. A library path
// carries the artist, album, and track title. This runs the command in the mode
// that has the most to say -- an apply that actually quarantines a file -- and
// asserts that no identifying fragment of the tree reaches stdout.
func TestRevalidateStdoutIsAggregateOnly(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	quarantine := filepath.Join(t.TempDir(), "q")

	for _, apply := range []bool{false, true} {
		var out bytes.Buffer
		code := runRevalidate(t.Context(), &out, RevalidateCmd{
			Roots: []string{root}, ConfigPath: cfgPath, Apply: apply, QuarantineDir: quarantine,
		})
		if code != 0 {
			t.Fatalf("apply=%v: exit = %d: %s", apply, code, out.String())
		}
		got := out.String()
		for _, forbidden := range []string{secretishTrackName, secretishAlbumDir, ".lrc", root} {
			if strings.Contains(got, forbidden) {
				t.Errorf("apply=%v: stdout leaked %q -- library paths carry artist/album/title and must never be printed.\ngot: %s",
					apply, forbidden, got)
			}
		}
	}
}

// TestRevalidateApplyQuarantinesNotDeletes is the CLI-level reversibility rail.
func TestRevalidateApplyQuarantinesNotDeletes(t *testing.T) {
	body := "[00:10.00]alpha\n[05:00.00]beta\n"
	cfgPath, root, lrc := revalidateFixture(t, body)
	quarantine := filepath.Join(t.TempDir(), "q")

	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: quarantine,
	})
	if code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the .lrc is still in the library: %v", err)
	}
	found := false
	_ = filepath.WalkDir(quarantine, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".lrc") {
			b, rerr := os.ReadFile(p)
			if rerr == nil && string(b) == body {
				found = true
			}
		}
		return nil
	})
	if !found {
		t.Error("no byte-identical copy under the quarantine root -- the file was DELETED, not moved aside")
	}
}

// TestRevalidateTailFileCarriesTheDetail: the per-file detail stdout refuses to
// print must be available to the operator who explicitly asks for it.
func TestRevalidateTailFileCarriesTheDetail(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	tail := filepath.Join(t.TempDir(), "offenders.tsv")

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Tail: tail,
	}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	b, err := os.ReadFile(tail)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	if !strings.Contains(string(b), secretishTrackName) {
		t.Errorf("the tail file does not carry the offender path: %s", b)
	}
	if strings.Contains(out.String(), secretishTrackName) {
		t.Error("writing a tail file must not also leak the path to stdout")
	}
}

// TestRevalidateUnknownOnFailIsAUsageError.
func TestRevalidateUnknownOnFailIsAUsageError(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, OnFail: "shred", QuarantineDir: t.TempDir(),
	}); code != 2 {
		t.Errorf("exit = %d, want 2: %s", code, out.String())
	}
}

// TestRevalidateIsReachableAsASubcommand guards the wiring the reachability test
// exists for, at the level an operator actually types.
func TestRevalidateIsReachableAsASubcommand(t *testing.T) {
	var out bytes.Buffer
	Run(t.Context(), []string{"revalidate", "--help"}, &out, Deps{})
	if strings.Contains(out.String(), legacyUsageMarker) {
		t.Fatalf("revalidate fell through to the legacy parser: %s", out.String())
	}
	for _, flag := range []string{"--apply", "--on-fail", "--purge", "--quarantine-dir", "--tail"} {
		if !strings.Contains(out.String(), flag) {
			t.Errorf("help does not offer %s: %s", flag, out.String())
		}
	}
}

// addRevalidateLibrary registers root as a library in the fixture database.
func addRevalidateLibrary(t *testing.T, cfgPath, name, root string) {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	if _, err := library.New(sqlDB).Add(t.Context(), root, name, models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}
}

// TestRevalidateDefaultsToEveryConfiguredLibrary: with no positional roots the
// pass walks what the database knows about.
func TestRevalidateDefaultsToEveryConfiguredLibrary(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	addRevalidateLibrary(t, cfgPath, "Main", root)

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "scanned=1") {
		t.Errorf("the configured library was not walked: %s", out.String())
	}
}

// TestRevalidateLibraryScopeResolvesByName.
func TestRevalidateLibraryScopeResolvesByName(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	addRevalidateLibrary(t, cfgPath, "Main", root)

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath, Library: "Main"}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "scanned=1") {
		t.Errorf("the scoped library was not walked: %s", out.String())
	}
}

// TestRevalidateUnknownLibraryIsAnError.
func TestRevalidateUnknownLibraryIsAnError(t *testing.T) {
	cfgPath, _, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath, Library: "nope"}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}

// TestRevalidateWithNoLibrariesIsANoOp: nothing configured, nothing to do, and
// the message says so rather than silently reporting zeros.
func TestRevalidateWithNoLibrariesIsANoOp(t *testing.T) {
	cfgPath, _, _ := revalidateFixture(t, "[00:10.00]alpha\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "no roots to scan") {
		t.Errorf("want an explicit no-roots message: %s", out.String())
	}
}

// TestRevalidateApplyWithNothingToRemediate: a clean library reports so and
// writes no backup file.
func TestRevalidateApplyWithNothingToRemediate(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[01:00.00]beta\n")
	backup := filepath.Join(t.TempDir(), "backup.jsonl")

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath, Apply: true,
		QuarantineDir: t.TempDir(), Backup: backup,
	}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "nothing to remediate") {
		t.Errorf("want a nothing-to-do message: %s", out.String())
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Errorf("a no-op apply wrote a backup file")
	}
	if _, err := os.Stat(lrc); err != nil {
		t.Errorf("a compliant .lrc was touched: %v", err)
	}
}

// TestRevalidateUnknownDurationIsCalledOut: the operator is told why files were
// skipped, so a cold duration cache does not look like a clean library.
func TestRevalidateUnknownDurationIsCalledOut(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	// Touching the audio invalidates the primed (mtime, size) key, so the
	// lookup misses exactly as a cold cache would.
	audio := filepath.Join(root, secretishAlbumDir, secretishTrackName+".mp3")
	if err := os.WriteFile(audio, []byte("changed bytes"), 0o600); err != nil {
		t.Fatalf("rewrite audio: %v", err)
	}
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "unknown-duration=1") {
		t.Errorf("the unknown-duration count is not reported: %s", out.String())
	}
	if !strings.Contains(out.String(), "left untouched") {
		t.Errorf("the operator is not told the files were skipped: %s", out.String())
	}
}

// The skip advice must name an operation that actually fills the cache for
// THESE files (#684). Before that fix a scan skipped any file that already had
// a sidecar before it ever probed a duration -- so "run a scan" was advice that
// provably did not work for the exact files being reported here, and an
// operator following it saw the identical count again. The fix makes a scan
// genuinely fill them, but a library scanned by an older build still needs one
// fresh pass, so the advice has to say so rather than implying the cache is
// merely cold.
func TestRevalidateUnknownDurationAdviceNamesAWorkingRemedy(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	audio := filepath.Join(root, secretishAlbumDir, secretishTrackName+".mp3")
	if err := os.WriteFile(audio, []byte("changed bytes"), 0o600); err != nil {
		t.Fatalf("rewrite audio: %v", err)
	}
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	got := out.String()
	// The remedy must still be named -- a bare count leaves the operator stuck.
	if !strings.Contains(got, "scan") {
		t.Errorf("the message names no remedy at all: %s", got)
	}
	// ...and it must flag that a library last scanned by an older build needs a
	// fresh pass, rather than implying the cache is merely cold.
	if !strings.Contains(got, "re-scan") || !strings.Contains(got, "older builds") {
		t.Errorf("the advice does not tell the operator a fresh scan is what fills these: %s", got)
	}
	// The advice must not pin a release number: the fix's version is unknown
	// when the string is written, and a stale one is worse than none.
	if strings.Contains(got, "1.31") {
		t.Errorf("the advice hardcodes a release number, which goes wrong if the release slips: %s", got)
	}
}

// TestRevalidateBadTailPathIsAnError: an unwritable tail file must fail the run
// rather than silently discarding the detail the operator asked for.
func TestRevalidateBadTailPathIsAnError(t *testing.T) {
	cfgPath, root, _ := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{
		Roots: []string{root}, ConfigPath: cfgPath,
		Tail: filepath.Join(t.TempDir(), "missing-dir", "tail.tsv"),
	}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}

// TestRevalidateBadConfigPathIsAnError.
func TestRevalidateBadConfigPathIsAnError(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("this is not = valid toml ["), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{ConfigPath: bad}); code != 1 {
		t.Errorf("exit = %d, want 1: %s", code, out.String())
	}
}

// TestRevalidateTailIncludesDegenerate covers the #673 report filter. The tail
// enumerates outcomes by hand, so a new remediable verdict is silently DROPPED
// from the operator's only per-file view unless it is added here.
//
// That matters more than a missing line: the tail is where an operator confirms
// what a dry run would touch, and revalidate is dry-run by default. A file the
// sweep will demote but never mentions is exactly the surprise the dry run
// exists to prevent.
func TestRevalidateTailIncludesDegenerate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.tsv")
	findings := []revalidate.Finding{
		{Path: "/lib/a.lrc", Outcome: timing.Degenerate, Duration: 240, Action: realign.KindDemote},
		{Path: "/lib/b.lrc", Outcome: timing.MisSynced, Duration: 100, Action: realign.KindDemote},
		{Path: "/lib/c.lrc", Outcome: timing.Ok, Duration: 100},
	}

	if err := writeRevalidateTail(path, findings); err != nil {
		t.Fatalf("writeRevalidateTail: %v", err)
	}
	body, err := os.ReadFile(path) //nolint:gosec // reason: test path from t.TempDir
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	got := string(body)

	if !strings.Contains(got, string(timing.Degenerate)) {
		t.Errorf("tail omits the degenerate finding; got %q", got)
	}
	if !strings.Contains(got, "/lib/a.lrc") {
		t.Errorf("tail omits the degenerate file path; got %q", got)
	}
	// The existing filter must be unchanged: Ok is not remediable and stays out.
	if strings.Contains(got, "/lib/c.lrc") {
		t.Errorf("tail includes a compliant file; got %q", got)
	}
}

// TestPrintRevalidateCountsIncludesDegenerate covers the aggregate summary
// (#673 review). The tail file is opt-in via --tail, so this one line is what
// a plain `revalidate` run actually shows an operator -- and revalidate is
// dry-run by default, which means a planned demotion that never appears in the
// summary is invisible at exactly the moment it matters.
//
// Nothing pinned this line before, which is why the omission survived: the
// counts printer enumerates fields by hand, so a new Counts field is silently
// absent rather than a compile error.
func TestPrintRevalidateCountsIncludesDegenerate(t *testing.T) {
	var buf bytes.Buffer
	printRevalidateCounts(&buf, revalidate.Counts{
		Scanned: 9, Ok: 4, MisSynced: 1, Categorical: 1, Degenerate: 2,
		UnknownDuration: 1, NoAudio: 0, Errored: 0,
	}, false)

	got := buf.String()
	if !strings.Contains(got, "degenerate=2") {
		t.Errorf("summary omits the degenerate count; got %q", got)
	}
	// The pre-existing fields must survive the edit: a reordered or dropped
	// counter would be a regression this test should also catch.
	for _, want := range []string{"scanned=9", "ok=4", "MisSynced=1", "categorical=1", "unknown-duration=1"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lost %q; got %q", want, got)
		}
	}
}

// seedRevalidateRow enqueues a done, synced work_queue row for audio (the
// coupled row the CLI must stamp, #1082) in the fixture's database.
func seedRevalidateRow(t *testing.T, cfgPath, audio string) *queue.DBQueue {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	seedBacklogRow(t, q, audio, "Some Artist", "Some Title")
	return q
}

func revalidateRowOutcome(t *testing.T, q *queue.DBQueue) string {
	t.Helper()
	outcome, _, _, err := q.LookupTiming(t.Context(), "Some Artist", "Some Title")
	if err != nil {
		t.Fatalf("lookup timing: %v", err)
	}
	return outcome
}

// TestRevalidateApplyStampsTheCoupledRow is the #1082 core: after --apply
// remediates a sidecar, the linked row carries the verdict (so the reports'
// remediation guard excludes it), stdout stays aggregate-only, and a dry run
// stamps nothing.
func TestRevalidateApplyStampsTheCoupledRow(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
	q := seedRevalidateRow(t, cfgPath, audio)
	quarantine := filepath.Join(t.TempDir(), "q")

	var dry bytes.Buffer
	if code := runRevalidate(t.Context(), &dry, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, QuarantineDir: quarantine}); code != 0 {
		t.Fatalf("dry run exit = %d: %s", code, dry.String())
	}
	if got := revalidateRowOutcome(t, q); got != "" {
		t.Fatalf("a dry run stamped the row: timing_outcome = %q", got)
	}

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: quarantine}); code != 0 {
		t.Fatalf("apply exit = %d: %s", code, out.String())
	}
	if got := revalidateRowOutcome(t, q); got != string(timing.MisSynced) {
		t.Errorf("timing_outcome = %q after a remediating apply, want %q", got, timing.MisSynced)
	}
	// A post-settle stamp (#1120): the row is owed one provider pass.
	if cfg, err := config.Load(cfgPath); err != nil {
		t.Fatal(err)
	} else if sqlDB, err := db.Open(t.Context(), cfg.DB.Path); err != nil {
		t.Fatal(err)
	} else {
		var src sql.NullString
		err := sqlDB.QueryRow(`SELECT timing_stamp_source FROM work_queue`).Scan(&src)
		_ = sqlDB.Close()
		if err != nil || src.String != queue.TimingSourceRevalidate {
			t.Errorf("timing_stamp_source = %+v, %v; want %q", src, err, queue.TimingSourceRevalidate)
		}
	}
	if !strings.Contains(out.String(), "1 work-queue row(s) stamped") {
		t.Errorf("no aggregate stamp count in the report: %s", out.String())
	}
	for _, forbidden := range []string{secretishTrackName, secretishAlbumDir, root} {
		if strings.Contains(out.String(), forbidden) {
			t.Errorf("stdout leaked %q: %s", forbidden, out.String())
		}
	}
}

// TestRevalidateApplyFailedRemediationLeavesRowUnstamped is the apply-before-
// stamp rail: when the action fails the row must stay retriable.
func TestRevalidateApplyFailedRemediationLeavesRowUnstamped(t *testing.T) {
	// A categorical lyric is quarantined; the quarantine root is a regular file,
	// so the move cannot land and the action fails.
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[05:00.00]beta\n")
	audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
	q := seedRevalidateRow(t, cfgPath, audio)
	blocker := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: blocker})
	if code == 0 {
		t.Fatalf("expected a failing exit for a failed remediation: %s", out.String())
	}
	if got := revalidateRowOutcome(t, q); got != "" {
		t.Errorf("a failed remediation stamped the row: timing_outcome = %q", got)
	}
}

// TestRevalidateApplyWithNoCoupledRowStillRemediates: a file with no row is
// skipped by the stamp, not an error.
func TestRevalidateApplyWithNoCoupledRowStillRemediates(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: filepath.Join(t.TempDir(), "q")}); code != 0 {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the .lrc was not remediated: %v", err)
	}
	if !strings.Contains(out.String(), "0 work-queue row(s) stamped") {
		t.Errorf("expected a zero stamp count: %s", out.String())
	}
}

// TestRevalidateApplyStampsRowNamingASiblingCopy is the #1082 review F1: the
// directory holds Track.flac AND Track.mp3 beside one Track.lrc, the resolver
// judges against the alphabetically-first copy (.flac), but the single
// work_queue row names the .mp3. The row must still be stamped.
func TestRevalidateApplyStampsRowNamingASiblingCopy(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	stem := strings.TrimSuffix(lrc, ".lrc")
	flac := stem + ".flac"
	if err := os.WriteFile(flac, []byte("stub"), 0o600); err != nil {
		t.Fatalf("write flac: %v", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	primeDuration(t, cfg.DB.Path, flac, fixtureDurationSeconds)
	q := seedRevalidateRow(t, cfgPath, stem+".mp3")

	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: filepath.Join(t.TempDir(), "q")}); code != 0 {
		t.Fatalf("apply exit = %d: %s", code, out.String())
	}
	if got := revalidateRowOutcome(t, q); got != string(timing.MisSynced) {
		t.Errorf("timing_outcome = %q for a row naming the sibling copy, want %q", got, timing.MisSynced)
	}
}

// TestStampRemediatedRowsSkipsAudioWithAnyFailedSidecar is the #1082 review F3:
// two case-variant sidecars share one audio file; when one failed the row must
// stay unstamped, and with no failure it is stamped.
func TestStampRemediatedRowsSkipsAudioWithAnyFailedSidecar(t *testing.T) {
	cfgPath, _, lrc := revalidateFixture(t, "[00:10.00]alpha\n")
	audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
	q := seedRevalidateRow(t, cfgPath, audio)
	findings := []revalidate.Finding{
		{Path: "/x/Track.lrc", AudioPath: audio, Outcome: timing.MisSynced, Action: "demote"},
		{Path: "/x/Track.LRC", AudioPath: audio, Outcome: timing.MisSynced, Action: "demote"},
	}

	failed := map[string]struct{}{"/x/Track.LRC": {}}
	if n, _, _ := stampRemediatedRows(t.Context(), q, findings, failed); n != 0 {
		t.Errorf("stamped %d row(s) despite a failed sibling sidecar, want 0", n)
	}
	if got := revalidateRowOutcome(t, q); got != "" {
		t.Errorf("timing_outcome = %q after a partial failure, want unstamped", got)
	}

	if n, _, _ := stampRemediatedRows(t.Context(), q, findings, map[string]struct{}{}); n == 0 {
		t.Errorf("stamped 0 rows with no failure, want the row stamped")
	}
	if got := revalidateRowOutcome(t, q); got != string(timing.MisSynced) {
		t.Errorf("timing_outcome = %q with no failure, want %q", got, timing.MisSynced)
	}
}

// TestStampRemediatedRowsStampsEachRowOnceMostSevereWins is the #1082 review
// (id 4126893714): two findings on one audio resolve to one row, which is
// stamped and counted once, and the verdict is the most severe regardless of
// finding order.
func TestStampRemediatedRowsStampsEachRowOnceMostSevereWins(t *testing.T) {
	orders := map[string][]timing.TimingOutcome{
		"categorical-first": {timing.Categorical, timing.MisSynced},
		"missynced-first":   {timing.MisSynced, timing.Categorical},
	}
	for name, outcomes := range orders {
		t.Run(name, func(t *testing.T) {
			cfgPath, _, lrc := revalidateFixture(t, "[00:10.00]alpha\n")
			audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
			q := seedRevalidateRow(t, cfgPath, audio)
			findings := []revalidate.Finding{
				{Path: "/x/Track.lrc", AudioPath: audio, Outcome: outcomes[0], Action: "quarantine"},
				{Path: "/x/Track.LRC", AudioPath: audio, Outcome: outcomes[1], Action: "quarantine"},
			}
			stamped, inFlight, failed := stampRemediatedRows(t.Context(), q, findings, map[string]struct{}{})
			if stamped != 1 || inFlight != 0 || failed != 0 {
				t.Errorf("stamped=%d inFlight=%d failed=%d, want 1, 0 and 0", stamped, inFlight, failed)
			}
			if got := revalidateRowOutcome(t, q); got != string(timing.Categorical) {
				t.Errorf("timing_outcome = %q, want the most severe (%q)", got, timing.Categorical)
			}
		})
	}
}

// TestRevalidateApplyStampFailureExitsNonZero is the #1082 review (id
// 4126893724): a stamp that cannot be written must fail the run, since the
// sidecar is already remediated and a re-run cannot find it again. A trigger
// makes the UPDATE fail while leaving lookups and the remediation intact.
func TestRevalidateApplyStampFailureExitsNonZero(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
	q := seedRevalidateRow(t, cfgPath, audio)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := sqlDB.ExecContext(t.Context(), `CREATE TRIGGER fail_stamp BEFORE UPDATE OF timing_outcome ON work_queue BEGIN SELECT RAISE(ABORT, 'stamp blocked'); END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: filepath.Join(t.TempDir(), "q")})
	if code == 0 {
		t.Fatalf("exit = 0 despite a failed stamp: %s", out.String())
	}
	if !strings.Contains(out.String(), "1 work-queue lookup/stamp operation(s) FAILED") {
		t.Errorf("no aggregate stamp-failure line: %s", out.String())
	}
	if _, err := os.Stat(lrc); !os.IsNotExist(err) {
		t.Errorf("the remediation itself should have happened: %v", err)
	}
	if strings.Contains(out.String(), root) {
		t.Errorf("stdout leaked a path: %s", out.String())
	}
	if got := revalidateRowOutcome(t, q); got != "" {
		t.Errorf("row stamped despite the blocked trigger: %q", got)
	}
}

// TestRevalidateApplyStampsRowWithMixedCaseExtension is the #1082 review (id
// 4126893732): the row's source_path carries an extension casing (.Mp3) outside
// the lower/upper pair; the row must still be found and stamped.
func TestRevalidateApplyStampsRowWithMixedCaseExtension(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	q := seedRevalidateRow(t, cfgPath, strings.TrimSuffix(lrc, ".lrc")+".Mp3")
	var out bytes.Buffer
	if code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: filepath.Join(t.TempDir(), "q")}); code != 0 {
		t.Fatalf("apply exit = %d: %s", code, out.String())
	}
	if got := revalidateRowOutcome(t, q); got != string(timing.MisSynced) {
		t.Errorf("timing_outcome = %q for a mixed-case-extension row, want %q", got, timing.MisSynced)
	}
}

// claimAfterLookup wraps the real queue and flips every row to 'processing'
// right after the lookup returns, modeling the worker claiming a row between
// IDsBySourcePaths and the stamp.
type claimAfterLookup struct {
	*queue.DBQueue
	sqlDB *sql.DB
}

func (c claimAfterLookup) IDsBySourcePaths(ctx context.Context, paths []string) ([]int64, error) {
	ids, err := c.DBQueue.IDsBySourcePaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	if _, err := c.sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing'`); err != nil {
		return nil, err
	}
	return ids, nil
}

// TestStampRemediatedRowsInFlightRowIsNotStamped is the #1082 review guard: a
// row the worker claims after the lookup is not stamped, is counted in flight,
// and is not a failure (so the run's exit stays 0).
func TestStampRemediatedRowsInFlightRowIsNotStamped(t *testing.T) {
	cfgPath, _, lrc := revalidateFixture(t, "[00:10.00]alpha\n")
	audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
	q := seedRevalidateRow(t, cfgPath, audio)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	findings := []revalidate.Finding{{Path: "/x/Track.lrc", AudioPath: audio, Outcome: timing.Categorical, Action: "quarantine"}}

	stamped, inFlight, failed := stampRemediatedRows(t.Context(), claimAfterLookup{q, sqlDB}, findings, map[string]struct{}{})
	if stamped != 0 || inFlight != 1 || failed != 0 {
		t.Errorf("stamped=%d inFlight=%d failed=%d, want 0, 1 and 0", stamped, inFlight, failed)
	}
	if got := revalidateRowOutcome(t, q); got != "" {
		t.Errorf("an in-flight row was stamped: timing_outcome = %q", got)
	}
}

// TestRevalidateApplyLookupFailureExitsNonZero: when the row lookup itself fails
// (work_queue gone after the plan), the run counts it and exits 1.
func TestRevalidateApplyLookupFailureExitsNonZero(t *testing.T) {
	cfgPath, root, lrc := revalidateFixture(t, "[00:10.00]alpha\n[02:30.00]beta\n")
	_ = seedRevalidateRow(t, cfgPath, strings.TrimSuffix(lrc, ".lrc")+".mp3")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sqlDB, err := db.Open(t.Context(), cfg.DB.Path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := sqlDB.ExecContext(t.Context(), `DROP TABLE work_queue`); err != nil {
		t.Fatalf("drop work_queue: %v", err)
	}
	var out bytes.Buffer
	code := runRevalidate(t.Context(), &out, RevalidateCmd{Roots: []string{root}, ConfigPath: cfgPath, Apply: true, QuarantineDir: filepath.Join(t.TempDir(), "q")})
	if code != 1 {
		t.Fatalf("exit = %d, want 1: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 work-queue lookup/stamp operation(s) FAILED") {
		t.Errorf("no aggregate failure line: %s", out.String())
	}
}

// triggerBusyErr returns a real SQLITE_BUSY from the driver by contending two
// connections on one file.
func triggerBusyErr(t *testing.T) error {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "busy.db") + "?_pragma=busy_timeout(0)"
	open := func() *sql.DB {
		d, err := sql.Open("sqlite", dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		d.SetMaxOpenConns(1)
		t.Cleanup(func() { _ = d.Close() })
		return d
	}
	a, b := open(), open()
	if _, err := a.ExecContext(t.Context(), "CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("create: %v", err)
	}
	tx, err := a.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if _, err := tx.ExecContext(t.Context(), "INSERT INTO t (id) VALUES (1)"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err = b.ExecContext(t.Context(), "INSERT INTO t (id) VALUES (2)")
	if !db.IsSQLiteBusy(err) {
		t.Fatalf("expected SQLITE_BUSY, got %v", err)
	}
	return err
}

// failFirstStamps fails the first n stamp writes with err against the real
// queue, then delegates, modeling a database error after a remediation applied.
type failFirstStamps struct {
	*queue.DBQueue
	n     int
	err   error
	calls int
}

func (f *failFirstStamps) SetTimingOutcomeIfIdle(ctx context.Context, id int64, rec queue.TimingRecord) (bool, error) {
	f.calls++
	if f.calls <= f.n {
		return false, f.err
	}
	return f.DBQueue.SetTimingOutcomeIfIdle(ctx, id, rec)
}

// TestStampRemediatedRowsRetriesOnlyBusyStampFailures (#1136): a SQLITE_BUSY
// stamp after the remediation applied is retried and lands; a permanent error is
// attempted exactly once and counted failed.
func TestStampRemediatedRowsRetriesOnlyBusyStampFailures(t *testing.T) {
	busy := triggerBusyErr(t)
	for _, tc := range []struct {
		name       string
		failures   int
		err        error
		wantCalls  int
		wantStamp  int
		wantFailed int
		wantRow    string
	}{
		{"busy then success", 2, busy, 3, 1, 0, "categorical"},
		{"busy exhausted", 99, busy, stampAttempts, 0, 1, ""},
		{"permanent", 99, errors.New("injected permanent failure"), 1, 0, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfgPath, _, lrc := revalidateFixture(t, "[00:10.00]alpha\n")
			audio := strings.TrimSuffix(lrc, ".lrc") + ".mp3"
			q := seedRevalidateRow(t, cfgPath, audio)
			findings := []revalidate.Finding{{Path: "/x/Track.lrc", AudioPath: audio, Outcome: timing.Categorical, Action: "quarantine"}}
			fq := &failFirstStamps{DBQueue: q, n: tc.failures, err: tc.err}
			stamped, _, failed := stampRemediatedRows(t.Context(), fq, findings, map[string]struct{}{})
			if fq.calls != tc.wantCalls {
				t.Errorf("stamp attempts = %d, want %d", fq.calls, tc.wantCalls)
			}
			if stamped != tc.wantStamp || failed != tc.wantFailed {
				t.Errorf("stamped=%d failed=%d, want %d and %d", stamped, failed, tc.wantStamp, tc.wantFailed)
			}
			if got := revalidateRowOutcome(t, q); got != tc.wantRow {
				t.Errorf("timing_outcome = %q, want %q", got, tc.wantRow)
			}
		})
	}
}
