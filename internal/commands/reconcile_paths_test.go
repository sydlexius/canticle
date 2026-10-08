package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/prune"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedReconcilePathsRow inserts a scan_results row for filePath plus a linked
// work_queue item (with a real file on disk), so reconcile-paths has a source to
// stat. It stamps a recording_mbid unique to filePath (no match ANYWHERE in
// the library) so the row still hits the #640 genuine-delete outcome the
// existing assertions here exercise -- an identity-absent row would instead be
// retained, not deleted. Returns nothing; the tests assert via row counts and
// command output.
func seedReconcilePathsRow(t *testing.T, ctx context.Context, dbPath, filePath string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open seed: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, recording_mbid) VALUES (1, ?, 'Artist', 'Title', 'processing', ?)`,
		filePath, "mbid-nomatch-"+filePath)
	if err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	srID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("scan_result id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: filepath.Base(filePath)},
		SourcePath:   filePath,
		OutputPaths:  []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Mimic the wedged state the ticket targets: a failed queue row.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'failed' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set failed: %v", err)
	}
}

func reconcilePathsCfg(t *testing.T, cfgPath, dbPath string) {
	t.Helper()
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

func countRows(t *testing.T, ctx context.Context, dbPath, table string) int {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open count: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var n int
	if err := sqlDB.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil { //nolint:gosec // table is a test literal
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func setupReconcilePaths(t *testing.T) (ctx context.Context, cfgPath, dbPath, root string) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	root = filepath.Join(dir, "music")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	reconcilePathsCfg(t, cfgPath, dbPath)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("library.Add: %v", err)
	}
	_ = sqlDB.Close()
	return ctx, cfgPath, dbPath, root
}

// TestReconcilePaths_DryRunLeavesEverything: without --yes the command reports
// what would be pruned but deletes nothing and writes no backup.
func TestReconcilePaths_DryRunLeavesEverything(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistA", "01. gone.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would prune 1 source") {
		t.Errorf("want 'would prune 1 source'; got: %s", buf.String())
	}
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 1 {
		t.Errorf("dry-run deleted scan_results: n=%d, want 1", n)
	}
	if n := countRows(t, ctx, dbPath, "work_queue"); n != 1 {
		t.Errorf("dry-run deleted work_queue: n=%d, want 1", n)
	}
	if matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl")); len(matches) != 0 {
		t.Errorf("dry-run wrote a backup: %v", matches)
	}
}

// TestReconcilePaths_ApplyDeletesBacksUpNoResurrect: --yes deletes the vanished
// source's rows across both tables, leaves a present source untouched, writes a
// decodable JSONL backup, and a second run finds nothing to prune.
func TestReconcilePaths_ApplyDeletesBacksUpNoResurrect(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistA", "01. gone.flac")
	present := filepath.Join(root, "ArtistB", "01. present.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	seedReconcilePathsRow(t, ctx, dbPath, present)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "pruned 1 source") {
		t.Errorf("want 'pruned 1 source'; got: %s", buf.String())
	}
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 1 {
		t.Errorf("after apply scan_results=%d, want 1 (present survives)", n)
	}
	if n := countRows(t, ctx, dbPath, "work_queue"); n != 1 {
		t.Errorf("after apply work_queue=%d, want 1 (present survives)", n)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-paths-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec reconcilePathsBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.SourcePath != gone || len(rec.ScanResultIDs) != 1 || len(rec.WorkItemIDs) != 1 {
		t.Errorf("backup record = %+v; want source=%q with 1 scan/1 wq id", rec, gone)
	}

	// Second run: nothing left to reconcile (no resurrection).
	var buf2 bytes.Buffer
	if code := runReconcilePaths(ctx, &buf2, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second run exit=%d out=%s", code, buf2.String())
	}
	if !strings.Contains(buf2.String(), "pruned 0 source") {
		t.Errorf("second run want 'pruned 0 source'; got: %s", buf2.String())
	}
}

// TestReconcilePaths_LibraryScoped: --library narrows reconciliation to the named
// library and prunes its vanished-source rows.
func TestReconcilePaths_LibraryScoped(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistA", "01. gone.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}
	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Library: "lib", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "pruned 1 source") {
		t.Errorf("want 'pruned 1 source'; got: %s", buf.String())
	}
	if n := countRows(t, ctx, dbPath, "scan_results"); n != 0 {
		t.Errorf("scoped apply left scan_results=%d, want 0", n)
	}
}

// TestConfigSweepIntervalGetSet covers the config get/set wiring for the new key.
func TestConfigSweepIntervalGetSet(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.SweepIntervalSeconds = 900
	if v, ok := configValue(cfg, "server.sweep_interval_seconds"); !ok || v != "900" {
		t.Errorf("configValue = %q,%v; want \"900\",true", v, ok)
	}
	if err := setConfigValue(&cfg, "server.sweep_interval_seconds", "300"); err != nil {
		t.Fatalf("setConfigValue: %v", err)
	}
	if cfg.Server.SweepIntervalSeconds != 300 {
		t.Errorf("after set = %d; want 300", cfg.Server.SweepIntervalSeconds)
	}
	if err := setConfigValue(&cfg, "server.sweep_interval_seconds", "-1"); err == nil {
		t.Error("setConfigValue(-1) = nil; want error for negative")
	}
}

// TestReconcilePaths_LibraryNotFound: an unknown --library exits 1.
func TestReconcilePaths_LibraryNotFound(t *testing.T) {
	ctx, cfgPath, _, _ := setupReconcilePaths(t)
	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Library: "no-such-library"}); code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("want 'not found'; got: %s", buf.String())
	}
}

// TestReconcilePaths_IsRecognizedSubcommand guards the dispatch wiring so
// "scan reconcile-paths" routes to the new handler.
func TestReconcilePaths_IsRecognizedSubcommand(t *testing.T) {
	if !usesSubcommand([]string{"scan", "reconcile-paths"}) {
		t.Error("`scan reconcile-paths` not recognized as a subcommand invocation")
	}
}

// TestReconcilePaths_ConfigLoadError: an unreadable/invalid config exits 1.
func TestReconcilePaths_ConfigLoadError(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(cfgPath, []byte("this is not = valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 1 {
		t.Fatalf("exit=%d want 1 for invalid config", code)
	}
}

// TestServeSweepInterval verifies the CLI > config precedence for the sweep
// interval, including the 0-disables case.
func TestServeSweepInterval(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.SweepIntervalSeconds = 3600
	if got := serveSweepInterval(cfg, ServeCmd{}); got != time.Hour {
		t.Errorf("config path: got %v, want 1h", got)
	}
	override := 0
	if got := serveSweepInterval(cfg, ServeCmd{SweepInterval: &override}); got != 0 {
		t.Errorf("CLI 0 override: got %v, want 0 (disabled)", got)
	}
	override2 := 120
	if got := serveSweepInterval(cfg, ServeCmd{SweepInterval: &override2}); got != 2*time.Minute {
		t.Errorf("CLI override: got %v, want 2m", got)
	}
}

// TestRunSweeperStartupReconciles verifies the sweeper prunes a pre-existing
// dead-path row on its startup run and then returns when the context is done.
func TestRunSweeperStartupReconciles(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	_ = cfgPath
	gone := filepath.Join(root, "ArtistA", "01. gone.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	// A surviving track in another artist keeps the library root non-empty, so the
	// availability guard treats the root as mounted (an empty root is skipped).
	seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistB", "01. kept.flac"))
	// runSweeper uses Directory granularity, so remove the whole directory (a
	// merged/renamed artist dir), not just the file.
	if err := os.RemoveAll(filepath.Dir(gone)); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	// Run the sweeper with a live context so its startup sweep can do DB work,
	// then cancel once it has reconciled to exit the ticker loop.
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, filepath.Join(filepath.Dir(dbPath), "sweep-backup.jsonl"))
		close(done)
	}()

	count := func() int {
		var n int
		if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM scan_results`).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	// The gone artist's row is pruned; the surviving artist's row remains, so the
	// count settles at 1.
	deadline := time.Now().Add(2 * time.Second)
	for count() > 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := count(); n != 1 {
		t.Errorf("startup sweep left %d scan_results, want 1 (gone pruned, kept survives)", n)
	}
	cancel()
	<-done
}

// TestRunSweeperRelinksInFolderSwap drives #1262's case through the production
// caller: a track replaced in place by another format, its folder surviving,
// is moved to the replacement by the sweeper's startup run.
func TestRunSweeperRelinksInFolderSwap(t *testing.T) {
	ctx, _, dbPath, root := setupReconcilePaths(t)
	mp3, flac := filepath.Join(root, "ArtistA", "01. a.mp3"), filepath.Join(root, "ArtistA", "01. a.flac")
	seedReconcilePathsRow(t, ctx, dbPath, mp3)
	if err := os.Remove(mp3); err != nil {
		t.Fatal(err)
	}
	seedReconcilePathsPresentFile(t, ctx, dbPath, flac, "")
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, filepath.Join(filepath.Dir(dbPath), "sweep-backup.jsonl"))
		close(done)
	}()
	var got string
	for deadline := time.Now().Add(2 * time.Second); got != flac && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if err := sqlDB.QueryRowContext(ctx, `SELECT source_path FROM work_queue`).Scan(&got); err != nil {
			t.Fatalf("read source_path: %v", err)
		}
	}
	cancel()
	<-done
	if got != flac {
		t.Errorf("source_path = %q, want the in-folder replacement %q", got, flac)
	}
	// A sweep that deleted nothing creates no backup file.
	if _, err := os.Stat(filepath.Join(filepath.Dir(dbPath), "sweep-backup.jsonl")); !os.IsNotExist(err) {
		t.Errorf("a sweep that deleted nothing left a backup file (stat err %v)", err)
	}
}

// TestRunSweeperLogsRelinkAndRetainOutcomes: the periodic sweep is unattended,
// so its LOG is the only surface an operator has. A relink silently moved a row
// to a new path and a retain is the keep-and-report path -- a row the sweep
// deliberately declined to touch, which will be declined again on every
// subsequent tick until a human resolves it. Logging only the prune count made
// both outcomes invisible.
//
// One sweep is driven over a fixture that produces BOTH: a gone row whose MBID
// resolves uniquely to a present file (relink) and a gone row with no identity
// at all (retain, "never deleted on a guess").
func TestRunSweeperLogsRelinkAndRetainOutcomes(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	_ = cfgPath

	// Relink pair: the gone row's directory is removed (Directory granularity),
	// and a present file elsewhere in the library carries the same MBID.
	goneMoved := filepath.Join(root, "ArtistR", "AlbumOld", "01. track.flac")
	movedTo := filepath.Join(root, "ArtistR", "AlbumNew", "01. track.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, goneMoved, "mbid-r-shared")
	seedReconcilePathsPresentFile(t, ctx, dbPath, movedTo, "mbid-r-shared")
	// Retain row: identity absent, so it is kept and reported, never deleted.
	goneNoIdentity := filepath.Join(root, "ArtistS", "01. gone.flac")
	seedReconcilePathsRowWithIdentity(t, ctx, dbPath, goneNoIdentity, "")
	// A surviving track keeps the library root non-empty so the availability
	// guard treats it as mounted.
	seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistT", "01. kept.flac"))

	for _, dir := range []string{filepath.Dir(goneMoved), filepath.Dir(goneNoIdentity)} {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatalf("remove %s: %v", dir, err)
		}
	}

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	var logBuf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, filepath.Join(filepath.Dir(dbPath), "sweep-backup.jsonl"))
		close(done)
	}()

	// Wait for the startup sweep to emit both lines, then stop the ticker loop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s := logBuf.String()
		if strings.Contains(s, "relinked moved sources") && strings.Contains(s, "retained sources") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done

	got := logBuf.String()
	// The relink line, with the paths an operator needs to verify the move.
	if !strings.Contains(got, "relinked moved sources") {
		t.Errorf("sweep did not log the relink outcome; log:\n%s", got)
	}
	if !strings.Contains(got, goneMoved) || !strings.Contains(got, movedTo) {
		t.Errorf("relink log line lacks the old/new paths (want %q -> %q); log:\n%s", goneMoved, movedTo, got)
	}
	// The retain line, with the path and a non-empty reason.
	if !strings.Contains(got, "retained sources it declined to act on") {
		t.Errorf("sweep did not log the retain outcome -- a held-back row would be invisible; log:\n%s", got)
	}
	if !strings.Contains(got, goneNoIdentity) {
		t.Errorf("retain log line lacks the retained source path %q; log:\n%s", goneNoIdentity, got)
	}
	if !strings.Contains(got, "identity absent") {
		t.Errorf("retain log line lacks the reason; log:\n%s", got)
	}
	// A retained row needs a human, so it must not be buried at Info.
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("retain outcome was not logged at WARN; log:\n%s", got)
	}
	// The artifact behind the log: the retained row really did survive, and the
	// relinked row's work_queue entry really did move.
	var kept int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM scan_results WHERE file_path = ?`, goneNoIdentity).Scan(&kept); err != nil {
		t.Fatalf("count retained: %v", err)
	}
	if kept != 1 {
		t.Errorf("retained row was deleted: %d rows for %s, want 1", kept, goneNoIdentity)
	}
	var moved int
	if err := sqlDB.QueryRowContext(ctx, `SELECT count(*) FROM work_queue WHERE source_path = ?`, movedTo).Scan(&moved); err != nil {
		t.Fatalf("count relinked: %v", err)
	}
	if moved != 1 {
		t.Errorf("relinked work_queue row not at the new path: %d rows for %s, want 1", moved, movedTo)
	}
}

// lockedBuffer is a concurrency-safe io.Writer for capturing slog output from
// the sweeper goroutine while the test goroutine polls it. slog handlers do not
// serialize writes across goroutines, and bytes.Buffer is not safe for
// concurrent use, so -race would flag a bare buffer here.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The unattended sweep's deletes are restorable: one "pruned" record per source
// in the CLI's own format, 0600. When the file cannot be opened the sweep still
// prunes and says so once, without naming a library path.
func TestRunSweeperRecordsDeletedSources(t *testing.T) {
	for name, mode := range map[string]string{"backup file opens": "ok", "existing 0644 backup file is tightened": "loose", "backup file cannot be opened": "open", "backup file cannot be appended to": "append"} {
		t.Run(name, func(t *testing.T) {
			openable := mode == "ok" || mode == "loose"
			ctx, _, dbPath, root := setupReconcilePaths(t)
			gone := []string{filepath.Join(root, "ArtistA", "01. gone.flac"), filepath.Join(root, "ArtistA", "02. gone.flac")}
			for _, g := range append(gone, filepath.Join(root, "ArtistB", "01. kept.flac")) {
				seedReconcilePathsRow(t, ctx, dbPath, g)
			}
			if err := os.RemoveAll(filepath.Dir(gone[0])); err != nil {
				t.Fatal(err)
			}
			// A directory younger than a minute is never recorded, so age the kept one.
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(filepath.Join(root, "ArtistB"), old, old); err != nil {
				t.Fatal(err)
			}
			backup := sweepBackupPath(config.Config{DB: config.DBConfig{Path: dbPath}})
			switch mode {
			case "loose":
				// A pre-existing, world-readable file: O_CREATE's 0600 does not apply to it.
				if err := os.WriteFile(backup, nil, 0o644); err != nil { //nolint:gosec // reason: the test needs a loose mode
					t.Fatal(err)
				}
				if err := os.Chmod(backup, 0o644); err != nil { //nolint:gosec // reason: defeat the umask
					t.Fatal(err)
				}
			case "open":
				backup = filepath.Join(filepath.Dir(dbPath), "no-such-dir", "backup.jsonl")
			case "append":
				// Opens for append, but fsync fails on a character device.
				backup = os.DevNull
			}
			sqlDB, err := db.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer sqlDB.Close() //nolint:errcheck // test cleanup
			var logBuf lockedBuffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			cctx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, backup); close(done) }()
			// Wait for the count line, which follows the directory-state save
			// (canceling earlier would race that save).
			for deadline := time.Now().Add(2 * time.Second); !strings.Contains(logBuf.String(), "pruned rows for vanished sources") && time.Now().Before(deadline); {
				time.Sleep(5 * time.Millisecond)
			}
			cancel()
			<-done // the startup sweep, its record and its log lines are complete
			logged := logBuf.String()
			if n := countRows(t, ctx, dbPath, "scan_results"); n != 1 {
				t.Fatalf("scan_results = %d, want 1: the sweep must prune whether or not it can record", n)
			}
			if strings.Contains(logged, root) {
				t.Errorf("sweep log names a library path:\n%s", logged)
			}
			// The sweep keeps its Result whatever the record did: its count line
			// is logged and its directory state stored.
			if !strings.Contains(logged, "pruned rows for vanished sources") || !strings.Contains(logged, "scan_results=2 work_items=2") {
				t.Errorf("pruned-count line missing:\n%s", logged)
			}
			if n := countRows(t, ctx, dbPath, "prune_dir_state"); n == 0 {
				t.Errorf("prune_dir_state is empty: the sweep must save directory state whether or not it can record")
			}
			b, err := os.ReadFile(backup) //nolint:gosec // test-controlled path
			// The sweep's only Warn in this fixture is the unrecorded-deletes one.
			warns := strings.Count(logged, "level=WARN")
			if !openable {
				if warns != 1 || !strings.Contains(logged, "could not record every source") || !strings.Contains(logged, "sources=2") || (mode == "open") != (err != nil) {
					t.Errorf("unrecorded-deletes Warn logged %d time(s), want once with sources=2 (read err %v):\n%s", warns, err, logged)
				}
				return
			}
			if fi, serr := os.Stat(backup); err != nil || serr != nil || fi.Mode().Perm() != 0o600 || warns != 0 {
				t.Fatalf("backup file: read %v, stat %v, %d Warn(s); want a 0600 file and no Warn", err, serr, warns)
			}
			lines := strings.Split(strings.TrimSpace(string(b)), "\n")
			if len(lines) != 2 {
				t.Fatalf("backup holds %d record(s), want 2:\n%s", len(lines), b)
			}
			for i, line := range lines {
				var rec reconcilePathsBackupRecord
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("decode record %d: %v", i, err)
				}
				if rec.Action != "pruned" || rec.SourcePath != gone[i] || len(rec.ScanResultIDs) != 1 || len(rec.WorkItemIDs) != 1 ||
					len(rec.Inputs) != 1 || rec.Inputs[0].SourcePath != gone[i] {
					t.Errorf("record %d = %+v, want a restorable pruned record for %q", i, rec, gone[i])
				}
			}
		})
	}
}

func TestSweepBackupPath(t *testing.T) {
	cfg := config.Config{DB: config.DBConfig{Path: filepath.Join("data", "canticle.db")}}
	if got, want := sweepBackupPath(cfg), filepath.Join("data", "reconcile-paths-serve-backup.jsonl"); got != want {
		t.Errorf("sweepBackupPath = %q, want %q", got, want)
	}
}

// A planned prune the serve sweeper's apply skipped (the scan_results row is
// held by another row's in-flight work) is logged as a count.
func TestRunSweeperLogsSkippedPrune(t *testing.T) {
	ctx, _, dbPath, root := setupReconcilePaths(t)
	gone, busy := filepath.Join(root, "ArtistA", "01. gone.flac"), filepath.Join(root, "ArtistB", "01. busy.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	seedReconcilePathsRow(t, ctx, dbPath, busy)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	_, err = sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE source_path = ?`, busy)
	if err == nil {
		_, err = sqlDB.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id)
          SELECT wq.id, sr.id FROM work_queue wq, scan_results sr WHERE wq.source_path = ? AND sr.file_path = ?`, busy, gone)
	}
	if err := errors.Join(err, os.RemoveAll(filepath.Dir(gone))); err != nil {
		t.Fatal(err)
	}
	var logBuf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	cctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	backup := sweepBackupPath(config.Config{DB: config.DBConfig{Path: dbPath}})
	go func() { runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, backup); close(done) }()
	for deadline := time.Now().Add(2 * time.Second); countRows(t, ctx, dbPath, "work_queue") > 1 && time.Now().Before(deadline); {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if logged := logBuf.String(); !strings.Contains(logged, "left sources it had planned to prune") || !strings.Contains(logged, "sources=1") {
		t.Errorf("skipped-prune count line missing:\n%s", logged)
	}
}

// A planned prune the apply only partly made (the scan_results row is held by
// another row's in-flight work) is not counted as pruned, is reported as a
// count, and its backup record lists only the id that was deleted.
func TestReconcilePaths_SkippedPruneIsNotCountedAsPruned(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone, busy := filepath.Join(root, "ArtistA", "01. gone.flac"), filepath.Join(root, "ArtistB", "01. busy.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	seedReconcilePathsRow(t, ctx, dbPath, busy)
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE source_path = ?`, busy)
	if err == nil {
		_, err = sqlDB.ExecContext(ctx, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id)
          SELECT wq.id, sr.id FROM work_queue wq, scan_results sr WHERE wq.source_path = ? AND sr.file_path = ?`, busy, gone)
	}
	if err := errors.Join(err, sqlDB.Close(), os.Remove(gone)); err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(filepath.Dir(dbPath), "skip.jsonl")
	for _, tc := range []struct {
		yes        bool
		want, skip string
	}{
		{false, "would prune 1 source(s) with a vanished file (1 scan_results, 1 work_items)", ""},
		{true, "pruned 0 source(s) with a vanished file (0 scan_results, 1 work_items)", "1 planned prune(s) not or only partly applied"},
	} {
		var buf bytes.Buffer
		if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: tc.yes, Backup: backup}); code != 0 {
			t.Fatalf("yes=%v exit=%d out=%s", tc.yes, code, buf.String())
		}
		// A dry run cannot observe a skip, so it reports the plan and no skip line.
		if out := buf.String(); !strings.Contains(out, tc.want) || !strings.Contains(out, tc.skip) || strings.Contains(out, "not or only partly applied") != tc.yes {
			t.Errorf("yes=%v: want %q and %q in:\n%s", tc.yes, tc.want, tc.skip, out)
		}
	}
	b, err := os.ReadFile(backup) //nolint:gosec // test-controlled path
	var rec reconcilePathsBackupRecord
	if err := errors.Join(err, json.Unmarshal(bytes.TrimSpace(b), &rec)); err != nil {
		t.Fatalf("read backup %q: %v", b, err)
	}
	if rec.Action != "pruned" || len(rec.WorkItemIDs) != 1 || len(rec.ScanResultIDs) != 0 {
		t.Errorf("backup record = %+v, want the deleted work id only (the scan_results row survived)", rec)
	}
}

// TestRunSweeperAgesOutGoneRow: a row whose file has been gone inside a
// surviving directory for the grace period, and confirmed gone, is deleted by
// the unattended sweep, which appends its restorable record to the sweep
// backup (#1262), and by no sweep that cannot write that backup: such a sweep
// keeps the row, fails nothing and says so in one Warn that names no path.
// Before that, `scan reconcile-paths` reports it as a count.
func TestRunSweeperAgesOutGoneRow(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	gone := filepath.Join(root, "ArtistA", "01. gone.flac")
	seedReconcilePathsRow(t, ctx, dbPath, gone)
	seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistA", "02. kept.flac"))
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := sqlDB.ExecContext(ctx, `INSERT INTO prune_gone_since (path, first_seen, confirmed_at) VALUES (?, ?, ?)`,
		gone, time.Now().Add(-8*24*time.Hour).Unix(), time.Now().Add(-2*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("dry run exit %d: %s", code, buf.String())
	}
	if want := "holding 0 gone source(s) for its one-week grace period; 1 more are past it"; !strings.Contains(buf.String(), want) {
		t.Errorf("dry run output lacks %q: %s", want, buf.String())
	}
	if strings.Contains(buf.String(), "gone.flac") {
		t.Errorf("dry run output names a path: %s", buf.String())
	}
	buf.Reset() // the marks are not per library: a scoped run says nothing of them
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Library: "lib"}); code != 0 || strings.Contains(buf.String(), "grace period") {
		t.Errorf("library-scoped dry run: exit %d, output %s", code, buf.String())
	}

	var logBuf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	backup := filepath.Join(filepath.Dir(dbPath), "sweep-backup.jsonl")
	bad := ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true, Backup: filepath.Join(root, "no such dir", "b.jsonl")}
	if code, got := runReconcilePaths(ctx, &buf, bad), logBuf.String(); code != 1 || countRows(t, ctx, dbPath, "work_queue") != 2 ||
		!strings.Contains(got, "1 backup record(s) could not be written") || !strings.Contains(got, "gone.flac") {
		t.Fatalf("unwritable backup: exit %d, want 1, no row deleted without its record, and the source named; log:\n%s", code, got)
	}
	// First with a backup that cannot be opened, then one that opens but takes
	// no record (a character device), then one that works.
	for _, path := range []string{filepath.Join(filepath.Dir(dbPath), "no such dir", "b.jsonl"), os.DevNull, backup} {
		before := len(logBuf.String())
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, path); close(done) }()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && (path == backup && countRows(t, ctx, dbPath, "work_queue") > 1 ||
			path != backup && !strings.Contains(logBuf.String()[before:], "sources due for age-out were kept")) {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
		if path == backup {
			break
		}
		got := logBuf.String()[before:]
		if n := countRows(t, ctx, dbPath, "work_queue"); n != 2 || strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "sources=0 aged_out_kept=1") ||
			strings.Contains(got, "sweep returned an error") || strings.Contains(got, root) {
			t.Fatalf("unwritable backup %q: work_queue rows = %d, want 2 kept and one Warn with the count and no library path; log:\n%s", path, n, got)
		}
		if path != os.DevNull {
			continue
		}
		// A row the delete skipped after its record was written: the correction
		// that cannot be written is counted as unrecorded, since the file now
		// overstates what was deleted, and nothing more is deleted for it.
		before = len(logBuf.String())
		b := &sweepBackup{path: path}
		if err := b.report(prune.PrunedRow{SourcePath: gone, AgedOut: true, Corrects: true}); !errors.Is(err, prune.ErrNotRecorded) {
			t.Fatalf("failed correction: err = %v, want prune.ErrNotRecorded", err)
		}
		b.close()
		if got := logBuf.String()[before:]; strings.Count(got, "level=WARN") != 1 || !strings.Contains(got, "could not record every source it deleted") ||
			!strings.Contains(got, " sources=1 ") || strings.Contains(got, root) {
			t.Fatalf("failed correction: want one Warn counting it as unrecorded (sources=1) and no library path; log:\n%s", got)
		}
	}
	if n := countRows(t, ctx, dbPath, "work_queue"); n != 1 {
		t.Fatalf("work_queue rows = %d, want 1 (the aged-out row deleted)", n)
	}
	raw, err := os.ReadFile(backup) //nolint:gosec // reason: a path under the test's temp dir
	if err != nil {
		t.Fatalf("read sweep backup: %v", err)
	}
	var rec reconcilePathsBackupRecord
	if err := json.Unmarshal(bytes.TrimSpace(raw), &rec); err != nil {
		t.Fatalf("sweep backup is not one JSON record: %v: %s", err, raw)
	}
	if rec.Action != "pruned" || rec.SourcePath != gone || len(rec.Inputs) != 1 || rec.Reason == "" || len(rec.WorkStates) != 1 || rec.WorkStates[0].Status != "failed" {
		t.Errorf("sweep backup record = %+v, want a restorable pruned record with the age-out reason and the row's state", rec)
	}
}

// TestRunSweeperBreakerAndItsAdvice: over the cap the unattended sweep deletes nothing and warns with a
// count and no path; the command its Warn names plans those deletes and with --yes makes them, identity
// or none (not a mark unconfirmed or with a same-name file on disk), and releases the marks still held.
func TestRunSweeperBreakerAndItsAdvice(t *testing.T) {
	ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
	const n = 51
	seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistA", "kept.flac"))
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	old := time.Now().Add(-2 * time.Hour).Unix()
	for i, confirmed := range append(make([]any, n, n+3), nil, time.Now().Add(-time.Minute).Unix(), old) {
		gone := filepath.Join(root, "ArtistA", fmt.Sprintf("%02d gone.flac", i))
		seedReconcilePathsRowWithIdentity(t, ctx, dbPath, gone, "")
		if i < n {
			confirmed = old
		}
		if _, err := sqlDB.ExecContext(ctx, `INSERT INTO prune_gone_since (path, first_seen, confirmed_at) VALUES (?, ?, ?)`,
			gone, time.Now().Add(-8*24*time.Hour).Unix(), confirmed); err != nil || os.Remove(gone) != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "ArtistA", "53 gone.mp3"), []byte("audio"), 0o600); err != nil || os.Chtimes(filepath.Join(root, "ArtistA"), time.Unix(old, 0), time.Unix(old, 0)) != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 || !strings.Contains(buf.String(), "would prune 51 source(s)") || !strings.Contains(buf.String(), "retained 3 source(s)") {
		t.Fatalf("dry run exit %d, want the 51 planned and 3 retained: %s", code, buf.String())
	}
	var logBuf lockedBuffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	sweep := func(until string) {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { runSweeper(cctx, sqlDB, time.Hour, config.RealignConfig{}, dbPath+".sweep.jsonl"); close(done) }()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline) && !strings.Contains(logBuf.String(), until); {
			time.Sleep(5 * time.Millisecond)
		}
		cancel()
		<-done
	}
	sweep("too many rows due")
	rows := func() int { return countRows(t, ctx, dbPath, "work_queue") }
	if got := logBuf.String(); !strings.Contains(got, "scan reconcile-paths --yes") || !strings.Contains(got, "sources=51") || strings.Contains(got, "gone.flac") || rows() != n+4 {
		t.Fatalf("tripped breaker: work_queue rows = %d, want %d and one Warn with the command and the count, no path; log:\n%s", rows(), n+4, got)
	}
	if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath}); code != 0 || strings.Count(buf.String(), "would prune 51 source(s)") != 2 ||
		countRows(t, ctx, dbPath, "prune_gone_since WHERE confirmed_at = -1") != n {
		t.Fatalf("dry run after the trip: exit %d, want the 51 planned again and every held mark left held: %s", code, buf.String())
	}
	// Held marks are reported as held, with the command that deletes them, never as due for a sweep.
	if out := buf.String(); !strings.Contains(out, "51 gone source(s) past the grace period are held") || !strings.Contains(out, "run `scan reconcile-paths --yes`") ||
		!strings.Contains(out, "; 2 more are past it") || strings.Contains(out, "gone.flac") {
		t.Fatalf("dry run after the trip: want the 51 held marks counted as held, not as due for a sweep, and no path: %s", out)
	}
	// An apply scoped to another library is not the exit: it releases no held mark.
	other := filepath.Join(filepath.Dir(root), "other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := library.New(sqlDB).Add(ctx, other, "other", models.LibrarySettings{}); err != nil || os.WriteFile(filepath.Join(other, "a.flac"), []byte("audio"), 0o600) != nil {
		t.Fatal(err)
	}
	scoped := ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true, Library: "other", Backup: filepath.Join(filepath.Dir(dbPath), "other-backup.jsonl")}
	if code := runReconcilePaths(ctx, &buf, scoped); code != 0 || rows() != n+4 || countRows(t, ctx, dbPath, "prune_gone_since WHERE confirmed_at = -1") != n {
		t.Fatalf("apply scoped to another library: exit %d, work_queue rows = %d, marks still held = %d, want 0, %d and %d", code, rows(), countRows(t, ctx, dbPath, "prune_gone_since WHERE confirmed_at = -1"), n+4, n)
	}
	if _, err := sqlDB.ExecContext(ctx, `CREATE TRIGGER keep BEFORE DELETE ON work_queue WHEN old.source_path LIKE '%50 gone.flac' BEGIN SELECT RAISE(IGNORE); END`); err != nil {
		t.Fatal(err) // the trigger stands in for the delete's own guards skipping one queue row
	}
	buf.Reset()
	backup := filepath.Join(filepath.Dir(dbPath), "cli-backup.jsonl")
	code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: true, Backup: backup})
	raw, err := os.ReadFile(backup)                                               //nolint:gosec // reason: a path under the test's temp dir
	held := countRows(t, ctx, dbPath, "prune_gone_since WHERE confirmed_at = -1") // the row the delete skipped
	if code != 0 || err != nil || rows() != 5 || held != 0 || !strings.Contains(buf.String(), "pruned 50 source(s)") ||
		bytes.Count(raw, []byte(`"action":"pruned"`)) != n || bytes.Count(raw, []byte(`"action":"pruned-corrected"`)) != 1 {
		t.Fatalf("the advised command: exit %d (err %v), work_queue rows = %d, marks still held = %d, want 5, 0, 50 pruned, %d records and one correction: %s", code, err, rows(), held, n, buf.String())
	}
	if sweep("left sources it had planned to prune"); !strings.Contains(logBuf.String(), "had moved to another file since it was read\" sources=1") {
		t.Fatalf("the sweep after it did not log its skipped delete as a count; log:\n%s", logBuf.String())
	}
}

// A retained source whose queue row is still dequeue-eligible is counted in one
// aggregate line (#1430): suffixed in a dry run, bare under --yes, absent when
// nothing is held, and never naming a path, artist or title.
func TestReconcilePaths_ReportsRetainedSourcesHoldingWork(t *testing.T) {
	const heldLine = "reconcile-paths: 1 retained source(s) still hold dequeue-eligible work, which the worker keeps attempting"
	for _, tc := range []struct {
		name      string
		ambiguous bool
		yes       bool
		wantLine  bool
	}{
		{"dry run", true, false, true},
		{"applied", true, true, true},
		{"nothing held", false, false, false},
	} {
		ctx, cfgPath, dbPath, root := setupReconcilePaths(t)
		gone := filepath.Join(root, "ArtistA", "01. gone.flac")
		seedReconcilePathsRow(t, ctx, dbPath, gone)
		if tc.ambiguous {
			// Two present files share the gone row's identity: it is retained, not pruned.
			for _, name := range []string{"D1", "D2"} {
				seedReconcilePathsRow(t, ctx, dbPath, filepath.Join(root, "ArtistA", name, "02. "+name+".flac"))
			}
			sqlDB, err := db.Open(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			_, err = sqlDB.ExecContext(ctx, `UPDATE scan_results SET recording_mbid = ? WHERE file_path <> ?`, "mbid-nomatch-"+gone, gone)
			if err := errors.Join(err, sqlDB.Close()); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Remove(gone); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if code := runReconcilePaths(ctx, &buf, ScanReconcilePathsCmd{ConfigPath: cfgPath, Yes: tc.yes, Backup: filepath.Join(filepath.Dir(dbPath), "held.jsonl")}); code != 0 {
			t.Fatalf("%s: exit=%d out=%s", tc.name, code, buf.String())
		}
		out := buf.String()
		want := heldLine
		if !tc.yes {
			want += suffixDryRun(false)
		}
		var line string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "still hold dequeue-eligible work") {
				line = l
			}
		}
		if tc.wantLine && line != want {
			t.Errorf("%s: held line = %q, want %q\nfull output:\n%s", tc.name, line, want, out)
		}
		if !tc.wantLine && line != "" {
			t.Errorf("%s: unexpected held line %q", tc.name, line)
		}
		for _, leak := range []string{"ArtistA", "Artist", "Title", "gone.flac", root} {
			if strings.Contains(line, leak) {
				t.Errorf("%s: held line leaks %q: %s", tc.name, leak, line)
			}
		}
	}
}
