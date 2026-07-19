package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

// seedDetectorStatsDB creates a database with one detected-instrumental row, one
// detector-miss row, and one never-detected row, and writes a config pointing at
// it.
func setupDetectorStats(t *testing.T) (ctx context.Context, cfgPath, dbPath string) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	rows := []struct {
		title  string
		result any
	}{
		{"Detected", 1},
		{"NotInstrumental", 0},
		{"NeverDetected", nil},
	}
	for _, r := range rows {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, instrumental_result, updated_at)
             VALUES (?, ?, ?, ?, ?, ?)`,
			"Artist", r.title, "Artist", r.title, r.result, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatalf("seed %q: %v", r.title, err)
		}
	}
	return ctx, cfgPath, dbPath
}

func TestRunReconcileDetectorStats_DryRunWritesNothing(t *testing.T) {
	ctx, cfgPath, dbPath := setupDetectorStats(t)

	var out bytes.Buffer
	if code := runReconcileDetectorStats(ctx, &out, ScanReconcileDetectorStatsCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit code = %d; want 0. output:\n%s", code, out.String())
	}

	if got := countRows(t, ctx, dbPath, "lane_attempts"); got != 0 {
		t.Errorf("lane_attempts rows = %d after dry run; want 0", got)
	}
	s := out.String()
	if !strings.Contains(s, "dry run") {
		t.Errorf("output lacks the dry-run suffix:\n%s", s)
	}
	if !strings.Contains(s, "would attribute 2") {
		t.Errorf("output should report 2 attributable rows:\n%s", s)
	}
}

func TestRunReconcileDetectorStats_ApplyWritesBothBucketsAndBackup(t *testing.T) {
	ctx, cfgPath, dbPath := setupDetectorStats(t)
	backup := filepath.Join(t.TempDir(), "backup.jsonl")

	var out bytes.Buffer
	code := runReconcileDetectorStats(ctx, &out, ScanReconcileDetectorStatsCmd{
		ConfigPath: cfgPath, Yes: true, Backup: backup,
	})
	if code != 0 {
		t.Fatalf("exit code = %d; want 0. output:\n%s", code, out.String())
	}

	if got := countRows(t, ctx, dbPath, "lane_attempts"); got != 2 {
		t.Errorf("lane_attempts rows = %d; want 2", got)
	}
	s := out.String()
	if !strings.Contains(s, "attributed 2 (1 hits, 1 misses") {
		t.Errorf("output should report both buckets:\n%s", s)
	}

	// Both uncovered remainders must be stated, not just the countable one.
	if !strings.Contains(s, "1 row(s) have no recorded detection verdict") {
		t.Errorf("output should report the NULL remainder:\n%s", s)
	}
	if !strings.Contains(s, "ClearDone") {
		t.Errorf("output should state the uncountable ClearDone remainder:\n%s", s)
	}

	data, err := os.ReadFile(backup) //nolint:gosec // G304: test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("backup has %d record(s); want 2", len(lines))
	}
	for _, ln := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(ln), &rec); err != nil {
			t.Fatalf("backup record is not JSON: %v (%q)", err, ln)
		}
		if rec["lane"] != "detector" {
			t.Errorf("backup record lane = %v; want detector", rec["lane"])
		}
	}
}

func TestRunReconcileDetectorStats_RerunIsNoOp(t *testing.T) {
	ctx, cfgPath, dbPath := setupDetectorStats(t)

	var first bytes.Buffer
	if code := runReconcileDetectorStats(ctx, &first, ScanReconcileDetectorStatsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("first run exit = %d", code)
	}
	before := countRows(t, ctx, dbPath, "lane_attempts")

	var second bytes.Buffer
	if code := runReconcileDetectorStats(ctx, &second, ScanReconcileDetectorStatsCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second run exit = %d", code)
	}

	if got := countRows(t, ctx, dbPath, "lane_attempts"); got != before {
		t.Errorf("lane_attempts rows = %d after rerun; want %d (no double-count)", got, before)
	}
	if !strings.Contains(second.String(), "2 already recorded") {
		t.Errorf("rerun should report the rows as already recorded:\n%s", second.String())
	}
}

// The dispatch case is only exercised through the real parser with real argv.
// Driving runReconcileDetectorStats directly proves the handler works and says
// nothing about whether anything can reach it -- the exact gap that let a
// declared-but-unreachable subcommand ship in v1.20.0.
func TestReconcileDetectorStats_ReachableThroughRun(t *testing.T) {
	_, cfgPath, dbPath := setupDetectorStats(t)

	var out bytes.Buffer
	code := Run(context.Background(),
		[]string{"scan", "reconcile-detector-stats", "--config", cfgPath}, &out, Deps{})
	if code != 0 {
		t.Fatalf("exit code = %d; want 0. output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "reconcile-detector-stats:") {
		t.Errorf("real argv did not reach the handler:\n%s", out.String())
	}
	if got := countRows(t, context.Background(), dbPath, "lane_attempts"); got != 0 {
		t.Errorf("lane_attempts rows = %d; want 0 (no --yes means dry run)", got)
	}
}

func TestRunReconcileDetectorStats_UnreadableConfig(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte("this is not = valid toml ["), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out bytes.Buffer
	if code := runReconcileDetectorStats(context.Background(), &out, ScanReconcileDetectorStatsCmd{ConfigPath: cfgPath}); code != 1 {
		t.Errorf("exit code = %d; want 1 on an unparsable config", code)
	}
}

func TestRunReconcileDetectorStats_UnopenableDatabase(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	// A db path under a file (not a directory) cannot be opened.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	reconcilePathsCfg(t, cfgPath, filepath.Join(blocker, "nested.db"))

	var out bytes.Buffer
	if code := runReconcileDetectorStats(context.Background(), &out, ScanReconcileDetectorStatsCmd{ConfigPath: cfgPath}); code != 1 {
		t.Errorf("exit code = %d; want 1 when the database cannot be opened", code)
	}
}

// An unwritable backup path must abort the run and leave nothing attributed.
// This exercises the backup-first ordering end to end: report runs inside the
// engine's transaction before commit, so a backup failure rolls the whole
// attribution back rather than applying it without a restorable record.
func TestRunReconcileDetectorStats_UnwritableBackupRollsBack(t *testing.T) {
	ctx, cfgPath, dbPath := setupDetectorStats(t)
	// A path under a non-existent directory cannot be created.
	backup := filepath.Join(t.TempDir(), "no-such-dir", "backup.jsonl")

	var out bytes.Buffer
	code := runReconcileDetectorStats(ctx, &out, ScanReconcileDetectorStatsCmd{
		ConfigPath: cfgPath, Yes: true, Backup: backup,
	})
	if code != 1 {
		t.Errorf("exit code = %d; want 1 when the backup cannot be written", code)
	}
	if got := countRows(t, ctx, dbPath, "lane_attempts"); got != 0 {
		t.Errorf("lane_attempts rows = %d; want 0 -- an unrecordable attribution must not be applied", got)
	}
}
