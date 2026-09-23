package commands

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// Fixture identity strings: none may ever reach stdout.
const (
	wsArtist = "Zyxwv Fixture Artist"
	wsTitle  = "Qqqq Fixture Title"
	wsPath   = "/fixture-lib/Zyxwv/qqqq-private.flac"
)

// setupWordSync writes a config (extra appended verbatim) and a DB holding two
// libraries and four candidate rows completed on 2026-01-01..04; rows 1-2 are
// linked to library "one", rows 3-4 to "two". A lyrics_cache row is seeded so
// the dry-run check covers a populated cache.
func setupWordSync(t *testing.T, extra string) (ctx context.Context, cfgPath string, dbh *sql.DB) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	content := "[db]\npath = \"" + strings.ReplaceAll(dbPath, `\`, `\\`) + "\"\n[api]\ncooldown = 60\n" + extra
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	dbh, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbh.ExecContext(ctx, q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	exec(`INSERT INTO libraries (id, path, name) VALUES (1, '/fixture-lib/a', 'one'), (2, '/fixture-lib/b', 'two')`)
	for i := 1; i <= 4; i++ {
		lib := 1 + (i-1)/2
		key := fmt.Sprintf("%s %d", wsTitle, i)
		path := fmt.Sprintf("%s.%d", wsPath, i)
		exec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, source_path, status, outcome_type, timing_outcome, completed_at, priority)
		      VALUES (?, ?, ?, 'zyxwv', ?, ?, 'done', 'synced', 'ok', ?, 5)`,
			i, wsArtist, key, strings.ToLower(key), path, fmt.Sprintf("2026-01-0%dT00:00:00Z", i))
		exec(`INSERT INTO scan_results (id, library_id, file_path, artist, title, artist_key, title_key, status)
		      VALUES (?, ?, ?, ?, ?, 'zyxwv', ?, 'done')`, i, lib, path, wsArtist, key, strings.ToLower(key))
		exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, i, i)
	}
	if err := cache.New(dbh).Store(ctx, wsArtist, wsTitle, 0, `{"x":1}`); err != nil {
		t.Fatalf("seed cache: %v", err)
	}
	return ctx, cfgPath, dbh
}

// dumpTable renders every row of table, every column, as one string.
func dumpTable(t *testing.T, dbh *sql.DB, table string) string {
	t.Helper()
	rows, err := dbh.Query("SELECT * FROM " + table + " ORDER BY rowid") //nolint:gosec // reason: G202 -- table is a test literal
	if err != nil {
		t.Fatalf("dump %s: %v", table, err)
	}
	defer rows.Close() //nolint:errcheck // reason: test cleanup
	cols, _ := rows.Columns()
	var b strings.Builder
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("scan %s: %v", table, err)
		}
		fmt.Fprintf(&b, "%v\n", vals)
	}
	return b.String()
}

// snapshot dumps every table a dry run could touch.
func snapshot(t *testing.T, dbh *sql.DB) map[string]string {
	t.Helper()
	m := map[string]string{}
	for _, tbl := range []string{"work_queue", "lyrics_cache", "scan_results", "work_queue_scan_results"} {
		m[tbl] = dumpTable(t, dbh, tbl)
	}
	return m
}

// queuedIDs lists the flipped row ids, e.g. "1,2,3".
func queuedIDs(t *testing.T, dbh *sql.DB) string {
	t.Helper()
	var ids sql.NullString
	if err := dbh.QueryRow(`SELECT group_concat(id) FROM (SELECT id FROM work_queue WHERE word_timing_state = 'queued' ORDER BY id)`).Scan(&ids); err != nil {
		t.Fatalf("queued ids: %v", err)
	}
	return ids.String
}

func runWS(t *testing.T, ctx context.Context, args ScanReconcileWordSyncCmd) (int, string) {
	t.Helper()
	var buf bytes.Buffer
	code := runReconcileWordSync(ctx, &buf, args)
	return code, buf.String()
}

// A dry run reports the aggregate and changes no table at all.
func TestReconcileWordSync_DryRunWritesNothing(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "")
	before := snapshot(t, dbh)
	code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Limit: 3})
	if code != 0 || !strings.Contains(out, "candidates=4 selected=3 already-queued=0 estimated-minimum-drain=3m0s") {
		t.Errorf("exit=%d; aggregate line missing: %s", code, out)
	}
	if !strings.Contains(out, "canticle serve") || !strings.Contains(out, "dry run") {
		t.Errorf("want the serve-drains and dry-run notes: %s", out)
	}
	for tbl, after := range snapshot(t, dbh) {
		if after != before[tbl] {
			t.Errorf("dry run changed %s:\nbefore %s\nafter  %s", tbl, before[tbl], after)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(cfgPath), "reconcile-word-sync-backup-*")); len(m) != 0 {
		t.Errorf("dry run wrote a backup: %v", m)
	}
	// The estimate paces at serve's worker interval, which outranks api.cooldown.
	ctx, cfgPath, _ = setupWordSync(t, "[server]\nwork_interval_seconds = 15\n")
	if _, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Limit: 3}); !strings.Contains(out, "estimated-minimum-drain=45s") {
		t.Errorf("drain ignores server.work_interval_seconds: %s", out)
	}
}

// Under word_sync_mode = off the command refuses, exits non-zero, and writes
// nothing, even with --yes.
func TestReconcileWordSync_RefusesModeOff(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "[output]\nword_sync_mode = \"off\"\n")
	before := snapshot(t, dbh)
	code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Yes: true})
	if code == 0 || !strings.Contains(out, "word_sync_mode is off") {
		t.Errorf("exit=%d; want the mode-off refusal: %s", code, out)
	}
	if after := snapshot(t, dbh); !reflect.DeepEqual(before, after) {
		t.Errorf("refused run changed a table")
	}
}

// --yes queues the oldest rows up to --limit, writes one backup record per row,
// prints no fixture identity, and the backup restores every row exactly.
func TestReconcileWordSync_ApplyBackupRoundTrip(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "")
	// Every column the flip writes gets a prior value the flip changes (a stale
	// generation's absent verdict re-opens), so a field missing from the backup
	// fails the restore.
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET attempts = 2 + id, last_error = 'prior ' || id, refused_waits = 1 + id,
		next_attempt_at = '2025-12-0' || id || 'T00:00:00Z', word_timing_state = 'absent', word_timing_generation = 7`); err != nil {
		t.Fatalf("seed prior state: %v", err)
	}
	before := dumpTable(t, dbh, "work_queue")
	backup := filepath.Join(t.TempDir(), "b.jsonl")
	code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Yes: true, Limit: 3, Backup: backup})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, out)
	}
	for _, s := range []string{wsArtist, wsTitle, "Zyxwv", "qqqq-private", "fixture-lib"} {
		if strings.Contains(out, s) {
			t.Errorf("stdout leaks fixture identity %q: %s", s, out)
		}
	}
	if !strings.Contains(out, "queued=3") {
		t.Errorf("want queued=3: %s", out)
	}
	if got := queuedIDs(t, dbh); got != "1,2,3" {
		t.Fatalf("queued ids = %v; want the three oldest", got)
	}
	if _, out2 := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath}); !strings.Contains(out2, "candidates=1 selected=1 already-queued=3") {
		t.Errorf("second run aggregate: %s", out2)
	}

	f, err := os.Open(backup) //nolint:gosec // reason: G304 -- test temp path
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer f.Close() //nolint:errcheck // reason: test cleanup
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r wordSyncBackupRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("decode backup line %q: %v", sc.Text(), err)
		}
		n++
		if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET status = ?, priority = ?, next_attempt_at = ?, attempts = ?,
		    last_error = ?, refused_waits = ?, word_timing_state = ?, word_timing_generation = ? WHERE id = ?`,
			r.Status, r.Priority, r.NextAttemptAt, r.Attempts, r.LastError, r.RefusedWaits,
			r.WordTimingState, r.WordTimingGeneration, r.ID); err != nil {
			t.Fatalf("restore id %d: %v", r.ID, err)
		}
	}
	if n != 3 {
		t.Errorf("backup records = %d; want 3", n)
	}
	if after := dumpTable(t, dbh, "work_queue"); after != before {
		t.Errorf("restore did not round-trip:\nbefore %s\nafter  %s", before, after)
	}
}

// --completed-before is strict (a row completed exactly at the cutoff is out),
// --library filters and is repeatable, and bad input exits non-zero.
func TestReconcileWordSync_CutoffAndLibraryScope(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "")
	cases := []struct {
		name string
		args ScanReconcileWordSyncCmd
		want string
	}{
		{"bad cutoff", ScanReconcileWordSyncCmd{CompletedBefore: "yesterday"}, "invalid --completed-before"},
		{"bad absent cutoff", ScanReconcileWordSyncCmd{RecheckAbsentBefore: "2026/01/01"}, "invalid --recheck-absent-before"},
		{"negative limit", ScanReconcileWordSyncCmd{Limit: -1}, "--limit must not be negative"},
		{"unknown library", ScanReconcileWordSyncCmd{Libraries: []string{"nope"}}, `library "nope" not found`},
		{"strict date cutoff", ScanReconcileWordSyncCmd{CompletedBefore: "2026-01-03"}, "candidates=2 "},
		{"strict instant cutoff", ScanReconcileWordSyncCmd{CompletedBefore: "2026-01-03T00:00:01Z"}, "candidates=3 "},
		{"library by name", ScanReconcileWordSyncCmd{Libraries: []string{"two"}}, "candidates=2 "},
		{"library by id, repeated", ScanReconcileWordSyncCmd{Libraries: []string{"1", "two"}}, "candidates=4 "},
		{"library and cutoff", ScanReconcileWordSyncCmd{Libraries: []string{"two"}, CompletedBefore: "2026-01-04"}, "candidates=1 "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args.ConfigPath = cfgPath
			code, out := runWS(t, ctx, tc.args)
			if (code != 0) != !strings.HasPrefix(tc.want, "candidates") || !strings.Contains(out, tc.want) {
				t.Errorf("exit=%d; want %q in %s", code, tc.want, out)
			}
		})
	}
	// Applied with a library scope, only that library's rows are queued.
	if code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Yes: true, Libraries: []string{"two"},
		Backup: filepath.Join(t.TempDir(), "b.jsonl")}); code != 0 {
		t.Fatalf("apply exit=%d out=%s", code, out)
	}
	if got := queuedIDs(t, dbh); got != "3,4" {
		t.Errorf("library-scoped apply queued %v; want [3 4]", got)
	}
}

// --recheck-absent-before re-admits a current-generation absent verdict
// reached strictly before the cutoff; without it such a row is terminal.
func TestReconcileWordSync_RecheckAbsentBefore(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "")
	gen, err := configuredWordGeneration(wordSyncTestConfig(t, cfgPath))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = ?,
	    word_timing_checked_at = '2026-02-01T00:00:00Z'`, gen); err != nil {
		t.Fatalf("stamp absent: %v", err)
	}
	for _, tc := range []struct{ cutoff, want string }{
		{"", "candidates=0 "},
		{"2026-02-01", "candidates=0 "},
		{"2026-02-02", "candidates=4 "},
	} {
		code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, RecheckAbsentBefore: tc.cutoff})
		if code != 0 || !strings.Contains(out, tc.want) {
			t.Errorf("cutoff %q: exit=%d; want %q in %s", tc.cutoff, code, tc.want, out)
		}
	}
}

// A backup write failing on a later row rolls the whole batch back: no row is
// left queued without its restorable record.
func TestReconcileWordSync_BackupFailureRollsBack(t *testing.T) {
	ctx, cfgPath, dbh := setupWordSync(t, "")
	before := dumpTable(t, dbh, "work_queue")
	orig := writeWordSyncBackup
	t.Cleanup(func() { writeWordSyncBackup = orig })
	calls := 0
	writeWordSyncBackup = func(f *os.File, p queue.WordRecheckPrior) error {
		if calls++; calls == 2 {
			return errors.New("disk full")
		}
		return orig(f, p)
	}
	code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Yes: true, Backup: filepath.Join(t.TempDir(), "b.jsonl")})
	if code == 0 || !strings.Contains(out, "rolled back") || !strings.Contains(out, "its last 1 record(s) belong to the rolled-back batch") {
		t.Errorf("exit=%d; want a non-zero exit with a rollback note naming the stale record: %s", code, out)
	}
	if after := dumpTable(t, dbh, "work_queue"); after != before {
		t.Errorf("a failed backup left rows flipped:\nbefore %s\nafter  %s", before, after)
	}
	// The batch's one fsync failing also rolls it back: no flip commits unsynced.
	writeWordSyncBackup = orig
	origSync := syncWordSyncBackup
	t.Cleanup(func() { syncWordSyncBackup = origSync })
	syncWordSyncBackup = func(*os.File) error { return errors.New("fsync failed") }
	if code, out := runWS(t, ctx, ScanReconcileWordSyncCmd{ConfigPath: cfgPath, Yes: true, Backup: filepath.Join(t.TempDir(), "c.jsonl")}); code == 0 ||
		!strings.Contains(out, "its last 4 record(s)") {
		t.Errorf("exit=%d; want a rollback on a failed batch fsync: %s", code, out)
	}
	if after := dumpTable(t, dbh, "work_queue"); after != before {
		t.Errorf("a failed fsync left rows flipped:\nbefore %s\nafter  %s", before, after)
	}
}

// The CLI's generation matches the one the serve worker stamps: the word-capable
// subset of primary + fallback_order, minus disabled lanes.
func TestConfiguredWordGeneration(t *testing.T) {
	gen := func(extra string) int64 {
		_, cfgPath, _ := setupWordSync(t, extra)
		g, err := configuredWordGeneration(wordSyncTestConfig(t, cfgPath))
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	mxmOnly := gen("")
	// Absolute parity with the worker, which hashes its lane names the same way.
	if want := providers.WordGeneration([]string{providers.Musixmatch}); mxmOnly != want {
		t.Errorf("default generation = %d; want the Musixmatch-only lane set %d", mxmOnly, want)
	}
	// A tokenless Musixmatch fallback counts: serve mints or reads a stored token.
	if got, want := gen("[providers]\nprimary = \"petitlyrics\"\nfallback_order = [\"musixmatch\"]\n"),
		providers.WordGeneration([]string{providers.PetitLyrics, providers.Musixmatch}); got != want {
		t.Errorf("petitlyrics + tokenless musixmatch generation = %d; want %d", got, want)
	}
	withPetit := gen("[providers]\nfallback_order = [\"petitlyrics\", \"innertube\"]\n")
	if mxmOnly == withPetit {
		t.Error("adding a word-capable fallback did not change the generation")
	}
	if got := gen("[providers]\nfallback_order = [\"innertube\"]\n"); got != mxmOnly {
		t.Error("a non-word-capable fallback changed the generation")
	}
	if got := gen("[providers]\nfallback_order = [\"petitlyrics\"]\ndisabled = [\"petitlyrics\"]\n"); got != mxmOnly {
		t.Error("a disabled fallback still counted toward the generation")
	}
}

func wordSyncTestConfig(t *testing.T, cfgPath string) config.Config {
	t.Helper()
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	return cfg
}
