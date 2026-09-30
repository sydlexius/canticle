package commands

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
)

const remArtist = "SecretArtist"

type remRow struct {
	name, files, extra string
	lrc                string
}

// seedRemediated builds a DB with one tier-unknown synced row per shape and a
// cache entry for each. files is the sidecar extension list to create.
func seedRemediated(t *testing.T) (ctx context.Context, cfgPath, dbPath, dir string, ids map[string]int64) {
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
	line := "[00:01.00]alpha\n[00:02.00]beta\n"
	rows := []remRow{
		{name: "missing"},
		{name: "txtonly", files: ".txt"},
		{name: "haslrc", files: ".lrc", lrc: line},
		{name: "upperlrc", files: ".LRC", lrc: line},
		{name: "keptverdict", files: ".lrc", lrc: line, extra: ", timing_outcome = 'mis_synced'"},
		{name: "plainlrc", files: ".lrc", lrc: "just words\n"},
		{name: "retired", extra: ", last_error = '" + queue.UnresolvableGoneError + "'"},
		{name: "inflight", extra: ", status = 'processing'"},
		{name: "wordq", files: ".lrc", lrc: line, extra: ", word_timing_state = 'queued'"},
	}
	ids = map[string]int64{}
	for _, r := range rows {
		var id int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type)
             VALUES (?, ?, 'a', ?, ?, 'done', 'synced') RETURNING id`, remArtist, r.name, r.name, filepath.Join(dir, r.name+".flac")).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", r.name, err)
		}
		if r.extra != "" {
			if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET id = id`+r.extra+` WHERE id = ?`, id); err != nil {
				t.Fatalf("extra %s: %v", r.name, err)
			}
		}
		if r.files != "" {
			if err := os.WriteFile(filepath.Join(dir, r.name+r.files), []byte(r.lrc), 0o600); err != nil {
				t.Fatalf("write %s: %v", r.name, err)
			}
		}
		if err := cache.New(sqlDB).Store(ctx, remArtist, r.name, 200, "cached"); err != nil {
			t.Fatalf("cache %s: %v", r.name, err)
		}
		ids[r.name] = id
	}
	return
}

func remSnapshot(t *testing.T, ctx context.Context, dbPath string) string {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var s string
	if err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(group_concat(id||'|'||status||'|'||COALESCE(outcome_type,'')||'|'||COALESCE(sync_tier,'')||'|'||COALESCE(timing_outcome,''), ';'),'') ||
        (SELECT count(*) FROM lyrics_cache) FROM (SELECT * FROM work_queue ORDER BY id)`).Scan(&s); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return s
}

func remRowState(t *testing.T, ctx context.Context, dbPath string, id int64) (status string, outcome, tier sql.NullString) {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if err := sqlDB.QueryRowContext(ctx, `SELECT status, outcome_type, sync_tier FROM work_queue WHERE id = ?`, id).Scan(&status, &outcome, &tier); err != nil {
		t.Fatalf("state: %v", err)
	}
	return
}

func TestRunReconcileRemediated_DryRunWritesNothingAndIsAggregateOnly(t *testing.T) {
	ctx, cfgPath, dbPath, dir, _ := seedRemediated(t)
	before := remSnapshot(t, ctx, dbPath)
	var buf bytes.Buffer
	if code := runReconcileRemediated(ctx, &buf, ScanReconcileRemediatedCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	out := buf.String()
	for _, want := range []string{"scanned 9 candidate row(s)", "would reset 1 for re-fetch", "would re-describe 1 as unsynced", "would record tier for 2", "[dry run; pass --yes to apply]",
		"kept_remediation_verdict=1", "unsynced_lrc=1", "retired=1", "processing=1", "word_recheck=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	for _, leak := range []string{remArtist, dir, "missing", "txtonly", "haslrc", "alpha"} {
		if strings.Contains(out, leak) {
			t.Errorf("output leaks %q:\n%s", leak, out)
		}
	}
	if after := remSnapshot(t, ctx, dbPath); after != before {
		t.Errorf("dry run changed the database:\nbefore %s\nafter  %s", before, after)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "reconcile-remediated-backup-*.jsonl")); len(m) != 0 {
		t.Errorf("dry run wrote a backup: %v", m)
	}
}

func TestRunReconcileRemediated_ApplyEachAction(t *testing.T) {
	ctx, cfgPath, dbPath, dir, ids := seedRemediated(t)
	backup := filepath.Join(dir, "b.jsonl")
	var buf bytes.Buffer
	if code := runReconcileRemediated(ctx, &buf, ScanReconcileRemediatedCmd{ConfigPath: cfgPath, Yes: true, Backup: backup}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "did reset 1 for re-fetch, did re-describe 1 as unsynced, did record tier for 2") {
		t.Errorf("summary: %s", buf.String())
	}
	if st, o, _ := remRowState(t, ctx, dbPath, ids["missing"]); st != "deferred" || o.Valid {
		t.Errorf("missing: status=%s outcome=%+v, want deferred/NULL", st, o)
	}
	if _, o, _ := remRowState(t, ctx, dbPath, ids["txtonly"]); o.String != "unsynced" {
		t.Errorf("txtonly outcome = %+v", o)
	}
	for _, n := range []string{"haslrc", "upperlrc", "keptverdict"} {
		if _, _, tier := remRowState(t, ctx, dbPath, ids[n]); tier.String != "line" {
			t.Errorf("%s tier = %+v, want line", n, tier)
		}
	}
	for _, n := range []string{"retired", "inflight", "wordq", "plainlrc"} {
		if _, o, tier := remRowState(t, ctx, dbPath, ids[n]); tier.Valid || o.String != "synced" {
			t.Errorf("%s was changed: outcome=%+v tier=%+v", n, o, tier)
		}
	}
	sqlDB, _ := db.Open(ctx, dbPath)
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if got, _ := cache.New(sqlDB).Lookup(ctx, remArtist, "missing", 200); got != "" {
		t.Errorf("reset row still served from cache: %q", got)
	}
	if got, _ := cache.New(sqlDB).Lookup(ctx, remArtist, "haslrc", 200); got == "" {
		t.Error("non-reset row lost its cache entry")
	}
	b, err := os.ReadFile(backup) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if n := strings.Count(string(b), "\n"); n != 5 {
		t.Errorf("want 5 backup records, got %d: %s", n, b)
	}
	if strings.Contains(string(b), remArtist) || strings.Contains(string(b), dir) {
		t.Errorf("backup leaks identity: %s", b)
	}
	// A rerun finds nothing left to change for the actioned rows.
	buf.Reset()
	if code := runReconcileRemediated(ctx, &buf, ScanReconcileRemediatedCmd{ConfigPath: cfgPath, Yes: true, Backup: backup}); code != 0 {
		t.Fatalf("rerun exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "did reset 0 for re-fetch, did re-describe 0 as unsynced, did record tier for 0") {
		t.Errorf("rerun not idempotent: %s", buf.String())
	}
}
