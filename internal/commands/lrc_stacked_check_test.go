package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lrcbackfill"
	"github.com/sydlexius/canticle/internal/models"
)

// withCapturedLog swaps slog's default logger for one writing into a
// concurrency-safe buffer for the duration of the test, restoring the previous
// default on cleanup.
func withCapturedLog(t *testing.T) *lockedBuffer {
	t.Helper()
	var buf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The marker gate itself: absent until stamped, present after.
func TestLRCStackedCheckMarker_Gate(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || done {
		t.Fatalf("fresh db: done=%v err=%v; want done=false", done, err)
	}
	if err := markLRCStackedCheckDone(ctx, sqlDB); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("after mark: done=%v err=%v; want done=true", done, err)
	}
}

// Marker present -> no walk performed (no scan log line at all), even though a
// real library with a stacked file is configured -- so a gate that failed to
// short-circuit would be caught here (unlike a library-less setup, which stays
// silent regardless of whether the gate fired).
func TestRunLRCStackedCheck_MarkerPresentSkipsWalk(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "stacked.lrc"), []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := markLRCStackedCheckDone(ctx, sqlDB); err != nil {
		t.Fatalf("mark: %v", err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)

	if strings.Contains(logBuf.String(), "lrc check") {
		t.Errorf("marker-gated run emitted a log line; want silence: %s", logBuf.String())
	}
	// The stacked file must be untouched: a gate that failed to short-circuit
	// would still not rewrite anything (this pass is report-only), but a walk
	// happening at all is exactly what the log-silence assertion above pins.
	if got, _ := os.ReadFile(filepath.Join(root, "stacked.lrc")); string(got) != "[00:30.00][01:05.00]C\n" {
		t.Errorf("marker-gated run mutated a file: %q", string(got))
	}
}

// Marker absent, one stacked file present -> correct count logged and the
// marker is stamped afterward.
func TestRunLRCStackedCheck_FindsStackedAndStamps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	stacked := filepath.Join(root, "stacked.lrc")
	if err := os.WriteFile(stacked, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	clean := filepath.Join(root, "clean.lrc")
	if err := os.WriteFile(clean, []byte("[00:10.00]a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)

	logged := logBuf.String()
	if !strings.Contains(logged, "lrc check") || !strings.Contains(logged, "stacked=1") {
		t.Errorf("want a log line reporting stacked=1; got: %s", logged)
	}
	if !strings.Contains(logged, "reconcile-lrc") {
		t.Errorf("want the fix command named in the log line; got: %s", logged)
	}

	// The file itself must be untouched -- this pass writes nothing.
	if got, _ := os.ReadFile(stacked); string(got) != "[00:30.00][01:05.00]C\n" {
		t.Errorf("runLRCStackedCheck mutated a file: %q", string(got))
	}
	if _, err := os.Stat(stacked + ".orig"); !os.IsNotExist(err) {
		t.Error("runLRCStackedCheck wrote a .orig backup; it must be report-only")
	}

	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("marker not stamped after a completed check: done=%v err=%v", done, err)
	}

	// A second run is a marker-gated no-op: even though the library still has a
	// stacked file, nothing is logged this time.
	logBuf2 := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	if strings.Contains(logBuf2.String(), "lrc check") {
		t.Errorf("second run (marker set) emitted a log line; want silence: %s", logBuf2.String())
	}
}

// Marker absent, all files already clean -> marker still stamped, and the log
// line says the library is clean rather than nagging about a fix command.
func TestRunLRCStackedCheck_AllCleanStampsWithoutNag(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "clean.lrc"), []byte("[00:10.00]a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)

	logged := logBuf.String()
	if !strings.Contains(logged, "lrc check") || !strings.Contains(logged, "clean") {
		t.Errorf("want a 'library clean' log line; got: %s", logged)
	}
	if strings.Contains(logged, "reconcile-lrc") {
		t.Errorf("an all-clean library must not nag the fix command; got: %s", logged)
	}

	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("marker not stamped after an all-clean check: done=%v err=%v", done, err)
	}
}

// No library roots configured -> the marker is left unset, so a later startup
// (once a library exists) still performs the check.
func TestRunLRCStackedCheck_NoRootsLeavesMarkerUnset(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	runLRCStackedCheck(ctx, sqlDB)

	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || done {
		t.Fatalf("no-roots run: done=%v err=%v; want unset so a later startup retries once a library exists", done, err)
	}
}

// A canceled context leaves the marker unset so the next startup retries the
// walk, mirroring runIdentityBackfill's shutdown behavior.
//
// This must cancel AFTER the marker-check/library-list preamble has already
// run, or the test never reaches runLRCStackedCheck's context.Canceled branch
// at all: a pre-canceled context fails at the FIRST statement (the marker
// query itself returns "context canceled"), which is
// TestRunLRCStackedCheck_MarkerQueryFailureIsNonFatal's case, not this one. An
// earlier version of this test used a pre-canceled context and passed
// identically against an empty temp dir -- i.e. it pinned nothing about the
// walk-cancellation path. This version substitutes runStackedWalk with a fake
// that blocks until it observes cancellation, so the real preamble (marker
// check, library list) provably completes first, and cancellation is only
// delivered once the fake signals the walk has actually started.
func TestRunLRCStackedCheck_CanceledLeavesMarkerUnset(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })

	started := make(chan struct{})
	var walkCalled bool
	runStackedWalk = func(walkCtx context.Context, opts lrcbackfill.Options) (lrcbackfill.Summary, error) {
		walkCalled = true
		close(started)
		<-walkCtx.Done()
		return lrcbackfill.Summary{}, walkCtx.Err()
	}

	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runLRCStackedCheck(cctx, sqlDB)
		close(done)
	}()

	select {
	case <-started:
		// The preamble (marker check, library list) has demonstrably completed
		// and the walk has demonstrably begun -- only now do we cancel.
		cancel()
	case <-time.After(5 * time.Second):
		t.Fatal("runStackedWalk was never called; the preamble did not reach the walk")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLRCStackedCheck did not return promptly after cancellation")
	}

	if !walkCalled {
		t.Fatal("runStackedWalk was never invoked")
	}
	if done, err := lrcStackedCheckDone(context.Background(), sqlDB); err != nil || done {
		t.Fatalf("canceled run: done=%v err=%v; want unset so the next startup resumes", done, err)
	}
}

// A walk failure (an unreadable root) leaves the marker unset -- the count the
// operator would see is untrustworthy, so the pass must not self-silence.
func TestRunLRCStackedCheck_WalkErrorLeavesMarkerUnset(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permission bits; cannot exercise an unreadable root")
	}
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "unreadable")
	if err := os.MkdirAll(root, 0o000); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) }) // let TempDir cleanup remove it
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	// 0o000 does not guarantee WalkDir cannot enter the directory on every
	// runner; verify the premise before asserting on the outcome.
	if entries, rerr := os.ReadDir(root); rerr == nil {
		t.Skipf("runner can list a 0o000 directory (%d entries); cannot exercise the walk-error path", len(entries))
	}

	runLRCStackedCheck(ctx, sqlDB)

	if done, err := lrcStackedCheckDone(ctx, sqlDB); err != nil || done {
		t.Fatalf("walk-error run: done=%v err=%v; want unset so the next startup retries", done, err)
	}
}

// A marker-check failure (queried against a closed database) leaves the marker
// alone and returns promptly without panicking.
func TestRunLRCStackedCheck_MarkerQueryFailureIsNonFatal(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db early: %v", err)
	}

	done := make(chan struct{})
	go func() {
		runLRCStackedCheck(ctx, sqlDB)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runLRCStackedCheck did not return promptly against a closed database")
	}
}

// The unattended startup path must never leak a library path into the log --
// a sidecar path encodes <root>/<Artist>/<Album>/<Title>.lrc, which is private
// library metadata. This exercises three of the FOUR path-bearing log sites
// in this package, reachable through the dry-run walk (inspect): a symlinked
// .lrc (load's symlink skip), an unreadable .lrc (inspect's read error), and
// a stacked .lrc blocked by a pre-existing .orig (classifyBackupExists'
// BLOCKED warn). The fixture's directory name is deliberately distinctive so
// the assertion has something concrete to search for -- a bare "no error
// text" check would miss a leak dressed up as debug context.
//
// The fourth site -- classifyBackupExists' "already expanded by a peer run"
// Debug log -- is deliberately NOT exercised here. Reaching it live requires
// the .lrc to change between inspect's load() and its .orig Lstat check (the
// reviewer needed 400 racing attempts to hit it once), which this fixture
// cannot stage deterministically; a real attempt would be flaky by
// construction. That site's Quiet-gating is instead pinned deterministically
// via the classify seam in
// internal/lrcbackfill.TestRun_PeerExpandedLogSiteRespectsQuiet, which drives
// the exact same Run()-side logging branch this test would otherwise need a
// race to reach.
func TestRunLRCStackedCheck_NeverLogsPaths(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permission bits; cannot exercise the unreadable-file case")
	}
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	const secretDirName = "TotallyPrivateArtistName"
	root := filepath.Join(dir, secretDirName)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	// Symlinked .lrc.
	target := filepath.Join(root, "real.lrc")
	if err := os.WriteFile(target, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.lrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Unreadable .lrc.
	unreadable := filepath.Join(root, "secretsong.lrc")
	if err := os.WriteFile(unreadable, []byte("[00:30.00][01:05.00]C\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })
	if _, rerr := os.ReadFile(unreadable); rerr == nil { //nolint:gosec // reason: test probe reads a fixture path this test just created
		t.Skip("runner can read a 0o000 file; cannot exercise the unreadable-file case")
	}

	// Stacked .lrc blocked by a pre-existing .orig.
	blocked := filepath.Join(root, "blockedsong.lrc")
	if err := os.WriteFile(blocked, []byte("[00:39.26][00:47.06]Chorus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked+".orig", []byte("operator backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if strings.Contains(logged, secretDirName) {
		t.Errorf("startup check log leaked the library path: %s", logged)
	}
	if strings.Contains(logged, "real.lrc") || strings.Contains(logged, "link.lrc") ||
		strings.Contains(logged, "secretsong.lrc") || strings.Contains(logged, "blockedsong.lrc") {
		t.Errorf("startup check log leaked a filename: %s", logged)
	}

	// With errors/blocked present, fix 2's guard means the marker must NOT be
	// stamped -- covered by the dedicated test below, but assert it here too
	// since a wrongly-stamped marker on this exact fixture is the scenario the
	// review reproduced.
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("degraded walk (errors+blocked present): done=%v err=%v; want unset", doneMarker, derr)
	}
}

// summary.Errors > 0 (e.g. every stacked file unreadable) must not be reported
// as "library clean", and must not stamp the marker -- the reproduced defect:
// three unreadable stacked files produced Errors=3, Normalized=0, and a nil
// walk error, which the old code read as an all-clear.
func TestRunLRCStackedCheck_ErrorsDoNotStampOrReportClean(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores file permission bits; cannot exercise the unreadable-file case")
	}
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	for i := 0; i < 3; i++ {
		p := filepath.Join(root, fmt.Sprintf("unreadable%d.lrc", i))
		if err := os.WriteFile(p, []byte("[00:30.00][01:05.00]C\n"), 0o000); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		if _, rerr := os.ReadFile(p); rerr == nil { //nolint:gosec // reason: test probe reads a fixture path this test just created
			t.Skip("runner can read a 0o000 file; cannot exercise this case")
		}
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if strings.Contains(logged, "clean") {
		t.Errorf("a walk with Errors>0 must not report the library clean; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("walk with errors present: done=%v err=%v; want unset so a later startup retries", doneMarker, derr)
	}
}

// A configured root that is empty (nothing scanned -- e.g. the mount has not
// landed yet at container start) must not stamp: the same epistemic state as
// zero libraries configured, which the existing code already handles.
func TestRunLRCStackedCheck_ZeroScannedDoesNotStamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	// No .lrc files written: root exists but is empty.

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "lrc check") {
		t.Errorf("want a log line about the empty/unmounted root; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("zero-scanned run: done=%v err=%v; want unset so a later startup retries", doneMarker, derr)
	}
}

// Round 2, Critical 1 (reproduced by the reviewer): a mounted root with a
// clean .lrc plus a second, not-yet-mounted (empty) root must NOT stamp. The
// old Scanned==0 guard aggregated across all roots, so root A's real .lrc file
// made the aggregate nonzero and papered over root B never being visited at
// all -- the exact case a multi-root deployment (a container start racing an
// NFS/NAS mount) hits in production.
func TestRunLRCStackedCheck_MultiRootOneEmptyDoesNotStamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	good := filepath.Join(dir, "good")
	notMountedYet := filepath.Join(dir, "notmountedyet")
	if err := os.MkdirAll(good, 0o755); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	if err := os.MkdirAll(notMountedYet, 0o755); err != nil {
		t.Fatalf("mkdir notmountedyet: %v", err)
	}
	if err := os.WriteFile(filepath.Join(good, "clean.lrc"), []byte("[00:10.00]a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libRepo := library.New(sqlDB)
	if _, err := libRepo.Add(ctx, good, "good", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add good: %v", err)
	}
	if _, err := libRepo.Add(ctx, notMountedYet, "notmountedyet", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add notmountedyet: %v", err)
	}
	// notMountedYet stays empty: the shape of an unmounted bind-mount that
	// nonetheless resolves and lets WalkDir succeed with nothing under it.

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if strings.Contains(logged, "clean") {
		t.Errorf("must not report the library clean while a second root was never visited; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("multi-root with one empty root: done=%v err=%v; want unset so a later startup retries", doneMarker, derr)
	}
}

// Round 4, Critical 1 (reproduced by the reviewer): root A genuinely holds a
// stacked .lrc -- the exact thing this feature exists to surface -- and root B
// is a second, not-yet-mounted (empty) root walked after it. The pre-fix code
// aggregated A's real Normalized=1 into `total` and then, on reaching B's
// MediaEntries==0 case, RETURNED before any reporting ran -- so the operator
// was never told a stacked file existed, on top of repeating the walk every
// boot forever (both failure modes at once). REPORTING must survive a later
// root's degradation; only STAMPING may not.
func TestRunLRCStackedCheck_MultiRootReportsFindingsFromEarlierRootDespiteLaterEmptyRoot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	stackedRoot := filepath.Join(dir, "a-stacked")
	emptyRoot := filepath.Join(dir, "b-notmountedyet")
	if err := os.MkdirAll(stackedRoot, 0o755); err != nil {
		t.Fatalf("mkdir stackedRoot: %v", err)
	}
	if err := os.MkdirAll(emptyRoot, 0o755); err != nil {
		t.Fatalf("mkdir emptyRoot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stackedRoot, "stacked.lrc"), []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	libRepo := library.New(sqlDB)
	// Insertion order matters: the walk visits roots in library.List order, and
	// this reproduces the defect only when the populated root is walked BEFORE
	// the empty one.
	if _, err := libRepo.Add(ctx, stackedRoot, "a-stacked", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add stackedRoot: %v", err)
	}
	if _, err := libRepo.Add(ctx, emptyRoot, "b-notmountedyet", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add emptyRoot: %v", err)
	}
	// emptyRoot stays empty: the shape of an unmounted bind-mount that
	// nonetheless resolves and lets WalkDir succeed with nothing under it.

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "stacked=1") {
		t.Errorf("the earlier root's genuinely stacked file must still be reported even though a later root was unavailable; got: %s", logged)
	}
	if strings.Contains(logged, "library clean") {
		t.Errorf("must never report the library clean when a stacked file was found; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("multi-root, one degraded root, despite a real finding on the other: done=%v err=%v; want unset so the degraded root is retried next startup", doneMarker, derr)
	}
}

// Round 2's over-correction check: a single MOUNTED root holding audio but no
// .lrc sidecars yet -- the normal shape of every fresh install before the
// first fetch -- must stamp (it is a legitimate clean result), and must NOT
// warn about an unmounted root. Scanned==0 here is expected and benign;
// Visited>0 is what proves the root was actually opened.
func TestRunLRCStackedCheck_MountedRootWithAudioNoLRCStamps(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	// Audio present, no .lrc sidecar at all yet.
	if err := os.WriteFile(filepath.Join(root, "track.mp3"), []byte("not really audio, just a probe file"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "clean") {
		t.Errorf("a mounted root with audio but zero .lrc files must report clean; got: %s", logged)
	}
	if strings.Contains(logged, "not mounted") {
		t.Errorf("must not warn about an unmounted root when the root was genuinely visited; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !doneMarker {
		t.Fatalf("mounted root, audio present, zero .lrc: done=%v err=%v; want stamped (a legitimate clean result)", doneMarker, derr)
	}
}

// Round 4, Critical 2 (reproduced by the reviewer): the test above passed
// only because its probe file happened to be named track.mp3 -- it would have
// passed identically with a sentinel file that has nothing to do with a music
// library. This proves the actual, intended semantics: a root whose ONLY
// content is a mount-checker's own sentinel dotfile (.mountcheck; the same
// shape as Syncthing's .stfolder, macOS's .DS_Store, or a stray README) must
// NOT read as mounted, and must NOT stamp -- exactly the unmounted-root case
// this whole guard exists to catch, now reached via a single stray file
// instead of zero files.
func TestRunLRCStackedCheck_SentinelDotfileOnlyRootDoesNotStamp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	// The mount is not actually there yet; a mount-checking tool's own
	// sentinel file is the only thing that landed in the not-yet-mounted
	// directory.
	if err := os.WriteFile(filepath.Join(root, ".mountcheck"), []byte("probe"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if strings.Contains(logged, "library clean") {
		t.Errorf("a root holding only a sentinel dotfile must not read as a legitimate clean library; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("root with only a sentinel dotfile: done=%v err=%v; want unset -- a stray sentinel must not read as mounted", doneMarker, derr)
	}
}

// Round 2, Important 2 (reproduced by the reviewer): a symlinked .lrc is
// counted Skipped and never judged by NormalizeBody. Before this fix the
// stamp guard checked only Errors/Blocked, so a stacked file hiding behind a
// symlink read as "library clean" and stamped permanently.
func TestRunLRCStackedCheck_SkippedDoesNotStampOrReportClean(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	// The real, stacked file lives OUTSIDE the library root; only a symlink to
	// it sits inside, so the only .lrc the walk sees is Skipped, never judged.
	target := filepath.Join(dir, "outside.lrc")
	if err := os.WriteFile(target, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.lrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if strings.Contains(logged, "clean") {
		t.Errorf("a walk with a Skipped (unjudged) file must not report the library clean; got: %s", logged)
	}
	if doneMarker, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || doneMarker {
		t.Fatalf("walk with a skipped symlink: done=%v err=%v; want unset so a later startup retries", doneMarker, derr)
	}
}

// classifyWalkError is the privacy boundary that keeps a configured library
// root out of the log on the walk-error path (lrcbackfill.Run wraps the
// failing root's path into the error it returns: fmt.Errorf("walk %s: %w",
// root, ...)). This test never skips -- unlike
// TestRunLRCStackedCheck_WalkErrorLeavesMarkerUnset above, which only reaches
// this code path on a runner that actually enforces 0o000 directory
// permissions -- so it is the only coverage guaranteed to run everywhere,
// including as root and on any CI image that can list a 0o000 dir.
func TestClassifyWalkError(t *testing.T) {
	const fakePath = "/fake/secret/library/root"

	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "permission denied",
			err:  fmt.Errorf("walk %s: %w", fakePath, &fs.PathError{Op: "lstat", Path: fakePath, Err: fs.ErrPermission}),
			want: "permission_denied",
		},
		{
			name: "not exist",
			err:  fmt.Errorf("walk %s: %w", fakePath, &fs.PathError{Op: "lstat", Path: fakePath, Err: fs.ErrNotExist}),
			want: "not_found",
		},
		{
			name: "other syscall cause",
			err:  fmt.Errorf("walk %s: %w", fakePath, &fs.PathError{Op: "lstat", Path: fakePath, Err: syscall.EIO}),
			want: syscall.EIO.Error(),
		},
		{
			name: "non-PathError",
			err:  fmt.Errorf("walk %s: %w", fakePath, errors.New("some other failure touching "+fakePath)),
			want: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyWalkError(tt.err)
			if got != tt.want {
				t.Errorf("classifyWalkError() = %q, want %q", got, tt.want)
			}
			// The point of this test: the classification must never leak the
			// path embedded in the wrapped error.
			if strings.Contains(got, "secret") {
				t.Errorf("classifyWalkError() = %q leaked the library path", got)
			}
		})
	}
}

// A library root configured as a symlink is a fully supported deployment
// shape (scanner.ScanLibrary resolves it deliberately, #643). Before the #925
// fix, lrcbackfill.Run's use of filepath.WalkDir on the unresolved root never
// descended into a symlinked root at all -- MediaEntries stayed 0, which this
// check reads as "not mounted yet" -- so a stacked .lrc anywhere under a
// symlinked root was silently never found, forever.
func TestRunLRCStackedCheck_FindsStackedUnderSymlinkedRoot(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	real := filepath.Join(dir, "real-music")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir real: %v", err)
	}
	stacked := filepath.Join(real, "stacked.lrc")
	if err := os.WriteFile(stacked, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	root := filepath.Join(dir, "music-link")
	if err := os.Symlink(real, root); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "stacked=1") {
		t.Errorf("symlinked root: want the stacked file found (stacked=1); got: %s", logged)
	}
	if strings.Contains(logged, "not available") || strings.Contains(logged, "not mounted") {
		t.Errorf("symlinked root wrongly read as empty/unmounted: %s", logged)
	}
	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("symlinked root, clean walk: done=%v err=%v; want stamped", done, derr)
	}
}

// Round 4's Critical 1 fixed reporting/stamping separation at the ROOT level;
// this is the same defect one level down. lrcbackfill.Run returns the Summary
// it had already accumulated when WalkDir hit an error -- per-file errors
// never abort the walk, so an earlier file in the same root can legitimately
// be found stacked before a later, unrelated directory fails to walk. Before
// the #925 fix the walk-error branch discarded that partial summary via a
// bare `continue`, so the earlier finding was silently lost and the report
// could wrongly claim nothing stacked was found.
//
// runStackedWalk is faked here (as TestRunLRCStackedCheck_CanceledLeavesMarkerUnset
// does above) rather than relying on real directory permissions, so this
// never skips and does not depend on a runner's ability to make a directory
// genuinely unwalkable.
func TestRunLRCStackedCheck_WalkErrorStillReportsEarlierFinding(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = func(_ context.Context, _ lrcbackfill.Options) (lrcbackfill.Summary, error) {
		// Bound to locals and returned on their own line, rather than returning a
		// multi-line composite literal alongside a second value. gofmt's
		// indentation for THAT construct differs between Go 1.26 and Go 1.27, so
		// the inline form cannot satisfy both toolchains at once: CI formats with
		// go.mod's 1.26.6 and rejects what a newer local gofmt produces (and the
		// reverse). This shape formats identically under both.
		partial := lrcbackfill.Summary{
			Visited:      2,
			MediaEntries: 1,
			Scanned:      1,
			Normalized:   1,
		}
		walkErr := fmt.Errorf("walk %s: %w", filepath.Join(root, "sub"),
			&fs.PathError{Op: "lstat", Path: filepath.Join(root, "sub"), Err: fs.ErrPermission})
		return partial, walkErr
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "stacked=1") {
		t.Errorf("want the earlier root's stacked finding (stacked=1) reported despite the later walk error; got: %s", logged)
	}
	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || done {
		t.Fatalf("degraded walk with a partial finding: done=%v err=%v; want unset so the next startup retries", done, derr)
	}
}

// markerDetailCount reads the detail_count column of a maintenance_markers
// row directly, for tests that need to distinguish a genuine completion from
// a #922 gave-up stamp (which the marker's mere presence cannot tell apart).
func markerDetailCount(t *testing.T, ctx context.Context, sqlDB *sql.DB, name string) (count sql.NullInt64, present bool) {
	t.Helper()
	err := sqlDB.QueryRowContext(ctx,
		`SELECT detail_count FROM maintenance_markers WHERE name = ?`, name).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return sql.NullInt64{}, false
	}
	if err != nil {
		t.Fatalf("query marker %q: %v", name, err)
	}
	return count, true
}

// degradedAttemptsFakeWalk always reports the same walk error, simulating a
// permanently-unreadable root (#922): every attempt against it is degraded,
// none is transient, so a correct implementation must eventually give up
// rather than retry it forever.
func degradedAttemptsFakeWalk(root string) func(context.Context, lrcbackfill.Options) (lrcbackfill.Summary, error) {
	return func(context.Context, lrcbackfill.Options) (lrcbackfill.Summary, error) {
		return lrcbackfill.Summary{}, fmt.Errorf("walk %s: %w", root,
			&fs.PathError{Op: "lstat", Path: root, Err: fs.ErrPermission})
	}
}

// stackedFileDegradedFakeWalk simulates the OTHER shape of permanent
// degradation (#922 fix round, C1): the walk itself succeeds (nil error) on
// every boot, but it always finds one unreadable file AND some genuinely
// stacked ones in the same pass -- the "one bad symlink among hundreds of
// legitimately stacked .lrc files" scenario the review flagged as the most
// common real-world trigger for the fileDegraded branch. Every boot reports
// the same summary, so a correct implementation must eventually give up
// (fileDegraded, not walkErr), and that give-up report must still carry the
// stacked count this fake keeps producing.
func stackedFileDegradedFakeWalk(normalized, errs int) func(context.Context, lrcbackfill.Options) (lrcbackfill.Summary, error) {
	return func(context.Context, lrcbackfill.Options) (lrcbackfill.Summary, error) {
		return lrcbackfill.Summary{
			// MediaEntries must be > 0 so this trips ONLY the fileDegraded
			// branch, not the separate empty-root/unmounted `degraded` check
			// (summary.MediaEntries == 0) above it -- conflating the two
			// degradation shapes would exercise a different code path than
			// the one C1 is about.
			Visited:      normalized + errs,
			MediaEntries: normalized + errs,
			Scanned:      normalized + errs,
			Normalized:   normalized,
			Errors:       errs,
		}, nil
	}
}

// The first maxDegradedAttempts-1 boots against a permanently-degraded root
// must behave exactly as before #922: retry unconditionally, never stamp.
// Only the Nth boot may stamp, and it must do so as a GAVE-UP outcome (a
// negative sentinel detail_count), never as a "clean" completion -- a bare
// count of 0 would be indistinguishable from a genuinely empty, successfully
// walked library.
func TestRunLRCStackedCheck_DegradedCeiling_NMinus1DoNotStampNthGivesUp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	// A throwaway library, added and removed before the one under test, so
	// the real library's id is provably not 1 (I1 fix round): "1" already
	// appears elsewhere in this log line (roots_configured=1,
	// unavailable_root_count=1), so asserting a bare "1" substring proves
	// nothing about whether library_id/unavailable_root_ids were actually
	// logged. A non-1 id makes that assertion meaningful.
	libRepo := library.New(sqlDB)
	throwawayDir := filepath.Join(dir, "throwaway")
	if err := os.MkdirAll(throwawayDir, 0o755); err != nil {
		t.Fatalf("mkdir throwaway root: %v", err)
	}
	throwaway, err := libRepo.Add(ctx, throwawayDir, "throwaway", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add (throwaway): %v", err)
	}
	if err := libRepo.Remove(ctx, throwaway.ID); err != nil {
		t.Fatalf("library.Remove (throwaway): %v", err)
	}

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	lib, err := libRepo.Add(ctx, root, "lib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	if lib.ID == 1 {
		t.Fatalf("test setup: library id is still 1 despite the throwaway; the id-specificity assertions below would be vacuous")
	}

	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = degradedAttemptsFakeWalk(root)

	for i := 1; i < maxDegradedAttempts; i++ {
		logBuf := withCapturedLog(t)
		runLRCStackedCheck(ctx, sqlDB)
		if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || done {
			t.Fatalf("attempt %d/%d: done=%v err=%v; want unset (still within the retry budget)", i, maxDegradedAttempts, done, derr)
		}
		if strings.Contains(logBuf.String(), "gave up") {
			t.Fatalf("attempt %d/%d: logged a give-up message before the ceiling was reached: %s", i, maxDegradedAttempts, logBuf.String())
		}
		// The library's own root path must never appear in a degraded-retry
		// log line, matching the existing NeverLogsPaths coverage for other
		// sites in this file.
		if strings.Contains(logBuf.String(), root) {
			t.Fatalf("attempt %d/%d: log leaked the library root path: %s", i, maxDegradedAttempts, logBuf.String())
		}
	}

	// The Nth attempt: the ceiling is reached and the marker must stamp, but
	// as a gave-up outcome.
	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("Nth attempt: done=%v err=%v; want stamped once the ceiling is reached", done, derr)
	}
	if !strings.Contains(logged, "gave up") {
		t.Errorf("Nth attempt: want a give-up log message; got: %s", logged)
	}
	if strings.Contains(logged, "library clean") {
		t.Errorf("Nth attempt: a gave-up stamp must never be reported as library clean; got: %s", logged)
	}
	if strings.Contains(logged, root) {
		t.Errorf("Nth attempt: give-up log leaked the library root path: %s", logged)
	}
	// slog's text handler renders key=value pairs; assert BOTH the key and
	// the value together (I1 fix round), not a bare digit substring that
	// "roots_configured=1"/"unavailable_root_count=1" would also satisfy.
	// The throwaway-library setup above makes lib.ID != 1, so these two
	// specific forms can only be present if the give-up Warn actually
	// carries the library_id/unavailable_root_ids attributes.
	wantLibraryID := fmt.Sprintf("library_id=%d", lib.ID)
	if !strings.Contains(logged, wantLibraryID) {
		t.Errorf("give-up log did not identify the unavailable root by library_id; want %q in: %s", wantLibraryID, logged)
	}
	wantUnavailableRootIDs := fmt.Sprintf("unavailable_root_ids=[%d]", lib.ID)
	if !strings.Contains(logged, wantUnavailableRootIDs) {
		t.Errorf("give-up log did not list the unavailable root id; want %q in: %s", wantUnavailableRootIDs, logged)
	}
	// M1 fix round: the Nth attempt must not ALSO log the per-attempt
	// degraded-roots report ("will retry the unavailable root(s) next
	// startup") right before the give-up line contradicts it with "will not
	// run again". The ceiling decision happens before that report, so it
	// must be suppressed on this, the final, attempt.
	for _, retry := range []string{"will retry the unavailable root", "retried next startup"} {
		if strings.Contains(logged, retry) {
			t.Errorf("Nth attempt logged a contradictory retry message (%q) alongside the give-up line: %s", retry, logged)
		}
	}

	count, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckMarker)
	if !present {
		t.Fatal("gave-up stamp: marker row missing")
	}
	if !count.Valid || count.Int64 != lrcStackedCheckGiveUpDetail {
		t.Errorf("gave-up stamp: detail_count = %+v; want the gave-up sentinel (%d)", count, lrcStackedCheckGiveUpDetail)
	}
}

// The give-up stamp must not silently drop a stacked-file finding when the
// degradation is FILE-level rather than a whole-root walk error (#922 fix
// round, C1). Reproduces the review's scenario: one unreadable/blocked/
// symlinked .lrc sitting alongside hundreds of legitimately stacked ones,
// on every boot, so fileDegraded (not the walkErr branch covered by
// TestRunLRCStackedCheck_DegradedCeiling_NMinus1DoNotStampNthGivesUp above)
// drives every attempt including the Nth. Before the fix, the give-up Warn
// omitted total.Normalized entirely, so the one notification this whole
// check exists to deliver was permanently lost once the ceiling hit.
func TestRunLRCStackedCheck_DegradedCeiling_FileDegradedGiveUpReportsStacked(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	const wantNormalized = 250
	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = stackedFileDegradedFakeWalk(wantNormalized, 1)

	for i := 1; i < maxDegradedAttempts; i++ {
		runLRCStackedCheck(ctx, sqlDB)
		if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || done {
			t.Fatalf("attempt %d/%d: done=%v err=%v; want unset (still within the retry budget)", i, maxDegradedAttempts, done, derr)
		}
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("Nth attempt: done=%v err=%v; want stamped once the ceiling is reached", done, derr)
	}
	if !strings.Contains(logged, "gave up") {
		t.Fatalf("Nth attempt: want a give-up log message; got: %s", logged)
	}
	wantStacked := fmt.Sprintf("stacked=%d", wantNormalized)
	if !strings.Contains(logged, wantStacked) {
		t.Errorf("give-up log dropped the stacked-file count; want %q in: %s", wantStacked, logged)
	}
	if !strings.Contains(logged, "reconcile-lrc --yes") {
		t.Errorf("give-up log did not name the remediation command; got: %s", logged)
	}
	// M1 fix round: the Nth attempt must not ALSO log the per-attempt
	// fileDegraded report ("will retry on next startup") right before the
	// give-up line contradicts it with "will not run again". The ceiling
	// decision happens before that report, so it must be suppressed here.
	if strings.Contains(logged, "will retry on next startup") {
		t.Errorf("Nth attempt logged a contradictory retry message alongside the give-up line: %s", logged)
	}
}

// A clean walk still stamps exactly as #470 always did, and resets/clears the
// degraded-attempt counter: a deployment that recovers from a transient
// degradation (a root remounts, a permission fix lands) must not carry a
// stale streak into some unrelated FUTURE degradation.
func TestRunLRCStackedCheck_CleanWalkStampsAndResetsCounter(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	// Degrade a few times, but stay under the ceiling.
	prevWalk := runStackedWalk
	runStackedWalk = degradedAttemptsFakeWalk(root)
	for i := 0; i < maxDegradedAttempts-2; i++ {
		runLRCStackedCheck(ctx, sqlDB)
	}
	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || done {
		t.Fatalf("pre-recovery: done=%v err=%v; want still unset", done, derr)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckDegradedAttemptsMarker); !present {
		t.Fatal("pre-recovery: want a nonzero degraded-attempt count recorded")
	}

	// Recover: the root is now genuinely walkable and clean.
	runStackedWalk = prevWalk
	if err := os.WriteFile(filepath.Join(root, "clean.lrc"), []byte("[00:10.00]a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if !strings.Contains(logged, "library clean") {
		t.Errorf("recovered clean walk: want a 'library clean' log line; got: %s", logged)
	}
	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("recovered clean walk: done=%v err=%v; want stamped", done, derr)
	}
	// A genuine completion via markLRCStackedCheckDone leaves detail_count
	// NULL (it never sets one) -- the point pinned here is only that it must
	// never equal the gave-up sentinel, which is the one value a future reader
	// must be able to rule out to trust a "clean" reading of this row.
	count, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckMarker)
	if !present || (count.Valid && count.Int64 == lrcStackedCheckGiveUpDetail) {
		t.Errorf("recovered clean walk: detail_count = %+v (present=%v); must never be the gave-up sentinel", count, present)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckDegradedAttemptsMarker); present {
		t.Error("recovered clean walk: degraded-attempt counter row was not cleared")
	}
}

// The degraded-attempt counter must be durable across a process restart
// (a fresh *sql.DB handle over the same on-disk file), not merely in-memory
// state -- otherwise a real server restart would reset the budget on every
// boot and the ceiling in the test above would never actually bind in
// production, only in a single long-lived test process.
func TestRunLRCStackedCheck_DegradedCounterSurvivesReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = degradedAttemptsFakeWalk(root)

	// One degraded attempt, then close the database -- simulating a server
	// process exiting after a failed startup check.
	runLRCStackedCheck(ctx, sqlDB)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	// Reopen against the same file: a fresh handle, as a real restart would
	// get, with no in-memory state carried over.
	sqlDB2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB2.Close() })

	// Drive the remaining attempts up to, but not including, the ceiling
	// against the reopened handle.
	for i := 2; i < maxDegradedAttempts; i++ {
		runLRCStackedCheck(ctx, sqlDB2)
		if done, derr := lrcStackedCheckDone(ctx, sqlDB2); derr != nil || done {
			t.Fatalf("attempt %d/%d after reopen: done=%v err=%v; want unset", i, maxDegradedAttempts, done, derr)
		}
	}
	// The final attempt must reach the ceiling -- provable only if the first
	// attempt (before the close/reopen) actually persisted.
	runLRCStackedCheck(ctx, sqlDB2)
	if done, derr := lrcStackedCheckDone(ctx, sqlDB2); derr != nil || !done {
		t.Fatalf("final attempt after reopen: done=%v err=%v; want stamped -- the pre-reopen attempt must have persisted", done, derr)
	}
}

// The final attempt with stacked files found on one root while another root
// cannot be walked (#922 round 2, m2): the stacked-found report must not say
// the check will run again, and the give-up line must still carry the count
// and the remediation command.
func TestRunLRCStackedCheck_DegradedCeiling_StackedPlusUnavailableRootFinalAttempt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	good := filepath.Join(dir, "good")
	bad := filepath.Join(dir, "bad")
	for _, r := range []string{good, bad} {
		if err := os.MkdirAll(r, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", r, err)
		}
	}
	repo := library.New(sqlDB)
	if _, err := repo.Add(ctx, good, "good", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add good: %v", err)
	}
	if _, err := repo.Add(ctx, bad, "bad", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add bad: %v", err)
	}

	const wantNormalized = 7
	stacked := stackedFileDegradedFakeWalk(wantNormalized, 0)
	failing := degradedAttemptsFakeWalk(bad)
	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = func(c context.Context, opts lrcbackfill.Options) (lrcbackfill.Summary, error) {
		if len(opts.Roots) == 1 && opts.Roots[0] == bad {
			return failing(c, opts)
		}
		return stacked(c, opts)
	}

	for i := 1; i < maxDegradedAttempts; i++ {
		runLRCStackedCheck(ctx, sqlDB)
	}
	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("Nth attempt: done=%v err=%v; want stamped once the ceiling is reached", done, derr)
	}
	if strings.Contains(logged, "will run again next startup") {
		t.Errorf("final attempt claimed the check will run again alongside the give-up line: %s", logged)
	}
	if want := fmt.Sprintf("stacked=%d", wantNormalized); !strings.Contains(logged, want) {
		t.Errorf("final attempt dropped the stacked count; want %q in: %s", want, logged)
	}
	if !strings.Contains(logged, "reconcile-lrc --yes") {
		t.Errorf("final attempt did not name the remediation command: %s", logged)
	}
}

// A stacked finding from an EARLIER degraded boot must reach the give-up line
// even when the final boot sees none (#922, Copilot review): each startup
// rebuilds its tally, so only the persisted streak maximum can carry it.
func TestRunLRCStackedCheck_DegradedCeiling_EarlierStackedFindingSurvivesGiveUp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sqlDB, err := db.Open(ctx, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}

	const earlyStacked = 42
	boot := 0
	prevWalk := runStackedWalk
	t.Cleanup(func() { runStackedWalk = prevWalk })
	runStackedWalk = func(c context.Context, opts lrcbackfill.Options) (lrcbackfill.Summary, error) {
		boot++
		if boot == 1 {
			return stackedFileDegradedFakeWalk(earlyStacked, 1)(c, opts)
		}
		return stackedFileDegradedFakeWalk(0, 1)(c, opts)
	}

	for i := 1; i < maxDegradedAttempts; i++ {
		runLRCStackedCheck(ctx, sqlDB)
	}
	logBuf := withCapturedLog(t)
	runLRCStackedCheck(ctx, sqlDB)
	logged := logBuf.String()

	if done, derr := lrcStackedCheckDone(ctx, sqlDB); derr != nil || !done {
		t.Fatalf("Nth attempt: done=%v err=%v; want stamped once the ceiling is reached", done, derr)
	}
	if want := fmt.Sprintf("stacked=%d", earlyStacked); !strings.Contains(logged, want) {
		t.Errorf("give-up line lost an earlier boot's stacked finding; want %q in: %s", want, logged)
	}
	if !strings.Contains(logged, "reconcile-lrc --yes") {
		t.Errorf("give-up line did not name the remediation command: %s", logged)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckDegradedStackedMarker); present {
		t.Error("the degraded stacked-count row should be cleared once the check gives up")
	}
}

// TestDegradedAttempts_IndependentCountersUnderDifferentMarkers proves the
// #1084 generalization: incrementDegradedAttempts/clearDegradedAttempts key
// entirely off the marker name argument, so a second startup pass (the #483
// editor-tag backfill) can keep its own bounded-retry counter under its own
// marker name without perturbing the #470 stacked-check's counter, and vice
// versa. This could not even be expressed against the pre-#1084 signatures
// (incrementDegradedAttempts(ctx, sqlDB) and clearDegradedAttempts(ctx,
// sqlDB) took no marker argument at all, hard-wired to the #470 markers), so
// the proof that it fails to compile there stands in for a red run.
func TestDegradedAttempts_IndependentCountersUnderDifferentMarkers(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	const (
		markerA = "test_degraded_attempts_a"
		markerB = "test_degraded_attempts_b"
	)

	if _, err := incrementDegradedAttempts(ctx, sqlDB, markerA); err != nil {
		t.Fatalf("increment A (1st): %v", err)
	}
	attemptsA, err := incrementDegradedAttempts(ctx, sqlDB, markerA)
	if err != nil {
		t.Fatalf("increment A (2nd): %v", err)
	}
	attemptsB, err := incrementDegradedAttempts(ctx, sqlDB, markerB)
	if err != nil {
		t.Fatalf("increment B (1st): %v", err)
	}

	if attemptsA != 2 {
		t.Errorf("marker A: attempts = %d, want 2 (incremented twice)", attemptsA)
	}
	if attemptsB != 1 {
		t.Errorf("marker B: attempts = %d, want 1 (incremented once)", attemptsB)
	}

	if err := clearDegradedAttempts(ctx, sqlDB, markerA); err != nil {
		t.Fatalf("clear A: %v", err)
	}

	if _, present := markerDetailCount(t, ctx, sqlDB, markerA); present {
		t.Error("marker A row should be gone after clearDegradedAttempts(markerA)")
	}
	countB, present := markerDetailCount(t, ctx, sqlDB, markerB)
	if !present {
		t.Fatal("marker B row should survive clearing marker A")
	}
	if !countB.Valid || countB.Int64 != 1 {
		t.Errorf("marker B count after clearing A = %v, want 1 (untouched)", countB)
	}
}

// TestClearDegradedAttempts_AllOrNothingOnPartialFailure proves the #1084
// review fix: clearDegradedAttempts runs its per-marker DELETE statements inside one
// transaction, so a later marker's DELETE failing rolls back an earlier
// marker's DELETE too, rather than leaving it permanently gone while the
// failed marker's stale, inflated row survives (the #470 degraded-stacked
// count that recordDegradedStacked's MAX() reads would otherwise carry a
// stale streak forward). A BEFORE DELETE trigger makes the second marker's
// delete fail deterministically and immediately -- no lock contention or
// timing involved -- which stands in for "a later DELETE fails" without
// needing a fault-injecting driver.
func TestClearDegradedAttempts_AllOrNothingOnPartialFailure(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	const (
		markerOK   = "test_clear_atomic_ok"
		markerFail = "test_clear_atomic_fail"
	)

	if _, err := incrementDegradedAttempts(ctx, sqlDB, markerOK); err != nil {
		t.Fatalf("seed markerOK: %v", err)
	}
	if _, err := incrementDegradedAttempts(ctx, sqlDB, markerFail); err != nil {
		t.Fatalf("seed markerFail: %v", err)
	}

	// Reject any delete of markerFail's row, simulating a mid-clear failure
	// after markerOK's delete has already run (but not yet committed) inside
	// the same transaction.
	if _, err := sqlDB.ExecContext(ctx, fmt.Sprintf(`
        CREATE TRIGGER test_reject_marker_fail_delete
        BEFORE DELETE ON maintenance_markers
        WHEN OLD.name = %q
        BEGIN
            SELECT RAISE(ABORT, 'simulated delete failure');
        END`, markerFail)); err != nil {
		t.Fatalf("create failing trigger: %v", err)
	}

	if err := clearDegradedAttempts(ctx, sqlDB, markerOK, markerFail); err == nil {
		t.Fatal("clearDegradedAttempts with a failing marker delete: want an error, got nil")
	}

	countOK, present := markerDetailCount(t, ctx, sqlDB, markerOK)
	if !present {
		t.Error("markerOK's row was deleted despite the transaction failing on markerFail -- clear is not atomic")
	} else if !countOK.Valid || countOK.Int64 != 1 {
		t.Errorf("markerOK count after failed clear = %v, want 1 (untouched)", countOK)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, markerFail); !present {
		t.Error("markerFail's row is gone despite its own delete being the one that failed")
	}
}
