package reports_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/reports"
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
	outcomeType        any // string ("synced"|"unsynced"|"instrumental") or nil
	// Buffered-panel columns (#572). These are applied as a post-insert UPDATE
	// only when non-zero/non-empty, so existing callers keep the schema defaults
	// (priority 0, batch_seq NULL, next_attempt_at 1970 = eligible, created_at now).
	priority      int    // 0 => default (PriorityScan); negative => miss tier
	batchSeq      int    // 0 => NULL (unbuffered); >0 => buffered at this seq
	nextAttemptAt string // "" => default (eligible); else RFC3339 (future => cooldown)
	createdAt     string // "" => default (now); else RFC3339
}

func insertWorkItem(t *testing.T, sqlDB *sql.DB, w workItem) int64 {
	t.Helper()
	// artist_key/title_key carry a UNIQUE index; the app normally stamps them at
	// enqueue. Tests use distinct titles, so derive the keys from artist+title to
	// satisfy the constraint without a normalize dependency.
	res, err := sqlDB.ExecContext(context.Background(),
		`INSERT INTO work_queue
            (artist, title, artist_key, title_key, album, status, last_error, output_paths,
             completed_at, provider_lane, instrumental_result, detect_instrumental, outcome_type)
         VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.artist, w.title, w.artist, w.title, w.album, w.status, w.lastError, w.outputPaths,
		w.completedAt, w.providerLane, w.instrumentalResult, w.detectInstrumental, w.outcomeType)
	if err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("last insert id: %v", err)
	}
	setWorkItemColumn(t, sqlDB, id, "priority", w.priority != 0, w.priority)
	setWorkItemColumn(t, sqlDB, id, "batch_seq", w.batchSeq != 0, w.batchSeq)
	setWorkItemColumn(t, sqlDB, id, "next_attempt_at", w.nextAttemptAt != "", w.nextAttemptAt)
	setWorkItemColumn(t, sqlDB, id, "created_at", w.createdAt != "", w.createdAt)
	return id
}

// setWorkItemColumn updates one work_queue column on row id when set is true.
// It keeps the base INSERT untouched for the NOT-NULL columns (priority,
// next_attempt_at, created_at) so unset fields fall through to their schema
// defaults instead of a NULL that would violate the constraint.
func setWorkItemColumn(t *testing.T, sqlDB *sql.DB, id int64, col string, set bool, val any) {
	t.Helper()
	if !set {
		return
	}
	if _, err := sqlDB.ExecContext(context.Background(),
		"UPDATE work_queue SET "+col+" = ? WHERE id = ?", val, id); err != nil {
		t.Fatalf("set work_queue.%s: %v", col, err)
	}
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

	// Classification is driven by outcome_type (the real written outcome stamped
	// at completion, #379), NOT the output_paths filename extension. output_paths
	// here deliberately disagrees with outcome_type (every row carries the stale
	// enqueue-time ".lrc") to prove the extension is no longer consulted.
	// Insert in scrambled completion order to verify DESC sort; one NULL
	// completed_at must sort last.
	insertWorkItem(t, sqlDB, workItem{
		artist: "Synced", title: "S", album: "Al1", status: "done",
		outputPaths: pathsJSON("song.lrc"), completedAt: "2026-06-10T10:00:00Z",
		providerLane: "musixmatch", outcomeType: "synced",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Unsynced", title: "U", status: "done",
		outputPaths: pathsJSON("song.lrc"), completedAt: "2026-06-12T10:00:00Z",
		outcomeType: "unsynced",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Instrumental", title: "I", status: "done",
		outputPaths: pathsJSON("song.lrc"), completedAt: "2026-06-13T10:00:00Z",
		outcomeType: "instrumental",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Miss", title: "M", status: "done",
		lastError: "miss limit reached", outputPaths: pathsJSON("ignored.lrc"),
		completedAt: "2026-06-11T10:00:00Z",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "Legacy", title: "L", status: "done",
		outputPaths: "", completedAt: nil, // NULL completed_at, NULL outcome_type
	})
	// A non-done row must be excluded.
	insertWorkItem(t, sqlDB, workItem{artist: "Pending", title: "P", status: "pending"})

	got, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatalf("RecentOutcomes: %v", err)
	}
	if len(got) != 5 {
		t.Fatalf("got %d outcomes, want 5 (done only): %+v", len(got), got)
	}

	// Order: newest completed first, NULL completed_at last.
	wantOrder := []struct {
		artist string
		result reports.ResultClass
	}{
		{"Instrumental", reports.ResultInstrumental},
		{"Unsynced", reports.ResultUnsynced},
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
	synced := got[3]
	if synced.Album != "Al1" || synced.ProviderLane != "musixmatch" {
		t.Errorf("synced row fields = album %q lane %q, want Al1/musixmatch", synced.Album, synced.ProviderLane)
	}
	if synced.CompletedAt != mustParse(t, "2026-06-10T10:00:00Z") {
		t.Errorf("synced CompletedAt = %v, want 2026-06-10T10:00:00Z", synced.CompletedAt)
	}
	// NULL completed_at -> zero time.
	if !got[4].CompletedAt.IsZero() {
		t.Errorf("legacy CompletedAt = %v, want zero", got[4].CompletedAt)
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
// TestRecentOutcomesIgnoresOutputPaths verifies output_paths no longer
// influences classification (#379): a row with garbage output_paths but a valid
// outcome_type classifies by outcome_type, and a row with no outcome_type is
// 'unknown' regardless of output_paths content. The query never parses
// output_paths, so malformed JSON cannot error or misclassify.
func TestRecentOutcomesIgnoresOutputPaths(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	insertWorkItem(t, sqlDB, workItem{
		artist: "GarbagePathsSynced", title: "G", status: "done",
		outputPaths: "garbage", completedAt: "2026-06-12T10:00:00Z",
		outcomeType: "synced",
	})
	insertWorkItem(t, sqlDB, workItem{
		artist: "TruncatedNoOutcome", title: "T", status: "done",
		outputPaths: "{", completedAt: "2026-06-11T10:00:00Z",
	})

	got, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatalf("RecentOutcomes with malformed output_paths: want no error, got %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d outcomes, want 2: %+v", len(got), got)
	}
	if got[0].Result != reports.ResultSynced {
		t.Errorf("garbage-output_paths row Result = %q, want %q (outcome_type wins)", got[0].Result, reports.ResultSynced)
	}
	if got[1].Result != reports.ResultUnknown {
		t.Errorf("no-outcome_type row Result = %q, want %q", got[1].Result, reports.ResultUnknown)
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
	if _, err := repo.UpNext(ctx, 5); err == nil {
		t.Error("UpNext on closed DB: want error")
	}
	if _, err := repo.QueueEligibility(ctx); err == nil {
		t.Error("QueueEligibility on closed DB: want error")
	}
}

// TestCountInstrumental verifies CountInstrumental counts every row whose
// recorded outcome is instrumental (outcome_type='instrumental'), regardless of
// source -- audio-detected and provider-flagged alike -- not only rows with
// instrumental_result=1 (#379).
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

	// Audio-detected instrumental (both outcome_type and instrumental_result set).
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "audio", status: "done", outcomeType: "instrumental", instrumentalResult: 1})
	// Provider-flagged instrumental: outcome_type set, instrumental_result NULL.
	// This is the row the old instrumental_result=1 count missed.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "provider", status: "done", outcomeType: "instrumental", instrumentalResult: nil})
	// Non-instrumental outcomes must not be counted.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "synced", status: "done", outcomeType: "synced"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "unsynced", status: "done", outcomeType: "unsynced"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "legacy", status: "done", outcomeType: nil})

	n, err = repo.CountInstrumental(ctx)
	if err != nil {
		t.Fatalf("CountInstrumental: %v", err)
	}
	if n != 2 {
		t.Errorf("CountInstrumental = %d, want 2 (all instrumental outcomes, incl. provider-flagged)", n)
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

// TestUpNext verifies the buffered list is returned in batch_seq order and that
// only eligible buffered rows appear (non-buffered, done, and future-cooldown
// rows are excluded), matching the batched-claim predicate. Synthetic fixtures.
func TestUpNext(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Buffered rows stamped out of insertion order to prove ordering is by
	// batch_seq, not id. Priorities span the miss/scan tiers.
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist 3", title: "Track Gamma", status: "pending", batchSeq: 3, priority: 0})
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist 1", title: "Track Alpha", album: "Album One", status: "failed", batchSeq: 1, priority: -100})
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist 2", title: "Track Beta", status: "deferred", batchSeq: 2, priority: 0})

	// Excluded: not buffered (batch_seq NULL).
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist U", title: "Unbuffered", status: "pending"})
	// Excluded: buffered but already done (fails the status predicate).
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist D", title: "Done", status: "done", batchSeq: 4})
	// Excluded: buffered and pending but on cooldown (next_attempt_at in future).
	insertWorkItem(t, sqlDB, workItem{artist: "Test Artist C", title: "Cooldown", status: "deferred", batchSeq: 5, nextAttemptAt: "3000-01-01T00:00:00Z"})

	got, err := repo.UpNext(ctx, 10)
	if err != nil {
		t.Fatalf("UpNext: %v", err)
	}
	wantTitles := []string{"Track Alpha", "Track Beta", "Track Gamma"}
	if len(got) != len(wantTitles) {
		t.Fatalf("UpNext returned %d rows, want %d: %+v", len(got), len(wantTitles), got)
	}
	for i, want := range wantTitles {
		if got[i].Title != want {
			t.Errorf("row %d: title = %q, want %q (order must be by batch_seq)", i, got[i].Title, want)
		}
	}
	// Album passes through for its own column.
	if got[0].Album != "Album One" {
		t.Errorf("row 0 album = %q, want %q", got[0].Album, "Album One")
	}
	// Priority passes through raw for the handler's tier mapping.
	if got[0].Priority != -100 {
		t.Errorf("row 0 priority = %d, want -100 (miss tier)", got[0].Priority)
	}
	if got[1].Priority != 0 {
		t.Errorf("row 1 priority = %d, want 0 (scan tier)", got[1].Priority)
	}
	if got[0].CreatedAt.IsZero() {
		t.Error("row 0 CreatedAt is zero; want the schema-default timestamp parsed")
	}
}

// TestUpNextLimit checks the limit guard and cap.
func TestUpNextLimit(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	for i := 1; i <= 4; i++ {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: "t" + string(rune('a'+i)), status: "pending", batchSeq: i})
	}
	if got, err := repo.UpNext(ctx, 0); err != nil || got != nil {
		t.Errorf("UpNext(limit=0) = (%v, %v), want (nil, nil)", got, err)
	}
	got, err := repo.UpNext(ctx, 2)
	if err != nil {
		t.Fatalf("UpNext(limit=2): %v", err)
	}
	if len(got) != 2 {
		t.Errorf("UpNext(limit=2) returned %d rows, want 2", len(got))
	}
}

// TestQueueEligibility checks the eligible/cooldown split and the exclusion of
// done/processing rows, all sharing one now so the two counts cannot skew.
func TestQueueEligibility(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	// Eligible: pending, failed, deferred with a past (default) next_attempt_at.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "elig-pending", status: "pending"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "elig-failed", status: "failed"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "elig-deferred", status: "deferred"})
	// Cooldown: same statuses but next_attempt_at in the future.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "cool-deferred", status: "deferred", nextAttemptAt: "3000-01-01T00:00:00Z"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "cool-failed", status: "failed", nextAttemptAt: "3000-01-01T00:00:00Z"})
	// Excluded from both: done and processing are not part of the backlog.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "done", status: "done"})
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "proc", status: "processing"})

	got, err := repo.QueueEligibility(ctx)
	if err != nil {
		t.Fatalf("QueueEligibility: %v", err)
	}
	if got.Eligible != 3 {
		t.Errorf("Eligible = %d, want 3", got.Eligible)
	}
	if got.Cooldown != 2 {
		t.Errorf("Cooldown = %d, want 2", got.Cooldown)
	}
}

// TestQueueEligibilityEmpty confirms the SUM-over-no-rows NULL collapses to 0.
func TestQueueEligibilityEmpty(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)

	got, err := repo.QueueEligibility(ctx)
	if err != nil {
		t.Fatalf("QueueEligibility: %v", err)
	}
	if got.Eligible != 0 || got.Cooldown != 0 {
		t.Errorf("empty queue = %+v, want {0 0}", got)
	}
}
