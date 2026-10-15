package queue

import (
	"context"
	"database/sql"
	"io/fs"
	"reflect"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// LookupTiming reads the empty-edition row only: a verdict on the instrumental
// row is never returned for the sung copy.
func TestLookupTimingNeverReadsTheEditionRow(t *testing.T) {
	q, dbh := newEditionQueue(t, albumEditionRule)
	ctx := context.Background()
	seedEditionRow(t, dbh, "Low Tide (Instrumental)", EditionInstrumental, `status = 'done', timing_outcome = 'categorical', providers_version = 3`)
	if outcome, _, _, found, err := q.LookupTiming(ctx, "Shore", "Quiet Shelf"); err != nil || found {
		t.Fatalf("LookupTiming with only an instrumental row = %q found %v err %v; want nothing found", outcome, found, err)
	}
	seedEditionRow(t, dbh, "Low Tide", EditionNone, `status = 'done', timing_outcome = 'ok', providers_version = 2`)
	outcome, version, _, found, err := q.LookupTiming(ctx, "Shore", "Quiet Shelf")
	if err != nil || !found || outcome != "ok" || version != 2 {
		t.Fatalf("LookupTiming = %q v%d found %v err %v; want the sung row's ok v2", outcome, version, found, err)
	}
}

// The gone-source move reads only the incoming copy's edition: a sung file
// never moves onto the instrumental row, even as its same-stem sibling, while
// the instrumental copy still moves its own row.
func TestGoneSourceMoveStaysInItsEdition(t *testing.T) {
	const gone, fresh = "/lib/shared/01 Quiet Shelf.mp3", "/lib/shared/01 Quiet Shelf.flac"
	incoming := func(album string) models.Inputs {
		in := editionInputs(album)
		in.Outdir, in.SourcePath = "/lib/shared", fresh
		return in
	}
	setup := func(t *testing.T) (*DBQueue, *sql.DB, int64) {
		q, dbh := newEditionQueue(t, albumEditionRule)
		q.stat = fakeStat(map[string]error{gone: fs.ErrNotExist})
		id := seedEditionRow(t, dbh, "Low Tide (Instrumental)", EditionInstrumental,
			`status = 'done', outcome_type = 'instrumental', outdir = '/lib/shared', source_path = '`+gone+`'`)
		return q, dbh, id
	}

	t.Run("sung copy", func(t *testing.T) {
		q, dbh, instID := setup(t)
		before := getRow(t, dbh, instID)
		if moved, err := q.RepointGoneSource(context.Background(), incoming("Low Tide")); err != nil || moved {
			t.Fatalf("RepointGoneSource(sung) = %v, %v; want no move", moved, err)
		}
		got := mustEnqueue(t, q, incoming("Low Tide"))
		if got.ID == instID || got.Edition != EditionNone || got.Inputs.SourcePath != fresh {
			t.Fatalf("sung enqueue = id %d edition %q; want its own '' row at the new file", got.ID, got.Edition)
		}
		if after := getRow(t, dbh, instID); !reflect.DeepEqual(after, before) {
			t.Errorf("instrumental row changed:\n got %+v\nwant %+v", after, before)
		}
	})

	t.Run("instrumental copy", func(t *testing.T) {
		q, dbh, instID := setup(t)
		got := mustEnqueue(t, q, incoming("Low Tide (Instrumental)"))
		if got.ID != instID || got.Inputs.SourcePath != fresh || got.Status != StatusDone {
			t.Fatalf("instrumental enqueue = id %d source moved %v status %q; want row %d moved and still done",
				got.ID, got.Inputs.SourcePath == fresh, got.Status, instID)
		}
		if n := countOf(t, dbh, `SELECT COUNT(*) FROM work_queue`); n != 1 {
			t.Errorf("rows = %d; want 1", n)
		}
	})
}

// A webhook delete removes the queued work of the deleted copy's edition only.
func TestCleanupRemovesOnlyTheEditionOfTheDeletedCopy(t *testing.T) {
	q, dbh := newEditionQueue(t, albumEditionRule)
	sungID := seedEditionRow(t, dbh, "Low Tide", EditionNone, "")
	instID := seedEditionRow(t, dbh, "Low Tide (Instrumental)", EditionInstrumental, "")
	n, err := q.Cleanup(context.Background(), editionInputs("Low Tide (Instrumental)"))
	if err != nil || n != 1 {
		t.Fatalf("Cleanup = %d, %v; want 1 row removed", n, err)
	}
	if countOf(t, dbh, `SELECT COUNT(*) FROM work_queue WHERE id = ?`, instID) != 0 ||
		countOf(t, dbh, `SELECT COUNT(*) FROM work_queue WHERE id = ?`, sungID) != 1 {
		t.Error("Cleanup removed the wrong edition's row")
	}
}
