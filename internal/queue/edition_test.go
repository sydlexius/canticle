package queue

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/models"
)

// albumEditionRule stands in for the album-level verdict #1562 wires: a copy on
// an album titled "... (Instrumental)" is the instrumental edition.
func albumEditionRule(t models.Track) string {
	if strings.HasSuffix(t.AlbumName, "(Instrumental)") {
		return EditionInstrumental
	}
	return EditionNone
}

// editionInputs is one copy of the track on album, with its own folder.
func editionInputs(album string) models.Inputs {
	dir := "/lib/" + album
	return models.Inputs{
		Track:      models.Track{ArtistName: "Shore", TrackName: "Quiet Shelf", AlbumName: album},
		Outdir:     dir,
		Filename:   "01 Quiet Shelf.lrc",
		SourcePath: dir + "/01 Quiet Shelf.flac",
	}
}

func newEditionQueue(t *testing.T, rule EditionRule) (*DBQueue, *sql.DB) {
	t.Helper()
	dbh := dbtest.Open(t)
	q := NewDBQueue(dbh)
	q.now = func() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }
	q.SetEditionRule(rule)
	return q, dbh
}

func getRow(t *testing.T, dbh *sql.DB, id int64) WorkItem {
	t.Helper()
	item, err := scanWorkItem(dbh.QueryRow(`SELECT `+workQueueColumns+` FROM work_queue WHERE id = ?`, id))
	if err != nil {
		t.Fatalf("read row %d: %v", id, err)
	}
	return item
}

func mustEnqueue(t *testing.T, q *DBQueue, in models.Inputs) WorkItem {
	t.Helper()
	item, err := q.Enqueue(context.Background(), in, PriorityScan)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	return item
}

// With a rule, the sung and the instrumental copy are two rows, each keeping its
// own album and folder, whichever is enqueued first.
func TestEnqueueEditionRuleSplitsTheTwoCopies(t *testing.T) {
	sung, inst := editionInputs("Low Tide"), editionInputs("Low Tide (Instrumental)")
	for _, order := range [][]models.Inputs{{sung, inst}, {inst, sung}} {
		q, dbh := newEditionQueue(t, albumEditionRule)
		first := mustEnqueue(t, q, order[0])
		second := mustEnqueue(t, q, order[1])
		if first.ID == second.ID {
			t.Fatalf("enqueue %q then %q: one row %d; want two", order[0].Track.AlbumName, order[1].Track.AlbumName, first.ID)
		}
		if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 2 {
			t.Fatalf("rows = %d; want 2", n)
		}
		for _, pair := range []struct {
			id int64
			in models.Inputs
		}{{first.ID, order[0]}, {second.ID, order[1]}} {
			got := getRow(t, dbh, pair.id)
			if got.Inputs.Track.AlbumName != pair.in.Track.AlbumName || got.Inputs.Outdir != pair.in.Outdir ||
				got.Inputs.SourcePath != pair.in.SourcePath || got.Edition != albumEditionRule(pair.in.Track) {
				t.Errorf("row %d = album %q outdir %q edition %q; want album %q outdir %q edition %q", pair.id,
					got.Inputs.Track.AlbumName, got.Inputs.Outdir, got.Edition,
					pair.in.Track.AlbumName, pair.in.Outdir, albumEditionRule(pair.in.Track))
			}
		}
	}
}

// A settled sung row is left exactly as it was when the instrumental copy
// arrives: that copy gets its own pending row and the sung row gains no link.
func TestEnqueueEditionRuleLeavesSettledSungRowUntouched(t *testing.T) {
	q, dbh := newEditionQueue(t, albumEditionRule)
	ctx := context.Background()
	lib := seedLibrary(t, dbh, "/lib")
	scanRow := func(in models.Inputs) int64 {
		var id int64
		if err := dbh.QueryRow(`INSERT INTO scan_results (library_id, file_path, outdir, filename, artist, title, status)
            VALUES (?, ?, ?, ?, ?, ?, 'processing') RETURNING id`, lib, in.SourcePath, in.Outdir, in.Filename,
			in.Track.ArtistName, in.Track.TrackName).Scan(&id); err != nil {
			t.Fatalf("seed scan row: %v", err)
		}
		return id
	}
	sung, inst := editionInputs("Low Tide"), editionInputs("Low Tide (Instrumental)")
	sung.ScanResultID, inst.ScanResultID = scanRow(sung), scanRow(inst)

	sungRow := mustEnqueue(t, q, sung)
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET status = 'done', completed_at = '2026-10-01T12:00:00Z', outcome_type = 'synced' WHERE id = ?`, sungRow.ID); err != nil {
		t.Fatal(err)
	}
	before := getRow(t, dbh, sungRow.ID)
	linksBefore := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, sungRow.ID)

	instRow := mustEnqueue(t, q, inst)
	if instRow.ID == sungRow.ID || instRow.Status != StatusPending || instRow.Edition != EditionInstrumental {
		t.Fatalf("instrumental enqueue = id %d status %q edition %q; want a new pending instrumental row beside %d",
			instRow.ID, instRow.Status, instRow.Edition, sungRow.ID)
	}
	if after := getRow(t, dbh, sungRow.ID); !reflect.DeepEqual(after, before) {
		t.Errorf("sung row changed:\n before %+v\n after  %+v", before, after)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, sungRow.ID); n != linksBefore {
		t.Errorf("sung row links = %d; want %d", n, linksBefore)
	}
}

// With no rule the two copies collapse into one row, as before the column.
func TestEnqueueNoEditionRuleCollapsesAsBefore(t *testing.T) {
	sung, inst := editionInputs("Low Tide"), editionInputs("Low Tide (Instrumental)")
	for _, order := range [][]models.Inputs{{sung, inst}, {inst, sung}} {
		q, dbh := newEditionQueue(t, nil)
		first := mustEnqueue(t, q, order[0])
		second := mustEnqueue(t, q, order[1])
		if first.ID != second.ID || second.Edition != EditionNone {
			t.Fatalf("no rule: ids %d, %d edition %q; want one row with edition ''", first.ID, second.ID, second.Edition)
		}
		// A pending row takes the later copy's paths, as on main.
		if second.Inputs.Outdir != order[1].Outdir {
			t.Errorf("outdir = %q; want %q", second.Inputs.Outdir, order[1].Outdir)
		}
		if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 1 {
			t.Fatalf("rows = %d; want 1", n)
		}
	}
}

// A legacy row with an empty edition (made before the rule was set) that already names the
// incoming file keeps it: no instrumental row is made for the same file, and
// the stored edition is never rewritten.
func TestEnqueueEditionRuleKeepsLegacyRowNamingTheFile(t *testing.T) {
	q, dbh := newEditionQueue(t, nil)
	inst := editionInputs("Low Tide (Instrumental)")
	legacy := mustEnqueue(t, q, inst)

	q.SetEditionRule(albumEditionRule)
	again := mustEnqueue(t, q, inst)
	if again.ID != legacy.ID || again.Edition != EditionNone {
		t.Fatalf("re-enqueue = id %d edition %q; want legacy row %d with edition ''", again.ID, again.Edition, legacy.ID)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 1 {
		t.Fatalf("rows = %d; want 1 (no duplicate for the same file)", n)
	}
}

// When rows of both editions name the incoming file, the rule's answer wins.
func TestEnqueueEditionRulePrefersItsOwnEditionForTheFile(t *testing.T) {
	q, dbh := newEditionQueue(t, nil)
	inst := editionInputs("Low Tide (Instrumental)")
	legacy := mustEnqueue(t, q, inst)
	var instID int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, edition, source_path)
        SELECT artist, title, artist_key, title_key, 'instrumental', source_path FROM work_queue WHERE id = ? RETURNING id`,
		legacy.ID).Scan(&instID); err != nil {
		t.Fatal(err)
	}
	q.SetEditionRule(albumEditionRule)
	if got := mustEnqueue(t, q, inst); got.ID != instID {
		t.Fatalf("enqueue landed on row %d; want the instrumental row %d", got.ID, instID)
	}
}

// A rule answer outside the CHECK set fails the enqueue loudly and writes nothing.
func TestEnqueueRejectsUnknownEdition(t *testing.T) {
	q, dbh := newEditionQueue(t, func(models.Track) string { return "karaoke" })
	_, err := q.Enqueue(context.Background(), editionInputs("Low Tide"), PriorityScan)
	if err == nil || !strings.Contains(err.Error(), "unknown edition") {
		t.Fatalf("Enqueue err = %v; want the unknown edition error", err)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 0 {
		t.Fatalf("rows = %d; want 0", n)
	}
}

// Adoption uses the same identity: an instrumental copy's settled file beside a
// settled sung row gets its own row, and LinkSettled never links it to the twin.
func TestAdoptionIsEditionAware(t *testing.T) {
	ctx := context.Background()
	q, dbh := newEditionQueue(t, albumEditionRule)
	lib := seedLibrary(t, dbh, "/lib")
	candidate := func(in models.Inputs) AdoptCandidate {
		ak, tk := IdentityKeys(in.Track)
		c := AdoptCandidate{FilePath: in.SourcePath, Outdir: in.Outdir, Filename: in.Filename, Track: in.Track, ArtistKey: ak, TitleKey: tk}
		if err := dbh.QueryRow(`INSERT INTO scan_results (library_id, file_path, outdir, filename, artist, title, artist_key, title_key, status)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'done') RETURNING id`, lib, c.FilePath, c.Outdir, c.Filename,
			in.Track.ArtistName, in.Track.TrackName, ak, tk).Scan(&c.ScanResultID); err != nil {
			t.Fatalf("seed scan row: %v", err)
		}
		return c
	}
	sung := mustEnqueue(t, q, editionInputs("Low Tide"))
	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET status = 'done' WHERE id = ?`, sung.ID); err != nil {
		t.Fatal(err)
	}
	c := candidate(editionInputs("Low Tide (Instrumental)"))

	if res, err := q.LinkSettled(ctx, c, adoptStamp, nil); err != nil || res != AdoptNoRow {
		t.Fatalf("LinkSettled = %v, %v; want AdoptNoRow (the sung twin is another identity)", res, err)
	}
	if res, err := q.InsertSettledDone(ctx, c, AdoptClass{OutcomeType: "synced"}, adoptStamp, nil); err != nil || res != AdoptInserted {
		t.Fatalf("InsertSettledDone = %v, %v; want AdoptInserted", res, err)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue WHERE edition = 'instrumental' AND source_path = ?`, c.FilePath); n != 1 {
		t.Fatalf("instrumental rows for the adopted file = %d; want 1", n)
	}
	if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = ?`, sung.ID); n != 0 {
		t.Fatalf("sung row links = %d; want 0", n)
	}
}

// seedEditionRow inserts a row for editionInputs(album) under edition, then
// applies set (a SET clause, "" for none) to shape the case under test.
func seedEditionRow(t *testing.T, dbh *sql.DB, album, edition, set string) int64 {
	t.Helper()
	in := editionInputs(album)
	a, ti := IdentityKeys(in.Track)
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, album, artist_key, title_key, edition, outdir, filename, source_path, output_paths, status)
        VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '[]', 'pending') RETURNING id`,
		in.Track.ArtistName, in.Track.TrackName, album, a, ti, edition, in.Outdir, in.Filename, in.SourcePath).Scan(&id); err != nil {
		t.Fatalf("seed %q row: %v", edition, err)
	}
	if set != "" {
		if _, err := dbh.Exec(`UPDATE work_queue SET `+set+` WHERE id = ?`, id); err != nil {
			t.Fatalf("shape row %d: %v", id, err)
		}
	}
	return id
}

// A scan-origin enqueue of the instrumental copy looks only at its own
// edition's row: the sung twin's categorical verdict neither refuses it nor is
// reopened by it, and the twin's word recheck and adoption mark stay as they are (#1559).
func TestScanReopensActOnlyOnTheirOwnEdition(t *testing.T) {
	cases := []struct {
		name string
		set  string
		in   func(models.Inputs) models.Inputs
	}{
		{
			// Would refuse (ErrCategoricalNotReopened): a retired categorical row.
			name: "categorical refusal",
			set:  `status = 'unavailable', timing_outcome = 'categorical'`,
			in:   func(in models.Inputs) models.Inputs { in.ReopenCategorical = true; return in },
		},
		{
			// Would reopen: a done, fileless categorical row.
			name: "categorical reopen",
			set:  `status = 'done', timing_outcome = 'categorical', completed_at = '2026-10-01T12:00:00Z'`,
			in:   func(in models.Inputs) models.Inputs { in.ReopenCategorical = true; return in },
		},
		{
			name: "word recheck",
			set:  `status = 'deferred', word_timing_state = 'queued', outcome_type = 'synced'`,
			in:   func(in models.Inputs) models.Inputs { return in },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, dbh := newEditionQueue(t, albumEditionRule)
			sungID := seedEditionRow(t, dbh, "Low Tide", EditionNone, tc.set)
			before := getRow(t, dbh, sungID)

			in := tc.in(editionInputs("Low Tide (Instrumental)"))
			in.FromScan = true
			got, err := q.Enqueue(context.Background(), in, PriorityScan)
			if err != nil {
				t.Fatalf("instrumental enqueue: %v; want its own row", err)
			}
			if got.ID == sungID || got.Edition != EditionInstrumental || got.Status != StatusPending {
				t.Fatalf("instrumental enqueue = id %d edition %q status %q; want a new pending instrumental row", got.ID, got.Edition, got.Status)
			}
			if after := getRow(t, dbh, sungID); !reflect.DeepEqual(after, before) {
				t.Errorf("sung row changed:\n got %+v\nwant %+v", after, before)
			}
		})
	}

	t.Run("adoption mark", func(t *testing.T) {
		q, dbh := newEditionQueue(t, albumEditionRule)
		in := editionInputs("Low Tide (Instrumental)")
		in.FromScan, in.ReopenAdopted = true, true
		// Both rows name the incoming file (a legacy row and the rule's own):
		// the rule's edition wins the pin, so the adopted sung row is not this copy's.
		sungID := seedEditionRow(t, dbh, "Low Tide", EditionNone, `status = 'done', adopted_at = '2026-10-01T12:00:00Z', source_path = '`+in.SourcePath+`'`)
		instID := seedEditionRow(t, dbh, "Low Tide (Instrumental)", EditionInstrumental, "")
		before := getRow(t, dbh, sungID)
		got := mustEnqueue(t, q, in)
		if got.ID != instID {
			t.Fatalf("enqueue landed on row %d; want the instrumental row %d", got.ID, instID)
		}
		if after := getRow(t, dbh, sungID); !reflect.DeepEqual(after, before) {
			t.Errorf("adopted sung row changed:\n got %+v\nwant %+v", after, before)
		}
		if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue WHERE id = ? AND adopted_at IS NOT NULL`, sungID); n != 1 {
			t.Error("the sung row's adoption mark was spent by the instrumental copy")
		}
	})
}
