package titlerepair

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/queue"
)

// Every artist, title and album below is invented.
const (
	splitRelease = "Low Tide (Instrumental)"
	splitSung    = "Low Tide"
)

// splitDB is a database with one library, and helpers to seed copies and rows.
type splitDB struct {
	t *testing.T
	d *sql.DB
}

func newSplitDB(t *testing.T) splitDB {
	t.Helper()
	d := dbtest.Open(t)
	if _, err := d.Exec(`INSERT INTO libraries (id, path, name) VALUES (1, '/lib', 'lib')`); err != nil {
		t.Fatal(err)
	}
	return splitDB{t, d}
}

// id runs one statement that returns an id.
func (s splitDB) id(query string, args ...any) int64 {
	s.t.Helper()
	var id int64
	if err := s.d.QueryRow(query, args...).Scan(&id); err != nil {
		s.t.Fatalf("%s: %v", query, err)
	}
	return id
}

// copyOf seeds the scan result of one file of title on album, in folder dir.
func (s splitDB) copyOf(title, album, dir, status string) int64 {
	return s.id(`INSERT INTO scan_results (library_id, file_path, artist, title, album, artist_key, title_key, outdir, filename, status)
	    VALUES (1, ?, 'Tin Lantern Band', ?, ?, 'tin lantern band', lower(?), ?, 'a.flac', ?) RETURNING id`,
		dir+"/"+title+".flac", title, album, title, dir, status)
}

// row seeds a legacy row (an empty edition) for title on album in dir, linked
// to every scan result in links; the first is its scan_result_id too.
func (s splitDB) row(title, album, dir, status, outputs string, links ...int64) int64 {
	var col any
	if len(links) > 0 {
		col = links[0]
	}
	id := s.id(`INSERT INTO work_queue (artist, title, album, artist_key, title_key, outdir, filename, source_path,
	        status, output_paths, scan_result_id, outcome_type, provider_lane, lyric_edited_at)
	    VALUES ('Tin Lantern Band', ?, ?, 'tin lantern band', lower(?), ?, 'a.flac', ?, ?, ?, ?, 'synced', 'musixmatch',
	        '2026-01-02T03:04:05Z') RETURNING id`, title, album, title, dir, dir+"/a.flac", status, outputs, col)
	for _, l := range links {
		if _, err := s.d.Exec(`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, l); err != nil {
			s.t.Fatal(err)
		}
	}
	return id
}

// state is the database as the split sees it: each row's edition, status, edit
// mark and scan_result_id, each link, and each scan result's status.
func (s splitDB) state() string {
	s.t.Helper()
	var b strings.Builder
	for _, q := range []string{
		`SELECT id || ':' || edition || ':' || status || ':' || COALESCE(lyric_edited_at, '-') || ':' || COALESCE(scan_result_id, '-') FROM work_queue ORDER BY id`,
		`SELECT work_queue_id || '>' || scan_result_id FROM work_queue_scan_results ORDER BY 1`,
		`SELECT id || '=' || status FROM scan_results ORDER BY id`,
	} {
		rows, err := s.d.Query(q)
		if err != nil {
			s.t.Fatal(err)
		}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				s.t.Fatal(err)
			}
			b.WriteString(v + " ")
		}
		_ = rows.Close()
		b.WriteString("| ")
	}
	return b.String()
}

// split runs Split; apply records into recs.
func (s splitDB) split(apply bool, recs *[]SplitRecord) SplitResult {
	s.t.Helper()
	o := SplitOptions{Apply: apply}
	if apply {
		o.Report = func(r SplitRecord) error { *recs = append(*recs, r); return nil }
	}
	res, err := New(s.d, nil).Split(context.Background(), o)
	if err != nil {
		s.t.Fatalf("Split: %v", err)
	}
	if res.Rows != res.Split+res.InFlight+res.BothFolders+res.Collision+res.Errors {
		s.t.Errorf("the split counts do not add up: %+v", res)
	}
	return res
}

// A legacy row naming the instrumental copy, linked to its sung twin too: the
// dry run counts it and changes nothing; --yes records it first, sets its
// edition, and returns the twin's scan result to pending. The row keeps its
// status and its hand edit, and its own copy stays linked and as it was.
func TestSplitReKeysAReleaseRowAndDetachesItsSungTwin(t *testing.T) {
	s := newSplitDB(t)
	inst := s.copyOf("Quiet Shelf", splitRelease, "/lib/inst", "processing")
	sung := s.copyOf("Quiet Shelf", splitSung, "/lib/sung", "processing")
	id := s.row("Quiet Shelf", splitRelease, "/lib/inst", "done", "", inst, sung)
	other := s.row("Paper Lighthouse", splitSung, "/lib/sung", "done", "", s.copyOf("Paper Lighthouse", splitSung, "/lib/sung", "done"))

	before := s.state()
	if res := s.split(false, nil); res != (SplitResult{Rows: 1, Split: 1, ReKeyed: 1, Detached: 1}) {
		t.Errorf("dry run = %+v, want one row split, re-keyed, one scan result detached", res)
	}
	if after := s.state(); after != before {
		t.Errorf("the dry run changed the database:\n%s\n%s", before, after)
	}

	var recs []SplitRecord
	if res := s.split(true, &recs); res != (SplitResult{Rows: 1, Split: 1, ReKeyed: 1, Detached: 1, Pending: 1}) {
		t.Errorf("apply = %+v, want one row split and one scan result returned to pending", res)
	}
	want := fmt.Sprintf("%d:instrumental:done:2026-01-02T03:04:05Z:%d %d::done:2026-01-02T03:04:05Z:%d | %d>%d %d>%d | %d=processing %d=pending %d=done | ",
		id, inst, other, sung+1, id, inst, other, sung+1, inst, sung, sung+1)
	if got := s.state(); got != want {
		t.Errorf("after apply:\n got %s\nwant %s", got, want)
	}
	if len(recs) != 1 || recs[0].Op != splitOp || recs[0].WorkItemID != id || recs[0].Edition != queue.EditionInstrumental ||
		len(recs[0].Detached) != 1 || recs[0].Detached[0] != (Detached{sung, "processing"}) || recs[0].ScanResultID != 0 {
		t.Errorf("records = %+v", recs)
	}
	if res := s.split(true, &recs); res != (SplitResult{}) || len(recs) != 1 {
		t.Errorf("a rerun = %+v with %d records, want nothing to do", res, len(recs))
	}
}

// A legacy row naming the sung copy: the instrumental copy's scan result is
// detached, its scan_result_id too when it named that copy, and the row keeps
// its empty edition.
func TestSplitDetachesTheReleaseCopyFromASungRow(t *testing.T) {
	s := newSplitDB(t)
	inst := s.copyOf("Quiet Shelf", splitRelease, "/lib/inst", "processing")
	sung := s.copyOf("Quiet Shelf", splitSung, "/lib/sung", "done")
	id := s.row("Quiet Shelf", splitSung, "/lib/sung", "done", `[{"outdir":"/lib/sung","filename":"a.flac"}]`, inst, sung)

	var recs []SplitRecord
	if res := s.split(true, &recs); res != (SplitResult{Rows: 1, Split: 1, Detached: 1, Pending: 1}) {
		t.Errorf("apply = %+v, want one row split with no re-key", res)
	}
	want := fmt.Sprintf("%d::done:2026-01-02T03:04:05Z:- | %d>%d | %d=pending %d=done | ", id, id, sung, inst, sung)
	if got := s.state(); got != want {
		t.Errorf("after apply:\n got %s\nwant %s", got, want)
	}
	if len(recs) != 1 || recs[0].Edition != "" || recs[0].ScanResultID != inst {
		t.Errorf("records = %+v, want the row's old scan_result_id and no edition", recs)
	}
}

// The skips, each counted and none changed: a row with an output in the other
// edition's folder, a re-key that would collide with an existing instrumental
// row, and a row in flight. A sung row with no twin is not counted at all, and
// a twin another row still links keeps its status.
func TestSplitSkips(t *testing.T) {
	s := newSplitDB(t)
	both := s.copyOf("Glass Harbor", splitSung, "/lib/sung", "processing")
	s.row("Glass Harbor", splitRelease, "/lib/inst", "done",
		`[{"outdir":"/lib/inst","filename":"a.flac"},{"outdir":"/lib/sung","filename":"a.flac"}]`,
		s.copyOf("Glass Harbor", splitRelease, "/lib/inst", "processing"), both)
	s.row("Paper Lighthouse", splitRelease, "/lib/inst", "done", "", s.copyOf("Paper Lighthouse", splitRelease, "/lib/inst", "done"))
	if _, err := s.d.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, edition, status)
	    VALUES ('Tin Lantern Band', 'Paper Lighthouse', 'tin lantern band', 'paper lighthouse', 'instrumental', 'pending')`); err != nil {
		t.Fatal(err)
	}
	s.row("Copper Finch", splitRelease, "/lib/inst", "processing", "", s.copyOf("Copper Finch", splitRelease, "/lib/inst", "processing"))
	s.row("Wren Song", splitSung, "/lib/sung", "done", "", s.copyOf("Wren Song", splitSung, "/lib/sung", "done"))
	shared := s.copyOf("Tin Bell", splitRelease, "/lib/inst", "processing")
	s.row("Tin Bell", splitSung, "/lib/sung", "done", "", s.copyOf("Tin Bell", splitSung, "/lib/sung", "done"), shared)
	// Its own scan_result_id is another copy: the junction alone links the twin.
	s.row("Tin Bell 2", splitRelease, "/lib/inst", "processing", "", s.copyOf("Tin Bell 2", splitRelease, "/lib/inst", "processing"), shared)

	before := s.state()
	var recs []SplitRecord
	res := s.split(true, &recs)
	if want := (SplitResult{Rows: 5, Split: 1, Detached: 1, InFlight: 2, BothFolders: 1, Collision: 1}); res != want {
		t.Errorf("apply = %+v, want %+v", res, want)
	}
	if after := s.state(); strings.Count(after, "=pending") != 0 || strings.Count(after, ":instrumental:") != 1 {
		t.Errorf("a skipped row changed, or the shared twin went pending:\n%s\n%s", before, after)
	}
}

// The record is written before the row changes: when it cannot be written,
// nothing changes and the row is counted an error.
func TestSplitChangesNothingWhenTheRecordFails(t *testing.T) {
	s := newSplitDB(t)
	inst := s.copyOf("Quiet Shelf", splitRelease, "/lib/inst", "processing")
	s.row("Quiet Shelf", splitRelease, "/lib/inst", "done", "", inst, s.copyOf("Quiet Shelf", splitSung, "/lib/sung", "processing"))
	before := s.state()
	res, err := New(s.d, nil).Split(context.Background(), SplitOptions{Apply: true,
		Report: func(SplitRecord) error { return errors.New("disk full") }})
	if err != nil || res != (SplitResult{Rows: 1, Errors: 1}) {
		t.Errorf("Split = %+v, %v; want one error", res, err)
	}
	if after := s.state(); after != before {
		t.Errorf("a row changed with no record:\n%s\n%s", before, after)
	}
	if _, err := New(s.d, nil).Split(context.Background(), SplitOptions{Apply: true}); err == nil {
		t.Error("Apply with no Report was accepted")
	}
}

// The writes refuse a row a worker holds in the statement itself, not only in
// the plan read before it, and changing no row is an error: a split that runs
// outside an IMMEDIATE transaction cannot re-key or detach a claimed row.
func TestSplitWritesRefuseAHeldRow(t *testing.T) {
	s := newSplitDB(t)
	sung := s.copyOf(splitSung, "Low Tide", "/lib/sung", "processing")
	id := s.row(splitRelease, "Low Tide (Instrumental)", "/lib/inst", "processing", "", sung)
	before := s.state()
	ctx := context.Background()
	if err := reKey(ctx, s.d, id); err == nil {
		t.Error("reKey changed a row a worker holds")
	}
	if err := detachColumn(ctx, s.d, id); err == nil {
		t.Error("detachColumn changed a row a worker holds")
	}
	if after := s.state(); after != before {
		t.Errorf("a held row changed:\nbefore %s\nafter  %s", before, after)
	}
	if _, err := s.d.Exec(`UPDATE work_queue SET status = 'done' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := reKey(ctx, s.d, id); err != nil {
		t.Errorf("reKey of a done row: %v", err)
	}
	if err := reKey(ctx, s.d, id); err == nil {
		t.Error("a second reKey of the same row reported no error")
	}
	if err := detachColumn(ctx, s.d, id); err != nil {
		t.Errorf("detachColumn of a done row: %v", err)
	}
}
