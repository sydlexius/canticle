package identityrepair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	dbpkg "github.com/doxazo-net/canticle/internal/db"
	"github.com/doxazo-net/canticle/internal/normalize"
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
	res, err := db.Exec(
		`INSERT INTO work_queue (artist, title, album_artist, artist_key, title_key, status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		artist, title, albumArtist, normalize.NormalizeKey(artist), normalize.NormalizeKey(title), status)
	if err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := db.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, scanID); err != nil {
		t.Fatalf("seed junction: %v", err)
	}
	return id
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
// the corrected key; the more-advanced status is preserved and the merged row
// is deleted.
func TestRun_MergeOnConflict(t *testing.T) {
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
	// The surviving row is the good one, still 'done'.
	if _, _, status := queueIdentity(t, db, wqGood); status != "done" {
		t.Errorf("surviving work_queue status = %q; want done", status)
	}
	if err := db.QueryRow(`SELECT 1 FROM work_queue WHERE id = ?`, wqBad).Scan(new(int)); err != sql.ErrNoRows {
		t.Errorf("bad work_queue row still present; want deleted (err=%v)", err)
	}
	// Both scan_results now link to the survivor.
	var links int
	if err := db.QueryRow(`SELECT count(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, wqGood).Scan(&links); err != nil {
		t.Fatalf("count junction: %v", err)
	}
	if links != 2 {
		t.Errorf("survivor junction links = %d; want 2", links)
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

// On merge, when the old-key row holds the more-advanced status, the surviving
// row is promoted so completed work is not downgraded.
func TestRun_MergePromotesStatus(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "done", srBad)                 // old-key row: done
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "pending", srGood) // keeper: pending

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	res, err := New(db, reader.read).Run(context.Background(), Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.QueueMerged != 1 {
		t.Fatalf("Result = %+v; want QueueMerged=1", res)
	}
	if _, _, status := queueIdentity(t, db, wqGood); status != "done" {
		t.Errorf("keeper status = %q; want promoted to done", status)
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

// A merge promotes a pending keeper to 'failed' when the dropped row was failed,
// exercising the mid-rank status path.
func TestRun_MergeFailedIntoPending(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	srBad := seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")
	srGood := seedScan(t, db, lib, "/m/2.mp3", "Alpha; Bravo", "", "Song")
	seedQueue(t, db, "AlphaBravo", "", "failed", srBad)
	wqGood := seedQueue(t, db, "Alpha; Bravo", "", "pending", srGood)

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}, "/m/2.mp3": {"Alpha; Bravo", ""}}
	if _, err := New(db, reader.read).Run(context.Background(), Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, _, status := queueIdentity(t, db, wqGood); status != "failed" {
		t.Errorf("keeper status = %q; want promoted to failed", status)
	}
}

// A Report error aborts the run.
func TestRun_ReportError(t *testing.T) {
	db := openDB(t)
	lib := seedLibrary(t, db)
	seedScan(t, db, lib, "/m/1.mp3", "AlphaBravo", "", "Song")

	reader := fakeReader{"/m/1.mp3": {"Alpha; Bravo", ""}}
	wantErr := errReport
	_, err := New(db, reader.read).Run(context.Background(), Options{
		Report: func(Change) error { return wantErr },
	})
	if err != wantErr {
		t.Fatalf("Run err = %v; want %v", err, wantErr)
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
