package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
// stat. Returns nothing; the tests assert via row counts and command output.
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
		`INSERT INTO scan_results (library_id, file_path, artist, title, status) VALUES (1, ?, 'Artist', 'Title', 'processing')`,
		filePath)
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
	go func() { runSweeper(cctx, sqlDB, time.Hour); close(done) }()

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
