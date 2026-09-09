package commands

import (
	"context"
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
