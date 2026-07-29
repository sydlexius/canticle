package commands

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/testutil"
)

// setupIndexMetadata builds a config + DB + one library rooted at a temp dir
// containing the given tagged audio files (each written with a distinct
// artist/title so a lookup can tell them apart), following the setup idiom
// from reconcile_lrc_test.go / reconcile_identity_test.go.
func setupIndexMetadata(t *testing.T, filenames ...string) (cfgPath, dbPath, root string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	root = filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	reconcilePathsCfg(t, cfgPath, dbPath)

	for i, name := range filenames {
		artist := "Artist" + string(rune('A'+i))
		if err := testutil.WriteAudioFile(root, name, artist, "Title", "Album", ""); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	return cfgPath, dbPath, root
}

// addSecondLibrary adds another library root (with the given files) to the
// same DB that setupIndexMetadata already configured, for tests that need two
// distinct library roots sharing one DB.
func addSecondLibrary(t *testing.T, dbPath, name string, filenames ...string) {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(filepath.Dir(dbPath), name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	for i, fn := range filenames {
		artist := "Artist" + string(rune('A'+i))
		if err := testutil.WriteAudioFile(root, fn, artist, "Title", "Album", ""); err != nil {
			t.Fatalf("write fixture %s: %v", fn, err)
		}
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := library.New(sqlDB).Add(ctx, root, name, models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
}

func countAudioMetadataRows(t *testing.T, dbPath string) int {
	t.Helper()
	return countRows(t, context.Background(), dbPath, "audio_metadata")
}

// indexedFilePaths returns the file_path column of every audio_metadata row,
// sorted, for tests that need to know WHICH files were indexed rather than
// just how many.
func indexedFilePaths(t *testing.T, dbPath string) []string {
	t.Helper()
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	rows, err := sqlDB.QueryContext(ctx, "SELECT file_path FROM audio_metadata ORDER BY file_path")
	if err != nil {
		t.Fatalf("query file_path: %v", err)
	}
	defer rows.Close() //nolint:errcheck // test cleanup
	var got []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan file_path: %v", err)
		}
		got = append(got, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err: %v", err)
	}
	return got
}

func TestIndexMetadataDryRunWritesNothing(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3")

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{
		ConfigPath: cfgPath,
		// Yes deliberately false: dry-run is the default.
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, out.String())
	}

	if n := countAudioMetadataRows(t, dbPath); n != 0 {
		t.Errorf("dry run wrote %d rows, want 0", n)
	}
	if !strings.Contains(out.String(), "would index") {
		t.Errorf("dry-run output must say what it would do; got: %s", out.String())
	}
}

func TestIndexMetadataYesWritesRows(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3")

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{
		ConfigPath: cfgPath,
		Yes:        true,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; output: %s", code, out.String())
	}

	if n := countAudioMetadataRows(t, dbPath); n != 2 {
		t.Errorf("indexed %d rows, want 2", n)
	}
}

func TestIndexMetadataSecondPassSkipsUnchangedFiles(t *testing.T) {
	cfgPath, _, _ := setupIndexMetadata(t, "a.mp3", "b.mp3")
	ctx := context.Background()

	var first bytes.Buffer
	if code := runIndexMetadata(ctx, &first, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("first pass exit = %d: %s", code, first.String())
	}

	var second bytes.Buffer
	if code := runIndexMetadata(ctx, &second, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second pass exit = %d: %s", code, second.String())
	}
	// The whole point of the validation rule: an unchanged file is not reopened.
	if !strings.Contains(second.String(), "2 skipped") {
		t.Errorf("second pass must skip both unchanged files; got: %s", second.String())
	}
}

func TestIndexMetadataUnreadableFileIsCountedNotFatal(t *testing.T) {
	// A file with a supported extension but garbage content: the walk must
	// count it and continue, never abort the run.
	cfgPath, _, root := setupIndexMetadata(t, "good.mp3")
	if err := os.WriteFile(filepath.Join(root, "bad.mp3"), []byte("not a real audio file"), 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("write garbage file: %v", err)
	}

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true})
	if code != 0 {
		t.Fatalf("an unreadable file must not fail the run; exit = %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 unreadable") {
		t.Errorf("output must report the unreadable count; got: %s", out.String())
	}
}

func TestIndexMetadataLibraryNotFound(t *testing.T) {
	cfgPath, _, _ := setupIndexMetadata(t, "a.mp3")
	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Library: "no-such-library"})
	if code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "not found") {
		t.Errorf("want 'not found'; got: %s", out.String())
	}
}

func TestIndexMetadataNoLibraryRoots(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	if code := runIndexMetadata(ctx, &out, ScanIndexMetadataCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "no library roots") {
		t.Errorf("output: %q", out.String())
	}
}

func TestIndexMetadataLimit(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3", "c.mp3")
	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true, Limit: 2})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if n := countAudioMetadataRows(t, dbPath); n != 2 {
		t.Errorf("indexed %d rows, want 2 (limit)", n)
	}
}

// TestIndexMetadataLimitResumesAcrossRuns proves --limit accounting is
// charged against work done, not files walked: a second bounded run must
// advance to the files the first run did not reach, not reprocess the same
// ones. Before the fix, the limit gate counted every file the walk reached
// (including ones skipped as already-current), so a second `--limit 2` run
// over 4 files re-hit files 1-2 forever and never reached 3-4. Finding 1.
func TestIndexMetadataLimitResumesAcrossRuns(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3", "c.mp3", "d.mp3")
	ctx := context.Background()

	var first bytes.Buffer
	if code := runIndexMetadata(ctx, &first, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true, Limit: 2}); code != 0 {
		t.Fatalf("first pass exit=%d out=%s", code, first.String())
	}
	firstRows := indexedFilePaths(t, dbPath)
	if len(firstRows) != 2 {
		t.Fatalf("first pass indexed %d rows, want 2: %v", len(firstRows), firstRows)
	}

	var second bytes.Buffer
	if code := runIndexMetadata(ctx, &second, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true, Limit: 2}); code != 0 {
		t.Fatalf("second pass exit=%d out=%s", code, second.String())
	}
	allRows := indexedFilePaths(t, dbPath)
	if len(allRows) != 4 {
		t.Fatalf("after two bounded runs of 2, want all 4 files indexed; got %d: %v", len(allRows), allRows)
	}

	// The second pass's own counters must show it did fresh work, not a
	// repeat of the first pass's files: 2 walked-and-already-indexed
	// (skipped) plus 2 newly indexed.
	if !strings.Contains(second.String(), "indexed 2") || !strings.Contains(second.String(), "2 skipped") {
		t.Errorf("second pass must skip the 2 already-indexed files and index 2 new ones; got: %s", second.String())
	}
}

// cancelDuringWalkCtx wraps a real, live context and cancels it the first
// time Err() is called after arm() has been invoked. filepath.WalkDir's
// walkFn checks ctx.Err() once per audio file reached (see walkIndexMetadata),
// so arming after config load / db open / library listing have already
// happened (all of which run before the walk starts) lets the cancellation
// land squarely inside the walk, on the next file it looks at -- reproducing
// an operator's Ctrl-C mid-walk without a fixed, brittle call count.
type cancelDuringWalkCtx struct {
	context.Context
	cancel context.CancelFunc
	armed  atomic.Bool
}

func newCancelDuringWalkCtx() *cancelDuringWalkCtx {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelDuringWalkCtx{Context: ctx, cancel: cancel}
}

func (c *cancelDuringWalkCtx) arm() { c.armed.Store(true) }

func (c *cancelDuringWalkCtx) Err() error {
	if c.armed.Load() {
		c.cancel()
	}
	return c.Context.Err()
}

// TestIndexMetadataInterruptStillPrintsSummary proves that a cancellation
// landing mid-walk (an operator's Ctrl-C) does not lose the summary line for
// work already committed to the DB before the interrupt: the coverage query
// taken after the walk stops on context.Canceled must not itself use the
// now-canceled context (it would fail, and the operator would get nothing).
// Finding 2.
func TestIndexMetadataInterruptStillPrintsSummary(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3", "c.mp3")

	ctx := newCancelDuringWalkCtx()

	// committed is signaled synchronously, from inside the walk goroutine,
	// immediately after each row commits -- see indexMetadataRowCommitted.
	// This replaces a 1ms-sleep polling loop that re-opened the (single-
	// connection) test DB on every iteration via countAudioMetadataRows,
	// which raced the walk goroutine's own db.Open for the WAL-mode pragma
	// and intermittently failed with "database is locked" well inside the
	// deadline, aborting the test outright rather than just mistiming the
	// interrupt. The channel also removes the interleaving gap the polling
	// loop left between "observed a row" and "called arm()", during which
	// the walk could make arbitrary further progress under load.
	committed := make(chan struct{}, 1)
	indexMetadataRowCommitted = func() {
		select {
		case committed <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { indexMetadataRowCommitted = nil })

	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runIndexMetadata(ctx, &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true})
	}()

	// Arm only once config load / db open / library listing are done and at
	// least one file has actually been committed, so the interrupt genuinely
	// lands mid-walk with real partial work behind it, not before the walk
	// even starts.
	select {
	case <-committed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the first row to be committed")
	}
	ctx.arm()

	var code int
	select {
	case code = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the interrupted run to finish")
	}

	// An operator-initiated interrupt after partial, already-committed work
	// is not treated as a failure.
	if code != 0 {
		t.Fatalf("interrupted run exit=%d, want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "index-metadata: walked") || !strings.Contains(out.String(), "coverage now") {
		t.Fatalf("interrupted run must still print the summary line; got: %s", out.String())
	}
	// Whatever the walk committed before stopping must be reflected in the
	// printed coverage number, not silently dropped.
	rows := countAudioMetadataRows(t, dbPath)
	if rows == 0 {
		t.Fatalf("expected at least one row committed before the interrupt, got 0")
	}
	wantCoverage := fmt.Sprintf("coverage now %d row(s)", rows)
	if !strings.Contains(out.String(), wantCoverage) {
		t.Errorf("printed coverage must reflect the %d rows actually committed; got: %s", rows, out.String())
	}
}

// TestIndexMetadataLibraryScopesCoverage proves --library scopes the printed
// coverage number to that library's own root, not the whole DB: before the
// fix, Coverage had no predicate and always printed the global total, so a
// single-library run over "lib1" would misleadingly report rows belonging to
// a second, unrelated library too. Finding 3.
func TestIndexMetadataLibraryScopesCoverage(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3")
	addSecondLibrary(t, dbPath, "lib2", "c.mp3", "d.mp3", "e.mp3")

	// Index everything first, across both libraries, so the DB genuinely
	// holds rows for both -- the scoping claim is only meaningful if a
	// global-scope run WOULD report more than the targeted library's own
	// count.
	var seed bytes.Buffer
	if code := runIndexMetadata(context.Background(), &seed, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("seed run exit=%d out=%s", code, seed.String())
	}
	if total := countAudioMetadataRows(t, dbPath); total != 5 {
		t.Fatalf("sanity: seed run should index all 5 files across both libraries, got %d", total)
	}

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{
		ConfigPath: cfgPath,
		Yes:        true,
		Library:    "lib",
	})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}

	// Only "lib"'s own 2 rows must be reported, never the global 5 -- the
	// bug this guards against is a Coverage query with no predicate at all.
	if !strings.Contains(out.String(), "coverage now 2 row(s)") {
		t.Errorf("scoped coverage must count only the targeted library's 2 rows, not the global 5; got: %s", out.String())
	}
}

// TestIndexMetadataUnreachableRootFailsRun proves a library root that
// vanishes (unmounted NAS, deleted directory, the realistic production
// failure) is reported as a hard failure, not a silently-empty successful
// walk. Before the fix, filepath.WalkDir's very first callback invocation
// for an unstat-able root took the walkErr branch, which logged at Debug and
// returned nil without incrementing any counter -- the run printed "walked 0
// file(s) ... coverage now 0 row(s)" and exited 0, indistinguishable from a
// genuinely empty, healthy library. Finding: CRITICAL 1.
func TestIndexMetadataUnreachableRootFailsRun(t *testing.T) {
	cfgPath, _, root := setupIndexMetadata(t, "a.mp3")
	// Simulate the root vanishing (an unmounted NAS) by removing it after the
	// library has already been registered against that path.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("remove root: %v", err)
	}

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true})
	if code == 0 {
		t.Fatalf("an unreachable library root must not exit 0; out: %s", out.String())
	}
	if !strings.Contains(out.String(), "walk error") {
		t.Errorf("output must report the walk error count; got: %s", out.String())
	}
}

// TestIndexMetadataPerEntryWalkErrorFailsRun proves the PER-ENTRY walk-error
// path (a subtree that fails mid-walk, e.g. permission denied on a
// subdirectory) is counted and fails the run, distinct from
// TestIndexMetadataUnreachableRootFailsRun above which covers the ROOT-STAT
// failure. Here the root itself stats fine (so the walk begins successfully),
// but a subdirectory chmod'd to 000 makes filepath.WalkDir report an error for
// that one subtree partway through.
func TestIndexMetadataPerEntryWalkErrorFailsRun(t *testing.T) {
	if os.Geteuid() == 0 {
		// Root ignores directory permission bits entirely, so chmod 000 would
		// not deny access and this test would silently assert nothing.
		t.Skip("cannot exercise a permission-denied walk error as root")
	}

	cfgPath, _, root := setupIndexMetadata(t, "good.mp3")

	blocked := filepath.Join(root, "blocked")
	if err := os.Mkdir(blocked, 0o755); err != nil {
		t.Fatalf("mkdir blocked subdir: %v", err)
	}
	if err := testutil.WriteAudioFile(blocked, "hidden.mp3", "ArtistZ", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture in blocked subdir: %v", err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatalf("chmod blocked subdir: %v", err)
	}
	t.Cleanup(func() {
		// t.TempDir()'s cleanup cannot remove a directory it cannot read into;
		// restore permissions so teardown succeeds instead of failing the run
		// with an unrelated-looking cleanup error.
		if err := os.Chmod(blocked, 0o700); err != nil {
			t.Fatalf("restore blocked subdir permissions: %v", err)
		}
	})

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true})
	if code != 1 {
		t.Fatalf("a per-entry walk error must fail the run; exit=%d out=%s", code, out.String())
	}
	if strings.Contains(out.String(), "0 walk error") || !strings.Contains(out.String(), "walk error") {
		t.Errorf("output must report a non-zero walk-error count; got: %s", out.String())
	}
}

// TestIndexMetadataLimitIsPerRunNotPerRoot proves --limit is a single budget
// shared across ALL configured library roots in one invocation, not a fresh
// allowance handed to each root. Before the fix, workDone was declared local
// to walkIndexMetadata (called once per root), so a run over multiple roots
// could read up to Limit * (number of roots) files. A single-root test
// cannot catch this bug by construction, since with one root "per run" and
// "per root" are the same budget -- hence three roots here. Finding:
// CRITICAL 2.
func TestIndexMetadataLimitIsPerRunNotPerRoot(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3", "b.mp3") // root 1: 2 files
	addSecondLibrary(t, dbPath, "lib2", makeNames(10)...)         // root 2: 10 files
	addSecondLibrary(t, dbPath, "lib3", makeNames(10)...)         // root 3: 10 files

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true, Limit: 3})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if n := countAudioMetadataRows(t, dbPath); n != 3 {
		t.Errorf("indexed %d rows across 3 roots with --limit 3, want exactly 3 (budget must be per-run, not per-root)", n)
	}
}

// makeNames returns n distinct audio filenames for a root that needs more
// files than the standard fixture set provides.
func makeNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("f%02d.mp3", i)
	}
	return names
}

// TestIndexMetadataSymlinkedFileIsSkippedOnSecondRun proves a symlinked audio
// file is not re-read and re-recorded on every single run. Before the fix,
// the per-file stat used d.Info() -- an LSTAT describing the symlink itself
// -- while scanner.ReadAudioFacts stamps MTimeNano/SizeBytes from an FSTAT on
// the opened (target) file. Those two never agreed for a symlink, so
// store.Lookup's (mtime, size) key never hit and the file was indexed again
// on every run. Finding: IMPORTANT 3.
func TestIndexMetadataSymlinkedFileIsSkippedOnSecondRun(t *testing.T) {
	cfgPath, dbPath, root := setupIndexMetadata(t, "real.mp3")
	linkPath := filepath.Join(root, "link.mp3")
	if err := os.Symlink(filepath.Join(root, "real.mp3"), linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	ctx := context.Background()

	var first bytes.Buffer
	if code := runIndexMetadata(ctx, &first, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("first pass exit=%d out=%s", code, first.String())
	}
	// Both real.mp3 and link.mp3 are distinct audio_metadata keys (rebased
	// under the canonical root as separate path strings), so the first pass
	// indexes 2 rows.
	if n := countAudioMetadataRows(t, dbPath); n != 2 {
		t.Fatalf("first pass indexed %d rows, want 2 (real.mp3 + link.mp3): %s", n, first.String())
	}

	var second bytes.Buffer
	if code := runIndexMetadata(ctx, &second, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second pass exit=%d out=%s", code, second.String())
	}
	if !strings.Contains(second.String(), "indexed 0") || !strings.Contains(second.String(), "2 skipped") {
		t.Errorf("second pass over a symlinked file must skip it (matching mtime/size via the followed target stat), not re-index it; got: %s", second.String())
	}
}

func TestIndexMetadataConfigLoadError(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(cfgPath, []byte("not = valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var out bytes.Buffer
	if code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath}); code != 1 {
		t.Fatalf("exit=%d want 1 for invalid config", code)
	}
}

func TestIndexMetadata_IsRecognizedSubcommand(t *testing.T) {
	if !usesSubcommand([]string{"scan", "index-metadata"}) {
		t.Error("`scan index-metadata` not recognized as a subcommand invocation")
	}
}

func TestRunScanCmd_DispatchesIndexMetadata(t *testing.T) {
	cfgPath, dbPath, _ := setupIndexMetadata(t, "a.mp3")
	var out bytes.Buffer
	code := runScanCmd(context.Background(), &out, ScanCmd{
		ConfigPath:    cfgPath,
		IndexMetadata: &ScanIndexMetadataCmd{Yes: true},
	})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out.String())
	}
	if n := countAudioMetadataRows(t, dbPath); n != 1 {
		t.Errorf("dispatched run indexed %d rows, want 1", n)
	}
}

// A file whose TAGS read fine but whose DURATION cannot be parsed is neither
// unreadable nor a walk error: the row is written, only the duration is
// missing. Before #651 that left no trace anywhere -- the failure was logged at
// Debug (off in production) and discarded -- which is how a third-party parser
// defect sat unnoticed on two library files until someone inspected an index
// run by hand.
//
// This is deliberately NOT the garbage-content case that
// TestIndexMetadataUnreadableFileIsCountedNotFatal covers: garbage fails the
// TAG read and is counted unreadable, never reaching the duration parser. Here
// the tag block is valid and only the audio frames are unparsable, which is
// the case that used to be invisible.
func TestIndexMetadataDurationParseFailureIsCounted(t *testing.T) {
	cfgPath, _, root := setupIndexMetadata(t, "good.mp3")

	// A valid ID3 tag block followed by bytes that are not decodable frames:
	// tag.ReadFrom succeeds, the duration parser runs and fails.
	if err := testutil.WriteAudioFile(root, "noduration.mp3", "Artist", "Title", "Album", ""); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	p := filepath.Join(root, "noduration.mp3")
	raw, err := os.ReadFile(p) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Keep the ID3 header, corrupt everything after it.
	if len(raw) > 128 {
		copy(raw[128:], bytes.Repeat([]byte{0x00}, len(raw)-128))
	}
	if err := os.WriteFile(p, raw, 0o644); err != nil { //nolint:gosec // test fixture file
		t.Fatalf("rewrite fixture: %v", err)
	}

	var out bytes.Buffer
	code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true})
	got := out.String()

	// Fail-safe is preserved: a parse failure must never abort or fail the run.
	if code != 0 {
		t.Fatalf("a duration parse failure must not fail the run; exit = %d: %s", code, got)
	}
	// The whole point: it must be COUNTED, and countable separately.
	if !strings.Contains(got, "duration parse failure") {
		t.Errorf("summary must report duration parse failures; got: %s", got)
	}
	// And it must NOT be conflated with the unreadable bucket -- the file's
	// tags read fine, so counting it there would hide it among genuinely
	// unopenable files.
	if strings.Contains(got, "1 unreadable") {
		t.Errorf("a duration failure must not be counted as unreadable; got: %s", got)
	}
}

// The healthy case must read exactly as before: no duration-failure text at
// all, so a non-zero count stays conspicuous instead of becoming a column
// operators learn to skip.
//
// FIXTURES MUST BE .flac HERE, and that is itself a finding worth recording.
// testutil.WriteAudioFile writes an ID3v2 tag block with NO MPEG AUDIO FRAMES,
// so every .mp3 fixture in this suite has a genuinely unparsable duration --
// the first draft of this test used .mp3 and correctly reported "2 duration
// parse failure(s)" on files it called clean. testutil.GenerateFLAC emits a
// real STREAMINFO block carrying sample rate and total samples, so a .flac
// fixture has a parseable duration and exercises the actually-clean path.
func TestIndexMetadataNoDurationNoteWhenClean(t *testing.T) {
	cfgPath, _, root := setupIndexMetadata(t)
	for _, name := range []string{"a.flac", "b.flac"} {
		if err := os.WriteFile(filepath.Join(root, name), testutil.GenerateFLAC(44100, 44100*30), 0o644); err != nil { //nolint:gosec // test fixture file
			t.Fatalf("write %s: %v", name, err)
		}
	}

	var out bytes.Buffer
	if code := runIndexMetadata(context.Background(), &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("clean run failed: %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "duration parse failure") {
		t.Errorf("a clean run must not mention duration failures; got: %s", out.String())
	}
}
