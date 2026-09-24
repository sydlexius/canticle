package commands

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
)

// seedSyncTierCandidate builds a config + DB with one library and one
// completed synced work_queue row whose source_path points at track.flac in
// dir. Returns the config/db paths and the row's id. No .lrc is written --
// callers do that themselves so each test controls the sidecar's shape.
func seedSyncTierCandidate(t *testing.T) (ctx context.Context, cfgPath, dbPath, dir string, id int64) {
	t.Helper()
	ctx = context.Background()
	dir = t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	audioPath := filepath.Join(dir, "track.flac")
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type)
         VALUES ('A', 'T', 'a', 't', ?, 'done', 'synced') RETURNING id`, audioPath).Scan(&id); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	return ctx, cfgPath, dbPath, dir, id
}

func readSyncTierCol(t *testing.T, ctx context.Context, dbPath string, id int64) sql.NullString {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var tier sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT sync_tier FROM work_queue WHERE id = ?`, id).Scan(&tier); err != nil {
		t.Fatalf("read sync_tier: %v", err)
	}
	return tier
}

// TestRunReconcileSyncTier_DryRun reports the classification but mutates
// nothing and writes no backup.
func TestRunReconcileSyncTier_DryRun(t *testing.T) {
	ctx, cfgPath, dbPath, dir, id := seedSyncTierCandidate(t)
	if err := os.WriteFile(filepath.Join(dir, "track.lrc"), []byte("[00:01.00]alpha\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	var buf bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf, ScanReconcileSyncTierCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would classify 0 as word, 1 as line, 0 as unsynced") {
		t.Errorf("want line=1 dry-run; got: %s", buf.String())
	}
	if got := readSyncTierCol(t, ctx, dbPath, id); got.Valid {
		t.Errorf("dry-run stamped sync_tier = %q; want untouched NULL", got.String)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "reconcile-sync-tier-backup-*.jsonl")); len(m) != 0 {
		t.Errorf("dry-run wrote a backup: %v", m)
	}
}

// TestRunReconcileSyncTier_ApplyClassifiesEachTier covers word/line/unsynced
// classification under --yes, with a decodable, restorable backup record for
// each applied row, and a second run finding nothing left to classify.
func TestRunReconcileSyncTier_ApplyClassifiesEachTier(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	seed := func(name, lrcBody string) int64 {
		audioPath := filepath.Join(dir, name+".flac")
		var id int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type)
             VALUES ('A', ?, 'a', ?, ?, 'done', 'synced') RETURNING id`, name, name, audioPath).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".lrc"), []byte(lrcBody), 0o600); err != nil {
			t.Fatalf("write lrc %s: %v", name, err)
		}
		return id
	}
	wordID := seed("word", "[00:01.00]<00:01.00>alpha <00:01.50>beta\n")
	lineID := seed("line", "[00:01.00]alpha\n[00:02.00]beta\n")
	unsyncedID := seed("unsynced", "plain words, no timestamps\n")
	sqlDB.Close() //nolint:errcheck // test cleanup

	var buf bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf, ScanReconcileSyncTierCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "classified 1 as word, 1 as line, 1 as unsynced") {
		t.Errorf("want one of each tier; got: %s", buf.String())
	}
	for id, want := range map[int64]string{wordID: "word", lineID: "line", unsyncedID: "unsynced"} {
		if got := readSyncTierCol(t, ctx, dbPath, id); !got.Valid || got.String != want {
			t.Errorf("id %d: sync_tier = %+v, want %q", id, got, want)
		}
	}

	matches, _ := filepath.Glob(filepath.Join(dir, "reconcile-sync-tier-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 backup records, got %d: %q", len(lines), b)
	}
	seen := map[int64]string{}
	for _, l := range lines {
		var rec syncTierBackupRecord
		if err := json.Unmarshal([]byte(l), &rec); err != nil {
			t.Fatalf("decode backup line %q: %v", l, err)
		}
		seen[rec.ID] = rec.Tier
	}
	if seen[wordID] != "word" || seen[lineID] != "line" || seen[unsyncedID] != "unsynced" {
		t.Errorf("backup records = %+v; want word/line/unsynced per id", seen)
	}

	var buf2 bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf2, ScanReconcileSyncTierCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second run exit=%d out=%s", code, buf2.String())
	}
	if !strings.Contains(buf2.String(), "scanned 0 row(s)") {
		t.Errorf("second run want nothing left to classify; got: %s", buf2.String())
	}
}

// TestRunReconcileSyncTier_MissingSidecarCounted: a candidate whose sidecar
// does not exist is counted and left NULL, never guessed.
func TestRunReconcileSyncTier_MissingSidecarCounted(t *testing.T) {
	ctx, cfgPath, dbPath, _, id := seedSyncTierCandidate(t)
	// No .lrc written at all.
	var buf bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf, ScanReconcileSyncTierCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "no_sidecar=1") {
		t.Errorf("want no_sidecar=1; got: %s", buf.String())
	}
	if got := readSyncTierCol(t, ctx, dbPath, id); got.Valid {
		t.Errorf("sync_tier = %q, want left NULL for a missing sidecar", got.String)
	}
}

// TestRunReconcileSyncTier_ExtensionCaseVariantResolved: a sidecar saved
// under an uppercase extension is still found and classified, reusing
// revalidate's bounded case-variant probe rather than a directory read.
func TestRunReconcileSyncTier_ExtensionCaseVariantResolved(t *testing.T) {
	ctx, cfgPath, dbPath, dir, id := seedSyncTierCandidate(t)
	if err := os.WriteFile(filepath.Join(dir, "track.LRC"), []byte("[00:01.00]alpha\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	var buf bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf, ScanReconcileSyncTierCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if got := readSyncTierCol(t, ctx, dbPath, id); !got.Valid || got.String != "line" {
		t.Errorf("sync_tier = %+v, want \"line\" via the extension-case variant", got)
	}
}

// TestReconcileSyncTier_ReachableThroughRun proves the dispatch case
// actually routes real argv to the handler -- driving runReconcileSyncTier
// directly (as every other test in this file does) says nothing about
// whether anything can reach it (a declared-but-unreachable subcommand
// shipped in v1.20.0, per reconcile_detector_stats_test.go's sibling check).
func TestReconcileSyncTier_ReachableThroughRun(t *testing.T) {
	ctx, cfgPath, dbPath, dir, id := seedSyncTierCandidate(t)
	if err := os.WriteFile(filepath.Join(dir, "track.lrc"), []byte("[00:01.00]alpha\n"), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "reconcile-sync-tier", "--yes", "--config", cfgPath}, &out, Deps{})
	if code != 0 {
		t.Fatalf("exit code = %d; want 0. output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "reconcile-sync-tier:") {
		t.Errorf("real argv did not reach the handler:\n%s", out.String())
	}
	if got := readSyncTierCol(t, ctx, dbPath, id); !got.Valid || got.String != "line" {
		t.Errorf("sync_tier = %+v, want \"line\" (real argv reached --yes)", got)
	}
}

// TestRunReconcileSyncTier_ExcludesNonCandidates: a row that is not
// (outcome_type='synced' AND status='done' AND sync_tier IS NULL) must never
// be touched -- an already-tiered row, a wrong status, and a wrong
// outcome_type all leave the candidate set untouched.
func TestRunReconcileSyncTier_ExcludesNonCandidates(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath := filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	mustSeed := func(key, status, outcome, tier any) {
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, sync_tier)
             VALUES ('A', ?, 'a', ?, '', ?, ?, ?)`, key, key, status, outcome, tier); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	mustSeed("t1", "pending", "synced", nil)
	mustSeed("t2", "done", "unsynced", nil)
	mustSeed("t3", "done", "synced", "line")
	sqlDB.Close() //nolint:errcheck // test cleanup

	var buf bytes.Buffer
	if code := runReconcileSyncTier(ctx, &buf, ScanReconcileSyncTierCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "scanned 0 row(s)") {
		t.Errorf("want no candidates admitted; got: %s", buf.String())
	}
}
