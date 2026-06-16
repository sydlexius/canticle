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
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
)

const testLRCFilename = "track.lrc"

// seedProvenanceDB inserts the minimal rows needed to test the backfill:
// a library, a scan_result, a work_queue row, the junction row, and optionally
// a lyrics_cache row.
func seedProvenanceDB(t *testing.T, sqlDB *sql.DB, outdir, providerLane string, completedAt time.Time, song *models.Song) {
	t.Helper()
	ctx := context.Background()

	// Insert library.
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`,
		outdir, "test-lib").Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}

	// Insert scan_result.
	var srID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
		 VALUES (?, ?, ?, ?, ?, ?, 'done') RETURNING id`,
		libID, filepath.Join(outdir, "source.flac"),
		song.Track.ArtistName, song.Track.TrackName, outdir, testLRCFilename,
	).Scan(&srID); err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}

	// Insert work_queue row.
	completedStr := completedAt.UTC().Format(time.RFC3339)
	var wqID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO work_queue (artist, title, outdir, filename, status, provider_lane, completed_at)
		 VALUES (?, ?, ?, ?, 'done', ?, ?) RETURNING id`,
		song.Track.ArtistName, song.Track.TrackName, outdir, testLRCFilename,
		providerLane, completedStr,
	).Scan(&wqID); err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}

	// Insert junction row.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		wqID, srID); err != nil {
		t.Fatalf("insert junction: %v", err)
	}

	// Insert lyrics_cache if a song is provided.
	if song != nil && (song.Track.ISRC != "" || song.Track.RecordingMBID != "") {
		encoded, err := json.Marshal(song)
		if err != nil {
			t.Fatalf("marshal song: %v", err)
		}
		// Cache keys are normalized (lowercase NFKC) - match what CacheRepo.Store writes.
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT OR REPLACE INTO lyrics_cache (artist, title, duration_bucket, lyrics)
			 VALUES (?, ?, 0, ?)`,
			normalize.NormalizeKey(song.Track.ArtistName),
			normalize.NormalizeKey(song.Track.TrackName),
			string(encoded),
		); err != nil {
			t.Fatalf("insert lyrics_cache: %v", err)
		}
	}
}

// writeLRCFile writes a minimal .lrc file at the given path for backfill tests.
func writeLRCFile(t *testing.T, path string) {
	t.Helper()
	content := "[by:mxlrcgo-svc]\n[ar:Test Artist]\n[ti:Test Track]\n[00:01.00]Hello world\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write lrc: %v", err)
	}
}

func TestProvenanceSchemaQuery(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{
		Track: models.Track{
			ArtistName:    "Test Artist",
			TrackName:     "Test Track",
			ISRC:          "USRC12345678",
			RecordingMBID: "abcd1234-0000-0000-0000-000000000000",
		},
	}

	seedProvenanceDB(t, sqlDB, outdir, "musixmatch", completedAt, song)

	// Run the schema query that lookupPlan uses.
	lrcPath := filepath.Join(outdir, testLRCFilename)
	dir := filepath.Dir(lrcPath)
	base := filepath.Base(lrcPath)

	var artist, title string
	var providerLane, completedAtStr sql.NullString
	err = sqlDB.QueryRowContext(ctx, `
		SELECT wq.artist, wq.title, wq.provider_lane, wq.completed_at
		FROM work_queue wq
		JOIN work_queue_scan_results wqsr ON wqsr.work_queue_id = wq.id
		JOIN scan_results sr ON sr.id = wqsr.scan_result_id
		WHERE sr.outdir = ? AND sr.filename = ?
		  AND wq.status = 'done'
		ORDER BY wq.id DESC
		LIMIT 1`,
		dir, base,
	).Scan(&artist, &title, &providerLane, &completedAtStr)
	if err != nil {
		t.Fatalf("schema query: %v", err)
	}

	if artist != "Test Artist" {
		t.Errorf("artist=%q, want %q", artist, "Test Artist")
	}
	if !providerLane.Valid || providerLane.String != "musixmatch" {
		t.Errorf("provider_lane=%v, want musixmatch", providerLane)
	}
	if !completedAtStr.Valid {
		t.Error("completed_at is NULL, want non-null")
	}

	// Verify ISRC/MBID come through from lyrics_cache.
	isrc, mbid := lyricsISRCMBID(ctx, sqlDB, artist, title)
	if isrc != "USRC12345678" {
		t.Errorf("isrc=%q, want USRC12345678", isrc)
	}
	if mbid != "abcd1234-0000-0000-0000-000000000000" {
		t.Errorf("mbid=%q, want abcd1234-...", mbid)
	}
}

// TestLookupPlan_OutdirNormalization verifies NEW-2: lookupPlan matches the
// stored scan_results.outdir against the query path's directory by a CANONICAL
// form, so a stored outdir that differs only by a trailing slash / redundant
// "." segment (uncleaned, as a relative-vs-absolute or sloppily-stored path
// would be) still resolves. Before the fix the exact `sr.outdir = ?` compare
// returned 0 rows and the file was silently counted as skipped.
func TestLookupPlan_OutdirNormalization(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	realDir := t.TempDir()
	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{
		Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"},
	}

	// Seed with a stored outdir that differs from the cleaned query directory
	// only by a trailing separator plus a redundant "." segment - an exact
	// string compare against the cleaned query dir would not match.
	storedOutdir := realDir + string(filepath.Separator) + "." + string(filepath.Separator)
	seedProvenanceDB(t, sqlDB, storedOutdir, "musixmatch", completedAt, song)

	lrcPath := filepath.Join(realDir, testLRCFilename)

	// Precondition: the old exact-match query finds nothing for this seed, so a
	// success below is attributable to the canonical-dir normalization.
	var n int
	if err := sqlDB.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM scan_results sr
		WHERE sr.outdir = ? AND sr.filename = ?`,
		filepath.Dir(lrcPath), testLRCFilename,
	).Scan(&n); err != nil {
		t.Fatalf("precondition count query: %v", err)
	}
	if n != 0 {
		t.Fatalf("precondition: expected 0 exact-match rows for uncleaned outdir, got %d", n)
	}

	plan, ok := lookupPlan(ctx, sqlDB, lrcPath)
	if !ok {
		t.Fatal("lookupPlan returned ok=false; expected canonical outdir match to succeed")
	}
	if plan.Path != lrcPath {
		t.Errorf("plan.Path=%q, want %q", plan.Path, lrcPath)
	}
}

func TestBackfillPlansFromPaths_Basic(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, testLRCFilename)
	writeLRCFile(t, lrcPath)

	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
	seedProvenanceDB(t, sqlDB, outdir, "musixmatch", completedAt, song)

	plans, skipped, err := backfillPlansFromPaths(ctx, sqlDB, []string{outdir})
	if err != nil {
		t.Fatalf("backfillPlansFromPaths: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("len(plans)=%d, want 1", len(plans))
	}
	if skipped != 0 {
		t.Errorf("skipped=%d, want 0 for a matched file", skipped)
	}
	if plans[0].Path != lrcPath {
		t.Errorf("plan.Path=%q, want %q", plans[0].Path, lrcPath)
	}
	if plans[0].Tags.Source != "musixmatch" {
		t.Errorf("Tags.Source=%q, want musixmatch", plans[0].Tags.Source)
	}
}

func TestBackfillPlansFromDB_Basic(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, testLRCFilename)
	writeLRCFile(t, lrcPath)

	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
	seedProvenanceDB(t, sqlDB, outdir, "musixmatch", completedAt, song)

	plans, _, err := backfillPlansFromDB(ctx, sqlDB)
	if err != nil {
		t.Fatalf("backfillPlansFromDB: %v", err)
	}
	if len(plans) != 1 {
		t.Fatalf("len(plans)=%d, want 1", len(plans))
	}
	if plans[0].Tags.Source != "musixmatch" {
		t.Errorf("Tags.Source=%q, want musixmatch", plans[0].Tags.Source)
	}
}

func TestRunProvenanceBackfill_YesGuard(t *testing.T) {
	ctx := context.Background()
	tmpDB := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(ctx, tmpDB)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, testLRCFilename)
	writeLRCFile(t, lrcPath)

	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
	seedProvenanceDB(t, sqlDB, outdir, "musixmatch", completedAt, song)
	_ = sqlDB.Close() // close before runProvenanceBackfill opens its own connection

	// Without --yes: plan output, no file modification.
	originalContent, _ := os.ReadFile(lrcPath)

	var out bytes.Buffer
	cfg := makeTempConfig(t, tmpDB)
	code := runProvenanceBackfill(ctx, &out, ProvenanceBackfillCmd{
		ConfigPath: cfg,
		Paths:      []string{outdir},
		Yes:        false,
	})
	if code != 0 {
		t.Fatalf("without --yes: exit=%d, output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "pass --yes") {
		t.Errorf("without --yes: expected 'pass --yes' in output, got: %s", out.String())
	}
	afterContent, _ := os.ReadFile(lrcPath)
	if string(afterContent) != string(originalContent) {
		t.Error("without --yes: file was modified")
	}

	// With --yes: file is modified.
	out.Reset()
	code = runProvenanceBackfill(ctx, &out, ProvenanceBackfillCmd{
		ConfigPath: cfg,
		Paths:      []string{outdir},
		Yes:        true,
	})
	if code != 0 {
		t.Fatalf("with --yes: exit=%d, output=%s", code, out.String())
	}
	afterYesContent, _ := os.ReadFile(lrcPath)
	if string(afterYesContent) == string(originalContent) {
		t.Error("with --yes: file was not modified")
	}
	if !strings.Contains(string(afterYesContent), "[source:musixmatch]") {
		t.Errorf("with --yes: expected [source:musixmatch] in file, got:\n%s", string(afterYesContent))
	}
}

func TestRunProvenanceBackfill_Idempotent(t *testing.T) {
	ctx := context.Background()
	tmpDB := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(ctx, tmpDB)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, testLRCFilename)
	writeLRCFile(t, lrcPath)

	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
	seedProvenanceDB(t, sqlDB, outdir, "musixmatch", completedAt, song)
	_ = sqlDB.Close() // close before runProvenanceBackfill opens its own connection

	cfg := makeTempConfig(t, tmpDB)
	args := ProvenanceBackfillCmd{ConfigPath: cfg, Paths: []string{outdir}, Yes: true}

	var out1 bytes.Buffer
	if code := runProvenanceBackfill(ctx, &out1, args); code != 0 {
		t.Fatalf("first run: exit=%d, output=%s", code, out1.String())
	}
	after1, _ := os.ReadFile(lrcPath)

	var out2 bytes.Buffer
	if code := runProvenanceBackfill(ctx, &out2, args); code != 0 {
		t.Fatalf("second run: exit=%d, output=%s", code, out2.String())
	}
	after2, _ := os.ReadFile(lrcPath)

	if string(after1) != string(after2) {
		t.Error("second run modified the file (not idempotent)")
	}

	// No duplicate [source:] tags.
	count := strings.Count(string(after2), "[source:")
	if count > 1 {
		t.Errorf("duplicate [source:] tags after second run: %d", count)
	}
}

func TestRunProvenanceBackfill_LRCOnly(t *testing.T) {
	ctx := context.Background()
	tmpDB := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(ctx, tmpDB)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// No data to seed; just ensure migrations ran, then close so the command opens its own connection.
	_ = sqlDB.Close()

	// Put a .txt file in the directory - it should not be processed.
	outdir := t.TempDir()
	txtPath := filepath.Join(outdir, "track.txt")
	if err := os.WriteFile(txtPath, []byte("instrumental\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := makeTempConfig(t, tmpDB)
	var out bytes.Buffer
	code := runProvenanceBackfill(ctx, &out, ProvenanceBackfillCmd{
		ConfigPath: cfg,
		Paths:      []string{outdir},
		Yes:        true,
	})
	// Should succeed with no files processed (txt ignored); exit 0 = no errors.
	if code != 0 {
		t.Fatalf("exit=%d, output=%s", code, out.String())
	}
}

func TestRunProvenanceBackfill_PartialCoverage(t *testing.T) {
	ctx := context.Background()
	tmpDB := filepath.Join(t.TempDir(), "test.db")
	sqlDB, err := db.Open(ctx, tmpDB)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, testLRCFilename)
	writeLRCFile(t, lrcPath)

	// provider_lane is NULL -> partial coverage (no [source:] available).
	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC)
	song := &models.Song{Track: models.Track{ArtistName: "Test Artist", TrackName: "Test Track"}}
	seedProvenanceDB(t, sqlDB, outdir, "", completedAt, song) // empty lane = partial
	_ = sqlDB.Close()                                         // close before runProvenanceBackfill opens its own connection

	cfg := makeTempConfig(t, tmpDB)
	var out bytes.Buffer
	code := runProvenanceBackfill(ctx, &out, ProvenanceBackfillCmd{
		ConfigPath: cfg,
		Paths:      []string{outdir},
		Yes:        false, // plan only
	})
	if code != 0 {
		t.Fatalf("exit=%d, output=%s", code, out.String())
	}
	// Should report partial coverage.
	if !strings.Contains(out.String(), "partial") {
		t.Errorf("expected 'partial' in plan output, got: %s", out.String())
	}
}

func TestRunProvenance_MissingSubcommand(t *testing.T) {
	var out bytes.Buffer
	code := runProvenance(context.Background(), &out, ProvenanceCmd{})
	if code != 2 {
		t.Errorf("exit=%d, want 2", code)
	}
	if !strings.Contains(out.String(), "missing") {
		t.Errorf("expected 'missing' in output, got: %s", out.String())
	}
}

func TestResolveOutputPaths(t *testing.T) {
	tests := []struct {
		name                              string
		outdir, filename, outputPathsJSON string
		wantLen                           int
		wantFirst                         string
	}{
		{
			name:   "empty json falls back to outdir/filename",
			outdir: "/music", filename: "track.lrc", outputPathsJSON: "",
			wantLen: 1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
		{
			name:   "null json falls back",
			outdir: "/music", filename: "track.lrc", outputPathsJSON: "null",
			wantLen: 1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
		{
			name:   "empty array falls back",
			outdir: "/music", filename: "track.lrc", outputPathsJSON: "[]",
			wantLen: 1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
		{
			name:   "valid json output_paths overrides outdir/filename",
			outdir: "/ignored", filename: "ignored.lrc",
			outputPathsJSON: `[{"outdir":"/music","filename":"track.lrc"}]`,
			wantLen:         1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
		{
			name:   "invalid json falls back to outdir/filename",
			outdir: "/music", filename: "track.lrc", outputPathsJSON: "{bad",
			wantLen: 1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
		{
			name:   "empty outdir and filename returns nil",
			outdir: "", filename: "", outputPathsJSON: "",
			wantLen: 0,
		},
		{
			name:   "json entry with no outdir is skipped; fallback used",
			outdir: "/music", filename: "track.lrc",
			outputPathsJSON: `[{"filename":"track.lrc"}]`,
			wantLen:         1, wantFirst: filepath.Join("/music", "track.lrc"),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveOutputPaths(tc.outdir, tc.filename, tc.outputPathsJSON)
			if len(got) != tc.wantLen {
				t.Fatalf("resolveOutputPaths: got %v (len %d), want len %d", got, len(got), tc.wantLen)
			}
			if tc.wantLen > 0 && got[0] != tc.wantFirst {
				t.Errorf("got[0]=%q, want %q", got[0], tc.wantFirst)
			}
		})
	}
}

func TestBackfillPlansFromPaths_StatError(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	nonExistent := filepath.Join(t.TempDir(), "no-such-subdir")
	_, _, err = backfillPlansFromPaths(ctx, sqlDB, []string{nonExistent})
	if err == nil {
		t.Error("expected error for non-existent path, got nil")
	}
}

// TestBackfillPlansFromPaths_SkippedCount asserts MAJOR-2: .lrc files with no
// matching DB row are counted as skipped, not silently dropped.
func TestBackfillPlansFromPaths_SkippedCount(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	// Write an .lrc file with no matching DB row (orphan).
	orphanPath := filepath.Join(outdir, "orphan.lrc")
	writeLRCFile(t, orphanPath)

	plans, skipped, err := backfillPlansFromPaths(ctx, sqlDB, []string{outdir})
	if err != nil {
		t.Fatalf("backfillPlansFromPaths: %v", err)
	}
	if len(plans) != 0 {
		t.Errorf("expected 0 plans for orphan file, got %d", len(plans))
	}
	if skipped != 1 {
		t.Errorf("expected skipped=1, got %d", skipped)
	}
}

// seedDualFormatDB inserts two scan_results rows that share the same (outdir, filename)
// linked to ONE work_queue row - the DUAL-FORMAT SINGLE-TRACK case
// (e.g. song.flac + song.mp3 both map to song.lrc; only one wq row exists per
// artist+title due to the unique index, but both scan_result rows join through it).
// This is NOT a collision: the join returns two rows for ONE distinct work_queue id,
// so lookupPlan must tag it normally (ok=true).
func seedDualFormatDB(t *testing.T, sqlDB *sql.DB, outdir, lrcFilename string) {
	t.Helper()
	ctx := context.Background()
	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339)

	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`,
		outdir, "test-lib").Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}

	// One work_queue row (unique per artist+title).
	var wqID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO work_queue (artist, title, outdir, filename, status, provider_lane, completed_at)
		 VALUES (?, ?, ?, ?, 'done', 'musixmatch', ?) RETURNING id`,
		"Test Artist", "Test Track", outdir, lrcFilename, completedAt,
	).Scan(&wqID); err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}

	// Two scan_results pointing to different audio files but the same output .lrc.
	for _, audioFile := range []string{"song.flac", "song.mp3"} {
		var srID int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
			 VALUES (?, ?, ?, ?, ?, ?, 'done') RETURNING id`,
			libID, filepath.Join(outdir, audioFile),
			"Test Artist", "Test Track", outdir, lrcFilename,
		).Scan(&srID); err != nil {
			t.Fatalf("insert scan_result for %s: %v", audioFile, err)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
			wqID, srID); err != nil {
			t.Fatalf("insert junction for %s: %v", audioFile, err)
		}
	}
}

// seedAmbiguousDB inserts TWO DISTINCT work_queue rows (different artist+title)
// each linked to its own scan_results row that points at the SAME output .lrc.
// This is the genuine cross-attribution collision: two distinct tracks both
// claim one .lrc file, so lookupPlan must warn + skip (ok=false).
func seedAmbiguousDB(t *testing.T, sqlDB *sql.DB, outdir, lrcFilename string) {
	t.Helper()
	ctx := context.Background()
	completedAt := time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC).UTC().Format(time.RFC3339)

	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`,
		outdir, "test-lib").Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}

	tracks := []struct{ artist, title, audioFile string }{
		{"Artist One", "Track One", "one.flac"},
		{"Artist Two", "Track Two", "two.flac"},
	}
	for _, tr := range tracks {
		// Set artist_key/title_key explicitly so the two distinct tracks do not
		// collide on the (artist_key, title_key) unique index (these columns
		// default to '' and are not auto-populated on a plain INSERT).
		var wqID int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO work_queue (artist, title, artist_key, title_key, outdir, filename, status, provider_lane, completed_at)
			 VALUES (?, ?, normalize_key(?), normalize_key(?), ?, ?, 'done', 'musixmatch', ?) RETURNING id`,
			tr.artist, tr.title, tr.artist, tr.title, outdir, lrcFilename, completedAt,
		).Scan(&wqID); err != nil {
			t.Fatalf("insert work_queue for %s: %v", tr.artist, err)
		}
		var srID int64
		if err := sqlDB.QueryRowContext(ctx,
			`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status)
			 VALUES (?, ?, ?, ?, ?, ?, 'done') RETURNING id`,
			libID, filepath.Join(outdir, tr.audioFile),
			tr.artist, tr.title, outdir, lrcFilename,
		).Scan(&srID); err != nil {
			t.Fatalf("insert scan_result for %s: %v", tr.artist, err)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
			wqID, srID); err != nil {
			t.Fatalf("insert junction for %s: %v", tr.artist, err)
		}
	}
}

// TestLookupPlan_AmbiguousCollision asserts MAJOR-3 / NEW-1 (path mode): when the
// same (outdir, filename) maps to more than one DISTINCT work_queue row,
// lookupPlan returns false (warn+skip) instead of silently picking one.
func TestLookupPlan_AmbiguousCollision(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, "song.lrc")
	writeLRCFile(t, lrcPath)
	seedAmbiguousDB(t, sqlDB, outdir, "song.lrc")

	plan, ok := lookupPlan(ctx, sqlDB, lrcPath)
	if ok {
		t.Errorf("lookupPlan returned ok=true for ambiguous collision; plan=%+v", plan)
	}
	_ = plan // silence unused warning
}

// TestLookupPlan_DualFormatSingleTrack asserts NEW-1 (path mode): one track
// released in two audio formats (one work_queue row, two scan_results rows at the
// same .lrc) is unambiguous - lookupPlan must build the plan (ok=true) with the
// correct provenance, matching what DB mode produces for the same data.
func TestLookupPlan_DualFormatSingleTrack(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, "song.lrc")
	writeLRCFile(t, lrcPath)
	seedDualFormatDB(t, sqlDB, outdir, "song.lrc")

	plan, ok := lookupPlan(ctx, sqlDB, lrcPath)
	if !ok {
		t.Fatalf("lookupPlan returned ok=false for dual-format single track; want ok=true")
	}
	if plan.Path != lrcPath {
		t.Errorf("plan.Path=%q, want %q", plan.Path, lrcPath)
	}
	if plan.Tags.Source != "musixmatch" {
		t.Errorf("plan.Tags.Source=%q, want musixmatch", plan.Tags.Source)
	}
	if plan.Tags.Fetched == "" {
		t.Error("plan.Tags.Fetched is empty, want the completed_at timestamp")
	}
	if plan.Partial {
		t.Error("plan.Partial=true, want false (source + fetched both present)")
	}
}

// TestBackfillPlansFromDB_CollisionProducesOnePlan asserts MAJOR-3 (DB mode): the
// same-stem setup (two scan_results linked to one work_queue row) produces exactly
// one plan in DB mode because the work_queue schema enforces one row per
// artist+title. DB-mode provenance comes from the wq row directly, so no
// cross-attribution is possible even with two scan_results.
func TestBackfillPlansFromDB_CollisionProducesOnePlan(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close() //nolint:errcheck // test cleanup

	outdir := t.TempDir()
	lrcPath := filepath.Join(outdir, "song.lrc")
	writeLRCFile(t, lrcPath)
	seedDualFormatDB(t, sqlDB, outdir, "song.lrc")

	plans, skipped, err := backfillPlansFromDB(ctx, sqlDB)
	if err != nil {
		t.Fatalf("backfillPlansFromDB: %v", err)
	}
	// DB mode iterates work_queue rows (one row per artist+title); even with two
	// scan_results linked, the provenance data is unambiguous and one plan is built.
	if len(plans) != 1 {
		t.Errorf("expected 1 plan from unambiguous wq row, got %d", len(plans))
	}
	if skipped != 0 {
		t.Errorf("expected skipped=0, got %d", skipped)
	}
	if len(plans) > 0 && plans[0].Tags.Source != "musixmatch" {
		t.Errorf("Tags.Source=%q, want musixmatch", plans[0].Tags.Source)
	}
	_ = lrcPath
}

// makeTempConfig creates a minimal TOML config file pointing at the given DB path.
func makeTempConfig(t *testing.T, dbPath string) string {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	content := "[db]\npath = \"" + dbPath + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}
