package queue

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/models"
)

var adoptStamp = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// seedSettled inserts a scan_results row (library lib) with the identity keys
// of artist/title and returns its candidate, as ListUnlinkedSettled would.
func seedSettled(t *testing.T, dbh *sql.DB, lib int64, status, path, title string) AdoptCandidate {
	const artist = "A"
	t.Helper()
	ak, tk := IdentityKeys(models.Track{ArtistName: artist, TrackName: title})
	c := AdoptCandidate{FilePath: path, Outdir: filepath.Dir(path), Filename: "x.lrc", ArtistKey: ak, TitleKey: tk,
		Track: models.Track{ArtistName: artist, TrackName: title}}
	if err := dbh.QueryRow(`INSERT INTO scan_results (library_id, file_path, outdir, filename, artist, title, artist_key, title_key, status)
        VALUES (?, ?, ?, 'x.lrc', ?, ?, ?, ?, ?) RETURNING id`, lib, path, c.Outdir, artist, title, ak, tk, status).Scan(&c.ScanResultID); err != nil {
		t.Fatalf("seed scan row: %v", err)
	}
	return c
}

func seedLibrary(t *testing.T, dbh *sql.DB, path string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`, path, path).Scan(&id); err != nil {
		t.Fatalf("seed library: %v", err)
	}
	return id
}

func seedQueueRow(t *testing.T, dbh *sql.DB, c AdoptCandidate, status string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, status, outcome_type)
        VALUES (?, ?, ?, ?, ?, 'synced') RETURNING id`, c.Track.ArtistName, c.Track.TrackName, c.ArtistKey, c.TitleKey, status).Scan(&id); err != nil {
		t.Fatalf("seed queue row: %v", err)
	}
	return id
}

func countOf(t *testing.T, dbh *sql.DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := dbh.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func ids(cs []AdoptCandidate) []int64 {
	out := make([]int64, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ScanResultID)
	}
	return out
}

func sameIDs(got []int64, want ...int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestListUnlinkedSettledReturnsOnlyUnlinkedDoneRowsUnderThePrefix(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	other := seedLibrary(t, dbh, "/o")
	a1 := seedSettled(t, dbh, lib, "done", "/m/a/1.flac", "One")
	linked := seedSettled(t, dbh, lib, "done", "/m/a/2.flac", "Two")
	seedSettled(t, dbh, lib, "pending", "/m/a/3.flac", "Three")
	sibling := seedSettled(t, dbh, lib, "done", "/m/ab/4.flac", "Four")
	a5 := seedSettled(t, dbh, lib, "done", "/m/a/5.flac", "Five")
	seedSettled(t, dbh, other, "done", "/o/a/6.flac", "Six")
	mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
		seedQueueRow(t, dbh, linked, "done"), linked.ScanResultID)

	list := func(prefix string, after int64, limit int) []int64 {
		t.Helper()
		got, err := q.ListUnlinkedSettled(ctx, lib, prefix, after, limit)
		if err != nil {
			t.Fatalf("ListUnlinkedSettled(%q): %v", prefix, err)
		}
		return ids(got)
	}
	if got := list("", 0, 100); !sameIDs(got, a1.ScanResultID, sibling.ScanResultID, a5.ScanResultID) {
		t.Errorf("whole library = %v, want the unlinked done rows of this library only", got)
	}
	// A sibling directory sharing the prefix's characters is NOT under it.
	for _, prefix := range []string{"/m/a", "/m/a/", "/m/a//"} {
		if got := list(prefix, 0, 100); !sameIDs(got, a1.ScanResultID, a5.ScanResultID) {
			t.Errorf("prefix %q = %v, want /m/a/1 and /m/a/5 and not the sibling /m/ab", prefix, got)
		}
	}
	if got := list("/m/a/5.flac", 0, 100); !sameIDs(got, a5.ScanResultID) {
		t.Errorf("file prefix = %v, want the one file", got)
	}
	if got := list("/", 0, 100); !sameIDs(got, a1.ScanResultID, sibling.ScanResultID, a5.ScanResultID) {
		t.Errorf("root prefix = %v, want every row (a root must not match nothing)", got)
	}
	// Keyset paging walks the set in id order without repeats.
	first := list("", 0, 2)
	rest := list("", first[len(first)-1], 2)
	if !sameIDs(first, a1.ScanResultID, sibling.ScanResultID) || !sameIDs(rest, a5.ScanResultID) {
		t.Errorf("pages = %v then %v, want [a1 sibling] then [a5]", first, rest)
	}
}

func TestLinkSettledLinksOnlyADoneRow(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		status string
		want   AdoptResult
	}{
		{StatusDone, AdoptLinked},
		{StatusProcessing, AdoptNotSettled},
		{StatusPending, AdoptNotSettled},
		{StatusUnavailable, AdoptNotSettled},
		{StatusFailed, AdoptNotSettled},
		{StatusDeferred, AdoptNotSettled},
	} {
		t.Run(tc.status, func(t *testing.T) {
			dbh := openQueueTestDB(t)
			q := NewDBQueue(dbh)
			c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "done", "/m/a.flac", "One")
			rowID := seedQueueRow(t, dbh, c, tc.status)
			var recs []AdoptRecord
			got, err := q.LinkSettled(ctx, c, adoptStamp, func(r AdoptRecord) error { recs = append(recs, r); return nil })
			if err != nil || got != tc.want {
				t.Fatalf("LinkSettled = %v, %v; want %v", got, err, tc.want)
			}
			links := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, rowID)
			if wantLinked := tc.want == AdoptLinked; (links == 1) != wantLinked || (len(recs) == 1) != wantLinked {
				t.Errorf("links=%d records=%d, want a link and a record only for a done row", links, len(recs))
			}
		})
	}

	// A later pass links it once the row settles.
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "done", "/m/a.flac", "One")
	rowID := seedQueueRow(t, dbh, c, StatusUnavailable)
	if got, _ := q.LinkSettled(ctx, c, adoptStamp, nil); got != AdoptNotSettled {
		t.Fatalf("first pass = %v, want AdoptNotSettled", got)
	}
	mustExec(t, dbh, `UPDATE work_queue SET status = 'done' WHERE id = ?`, rowID)
	if got, err := q.LinkSettled(ctx, c, adoptStamp, nil); err != nil || got != AdoptLinked {
		t.Errorf("after the row settled = %v, %v; want AdoptLinked", got, err)
	}
}

func TestInsertSettledDoneWritesTheRowAndJunctionTogether(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	synced := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	text := seedSettled(t, dbh, lib, "done", "/m/b.flac", "Two")

	var rec AdoptRecord
	got, err := q.InsertSettledDone(ctx, synced, AdoptClass{OutcomeType: "synced", SyncTier: "line"}, adoptStamp, func(r AdoptRecord) error { rec = r; return nil })
	if err != nil || got != AdoptInserted {
		t.Fatalf("InsertSettledDone = %v, %v", got, err)
	}
	var status, outcome, tier, source string
	var lane, upstream, wordState sql.NullString
	var attempts int
	var completed, wordChecked sql.NullString
	err = dbh.QueryRow(`SELECT status, outcome_type, sync_tier, source_path, provider_lane, upstream, attempts, completed_at, word_timing_state, word_timing_checked_at
        FROM work_queue WHERE id = ?`, rec.WorkQueueID).Scan(&status, &outcome, &tier, &source, &lane, &upstream, &attempts, &completed, &wordState, &wordChecked)
	if err != nil {
		t.Fatal(err)
	}
	if status != "done" || outcome != "synced" || tier != "line" || source != "/m/a.flac" || lane.Valid || upstream.Valid || attempts != 0 {
		t.Errorf("row = %s/%s/%s/%s lane=%v upstream=%v attempts=%d", status, outcome, tier, source, lane, upstream, attempts)
	}
	stamp := formatTime(adoptStamp)
	if completed.String != stamp {
		t.Errorf("completed_at = %q, want the adoption time %q", completed.String, stamp)
	}
	// The word-recheck sweep's own cooldown is stamped; no verdict is recorded.
	if wordChecked.String != stamp || wordState.Valid {
		t.Errorf("word_timing_checked_at=%q word_timing_state=%v, want the stamp and NULL (no verdict reached)", wordChecked.String, wordState)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`, rec.WorkQueueID, synced.ScanResultID); n != 1 {
		t.Errorf("junction rows = %d, want 1", n)
	}
	if rec.ScanResultID != synced.ScanResultID || rec.Linked || rec.SourcePath != "/m/a.flac" {
		t.Errorf("record = %+v", rec)
	}

	// A non-synced outcome has no word-timing stamp.
	var tid int64
	if res, err := q.InsertSettledDone(ctx, text, AdoptClass{OutcomeType: "unsynced"}, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("insert unsynced = %v, %v", res, err)
	}
	if err := dbh.QueryRow(`SELECT id FROM work_queue WHERE artist_key = ? AND title_key = ?`, text.ArtistKey, text.TitleKey).Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue WHERE id = ? AND word_timing_checked_at IS NULL`, tid); n != 1 {
		t.Errorf("unsynced row carries a word-timing stamp")
	}
}

func TestInsertSettledDoneCollapsesARacingInsertIntoALink(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	first := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	second := seedSettled(t, dbh, lib, "done", "/m/a-copy.flac", "One")
	cls := AdoptClass{OutcomeType: "synced", SyncTier: "line"}
	if res, err := q.InsertSettledDone(ctx, first, cls, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("first = %v, %v", res, err)
	}
	if res, err := q.InsertSettledDone(ctx, second, cls, adoptStamp, nil); err != nil || res != AdoptLinked {
		t.Fatalf("second = %v, %v; want a link to the row that won the key", res, err)
	}
	if rows, links := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`), countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`); rows != 1 || links != 2 {
		t.Errorf("rows=%d links=%d, want one row linked to both files", rows, links)
	}

	// The conflict fallback obeys the same settled-only rule, and writes nothing.
	pend := seedSettled(t, dbh, lib, "done", "/m/p.flac", "Pending")
	seedQueueRow(t, dbh, pend, StatusPending)
	before := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`)
	if res, err := q.InsertSettledDone(ctx, pend, cls, adoptStamp, nil); err != nil || res != AdoptNotSettled {
		t.Fatalf("against a pending row = %v, %v; want AdoptNotSettled", res, err)
	}
	if after := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`); after != before {
		t.Errorf("links %d -> %d, want no link to a pending row", before, after)
	}
}

func TestAdoptBackupRunsBeforeCommitAndItsFailureLeavesTheDBUnchanged(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	dbh, err := dbtest.OpenAt(ctx, t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	// A second connection reads committed state only.
	other, err := dbtest.OpenAt(ctx, t, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	q := NewDBQueue(dbh)
	cls := AdoptClass{OutcomeType: "synced", SyncTier: "line"}
	lib := seedLibrary(t, dbh, "/m")

	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	visible := -1
	if _, err := q.InsertSettledDone(ctx, c, cls, adoptStamp, func(AdoptRecord) error {
		return other.QueryRow(`SELECT COUNT(*) FROM work_queue`).Scan(&visible)
	}); err != nil || visible != 0 {
		t.Fatalf("insert err=%v rows visible at backup time=%d, want 0 (record precedes commit)", err, visible)
	}

	boom := errors.New("disk full")
	ins := seedSettled(t, dbh, lib, "done", "/m/b.flac", "Two")
	_, err = q.InsertSettledDone(ctx, ins, cls, adoptStamp, func(AdoptRecord) error { return boom })
	if !errors.Is(err, ErrAdoptBackup) || !errors.Is(err, boom) {
		t.Fatalf("insert err = %v, want ErrAdoptBackup wrapping the cause", err)
	}
	lnk := seedSettled(t, dbh, lib, "done", "/m/c.flac", "Three")
	rowID := seedQueueRow(t, dbh, lnk, StatusDone)
	_, err = q.LinkSettled(ctx, lnk, adoptStamp, func(AdoptRecord) error { return boom })
	if !errors.Is(err, ErrAdoptBackup) {
		t.Fatalf("link err = %v, want ErrAdoptBackup", err)
	}
	// Observed from the other connection: only the first adoption exists.
	if rows := countOf(t, other, `SELECT COUNT(*) FROM work_queue`); rows != 2 {
		t.Errorf("work_queue rows = %d, want 2 (first insert + the seeded row)", rows)
	}
	if n := countOf(t, other, `SELECT COUNT(*) FROM work_queue_scan_results WHERE scan_result_id IN (?, ?)`, ins.ScanResultID, lnk.ScanResultID); n != 0 {
		t.Errorf("links after failed backups = %d, want none (rowID %d)", n, rowID)
	}
}

func TestAdoptedRowIsNeverDequeuedAndEnqueueKeepsItDone(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "done", "/m/a.flac", "One")
	if res, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "synced", SyncTier: "line"}, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("insert = %v, %v", res, err)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue err = %v, want sql.ErrNoRows (an adopted done row is not work)", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: c.Track, Outdir: c.Outdir, Filename: c.Filename, SourcePath: c.FilePath}, PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	var status, outcome string
	if err := dbh.QueryRow(`SELECT status, outcome_type FROM work_queue WHERE artist_key = ? AND title_key = ?`, c.ArtistKey, c.TitleKey).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	if status != "done" || outcome != "synced" {
		t.Errorf("after Enqueue status=%q outcome=%q, want the row kept done/synced", status, outcome)
	}
}

func TestLinkSettledAlreadyLinkedWritesNoBackup(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "done", "/m/a.flac", "One")
	rowID := seedQueueRow(t, dbh, c, StatusDone)

	var recs []AdoptRecord
	backup := func(r AdoptRecord) error { recs = append(recs, r); return nil }
	if got, err := q.LinkSettled(ctx, c, adoptStamp, backup); err != nil || got != AdoptLinked {
		t.Fatalf("first link = %v, %v; want AdoptLinked", got, err)
	}
	if len(recs) != 1 {
		t.Fatalf("backup calls after first link = %d, want 1", len(recs))
	}
	if got, err := q.LinkSettled(ctx, c, adoptStamp, backup); err != nil || got != AdoptAlreadyLinked {
		t.Fatalf("second link = %v, %v; want AdoptAlreadyLinked", got, err)
	}
	if len(recs) != 1 {
		t.Errorf("backup calls after already-linked = %d, want still 1", len(recs))
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ? AND scan_result_id = ?`, rowID, c.ScanResultID); n != 1 {
		t.Errorf("junction rows = %d, want exactly 1", n)
	}
}

func TestInsertSettledDoneConflictFallbackAlreadyLinked(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "done", "/m/a.flac", "One")
	rowID := seedQueueRow(t, dbh, c, StatusDone)
	if _, err := dbh.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, rowID, c.ScanResultID); err != nil {
		t.Fatal(err)
	}
	calls := 0
	got, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "synced"}, adoptStamp, func(AdoptRecord) error { calls++; return nil })
	if err != nil || got != AdoptAlreadyLinked {
		t.Fatalf("InsertSettledDone = %v, %v; want AdoptAlreadyLinked", got, err)
	}
	if calls != 0 {
		t.Errorf("backup calls = %d, want 0", calls)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE scan_result_id = ?`, c.ScanResultID); n != 1 {
		t.Errorf("junction rows = %d, want 1", n)
	}
}

func TestAdoptWritersReturnTheContextErrorAndWriteNothing(t *testing.T) {
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	seedQueueRow(t, dbh, c, StatusDone)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := q.ListUnlinkedSettled(ctx, lib, "", 0, 10); !errors.Is(err, context.Canceled) {
		t.Errorf("list err = %v, want context.Canceled", err)
	}
	if res, err := q.LinkSettled(ctx, c, adoptStamp, nil); !errors.Is(err, context.Canceled) || res != AdoptNoRow {
		t.Errorf("link = %v, %v, want AdoptNoRow and context.Canceled", res, err)
	}
	if res, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "synced"}, adoptStamp, nil); !errors.Is(err, context.Canceled) || res != AdoptNoRow {
		t.Errorf("insert = %v, %v, want AdoptNoRow and context.Canceled", res, err)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`); n != 0 {
		t.Errorf("links after canceled writers = %d, want 0", n)
	}
}

func TestLinkSettledWithNoQueueRowWritesNothing(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	called := false
	res, err := q.LinkSettled(ctx, c, adoptStamp, func(AdoptRecord) error { called = true; return nil })
	if err != nil || res != AdoptNoRow || called {
		t.Fatalf("link = %v, %v, backup called=%v, want AdoptNoRow, nil, false", res, err, called)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`); n != 0 {
		t.Errorf("links = %d, want 0", n)
	}
}

func TestListUnlinkedSettledDerivesMissingIdentityKeys(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	if _, err := dbh.Exec(`UPDATE scan_results SET artist_key = '', title_key = '' WHERE id = ?`, c.ScanResultID); err != nil {
		t.Fatal(err)
	}
	got, err := q.ListUnlinkedSettled(ctx, lib, "", 0, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("list = %v, %v, want one candidate", got, err)
	}
	if got[0].ArtistKey != c.ArtistKey || got[0].TitleKey != c.TitleKey {
		t.Errorf("keys = %q/%q, want derived %q/%q", got[0].ArtistKey, got[0].TitleKey, c.ArtistKey, c.TitleKey)
	}
}

func TestAdoptWritersSurfaceAJunctionOrInsertFailure(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	seedQueueRow(t, dbh, c, StatusDone)
	// A scan_results id that does not exist violates the junction foreign key.
	orphan := c
	orphan.ScanResultID = 987654
	if res, err := q.LinkSettled(ctx, orphan, adoptStamp, nil); err == nil || res != AdoptNoRow {
		t.Errorf("link orphan = %v, %v, want AdoptNoRow and an error", res, err)
	}
	fresh := seedSettled(t, dbh, lib, "done", "/m/b.flac", "Two")
	fresh.ScanResultID = 987655
	if res, err := q.InsertSettledDone(ctx, fresh, AdoptClass{OutcomeType: "synced"}, adoptStamp, nil); err == nil || res != AdoptNoRow {
		t.Errorf("insert orphan = %v, %v, want AdoptNoRow and an error", res, err)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 1 {
		t.Errorf("work_queue rows = %d, want 1 (only the seeded row)", n)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results`); n != 0 {
		t.Errorf("links = %d, want 0", n)
	}
}

func TestAdoptInsertFailsWhenTheJunctionWriteIsRefused(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	c := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	if _, err := dbh.Exec(`CREATE TEMP TRIGGER refuse_link BEFORE INSERT ON work_queue_scan_results
        BEGIN SELECT RAISE(ABORT, 'link refused'); END`); err != nil {
		t.Fatal(err)
	}
	called := false
	res, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "synced"}, adoptStamp, func(AdoptRecord) error { called = true; return nil })
	if err == nil || res != AdoptNoRow || called {
		t.Fatalf("insert = %v, %v, backup called=%v, want AdoptNoRow, an error, no backup", res, err, called)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 0 {
		t.Errorf("work_queue rows = %d, want 0 (the insert rolled back with the refused link)", n)
	}
}

// adoptedState reads the columns the #1586 reopen decides on and writes.
func adoptedState(t *testing.T, dbh *sql.DB, c AdoptCandidate) (status string, adopted bool, outcome sql.NullString) {
	t.Helper()
	if err := dbh.QueryRow(`SELECT status, adopted_at IS NOT NULL, outcome_type FROM work_queue WHERE artist_key = ? AND title_key = ?`,
		c.ArtistKey, c.TitleKey).Scan(&status, &adopted, &outcome); err != nil {
		t.Fatal(err)
	}
	return status, adopted, outcome
}

func scanInputsFor(c AdoptCandidate, reopen bool) models.Inputs {
	return models.Inputs{Track: c.Track, Outdir: c.Outdir, Filename: c.Filename, SourcePath: c.FilePath,
		ScanResultID: c.ScanResultID, FromScan: true, ReopenAdopted: reopen}
}

// Only an insert-adoption stamps adopted_at; a link to a row the worker
// completed does not, and a reopen-flag enqueue keeps that row done.
func TestOnlyAnInsertAdoptionIsMarkedAdopted(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	ins := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	if res, err := q.InsertSettledDone(ctx, ins, AdoptClass{OutcomeType: "unsynced"}, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("insert = %v, %v", res, err)
	}
	var stamp string
	if err := dbh.QueryRow(`SELECT adopted_at FROM work_queue WHERE title_key = ?`, ins.TitleKey).Scan(&stamp); err != nil || stamp != formatTime(adoptStamp) {
		t.Errorf("inserted row adopted_at = %q, %v; want the adoption time", stamp, err)
	}
	link := seedSettled(t, dbh, lib, "done", "/m/b.flac", "Two")
	seedQueueRow(t, dbh, link, "done")
	if res, err := q.LinkSettled(ctx, link, adoptStamp, nil); err != nil || res != AdoptLinked {
		t.Fatalf("link = %v, %v", res, err)
	}
	if _, err := q.Enqueue(ctx, scanInputsFor(link, true), PriorityScan); err != nil {
		t.Fatal(err)
	}
	if status, adopted, _ := adoptedState(t, dbh, link); status != "done" || adopted {
		t.Errorf("linked worker row = %s adopted=%v, want done and unmarked", status, adopted)
	}
}

// claimAndSettle puts the row through one worker claim in the dequeue mode set
// selects, then settles it back to done as an upgrade trip does.
func claimAndSettle(set func(*DBQueue)) func(*testing.T, *DBQueue, *sql.DB, int64) {
	return func(t *testing.T, q *DBQueue, dbh *sql.DB, id int64) {
		t.Helper()
		set(q)
		if _, err := dbh.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
		if item, err := q.Dequeue(context.Background()); err != nil || item.ID != id {
			t.Fatalf("Dequeue = %d, %v", item.ID, err)
		}
		if _, err := dbh.Exec(`UPDATE work_queue SET status = 'done' WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}
}

// A scan enqueue carrying ReopenAdopted reopens the adopted row of its own
// file; every guard in reopenAdoptedForScan keeps the row done instead.
func TestEnqueueReopensAnAdoptedRowOnlyUnderItsGuards(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		prep   func(t *testing.T, q *DBQueue, dbh *sql.DB, id int64)
		inputs func(c AdoptCandidate) models.Inputs
		reopen bool
	}{
		"reopen flag":        {reopen: true},
		"ordinary scan":      {inputs: func(c AdoptCandidate) models.Inputs { return scanInputsFor(c, false) }},
		"not a scan enqueue": {inputs: func(c AdoptCandidate) models.Inputs { in := scanInputsFor(c, true); in.FromScan = false; return in }},
		"another copy of the song": {inputs: func(c AdoptCandidate) models.Inputs {
			in := scanInputsFor(c, true)
			in.SourcePath, in.ScanResultID = "/m/other/a.flac", 0
			return in
		}},
		"hand edit": {prep: func(t *testing.T, q *DBQueue, _ *sql.DB, id int64) {
			if err := q.SetLyricEdit(ctx, id, 100); err != nil {
				t.Fatal(err)
			}
		}},
		"manual instrumental mark": {prep: func(t *testing.T, q *DBQueue, _ *sql.DB, id int64) {
			if _, err := q.MarkManualInstrumental(ctx, id); err != nil {
				t.Fatal(err)
			}
		}},
		// A trip a worker claimed and settled: fetched, so not reopened. One
		// case per claim statement (batched, per-item random, deterministic).
		"claimed, batched":       {prep: claimAndSettle(func(*DBQueue) {})},
		"claimed, random":        {prep: claimAndSettle(func(q *DBQueue) { q.SetBatchSize(0) })},
		"claimed, deterministic": {prep: claimAndSettle(func(q *DBQueue) { q.SetRandomized(false) })},
	} {
		t.Run(name, func(t *testing.T) {
			dbh := openQueueTestDB(t)
			q := NewDBQueue(dbh)
			c := seedSettled(t, dbh, seedLibrary(t, dbh, "/m"), "pending", "/m/a.flac", "One")
			if res, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "unsynced"}, adoptStamp, nil); err != nil || res != AdoptInserted {
				t.Fatalf("insert = %v, %v", res, err)
			}
			id := int64(countOf(t, dbh, `SELECT id FROM work_queue`))
			if tc.prep != nil {
				tc.prep(t, q, dbh, id)
			}
			in := scanInputsFor(c, true)
			if tc.inputs != nil {
				in = tc.inputs(c)
			}
			item, err := q.Enqueue(ctx, in, PriorityScan)
			if err != nil {
				t.Fatal(err)
			}
			status, adopted, outcome := adoptedState(t, dbh, c)
			if !tc.reopen {
				if status != "done" || item.Status != StatusDone {
					t.Errorf("status = %s (returned %s), want the row kept done", status, item.Status)
				}
				return
			}
			if status != "pending" || item.Status != StatusPending || adopted || outcome.Valid {
				t.Errorf("status=%s returned=%s adopted=%v outcome=%v, want a pending row with the mark and outcome cleared", status, item.Status, adopted, outcome)
			}
			// The worker's completion makes it an ordinary row: never reopened again.
			if _, err := q.Dequeue(ctx); err != nil {
				t.Fatal(err)
			}
			if err := q.Complete(ctx, id); err != nil {
				t.Fatal(err)
			}
			if _, err := q.Enqueue(ctx, scanInputsFor(c, true), PriorityScan); err != nil {
				t.Fatal(err)
			}
			if status, _, _ := adoptedState(t, dbh, c); status != "done" {
				t.Errorf("after the worker completed it, a reopen flag set status %s, want done", status)
			}
		})
	}
}

// adoptOne adopts a file with an unsynced lyric file and returns its candidate
// and queue row id.
func adoptOne(t *testing.T, q *DBQueue, dbh *sql.DB, lib int64, path string) (AdoptCandidate, int64) {
	t.Helper()
	c := seedSettled(t, dbh, lib, "processing", path, "One")
	if res, err := q.InsertSettledDone(context.Background(), c, AdoptClass{OutcomeType: "unsynced"}, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("insert = %v, %v", res, err)
	}
	return c, int64(countOf(t, dbh, `SELECT id FROM work_queue WHERE title_key = ?`, c.TitleKey))
}

// An adopted row the upgrade sweep has armed is no longer done but still
// carries the mark; a reopen-flag enqueue leaves it in its trip.
func TestReopenFlagLeavesAnArmedUpgradeTripAlone(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c, id := adoptOne(t, q, dbh, seedLibrary(t, dbh, "/m"), "/m/a.flac")
	if armed, err := q.MarkUpgradeQueued(ctx, []int64{id}, adoptStamp.Add(time.Hour)); err != nil || len(armed) != 1 {
		t.Fatalf("arm = %v, %v; want the adopted row armed", armed, err)
	}
	item, err := q.Enqueue(ctx, scanInputsFor(c, true), PriorityScan)
	if err != nil {
		t.Fatalf("enqueue onto an armed trip: %v", err)
	}
	status, adopted, outcome := adoptedState(t, dbh, c)
	if status != "pending" || !item.UpgradeQueued || !adopted || outcome.String != "unsynced" {
		t.Errorf("status=%s armed=%v adopted=%v outcome=%q, want the trip untouched", status, item.UpgradeQueued, adopted, outcome.String)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM scan_results WHERE status = 'processing'`); n != 1 {
		t.Errorf("scan row was settled against a row that is in flight")
	}
}

// The mark follows the row to the same-stem file that replaced its source:
// the flag reopens it at the new path, and not at the vanished one.
func TestReopenFlagFollowsARepointedAdoptedRow(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	lib := seedLibrary(t, dbh, "/m")
	old, _ := adoptOne(t, q, dbh, lib, "/m/a.mp3")
	q.stat = func(p string) (fs.FileInfo, error) {
		if p == old.FilePath {
			return nil, fs.ErrNotExist
		}
		return nil, nil
	}
	moved := seedSettled(t, dbh, lib, "done", "/m/a.flac", "One")
	if ok, err := q.RepointGoneSource(ctx, models.Inputs{Track: moved.Track, SourcePath: moved.FilePath, LibraryID: lib}); err != nil || !ok {
		t.Fatalf("repoint = %v, %v; want the row moved", ok, err)
	}
	gone := scanInputsFor(old, true)
	gone.ScanResultID = 0 // the move removed the vanished file's scan row
	if _, err := q.Enqueue(ctx, gone, PriorityScan); err != nil {
		t.Fatal(err)
	}
	if status, adopted, _ := adoptedState(t, dbh, old); status != "done" || !adopted {
		t.Fatalf("enqueue at the vanished path: status=%s adopted=%v, want done and still marked", status, adopted)
	}
	if _, err := q.Enqueue(ctx, scanInputsFor(moved, true), PriorityScan); err != nil {
		t.Fatal(err)
	}
	if status, adopted, _ := adoptedState(t, dbh, old); status != "pending" || adopted {
		t.Errorf("enqueue at the new path: status=%s adopted=%v, want pending with the mark spent", status, adopted)
	}
}

// A scan enqueue that lands on a terminal row settles the scan row it was
// reserved for, unless another live queue row still owns that scan row.
func TestScanEnqueueOntoATerminalRowSettlesItsScanRow(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		status, want string
		sibling      bool
		webhook      bool
	}{
		"done":                    {status: "done", want: "done"},
		"unavailable":             {status: "unavailable", want: "done"},
		"live sibling owns it":    {status: "done", want: "processing", sibling: true},
		"not a scan enqueue":      {status: "done", want: "processing", webhook: true},
		"row that is not settled": {status: "failed", want: "processing"},
	} {
		t.Run(name, func(t *testing.T) {
			dbh := openQueueTestDB(t)
			q := NewDBQueue(dbh)
			lib := seedLibrary(t, dbh, "/m")
			c := seedSettled(t, dbh, lib, "processing", "/m/a.flac", "One")
			seedQueueRow(t, dbh, c, tc.status)
			if tc.sibling {
				mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`,
					seedQueueRow(t, dbh, seedSettled(t, dbh, lib, "done", "/m/b.flac", "Two"), "pending"), c.ScanResultID)
			}
			in := scanInputsFor(c, false)
			in.FromScan = !tc.webhook
			if _, err := q.Enqueue(ctx, in, PriorityScan); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := dbh.QueryRow(`SELECT status FROM scan_results WHERE id = ?`, c.ScanResultID).Scan(&got); err != nil || got != tc.want {
				t.Errorf("scan row = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}
