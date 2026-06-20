package reports_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/reports"
)

// openTestDB opens a temp-file SQLite with all migrations applied, mirroring the
// helper in internal/cache. Real SQLite, no mocks.
func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// insertWorkItem inserts one work_queue row and returns its id. Only the columns
// a test cares about are passed; everything else takes its schema default.
type workItem struct {
	artist             string
	title              string
	album              string
	status             string
	lastError          string
	outputPaths        string
	completedAt        any // string (RFC3339) or nil
	providerLane       any // string or nil
	instrumentalResult any // int or nil
	detectInstrumental any // int or nil
}

func insertWorkItem(t *testing.T, sqlDB *sql.DB, w workItem) int64 {
	t.Helper()
	// artist_key/title_key carry a UNIQUE index; the app normally stamps them at
	// enqueue. Tests use distinct titles, so derive the keys from artist+title to
	// satisfy the constraint without a normalize dependency.
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, album, status, last_error, output_paths,
             completed_at, provider_lane, instrumental_result, detect_instrumental)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.artist, w.title, w.artist, w.title, w.album, w.status, w.lastError, w.outputPaths,
		w.completedAt, w.providerLane, w.instrumentalResult, w.detectInstrumental)
	if err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func insertLibrary(t *testing.T, sqlDB *sql.DB) int64 {
	t.Helper()
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO libraries (path, name) VALUES (?, ?)`, "/music", "lib")
	if err != nil {
		t.Fatalf("insert library: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func insertScanResult(t *testing.T, sqlDB *sql.DB, libraryID int64, filePath string) int64 {
	t.Helper()
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO scan_results (library_id, file_path) VALUES (?, ?)`, libraryID, filePath)
	if err != nil {
		t.Fatalf("insert scan_results: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	return id
}

func linkScanResult(t *testing.T, sqlDB *sql.DB, workQueueID, scanResultID int64) {
	t.Helper()
	if _, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		workQueueID, scanResultID); err != nil {
		t.Fatalf("link scan_results: %v", err)
	}
}

// insertLaneAttempts seeds lane_attempts with `hits` hit rows and `misses` miss
// rows for the named lane, one per-track row each. queue_id is unique within the
// lane (the UNIQUE constraint is (queue_id, lane), so different lanes may reuse
// the same ids). This is the true per-track source for Report 3 (issue #282).
func insertLaneAttempts(t *testing.T, sqlDB *sql.DB, lane string, hits, misses int64) {
	t.Helper()
	insert := func(qid, hit int64) {
		if _, err := sqlDB.ExecContext(context.Background(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, ?, ?, ?)`,
			qid, lane, hit, "2026-06-18T00:00:00Z"); err != nil {
			t.Fatalf("insert lane_attempts: %v", err)
		}
	}
	var qid int64
	for i := int64(0); i < hits; i++ {
		qid++
		insert(qid, 1)
	}
	for i := int64(0); i < misses; i++ {
		qid++
		insert(qid, 0)
	}
}

// pathsJSON builds an output_paths JSON array with one entry, matching the
// shape internal/queue.marshalOutputPaths writes ([{outdir,filename}]).
func pathsJSON(filename string) string { return `[{"outdir":"/out","filename":"` + filename + `"}]` }

func TestQueueSummary(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// 2 pending, 1 processing, 3 done, 1 failed, 2 deferred. No 'processing'
	// extras so we confirm zero-count statuses still report.
	for i := 0; i < 2; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "p" + string(rune('a'+i)), status: "pending"})
	}
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "proc", status: "processing"})
	for i := 0; i < 3; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "d" + string(rune('a'+i)), status: "done"})
	}
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "f1", status: "failed"})
	for i := 0; i < 2; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "def" + string(rune('a'+i)), status: "deferred"})
	}

	got, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	want := reports.QueueSummary{Pending: 2, Processing: 1, Done: 3, Failed: 1, Deferred: 2, Total: 9}
	if got != want {
		t.Errorf("QueueSummary = %+v, want %+v", got, want)
	}
}

func TestQueueSummaryEmpty(t *testing.T) {
	got, err := reports.New(openTestDB(t)).QueueSummary(context.Background())
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	if (got != reports.QueueSummary{}) {
		t.Errorf("QueueSummary on empty DB = %+v, want zero value", got)
	}
}

func TestRecentOutcomesClassificationAndOrder(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Insert in scrambled completion order to verify DESC sort; one NULL
	// completed_at must sort last.
	insertWorkItem(t, sqlDB, workItem{
		artist: "Synced", title: "S", album: "Al1", status: "done",
		outputPaths: pathsJSON("song.lrc"), completedAt: "2026-06-10T10:00:00Z",
		providerLane: "musixmatch",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Unsynced", title: "U", status: "done",
		outputPaths: pathsJSON("song.txt"), completedAt: "2026-06-12T10:00:00Z",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Miss", title: "M", status: "done",
		lastError: "miss limit reached", outputPaths: pathsJSON("ignored.lrc"),
		completedAt: "2026-06-11T10:00:00Z",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Legacy", title: "L", status: "done",
		outputPaths: "", completedAt: nil, // NULL completed_at, empty output_paths
	})
	// A non-done row must be excluded.
	insertWorkItem(t, sqlDB, workItem{artist: "Pending", title: "P", status: "pending"})

	got, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d outcomes, want 4 (done only): %+v", len(got), got)
	}

	// Order: newest completed first, NULL completed_at last.
	wantOrder := []struct {
		artist string
		result reports.ResultClass
	}{
		{"Unsynced", reports.ResultUnsyncedOrInstrumental},
		{"Miss", reports.ResultMiss},
		{"Synced", reports.ResultSynced},
		{"Legacy", reports.ResultUnknown},
	}
	for i, w := range wantOrder {
		if got[i].Artist != w.artist {
			t.Errorf("outcome[%d].Artist = %q, want %q", i, got[i].Artist, w.artist)
		}
		if got[i].Result != w.result {
			t.Errorf("outcome[%d].Result = %q, want %q", i, got[i].Result, w.result)
		}
	}

	// Field carry-through on the synced row.
	synced := got[2]
	if synced.Album != "Al1" || synced.ProviderLane != "musixmatch" {
		t.Errorf("synced row fields = album %q lane %q, want Al1/musixmatch", synced.Album, synced.ProviderLane)
	}
	if synced.CompletedAt != mustParse(t, "2026-06-10T10:00:00Z") {
		t.Errorf("synced CompletedAt = %v, want 2026-06-10T10:00:00Z", synced.CompletedAt)
	}
	// NULL completed_at -> zero time.
	if !got[3].CompletedAt.IsZero() {
		t.Errorf("legacy CompletedAt = %v, want zero", got[3].CompletedAt)
	}
}

func TestRecentOutcomesLimit(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	for i := 0; i < 5; i++ {
		insertWorkItem(t, sqlDB, workItem{
			artist: "A", title: "t" + string(rune('a'+i)), status: "done",
			outputPaths: pathsJSON("x.lrc"), completedAt: "2026-06-0" + string(rune('1'+i)) + "T10:00:00Z",
		})
	}
	got, err := repo.RecentOutcomes(ctx, 2)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limit 2 returned %d rows", len(got))
	}
	// Zero/negative limit returns nothing without touching the DB.
	if out, err := repo.RecentOutcomes(ctx, 0); err != nil || out != nil {
		t.Errorf("RecentOutcomes(0) = %v, %v; want nil, nil", out, err)
	}
}

func TestProviderEffectiveness(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Per-track attempt rows in lane_attempts (the true source, issue #282).
	insertLaneAttempts(t, sqlDB, "musixmatch", 75, 25) // 0.75
	insertLaneAttempts(t, sqlDB, "aaa", 1, 3)          // 0.25, sorts first

	got, err := repo.ProviderEffectiveness(ctx)
	if err != nil {
		t.Fatalf("ProviderEffectiveness: %v", err)
	}
	// petitlyrics has no attempts, so it does not appear (GROUP BY lane over
	// lane_attempts only yields lanes with at least one recorded attempt).
	if len(got) != 2 {
		t.Fatalf("got %d lanes, want 2", len(got))
	}
	// ORDER BY lane: aaa, musixmatch.
	if got[0].Lane != "aaa" || got[0].Hits != 1 || got[0].Misses != 3 || got[0].HitRate != 0.25 {
		t.Errorf("got[0] = %+v, want aaa hits=1 misses=3 rate=0.25", got[0])
	}
	if got[1].Lane != "musixmatch" || got[1].Hits != 75 || got[1].Misses != 25 || got[1].HitRate != 0.75 {
		t.Errorf("got[1] = %+v, want musixmatch hits=75 misses=25 rate=0.75", got[1])
	}
}

func TestInstrumentalInventory(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	libID := insertLibrary(t, sqlDB)

	// Detected instrumental, detection explicitly requested, with a file link.
	one := insertWorkItem(t, sqlDB, workItem{
		artist: "Mogwai", title: "Inst", status: "done",
		instrumentalResult: 1, detectInstrumental: 1,
	})
	sr := insertScanResult(t, sqlDB, libID, "/music/mogwai.flac")
	linkScanResult(t, sqlDB, one, sr)

	// Detected instrumental, detection flag NULL (used global default), no link.
	insertWorkItem(t, sqlDB, workItem{
		artist: "CLI", title: "Track", status: "done",
		instrumentalResult: 1, detectInstrumental: nil,
	})

	// Detection ran but NOT instrumental -> excluded.
	insertWorkItem(t, sqlDB, workItem{
		artist: "Vocal", title: "Song", status: "done",
		instrumentalResult: 0, detectInstrumental: 1,
	})
	// Detection not run -> excluded.
	insertWorkItem(t, sqlDB, workItem{
		artist: "NoDetect", title: "Song", status: "done",
		instrumentalResult: nil, detectInstrumental: 0,
	})

	got, err := repo.InstrumentalInventory(ctx)
	if err != nil {
		t.Fatalf("InstrumentalInventory: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d instrumental tracks, want 2: %+v", len(got), got)
	}

	// First row (lower id): file-linked, detect requested.
	if got[0].FilePath != "/music/mogwai.flac" {
		t.Errorf("got[0].FilePath = %q, want /music/mogwai.flac", got[0].FilePath)
	}
	if !got[0].DetectRequested.Valid || got[0].DetectRequested.Int64 != 1 {
		t.Errorf("got[0].DetectRequested = %+v, want Valid 1", got[0].DetectRequested)
	}
	// Second row: no link, NULL request flag.
	if got[1].FilePath != "" {
		t.Errorf("got[1].FilePath = %q, want empty (no scan link)", got[1].FilePath)
	}
	if got[1].DetectRequested.Valid {
		t.Errorf("got[1].DetectRequested = %+v, want NULL (global default)", got[1].DetectRequested)
	}
}

func TestFailureAnalysis(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// 3 failed with "timeout", 1 failed with "auth", 2 deferred with "miss".
	for i := 0; i < 3; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "to" + string(rune('a'+i)), status: "failed", lastError: "timeout"})
	}
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "auth", status: "failed", lastError: "auth error"})
	for i := 0; i < 2; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "df" + string(rune('a'+i)), status: "deferred", lastError: "miss"})
	}
	// done/pending rows must be excluded.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "ok", status: "done"})

	got, err := repo.FailureAnalysis(ctx)
	if err != nil {
		t.Fatalf("FailureAnalysis: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d groups, want 3: %+v", len(got), got)
	}
	// ORDER BY count DESC: timeout(3), miss(2), auth(1).
	if got[0].Status != "failed" || got[0].Reason != "timeout" || got[0].Count != 3 {
		t.Errorf("got[0] = %+v, want failed/timeout/3", got[0])
	}
	if got[1].Status != "deferred" || got[1].Reason != "miss" || got[1].Count != 2 {
		t.Errorf("got[1] = %+v, want deferred/miss/2", got[1])
	}
	if got[2].Status != "failed" || got[2].Reason != "auth error" || got[2].Count != 1 {
		t.Errorf("got[2] = %+v, want failed/auth error/1", got[2])
	}
}

// TestRecentOutcomesMalformedJSON exercises the json_valid guard: a done row
// whose output_paths is NON-EMPTY but invalid JSON must classify as "unknown"
// (the else branch) without erroring, proving json_valid short-circuits before
// json_extract is ever evaluated on the malformed value.
func TestRecentOutcomesMalformedJSON(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	insertWorkItem(t, sqlDB, workItem{
		artist: "Garbage", title: "G", status: "done",
		outputPaths: "garbage", completedAt: "2026-06-12T10:00:00Z",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Truncated", title: "T", status: "done",
		outputPaths: "{", completedAt: "2026-06-11T10:00:00Z",
	})

	got, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatalf("RecentOutcomes with malformed output_paths: want no error, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d outcomes, want 2: %+v", len(got), got)
	}
	for _, o := range got {
		if o.Result != reports.ResultUnknown {
			t.Errorf("outcome %q Result = %q, want %q (json_valid else branch)", o.Artist, o.Result, reports.ResultUnknown)
		}
	}
}

// TestFailureAnalysisEmptyReasonNormalized verifies an empty last_error
// normalizes to reason "unknown", matching internal/queue.CountFailuresByReason.
func TestFailureAnalysisEmptyReasonNormalized(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// A failed and a deferred row, each with an empty last_error: both must land
	// under reason 'unknown', kept distinct by status.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "ferr", status: "failed", lastError: ""})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "derr", status: "deferred", lastError: ""})

	got, err := repo.FailureAnalysis(ctx)
	if err != nil {
		t.Fatalf("FailureAnalysis: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2: %+v", len(got), got)
	}
	for _, g := range got {
		if g.Reason != "unknown" {
			t.Errorf("group %+v Reason = %q, want unknown (empty last_error normalized)", g, g.Reason)
		}
		if g.Count != 1 {
			t.Errorf("group %+v Count = %d, want 1", g, g.Count)
		}
	}
}

// TestRecentOutcomesMalformedTimestamp exercises the completed_at parse-error
// path: a done row whose completed_at is not RFC3339 must surface an error
// rather than silently zeroing the field.
func TestRecentOutcomesMalformedTimestamp(t *testing.T) {
	sqlDB := openTestDB(t)
	insertWorkItem(t, sqlDB, workItem{
		artist: "Bad", title: "T", status: "done",
		outputPaths: pathsJSON("x.lrc"), completedAt: "not-a-timestamp",
	})
	if _, err := reports.New(sqlDB).RecentOutcomes(context.Background(), 5); err == nil {
		t.Fatal("RecentOutcomes with malformed completed_at: want error, got nil")
	}
}

// TestQueryErrorsSurface verifies every report returns an error (rather than
// panicking or returning a bogus zero value) when the underlying query fails.
// Closing the DB makes the next query fail deterministically and
// env-independently.
func TestQueryErrorsSurface(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	repo := reports.New(sqlDB)

	if _, err := repo.QueueSummary(ctx); err == nil {
		t.Error("QueueSummary on closed DB: want error")
	}
	if _, err := repo.RecentOutcomes(ctx, 5); err == nil {
		t.Error("RecentOutcomes on closed DB: want error")
	}
	if _, err := repo.ProviderEffectiveness(ctx); err == nil {
		t.Error("ProviderEffectiveness on closed DB: want error")
	}
	if _, err := repo.InstrumentalInventory(ctx); err == nil {
		t.Error("InstrumentalInventory on closed DB: want error")
	}
	if _, err := repo.FailureAnalysis(ctx); err == nil {
		t.Error("FailureAnalysis on closed DB: want error")
	}
	if _, err := repo.CountInstrumental(ctx); err == nil {
		t.Error("CountInstrumental on closed DB: want error")
	}
}

// TestCountInstrumental verifies CountInstrumental returns the number of
// work_queue rows with instrumental_result = 1, excluding other values.
func TestCountInstrumental(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Start: zero instrumentals.
	n, err := repo.CountInstrumental(ctx)
	if err != nil {
		t.Fatalf("CountInstrumental on empty DB: %v", err)
	}
	if n != 0 {
		t.Errorf("CountInstrumental = %d, want 0 on empty DB", n)
	}

	// Two confirmed instrumental, one not-instrumental (0), one not-run (nil).
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "i1", status: "done", instrumentalResult: 1})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "i2", status: "done", instrumentalResult: 1})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "v1", status: "done", instrumentalResult: 0})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "n1", status: "done", instrumentalResult: nil})

	n, err = repo.CountInstrumental(ctx)
	if err != nil {
		t.Fatalf("CountInstrumental: %v", err)
	}
	if n != 2 {
		t.Errorf("CountInstrumental = %d, want 2 (only instrumental_result=1)", n)
	}
}

// TestCountInstrumentalClosedDB verifies CountInstrumental surfaces the error
// when the underlying query fails.
func TestCountInstrumentalClosedDB(t *testing.T) {
	sqlDB := openTestDB(t)
	if err := sqlDB.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	if _, err := reports.New(sqlDB).CountInstrumental(context.Background()); err == nil {
		t.Error("CountInstrumental on closed DB: want error, got nil")
	}
}

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse time %q: %v", s, err)
	}
	return parsed
}
