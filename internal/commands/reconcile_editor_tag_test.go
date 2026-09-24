package commands

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/selfwrite"
)

const canticleLRCFixture = "[by:canticle]\n[ar:A]\n[ti:T]\n[ve:1.14.0]\n[00:01.00]hello\n"

// chmodT chmods path to 0 and restores it to restore on cleanup, failing the
// test on either error.
func chmodT(t *testing.T, path string, restore os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}

// mkdirT MkdirAll's path, failing the test on error.
func mkdirT(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestRunReconcileEditorTag_DryRunWritesNothingThenApplyIdempotent(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	before := readFile(t, target)
	// Dry run: reports one candidate, byte-identical file afterward.
	var dry bytes.Buffer
	if rc := runReconcileEditorTag(context.Background(), &dry, ScanReconcileEditorTagCmd{ConfigPath: cfgPath}); rc != 0 {
		t.Fatalf("dry-run rc=%d out=%s", rc, dry.String())
	}
	if !strings.Contains(dry.String(), "would stamp 1") {
		t.Errorf("dry-run output: %q", dry.String())
	}
	if got := readFile(t, target); got != before {
		t.Errorf("dry run mutated the file:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	// Aggregate-only: stdout must never carry the path.
	if strings.Contains(dry.String(), target) {
		t.Errorf("dry-run stdout leaked a file path: %q", dry.String())
	}
	// Apply: stamps the tag.
	var apply bytes.Buffer
	if rc := runReconcileEditorTag(context.Background(), &apply, ScanReconcileEditorTagCmd{ConfigPath: cfgPath, Yes: true}); rc != 0 {
		t.Fatalf("apply rc=%d out=%s", rc, apply.String())
	}
	if !strings.Contains(apply.String(), "stamped 1") {
		t.Errorf("apply output: %q", apply.String())
	}
	if strings.Contains(apply.String(), target) {
		t.Errorf("apply stdout leaked a file path: %q", apply.String())
	}
	got := readFile(t, target)
	if !strings.Contains(got, "[re:canticle]") {
		t.Errorf("tag not stamped: %q", got)
	}
	reIdx := strings.Index(got, "[re:canticle]")
	veIdx := strings.Index(got, "[ve:1.14.0]")
	if reIdx < 0 || veIdx < 0 || reIdx >= veIdx {
		t.Errorf("[re:canticle] must sit immediately before [ve:], got:\n%s", got)
	}
	// Re-run apply: idempotent, nothing left to stamp.
	var again bytes.Buffer
	if rc := runReconcileEditorTag(context.Background(), &again, ScanReconcileEditorTagCmd{ConfigPath: cfgPath, Yes: true}); rc != 0 {
		t.Fatalf("re-run rc=%d out=%s", rc, again.String())
	}
	if !strings.Contains(again.String(), "stamped 0") {
		t.Errorf("re-run not idempotent: %q", again.String())
	}
	if strings.Count(readFile(t, target), "[re:") != 1 {
		t.Errorf("expected exactly one [re:] tag after re-run, got:\n%s", readFile(t, target))
	}
}

// TestRunReconcileEditorTag_BackupWrittenBeforeMutation: an undurable
// backup record must leave the target untouched (write-ahead).
func TestRunReconcileEditorTag_BackupWrittenBeforeMutation(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	before := readFile(t, target)
	badBackup := filepath.Join(root, "does-not-exist", "backup.jsonl")
	var buf bytes.Buffer
	rc := runReconcileEditorTag(context.Background(), &buf, ScanReconcileEditorTagCmd{ConfigPath: cfgPath, Yes: true, Backup: badBackup})
	if rc == 0 {
		t.Fatalf("expected non-zero rc when the backup file cannot be opened; out=%s", buf.String())
	}
	if got := readFile(t, target); got != before {
		t.Errorf("target file was mutated despite the backup write failing:\nbefore:\n%s\nafter:\n%s", before, got)
	}
}

// TestRunEditorTagBackfill_MarkerGatedStartupPass: the first run stamps and
// marks; a second run is a no-op even with a new eligible file present.
func TestRunEditorTagBackfill_MarkerGatedStartupPass(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	sqlDB := openDBFromConfig(t, cfgPath)
	ctx := context.Background()
	reg := selfwrite.New(0)
	runEditorTagBackfill(ctx, sqlDB, reg)

	if got := readFile(t, target); !strings.Contains(got, "[re:canticle]") {
		t.Fatalf("startup pass did not stamp the file: %q", got)
	}
	done, err := editorTagBackfillDone(ctx, sqlDB)
	if err != nil {
		t.Fatalf("marker query: %v", err)
	}
	if !done {
		t.Fatal("marker not stamped after a completed run")
	}

	// Second run: marker present, so a freshly-added eligible file must be
	// left untouched (the gate is the marker, not per-file idempotency).
	other := filepath.Join(root, "other.lrc")
	mustWrite(t, other, canticleLRCFixture)
	runEditorTagBackfill(ctx, sqlDB, reg)
	if got := readFile(t, other); strings.Contains(got, "[re:") {
		t.Errorf("a second, marker-gated run must not touch a new file: %q", got)
	}
}

// TestDegradedSubdir pins findings 2 and 6: an unreadable subdirectory must
// not abort the walk (counts degraded, continues); the CLI exits non-zero
// aggregate-only (#487's precedent); a wrapped walk error never logs the path.
func TestDegradedSubdir(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, directory permission restrictions do not apply")
	}

	t.Run("walk continues past it", func(t *testing.T) {
		root := t.TempDir()
		dirA, dirB := filepath.Join(root, "dirA"), filepath.Join(root, "dirB")
		mkdirT(t, dirA)
		mkdirT(t, dirB)
		mustWrite(t, filepath.Join(dirA, "song.lrc"), canticleLRCFixture)
		mustWrite(t, filepath.Join(dirB, "other.lrc"), canticleLRCFixture)
		chmodT(t, dirB, 0o755)
		var visited []string
		scanned, stamped, errored, degraded, err := walkEditorTagRoot(context.Background(), root, func(path string) (bool, error) {
			visited = append(visited, path)
			return injectEditorTag(path)
		})
		if err != nil {
			t.Fatalf("an unreadable subdir must not abort the whole walk: %v", err)
		}
		if degraded != 1 || scanned != 1 || stamped != 1 || errored != 0 {
			t.Errorf("scanned=%d stamped=%d errored=%d degraded=%d; want 1,1,0,1", scanned, stamped, errored, degraded)
		}
		if len(visited) != 1 || filepath.Base(visited[0]) != "song.lrc" {
			t.Errorf("fn called for %v; want only dirA's song.lrc", visited)
		}
	})

	t.Run("CLI exits non-zero without leaking the path", func(t *testing.T) {
		cfgPath, root := setupReconcileLRC(t)
		blocked := filepath.Join(root, "blocked")
		mkdirT(t, blocked)
		mustWrite(t, filepath.Join(blocked, "song.lrc"), canticleLRCFixture)
		chmodT(t, blocked, 0o755)
		var buf bytes.Buffer
		rc := runReconcileEditorTag(context.Background(), &buf, ScanReconcileEditorTagCmd{ConfigPath: cfgPath})
		if rc == 0 {
			t.Fatalf("expected non-zero rc when a subdirectory is unreadable; out=%s", buf.String())
		}
		if !strings.Contains(buf.String(), "degraded 1") {
			t.Errorf("output does not report the degraded count: %q", buf.String())
		}
		if strings.Contains(buf.String(), blocked) {
			t.Errorf("stdout leaked the blocked subdirectory's path: %q", buf.String())
		}
	})

	t.Run("root itself unreadable never logs the path", func(t *testing.T) {
		cfgPath, root := setupReconcileLRC(t)
		// os.Stat(root) still succeeds (ancestor-only); WalkDir then fails
		// ReadDir -- a root-level failure that aborts the whole walk.
		chmodT(t, root, 0o755)

		logBuf := withCapturedLog(t)
		var buf bytes.Buffer
		if rc := runReconcileEditorTag(context.Background(), &buf, ScanReconcileEditorTagCmd{ConfigPath: cfgPath}); rc == 0 {
			t.Fatalf("expected non-zero rc for an unreadable library root; out=%s", buf.String())
		}
		if strings.Contains(logBuf.String(), root) {
			t.Errorf("walk-failure log leaked the library root path: %s", logBuf.String())
		}
	})
}

// TestRunEditorTagBackfill_LeavesMarkerUnset pins finding 1: an
// empty/unmounted root or a per-file error must leave the marker unset.
func TestRunEditorTagBackfill_LeavesMarkerUnset(t *testing.T) {
	t.Run("empty or unmounted root", func(t *testing.T) {
		cfgPath, root := setupReconcileLRC(t)
		target := filepath.Join(root, "song.lrc")
		mustWrite(t, target, canticleLRCFixture)
		sqlDB := openDBFromConfig(t, cfgPath)
		ctx := context.Background()
		emptyRoot := t.TempDir()
		if _, err := library.New(sqlDB).Add(ctx, emptyRoot, "empty", models.LibrarySettings{}); err != nil {
			t.Fatalf("library.Add: %v", err)
		}
		runEditorTagBackfill(ctx, sqlDB, selfwrite.New(0))
		if done, err := editorTagBackfillDone(ctx, sqlDB); err != nil || done {
			t.Errorf("done=%v err=%v; want unset for a retry", done, err)
		}
	})

	t.Run("per-file error", func(t *testing.T) {
		if os.Getuid() == 0 {
			t.Skip("skipping: running as root, file permission restrictions do not apply")
		}
		cfgPath, root := setupReconcileLRC(t)
		target := filepath.Join(root, "song.lrc")
		mustWrite(t, target, canticleLRCFixture)
		chmodT(t, target, 0o644)
		sqlDB := openDBFromConfig(t, cfgPath)
		ctx := context.Background()
		runEditorTagBackfill(ctx, sqlDB, selfwrite.New(0))
		if done, err := editorTagBackfillDone(ctx, sqlDB); err != nil || done {
			t.Errorf("done=%v err=%v; want unset for a retry", done, err)
		}
	})
}

// TestRunEditorTagBackfill_DegradedCeiling_BoundedRetryThenGivesUp pins the
// #1084 wiring: a permanently degraded root (here, one file this pass can
// never read) must get maxDegradedAttempts-1 unconditional retries -- the
// marker stays unset -- and only the Nth consecutive degraded startup may
// stamp the done-marker, as a bounded give-up rather than an infinite retry.
func TestRunEditorTagBackfill_DegradedCeiling_BoundedRetryThenGivesUp(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, file permission restrictions do not apply")
	}
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	// Permanently unreadable for the life of this test: every attempt below
	// degrades the same way, simulating a permanently degraded root/file
	// rather than a transient blip.
	chmodT(t, target, 0o644)
	sqlDB := openDBFromConfig(t, cfgPath)
	ctx := context.Background()
	reg := selfwrite.New(0)

	for i := 1; i < maxDegradedAttempts; i++ {
		runEditorTagBackfill(ctx, sqlDB, reg)
		if done, err := editorTagBackfillDone(ctx, sqlDB); err != nil || done {
			t.Fatalf("attempt %d/%d: done=%v err=%v; want unset (still within the retry budget)", i, maxDegradedAttempts, done, err)
		}
	}

	// The maxDegradedAttempts-th consecutive degraded startup: ceiling
	// reached, must give up and stamp rather than retry forever.
	runEditorTagBackfill(ctx, sqlDB, reg)
	done, err := editorTagBackfillDone(ctx, sqlDB)
	if err != nil {
		t.Fatalf("marker query: %v", err)
	}
	if !done {
		t.Fatalf("attempt %d/%d: done=%v; want stamped (ceiling reached, bounded give-up)", maxDegradedAttempts, maxDegradedAttempts, done)
	}
}

// TestRunEditorTagBackfill_CleanRunClearsDegradedCounter proves a genuinely
// clean, fully-covered run resets the degraded-attempt counter rather than
// carrying a stale streak into some unrelated future degradation.
func TestRunEditorTagBackfill_CleanRunClearsDegradedCounter(t *testing.T) {
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	sqlDB := openDBFromConfig(t, cfgPath)
	ctx := context.Background()

	// Seed the counter as if an earlier boot had degraded.
	if _, err := incrementDegradedAttempts(ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); err != nil {
		t.Fatalf("seed degraded counter: %v", err)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); !present {
		t.Fatal("setup: expected the seeded degraded counter row to be present")
	}

	runEditorTagBackfill(ctx, sqlDB, selfwrite.New(0))

	if done, err := editorTagBackfillDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("done=%v err=%v; want stamped after a clean run", done, err)
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); present {
		t.Error("a clean run did not clear the degraded-attempt counter")
	}
}

// TestRunEditorTagBackfill_DegradedCounterIndependentOf470 proves the #1084
// generalization actually holds for these two real call sites, not just the
// synthetic markers in TestDegradedAttempts_IndependentCountersUnderDifferentMarkers:
// degrading the editor-tag backfill must never touch the #470 stacked-check's
// own degraded-attempt counter.
func TestRunEditorTagBackfill_DegradedCounterIndependentOf470(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("skipping: running as root, file permission restrictions do not apply")
	}
	cfgPath, root := setupReconcileLRC(t)
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)
	chmodT(t, target, 0o644)
	sqlDB := openDBFromConfig(t, cfgPath)
	ctx := context.Background()

	runEditorTagBackfill(ctx, sqlDB, selfwrite.New(0))

	if _, present := markerDetailCount(t, ctx, sqlDB, editorTagBackfillDegradedAttemptsMarker); !present {
		t.Fatal("expected the editor-tag degraded-attempt counter to be recorded")
	}
	if _, present := markerDetailCount(t, ctx, sqlDB, lrcStackedCheckDegradedAttemptsMarker); present {
		t.Error("editor-tag degradation must not touch the #470 stacked-check's degraded-attempt counter")
	}
}

// TestRunServe_EditorTagBackfillCompletesBeforeWorkerStarts pins #483 finding
// 3b: the [re:canticle] editor-tag backfill must finish before the worker (or
// any other in-process writer of a .lrc file) starts, since InjectEditorTag's
// own pre-rename concurrency guard is best-effort, not a real lock (Copilot
// 4100857291, CodeRabbit 4100866202). serveStartupOrderHook observes the two
// checkpoints runServe's own source records in program order -- this is a
// sequencing assertion, not a race the test wins or loses on scheduling luck:
// Go cannot start the worker goroutine before the (synchronous) call that
// precedes it in runServe returns.
func TestRunServe_EditorTagBackfillCompletesBeforeWorkerStarts(t *testing.T) {
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	t.Setenv("MXLRC_DOCKER", "")
	t.Setenv("MUSIXMATCH_TOKEN", "tok")
	keyFile := filepath.Join(t.TempDir(), "test.key")
	t.Setenv("MXLRC_SECRETS_KEY_FILE", keyFile)

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "serve.db")
	root := filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	target := filepath.Join(root, "song.lrc")
	mustWrite(t, target, canticleLRCFixture)

	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}

	httpsAddr := freePort(t)
	cfgPath := filepath.Join(dir, "config.toml")
	var b strings.Builder
	b.WriteString("[db]\npath = " + tomlString(dbPath) + "\n\n")
	b.WriteString("[providers]\nprimary = \"musixmatch\"\n\n")
	b.WriteString("[server]\naddr = " + tomlString(httpsAddr) + "\n")
	if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var mu sync.Mutex
	var order []string
	serveStartupOrderHook = func(checkpoint string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, checkpoint)
	}
	t.Cleanup(func() { serveStartupOrderHook = nil })

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan int, 1)
	var out bytes.Buffer
	go func() {
		done <- runServe(
			runCtx,
			&out,
			ServeCmd{ConfigPath: cfgPath},
			func(string) musixmatch.Fetcher { return fakeFetcher{} },
			func(...string) lyrics.Writer { return fakeWriter{} },
		)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runServe did not return after cancel")
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(order)
		mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for both startup checkpoints; got: %v", order)
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	want := []string{"editor_tag_backfill_done", "worker_starting"}
	if len(got) < 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("startup order = %v; want %v (backfill must complete before the worker starts)", got, want)
	}

	if got := readFile(t, target); !strings.Contains(got, "[re:canticle]") {
		t.Errorf("editor-tag backfill did not stamp the file before the worker started: %q", got)
	}
}
