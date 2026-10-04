package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
