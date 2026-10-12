package queue

import (
	"context"
	"database/sql"
	"errors"
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
