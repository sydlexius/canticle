package identityrepair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	dbpkg "github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/normalize"
)

// errReport is the sentinel a Report callback returns to assert Run aborts.
var errReport = errors.New("report boom")

// fakeReader maps a file path to the corrected identity a re-read would return.
// A path absent from the map yields an error, standing in for an unreadable file.
type fakeReader map[string][2]string

func (f fakeReader) read(path string) (string, string, error) {
	v, ok := f[path]
	if !ok {
		return "", "", sql.ErrNoRows // any non-nil error: file unreadable
	}
	return v[0], v[1], nil
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := dbpkg.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// libSeq gives each seeded library a unique path/name within a test, since
// libraries.path carries a UNIQUE constraint.
var libSeq int

func seedLibrary(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	libSeq++
	res, err := db.Exec(`INSERT INTO libraries (path, name) VALUES (?, ?)`,
		fmt.Sprintf("/lib%d", libSeq), fmt.Sprintf("Lib%d", libSeq))
	if err != nil {
		t.Fatalf("seed library: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedScan inserts a scan_results row and returns its id. artist_key/title_key
// are normalized from artist/title, matching how a real scan populates them.
func seedScan(t *testing.T, db *sql.DB, libID int64, filePath, artist, albumArtist, title string) int64 {
	t.Helper()
	res, err := db.Exec(
		`INSERT INTO scan_results (library_id, file_path, artist, title, album, album_artist, artist_key, title_key, outdir, filename, status)
		 VALUES (?, ?, ?, ?, '', ?, ?, ?, '/out', 'f.lrc', 'pending')`,
		libID, filePath, artist, title, albumArtist, normalize.NormalizeKey(artist), normalize.NormalizeKey(title))
	if err != nil {
		t.Fatalf("seed scan_results: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// seedQueue inserts a work_queue row keyed like a real enqueue and links it to
// scanID via the junction. The title matches the seeded scan row's ("Song") so
// (artist_key, title_key) lines up with the coupled scan_results row. Returns
// the work_queue id.
func seedQueue(t *testing.T, db *sql.DB, artist, albumArtist, status string, scanID int64) int64 {
	t.Helper()
	const title = "Song"
	// A distinct output path per row so a merge's output_paths union is observable.
	outputPaths := fmt.Sprintf(`[{"outdir":"/out","filename":"f%d.lrc"}]`, scanID)
	res, err := db.Exec(
		`INSERT INTO work_queue (artist, title, album_artist, artist_key, title_key, status, output_paths)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		artist, title, albumArtist, normalize.NormalizeKey(artist), normalize.NormalizeKey(title), status, outputPaths)
	if err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, scanID); err != nil {
		t.Fatalf("seed junction: %v", err)
	}
	return id
}

// queueOutputPaths returns the raw output_paths JSON of a work_queue row.
func queueOutputPaths(t *testing.T, db *sql.DB, id int64) string {
	t.Helper()
	var raw string
	if err := db.QueryRow(`SELECT output_paths FROM work_queue WHERE id = ?`, id).Scan(&raw); err != nil {
		t.Fatalf("read output_paths %d: %v", id, err)
	}
	return raw
}

func scanIdentity(t *testing.T, db *sql.DB, id int64) (artist, albumArtist, artistKey string) {
	t.Helper()
	if err := db.QueryRow(`SELECT artist, album_artist, artist_key FROM scan_results WHERE id = ?`, id).
		Scan(&artist, &albumArtist, &artistKey); err != nil {
		t.Fatalf("read scan_results %d: %v", id, err)
	}
	return
}

func queueIdentity(t *testing.T, db *sql.DB, id int64) (artist, artistKey, status string) {
	t.Helper()
	if err := db.QueryRow(`SELECT artist, artist_key, status FROM work_queue WHERE id = ?`, id).
		Scan(&artist, &artistKey, &status); err != nil {
		t.Fatalf("read work_queue %d: %v", id, err)
	}
	return
}

func queueCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM work_queue`).Scan(&n); err != nil {
		t.Fatalf("count work_queue: %v", err)
	}
	return n
}

// stampSettleState stamps every settle-state column a reopen (#960) must
// clear, plus attempts and completed_at, so a test can assert an ACTUAL reset
// rather than columns that were already NULL/zero.
func stampSettleState(t *testing.T, db *sql.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(
		`UPDATE work_queue SET provider_lane = 'musixmatch', outcome_type = 'lrc',
		 outcome_detail = 'ok', timing_outcome = 'ok', overrun_magnitude = -1.5,
		 overrun_ratio = 0.98, evaluated_at = '2026-01-01T00:00:00Z',
		 completed_at = '2026-01-01T00:00:00Z', attempts = 3
		 WHERE id = ?`, id); err != nil {
		t.Fatalf("stamp settle state %d: %v", id, err)
	}
}

// settleState is the full set of columns #960's reopen touches (or must
// deliberately leave alone).
type settleState struct {
	status                                   string
	attempts                                 int
	providerLane, outcomeType, outcomeDetail sql.NullString
	timingOutcome                            sql.NullString
	overrunMagnitude, overrunRatio           sql.NullFloat64
	evaluatedAt, completedAt                 sql.NullString
}

func readSettleState(t *testing.T, db *sql.DB, id int64) settleState {
	t.Helper()
	var s settleState
	if err := db.QueryRow(
		`SELECT status, attempts, provider_lane, outcome_type, outcome_detail, timing_outcome,
		        overrun_magnitude, overrun_ratio, evaluated_at, completed_at
		 FROM work_queue WHERE id = ?`, id).Scan(
		&s.status, &s.attempts, &s.providerLane, &s.outcomeType, &s.outcomeDetail, &s.timingOutcome,
		&s.overrunMagnitude, &s.overrunRatio, &s.evaluatedAt, &s.completedAt); err != nil {
		t.Fatalf("read settle state %d: %v", id, err)
	}
	return s
}

// Basic recovery: a mangled multi-value artist is corrected in scan_results and
// its coupled work_queue row is re-keyed in place.
func TestRun_RekeyInPlace(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 1 || res.Changed != 1 || res.QueueUpdated != 1 || res.QueueMerged != 0 {
		t.Fatalf("Result = %+v; want Scanned=1 Changed=1 QueueUpdated=1 QueueMerged=0", res)
	}

	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a, _, k := scanIdentity(t, db, sr); a != "Alpha; Bravo" || k != wantKey {
		t.Errorf("scan_results = (%q,%q); want (%q,%q)", a, k, "Alpha; Bravo", wantKey)
	}
	if a, k, _ := queueIdentity(t, db, wq); a != "Alpha; Bravo" || k != wantKey {
		t.Errorf("work_queue = (%q,%q); want (%q,%q)", a, k, "Alpha; Bravo", wantKey)
	}
}

// Dry run reports the change and mutates nothing.
func TestRun_DryRun(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	var reported []Change
	res, err := New(db, reader.read).Run(context.Background(), Options{
		DryRun: true,
		Report: func(c Change) error { reported = append(reported, c); return nil },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 0 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=0", res)
	}
	if len(reported) != 1 || reported[0].NewArtist != "Alpha; Bravo" || reported[0].OldArtist != "AlphaBravo" {
		t.Fatalf("reported = %+v; want one change AlphaBravo->Alpha; Bravo", reported)
	}
	if a, _, _ := scanIdentity(t, db, sr); a != "AlphaBravo" {
		t.Errorf("dry run mutated scan_results: artist = %q", a)
	}
}

// On a key collision, the old-key queue row is merged into the row already at
// the corrected key: their output_paths are unioned, the merged row is deleted,
// both scan_results link to the survivor, and a survivor that had already
// completed is reopened so the re-fetch writes the newly-linked file's lyrics
// (never fabricating a 'done' status that would violate the scan_results
// invariant).
func TestRun_MergeReopensDoneSurvivor(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	wqBad := seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)

	reader := fakeReader{
		"/m/1.mp3": {"Alpha; Bravo", ""},
		"/m/2.mp3": {"Alpha; Bravo", ""}, // already correct -> unchanged
	}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueMerged != 1 || res.QueueUpdated != 0 {
		t.Fatalf("Result = %+v; want Changed=1 QueueMerged=1 QueueUpdated=0", res)
	}
	if n := queueCount(t, db); n != 1 {
		t.Fatalf("work_queue count = %d; want 1 (bad merged into good)", n)
	}
	// The 'done' survivor is reopened so it re-fetches and writes both files.
	if _, _, status := queueIdentity(t, db, wqGood); status != "pending" {
		t.Errorf("survivor status = %q; want pending (reopened, not fabricated done)", status)
	}
	// Its output_paths now cover both files.
	op := queueOutputPaths(t, db, wqGood)
	if !strings.Contains(op, fmt.Sprintf("f%d.lrc", srBad)) || !strings.Contains(op, fmt.Sprintf("f%d.lrc", srGood)) {
		t.Errorf("survivor output_paths = %q; want both f%d.lrc and f%d.lrc", op, srBad, srGood)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wqBad).Scan(new(int)); err != sql.ErrNoRows {
		t.Errorf("bad work_queue row still present; want deleted (err=%v)", err)
	}
	var links int
	if err := db.QueryRow(`SELECT count(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, wqGood).Scan(&links); err != nil {
		t.Fatalf("count junction: %v", err)
	}
	if links != 2 {
		t.Errorf("survivor junction links = %d; want 2", links)
	}
}

// An 'unavailable' survivor (#477: RetireMiss's terminal state for an
// exhausted benign miss) is NOT reopened on merge, unlike a 'done' survivor:
// the merged key is the one that already exhausted its miss budget, so a
// reopen buys one repeat of the same lookup. It keeps its status, sentinel and
// miss_count, and gains the dropped row's output_paths, which
// queue.RecheckRetired carries when it revives the row.
func TestRun_MergeKeepsUnavailableSurvivorRetired(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	wqBad := seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "unavailable", srGood)
	if _, err := db.Exec(`UPDATE work_queue SET last_error = 'miss limit reached', miss_count = 15 WHERE id = ?`, wqGood); err != nil {
		t.Fatalf("stamp miss-limit sentinel: %v", err)
	}

	reader := fakeReader{
		"/m/1.mp3": {"Alpha; Bravo", ""},
		"/m/2.mp3": {"Alpha; Bravo", ""}, // already correct -> unchanged
	}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueMerged != 1 || res.QueueUpdated != 0 {
		t.Fatalf("Result = %+v; want Changed=1 QueueMerged=1 QueueUpdated=0", res)
	}
	if n := queueCount(t, db); n != 1 {
		t.Fatalf("work_queue count = %d; want 1 (bad merged into good)", n)
	}
	var status, lastError string
	var missCount int
	if err := db.QueryRow(`SELECT status, last_error, miss_count FROM work_queue WHERE id = ?`, wqGood).
		Scan(&status, &lastError, &missCount); err != nil {
		t.Fatalf("read survivor: %v", err)
	}
	if status != "unavailable" || lastError != "miss limit reached" || missCount != 15 {
		t.Errorf("survivor = (%q, %q, miss_count=%d); want (unavailable, miss limit reached, 15): "+
			"an exhausted-miss survivor must not be reopened by a merge", status, lastError, missCount)
	}
	op := queueOutputPaths(t, db, wqGood)
	if !strings.Contains(op, fmt.Sprintf("f%d.lrc", srBad)) || !strings.Contains(op, fmt.Sprintf("f%d.lrc", srGood)) {
		t.Errorf("survivor output_paths = %q; want both f%d.lrc and f%d.lrc", op, srBad, srGood)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wqBad).Scan(new(int)); err != sql.ErrNoRows {
		t.Errorf("bad work_queue row still present; want deleted (err=%v)", err)
	}
}

// A file that cannot be re-read leaves its row untouched and is tallied.
func TestRun_ReadFailureSkips(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/gone.mp3", "AlphaBravo", "", "Song")

	res, err := New(db, fakeReader{}.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 1 || res.ReadFailures != 1 || res.Changed != 0 {
		t.Fatalf("Result = %+v; want Scanned=1 ReadFailures=1 Changed=0", res)
	}
	if a, _, _ := scanIdentity(t, db, sr); a != "AlphaBravo" {
		t.Errorf("read-failure row mutated: artist = %q", a)
	}
}

// A row already correct on disk is not counted as changed.
func TestRun_UnchangedRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	seedScan(t, db, lib, "/m/1.mp3", "Solo Artist", "", "Song")

	reader := fakeReader{"/m/1.mp3": {"Solo Artist", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 1 || res.Changed != 0 {
		t.Fatalf("Result = %+v; want Scanned=1 Changed=0", res)
	}
}

// When only the album-artist differs (artist key stable), scan_results and the
// coupled queue row have their display columns synced with no re-key.
func TestRun_AlbumArtistOnly(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "Alpha", "OldAA", "Song")
	wq := seedQueue(t, db, "Alpha", "OldAA", "pending", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha", "New; AA"}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 1 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=1", res)
	}
	if a, aa, k := scanIdentity(t, db, sr); a != "Alpha" || aa != "New; AA" || k != normalize.NormalizeKey("Alpha") {
		t.Errorf("scan_results = (%q,%q,%q); want (Alpha, New; AA, %q)", a, aa, k, normalize.NormalizeKey("Alpha"))
	}
	var wqAA string
	if err := db.QueryRow(`SELECT album_artist FROM work_queue WHERE id = ?`, wq).Scan(&wqAA); err != nil {
		t.Fatalf("read wq album_artist: %v", err)
	}
	if wqAA != "New; AA" {
		t.Errorf("work_queue album_artist = %q; want New; AA", wqAA)
	}
}

// A change whose coupled queue row is mid-flight ('processing') is skipped
// entirely, so scan_results and work_queue never drift apart.
func TestRun_ProcessingSkipped(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "processing", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProcessingSkips != 1 || res.Changed != 0 {
		t.Fatalf("Result = %+v; want ProcessingSkips=1 Changed=0", res)
	}
	if a, _, _ := scanIdentity(t, db, sr); a != "AlphaBravo" {
		t.Errorf("processing-skipped row mutated: artist = %q", a)
	}
}

// A corrected scan row with no coupled work_queue row updates scan_results only.
func TestRun_NoQueueRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song") // no seedQueue

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 0 || res.QueueMerged != 0 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=0 QueueMerged=0", res)
	}
	if a, _, _ := scanIdentity(t, db, sr); a != "Alpha; Bravo" {
		t.Errorf("scan_results artist = %q; want Alpha; Bravo", a)
	}
}

// A not-yet-completed survivor keeps its own status on merge (never downgraded
// or fabricated) and absorbs the dropped row's output_paths, so its pending
// fetch will write both files.
func TestRun_MergeKeepsPendingSurvivor(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", srBad)                 // dropped row: done
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "pending", srGood) // survivor: pending

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueueMerged != 1 {
		t.Fatalf("Result = %+v; want QueueMerged=1", res)
	}
	if _, _, status := queueIdentity(t, db, wqGood); status != "pending" {
		t.Errorf("survivor status = %q; want unchanged pending (no fabricated done)", status)
	}
	op := queueOutputPaths(t, db, wqGood)
	if !strings.Contains(op, fmt.Sprintf("f%d.lrc", srBad)) || !strings.Contains(op, fmt.Sprintf("f%d.lrc", srGood)) {
		t.Errorf("survivor output_paths = %q; want both files' paths unioned", op)
	}
}

// When the row already at the corrected key is mid-flight ('processing'), the
// change is skipped rather than disturbing the in-flight row.
func TestRun_ConflictProcessingSkipped(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	seedQueue(t, db, "Alpha; Bravo", "", "processing", srGood) // corrected key in-flight

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ProcessingSkips != 1 || res.Changed != 0 {
		t.Fatalf("Result = %+v; want ProcessingSkips=1 Changed=0", res)
	}
	if a, _, _ := scanIdentity(t, db, srBad); a != "AlphaBravo" {
		t.Errorf("conflict-processing-skipped row mutated: artist = %q", a)
	}
}

// When the dropped row's output_paths is malformed (and the survivor's empty),
// the merge reconstructs both rows' write targets from their linked
// scan_results rather than losing them, so the deleted row's file remains a
// rewrite target.
func TestRun_MergeReconstructsLostOutputPaths(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")    // outdir /out, filename f.lrc
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song") // outdir /out, filename f.lrc
	// Give the two scans distinct write targets so both are observable.
	if _, err := db.Exec(`UPDATE scan_results SET outdir = '/a', filename = 'bad.lrc' WHERE id = ?`, srBad); err != nil {
		t.Fatalf("set srBad target: %v", err)
	}
	if _, err := db.Exec(`UPDATE scan_results SET outdir = '/b', filename = 'good.lrc' WHERE id = ?`, srGood); err != nil {
		t.Fatalf("set srGood target: %v", err)
	}
	wqBad := seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "pending", srGood)
	// Survivor has a legacy-empty column; the dropped row has malformed JSON.
	if _, err := db.Exec(`UPDATE work_queue SET output_paths = '' WHERE id = ?`, wqGood); err != nil {
		t.Fatalf("blank survivor output_paths: %v", err)
	}
	if _, err := db.Exec(`UPDATE work_queue SET output_paths = 'not json' WHERE id = ?`, wqBad); err != nil {
		t.Fatalf("corrupt dropped output_paths: %v", err)
	}

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueueMerged != 1 {
		t.Fatalf("Result = %+v; want QueueMerged=1", res)
	}
	// The dropped row's target (reconstructed from srBad) survives in the union.
	op := queueOutputPaths(t, db, wqGood)
	if !strings.Contains(op, "bad.lrc") {
		t.Errorf("survivor output_paths = %q; want the dropped row's reconstructed target bad.lrc", op)
	}
	if !strings.Contains(op, "good.lrc") {
		t.Errorf("survivor output_paths = %q; want the survivor's reconstructed target good.lrc", op)
	}
}

// The divergent shared-row case: two scans share one mangled queue row but
// re-read to DIFFERENT corrected identities. The first re-keys the shared row;
// the second must not stay linked to that now-wrong-identity row -- its stale
// junction link is dropped so it re-enqueues cleanly.
func TestRun_DivergentSharedRow(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srA := seedScan(t, db, lib, "/m/1.mp3", "AB C", "", "Song")
	srB := seedScan(t, db, lib, "/m/2.mp3", "AB C", "", "Song")
	// Force both scans onto one shared mangled queue row (same old key/title).
	if _, err := db.Exec(`UPDATE scan_results SET artist_key = 'abc' WHERE id IN (?, ?)`, srA, srB); err != nil {
		t.Fatalf("force shared key: %v", err)
	}
	wq := seedQueue(t, db, "AB C", "", "pending", srA)
	if _, err := db.Exec(`UPDATE work_queue SET artist_key = 'abc' WHERE id = ?`, wq); err != nil {
		t.Fatalf("force queue key: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, wq, srB); err != nil {
		t.Fatalf("link srB to shared row: %v", err)
	}

	// The two files correct to DIFFERENT identities.
	reader := fakeReader{"/m/1.mp3": {"A; BC", ""}, "/m/2.mp3": {"AB; C", ""}}
	if _, err := New(db, reader.read).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The shared row is re-keyed to whichever scan processed first (srA -> "a; bc").
	// srB corrected to "ab; c" must NOT remain linked to that row.
	var srBLinkedToWq int
	if err := db.QueryRow(
		`SELECT count(*) FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`, wq, srB).Scan(&srBLinkedToWq); err != nil {
		t.Fatalf("count srB link: %v", err)
	}
	if srBLinkedToWq != 0 {
		t.Errorf("srB still linked to the re-keyed shared row; want the stale link dropped")
	}
	// srB's own identity is still corrected in scan_results.
	if a, _, k := scanIdentity(t, db, srB); a != "AB; C" || k != normalize.NormalizeKey("AB; C") {
		t.Errorf("srB scan_results = (%q,%q); want corrected to AB; C", a, k)
	}
}

// A Report failure in apply mode aborts the run AND rolls the correction back:
// the report (backup) is written inside the transaction before commit, so a
// report error must leave the stored identity unchanged.
func TestRun_ReportFailureRollsBack(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "pending", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	_, err := New(db, reader.read).Run(context.Background(), Options{
		Report: func(Change) error { return errReport },
	})
	if !errors.Is(err, errReport) {
		t.Fatalf("Run err = %v; want wrapped %v", err, errReport)
	}
	// Rolled back: both tables keep the pre-correction identity.
	if a, _, _ := scanIdentity(t, db, sr); a != "AlphaBravo" {
		t.Errorf("scan_results mutated despite report failure: artist = %q", a)
	}
	if a, _, _ := queueIdentity(t, db, wq); a != "AlphaBravo" {
		t.Errorf("work_queue mutated despite report failure: artist = %q", a)
	}
}

// A canceled context stops the run before it mutates.
func TestRun_ContextCanceled(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	if _, err := New(db, reader.read).Run(ctx, Options{}); err == nil {
		t.Fatal("Run err = nil; want context.Canceled")
	}
	if a, _, _ := scanIdentity(t, db, sr); a != "AlphaBravo" {
		t.Errorf("canceled run mutated artist = %q", a)
	}
}

// LibraryID scopes the repair to a single library.
func TestRun_LibraryScope(t *testing.T) {
	db := openDB(t)
	lib1 := seedLibrary(t, db)
	lib2 := seedLibrary(t, db)
	sr1 := seedScan(t, db, lib1, "/m/1.mp3", "AlphaBravo", "", "Song")
	sr2 := seedScan(t, db, lib2, "/m/2.mp3", "CharlieDelta", "", "Song2")

	reader := fakeReader{
		"/m/1.mp3": {"Alpha; Bravo", ""},
		"/m/2.mp3": {"Charlie; Delta", ""},
	}
	res, err := New(db, reader.read).Run(context.Background(), Options{LibraryID: &lib1})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Scanned != 1 || res.Changed != 1 {
		t.Fatalf("Result = %+v; want Scanned=1 Changed=1 (lib1 only)", res)
	}
	if a, _, _ := scanIdentity(t, db, sr1); a != "Alpha; Bravo" {
		t.Errorf("lib1 row not corrected: %q", a)
	}
	if a, _, _ := scanIdentity(t, db, sr2); a != "CharlieDelta" {
		t.Errorf("lib2 row should be untouched: %q", a)
	}
}

// #960: the re-key-in-place path (corrected key collides with nothing) is the
// COMMON case for a genuine identity correction, and it must reopen a 'done'
// row so the worker actually re-fetches under the corrected identity -- not
// just sync the display columns and leave status='done' forever settled.
// Every settle-state column (#655/#773's stale-vs-NULL concern) must be
// cleared, not just status, mirroring queue.go:2806's ResetInstrumental shape.
func TestRun_RekeyReopensDoneRowAndClearsSettleState(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "done", sr)
	stampSettleState(t, db, wq)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 1 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=1", res)
	}

	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	if a, k, status := queueIdentity(t, db, wq); a != "Alpha; Bravo" || k != wantKey || status != "pending" {
		t.Fatalf("work_queue = (%q,%q,%q); want (Alpha; Bravo,%q,pending) -- a done row must be reopened, not left settled", a, k, status, wantKey)
	}

	s := readSettleState(t, db, wq)
	if s.attempts != 0 {
		t.Errorf("attempts = %d; want 0 (reset on reopen)", s.attempts)
	}
	if s.completedAt.Valid {
		t.Errorf("completed_at = %q; want NULL after reopen", s.completedAt.String)
	}
	if s.providerLane.Valid {
		t.Errorf("provider_lane = %q; want NULL after reopen (stale lane from the done-era fetch)", s.providerLane.String)
	}
	if s.outcomeType.Valid {
		t.Errorf("outcome_type = %q; want NULL after reopen -- a done-era outcome_type on a pending row is the #655 stale-vs-NULL ambiguity", s.outcomeType.String)
	}
	if s.outcomeDetail.Valid {
		t.Errorf("outcome_detail = %q; want NULL after reopen", s.outcomeDetail.String)
	}
	if s.timingOutcome.Valid {
		t.Errorf("timing_outcome = %q; want NULL after reopen", s.timingOutcome.String)
	}
	if s.overrunMagnitude.Valid {
		t.Errorf("overrun_magnitude = %v; want NULL after reopen", s.overrunMagnitude.Float64)
	}
	if s.overrunRatio.Valid {
		t.Errorf("overrun_ratio = %v; want NULL after reopen", s.overrunRatio.Float64)
	}
	if s.evaluatedAt.Valid {
		t.Errorf("evaluated_at = %q; want NULL after reopen", s.evaluatedAt.String)
	}
}

// #960 / #477: an 'unavailable' row's identity is still corrected (the key
// must stay accurate) but the row itself must NOT be reopened. That key
// already exhausted its miss budget; queue.RecheckRetired is the designed
// revival path, and reopening here would just buy one fetch before
// RetireMiss re-retires it.
func TestRun_RekeyLeavesUnavailableRowRetired(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "unavailable", sr)
	if _, err := db.Exec(`UPDATE work_queue SET last_error = 'miss limit reached', miss_count = 15 WHERE id = ?`, wq); err != nil {
		t.Fatalf("stamp miss-limit sentinel: %v", err)
	}

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Changed != 1 || res.QueueUpdated != 1 {
		t.Fatalf("Result = %+v; want Changed=1 QueueUpdated=1", res)
	}

	wantKey := normalize.NormalizeKey("Alpha; Bravo")
	a, k, status := queueIdentity(t, db, wq)
	if a != "Alpha; Bravo" || k != wantKey {
		t.Errorf("work_queue identity = (%q,%q); want corrected to (Alpha; Bravo,%q) even though retired", a, k, wantKey)
	}
	if status != "unavailable" {
		t.Errorf("status = %q; want unavailable (must not be reopened -- RecheckRetired is the designed revival path)", status)
	}
	var lastError string
	var missCount int
	if err := db.QueryRow(`SELECT last_error, miss_count FROM work_queue WHERE id = ?`, wq).Scan(&lastError, &missCount); err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	if lastError != "miss limit reached" || missCount != 15 {
		t.Errorf("sentinel = (%q, miss_count=%d); want preserved (miss limit reached, 15)", lastError, missCount)
	}
}

// #960: a 'deferred' row on the re-key path keeps its status -- only a 'done'
// row is fabricated-status-free to reopen; a row that never completed has
// nothing to reopen.
func TestRun_RekeyDeferredRowKeepsStatus(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	wq := seedQueue(t, db, "AlphaBravo", "", "deferred", sr)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	if _, err := New(db, reader.read).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, _, status := queueIdentity(t, db, wq); status != "deferred" {
		t.Errorf("status = %q; want deferred (unchanged)", status)
	}
}

// #960: the album-artist-only path (!keyChanged) syncs display columns only.
// The artist_key is stable, so the cache lookup is unaffected and no
// re-fetch is warranted -- this is a deliberate decision, asserted here so a
// future change has to break this test to alter it.
func TestRun_AlbumArtistOnlyDoesNotResetSettleState(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	sr := seedScan(t, db, lib, "/m/1.mp3", "Alpha", "OldAA", "Song")
	wq := seedQueue(t, db, "Alpha", "OldAA", "done", sr)
	stampSettleState(t, db, wq)

	reader := fakeReader{"/m/1.mp3": {"Alpha", "New; AA"}}
	if _, err := New(db, reader.read).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	s := readSettleState(t, db, wq)
	if s.status != "done" {
		t.Errorf("status = %q; want done (album-artist-only never reopens)", s.status)
	}
	if !s.outcomeType.Valid || s.outcomeType.String != "lrc" {
		t.Errorf("outcome_type = %+v; want preserved 'lrc' (album-artist-only leaves settle state alone)", s.outcomeType)
	}
	if !s.providerLane.Valid || s.providerLane.String != "musixmatch" {
		t.Errorf("provider_lane = %+v; want preserved 'musixmatch'", s.providerLane)
	}
}

// #960 finding: mergeQueueRows' existing 'done'-survivor reopen only ever
// cleared status/attempts/next_attempt_at/last_error/completed_at, leaving a
// done-era outcome_type/provider_lane/timing_outcome attached to a row that
// is now 'pending'. The shared reopen helper must close this gap on the merge
// path too, not just the re-key path.
func TestRun_MergeClearsSettleStateOnReopenedSurvivor(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "pending", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "done", srGood)
	stampSettleState(t, db, wqGood)

	reader := fakeReader{
		"/m/1.mp3": {"Alpha; Bravo", ""},
		"/m/2.mp3": {"Alpha; Bravo", ""},
	}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueueMerged != 1 {
		t.Fatalf("Result = %+v; want QueueMerged=1", res)
	}

	s := readSettleState(t, db, wqGood)
	if s.status != "pending" {
		t.Fatalf("survivor status = %q; want pending (reopened)", s.status)
	}
	if s.outcomeType.Valid {
		t.Errorf("survivor outcome_type = %q; want NULL after merge-reopen", s.outcomeType.String)
	}
	if s.providerLane.Valid {
		t.Errorf("survivor provider_lane = %q; want NULL after merge-reopen", s.providerLane.String)
	}
	if s.outcomeDetail.Valid {
		t.Errorf("survivor outcome_detail = %q; want NULL after merge-reopen", s.outcomeDetail.String)
	}
	if s.timingOutcome.Valid {
		t.Errorf("survivor timing_outcome = %q; want NULL after merge-reopen", s.timingOutcome.String)
	}
	if s.overrunMagnitude.Valid || s.overrunRatio.Valid || s.evaluatedAt.Valid {
		t.Errorf("survivor overrun/evaluated columns still set (%v,%v,%v); want all NULL after merge-reopen",
			s.overrunMagnitude, s.overrunRatio, s.evaluatedAt)
	}
}
