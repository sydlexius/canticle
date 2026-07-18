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
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/testutil"
)

// setupReconcileIdentity builds a config + DB + one library with a real,
// multi-value-tagged file whose scan_results row carries the mangled
// run-together artist a pre-fix scan would have written. Returns the config
// path, db path, and the seeded file path.
func setupReconcileIdentity(t *testing.T) (ctx context.Context, cfgPath, dbPath, filePath string) {
	t.Helper()
	ctx = context.Background()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "test.db")
	cfgPath = filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, dbPath)

	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha\x00Bravo", "Song", "Album", "",
		nil, map[string]string{"ARTISTS": "Alpha\x00Bravo"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	filePath = filepath.Join(dir, "track.mp3")

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, 'lib')`, dir); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, album, album_artist, artist_key, title_key, status)
		 VALUES (1, ?, 'AlphaBravo', 'Song', 'Album', '', ?, ?, 'pending')`,
		filePath, normalize.NormalizeKey("AlphaBravo"), normalize.NormalizeKey("Song")); err != nil {
		t.Fatalf("seed scan_results: %v", err)
	}
	return ctx, cfgPath, dbPath, filePath
}

func scanArtist(t *testing.T, ctx context.Context, dbPath, filePath string) string {
	t.Helper()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup
	var artist string
	if err := sqlDB.QueryRowContext(ctx, `SELECT artist FROM scan_results WHERE file_path = ?`, filePath).Scan(&artist); err != nil {
		t.Fatalf("read artist: %v", err)
	}
	return artist
}

// Dry-run reports the correction but mutates nothing and writes no backup.
func TestRunReconcileIdentity_DryRun(t *testing.T) {
	ctx, cfgPath, dbPath, filePath := setupReconcileIdentity(t)
	var buf bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf, ScanReconcileIdentityCmd{ConfigPath: cfgPath}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "would correct 1") {
		t.Errorf("want 'would correct 1'; got: %s", buf.String())
	}
	if a := scanArtist(t, ctx, dbPath, filePath); a != "AlphaBravo" {
		t.Errorf("dry-run mutated artist = %q; want AlphaBravo", a)
	}
	if m, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-identity-backup-*.jsonl")); len(m) != 0 {
		t.Errorf("dry-run wrote a backup: %v", m)
	}
}

// --yes corrects the row, writes a decodable JSONL backup, and a second run
// finds nothing left to correct.
func TestRunReconcileIdentity_ApplyAndBackup(t *testing.T) {
	ctx, cfgPath, dbPath, filePath := setupReconcileIdentity(t)
	var buf bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf, ScanReconcileIdentityCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "corrected 1") {
		t.Errorf("want 'corrected 1'; got: %s", buf.String())
	}
	if a := scanArtist(t, ctx, dbPath, filePath); a != "Alpha; Bravo" {
		t.Errorf("apply left artist = %q; want Alpha; Bravo", a)
	}

	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(dbPath), "reconcile-identity-backup-*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("want one backup file; got %v", matches)
	}
	b, err := os.ReadFile(matches[0]) //nolint:gosec // test-controlled path
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	var rec reconcileIdentityBackupRecord
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(b))), &rec); err != nil {
		t.Fatalf("decode backup: %v", err)
	}
	if rec.OldArtist != "AlphaBravo" || rec.NewArtist != "Alpha; Bravo" || rec.FilePath != filePath {
		t.Errorf("backup record = %+v; want AlphaBravo->Alpha; Bravo for %q", rec, filePath)
	}

	var buf2 bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf2, ScanReconcileIdentityCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("second run exit=%d out=%s", code, buf2.String())
	}
	if !strings.Contains(buf2.String(), "corrected 0") {
		t.Errorf("second run want 'corrected 0'; got: %s", buf2.String())
	}
}

// An unknown --library exits 1 with a clear message.
func TestRunReconcileIdentity_LibraryNotFound(t *testing.T) {
	ctx, cfgPath, _, _ := setupReconcileIdentity(t)
	var buf bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf, ScanReconcileIdentityCmd{ConfigPath: cfgPath, Library: "no-such-library"}); code != 1 {
		t.Fatalf("exit=%d want 1; out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "not found") {
		t.Errorf("want 'not found'; got: %s", buf.String())
	}
}

// An invalid config exits 1.
func TestRunReconcileIdentity_ConfigLoadError(t *testing.T) {
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(cfgPath, []byte("not = valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var buf bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf, ScanReconcileIdentityCmd{ConfigPath: cfgPath}); code != 1 {
		t.Fatalf("exit=%d want 1 for invalid config", code)
	}
}

// --library scopes the correction to the named library.
func TestRunReconcileIdentity_LibraryScoped(t *testing.T) {
	ctx, cfgPath, dbPath, filePath := setupReconcileIdentity(t)
	var buf bytes.Buffer
	if code := runReconcileIdentity(ctx, &buf, ScanReconcileIdentityCmd{ConfigPath: cfgPath, Library: "lib", Yes: true}); code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "corrected 1") {
		t.Errorf("want 'corrected 1'; got: %s", buf.String())
	}
	if a := scanArtist(t, ctx, dbPath, filePath); a != "Alpha; Bravo" {
		t.Errorf("scoped apply left artist = %q; want Alpha; Bravo", a)
	}
}

// runIdentityBackfill with an already-canceled context does no work and leaves
// the marker unset, so the pass resumes on the next startup.
func TestRunIdentityBackfill_CanceledLeavesUnset(t *testing.T) {
	sqlDB := openBackfillDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runIdentityBackfill(ctx, sqlDB)
	if done, _ := identityBackfillDone(context.Background(), sqlDB); done {
		t.Error("marker set after a canceled backfill; want unset so it resumes")
	}
}

// The dispatch wiring routes `scan reconcile-identity` to the handler.
func TestReconcileIdentity_IsRecognizedSubcommand(t *testing.T) {
	if !usesSubcommand([]string{"scan", "reconcile-identity"}) {
		t.Error("`scan reconcile-identity` not recognized as a subcommand invocation")
	}
}

// runScanCmd routes the reconcile-identity subcommand to its handler and
// propagates the parent --config down to it.
func TestRunScanCmd_DispatchesReconcileIdentity(t *testing.T) {
	ctx, cfgPath, dbPath, filePath := setupReconcileIdentity(t)
	var buf bytes.Buffer
	code := runScanCmd(ctx, &buf, ScanCmd{
		ConfigPath:        cfgPath,
		ReconcileIdentity: &ScanReconcileIdentityCmd{Yes: true},
	})
	if code != 0 {
		t.Fatalf("exit=%d out=%s", code, buf.String())
	}
	if !strings.Contains(buf.String(), "corrected 1") {
		t.Errorf("want 'corrected 1' via runScanCmd; got: %s", buf.String())
	}
	if a := scanArtist(t, ctx, dbPath, filePath); a != "Alpha; Bravo" {
		t.Errorf("dispatch apply left artist = %q; want Alpha; Bravo", a)
	}
}

func openBackfillDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// The marker gate reports not-done until stamped, then done.
func TestIdentityBackfillMarker_Gate(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	if done, err := identityBackfillDone(ctx, sqlDB); err != nil || done {
		t.Fatalf("fresh db: done=%v err=%v; want done=false", done, err)
	}
	if err := markIdentityBackfillDone(ctx, sqlDB); err != nil {
		t.Fatalf("mark: %v", err)
	}
	if done, err := identityBackfillDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("after mark: done=%v err=%v; want done=true", done, err)
	}
}

// runIdentityBackfill corrects a real mangled multi-value row end-to-end (real
// file tags -> scanner.ReadArtistIdentity -> engine) and stamps the marker; a
// second run is a marker-gated no-op.
func TestRunIdentityBackfill_CorrectsAndGates(t *testing.T) {
	ctx := context.Background()
	sqlDB := openBackfillDB(t)

	// A real file whose multi-value TPE1 is mangled by dhowden/tag but whose
	// TXXX ARTISTS preserves the boundaries.
	dir := t.TempDir()
	if err := testutil.WriteAudioFileExtended(dir, "track.mp3",
		"Alpha\x00Bravo", "Song", "Album", "",
		nil, map[string]string{"ARTISTS": "Alpha\x00Bravo"}); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	path := filepath.Join(dir, "track.mp3")

	res, err := sqlDB.Exec(`INSERT INTO libraries (path, name) VALUES (?, 'Lib')`, dir)
	if err != nil {
		t.Fatalf("seed library: %v", err)
	}
	libID, _ := res.LastInsertId()
	// The stored identity is the mangled run-together form a pre-fix scan wrote.
	if _, err := sqlDB.Exec(
		`INSERT INTO scan_results (library_id, file_path, artist, title, album, album_artist, artist_key, title_key, outdir, filename, status)
		 VALUES (?, ?, 'AlphaBravo', 'Song', 'Album', '', ?, ?, ?, 'track.lrc', 'pending')`,
		libID, path, normalize.NormalizeKey("AlphaBravo"), normalize.NormalizeKey("Song"), dir); err != nil {
		t.Fatalf("seed scan_results: %v", err)
	}

	runIdentityBackfill(ctx, sqlDB)

	var artist, artistKey string
	if err := sqlDB.QueryRow(`SELECT artist, artist_key FROM scan_results WHERE file_path = ?`, path).
		Scan(&artist, &artistKey); err != nil {
		t.Fatalf("read scan_results: %v", err)
	}
	if artist != "Alpha; Bravo" || artistKey != normalize.NormalizeKey("Alpha; Bravo") {
		t.Errorf("after backfill: artist=%q key=%q; want %q / %q",
			artist, artistKey, "Alpha; Bravo", normalize.NormalizeKey("Alpha; Bravo"))
	}
	if done, err := identityBackfillDone(ctx, sqlDB); err != nil || !done {
		t.Fatalf("marker not set after backfill: done=%v err=%v", done, err)
	}

	// Corrupt the row again and confirm a second run is a marker-gated no-op.
	if _, err := sqlDB.Exec(`UPDATE scan_results SET artist = 'AlphaBravo' WHERE file_path = ?`, path); err != nil {
		t.Fatalf("re-corrupt: %v", err)
	}
	runIdentityBackfill(ctx, sqlDB)
	if err := sqlDB.QueryRow(`SELECT artist FROM scan_results WHERE file_path = ?`, path).Scan(&artist); err != nil {
		t.Fatalf("read scan_results (2nd): %v", err)
	}
	if artist != "AlphaBravo" {
		t.Errorf("second run mutated a marker-gated db: artist=%q; want AlphaBravo (no-op)", artist)
	}
}
