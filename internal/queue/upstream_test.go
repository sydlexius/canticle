package queue

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// TestUpstreamFollowsProviderLane (#1297): the licensor is written with the
// lane in one statement (empty clears it), and every path that clears or
// replaces provider_lane clears it too.
func TestUpstreamFollowsProviderLane(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	read := func(id int64) string {
		t.Helper()
		var up sql.NullString
		if err := sqlDB.QueryRowContext(ctx, `SELECT upstream FROM work_queue WHERE id = ?`, id).Scan(&up); err != nil {
			t.Fatal(err)
		}
		return up.String
	}
	// settled returns a done row carrying a lane and an upstream.
	settled := func(title string) int64 {
		t.Helper()
		item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: title}, SourcePath: "/m/" + title + ".mp3"}, PriorityScan)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done', instrumental_result = 1,
			provider_lane = 'innertube', upstream = 'lyricfind' WHERE id = ?`, item.ID); err != nil {
			t.Fatal(err)
		}
		return item.ID
	}

	id := settled("stamp")
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := q.SetProviderLane(ctx, id, "musixmatch", "lyricfind"); err != nil || read(id) != "lyricfind" {
		t.Fatalf("stamp = %v, upstream %q; want lyricfind", err, read(id))
	}
	if err := q.SetProviderLane(ctx, id, "petitlyrics", ""); err != nil || read(id) != "" {
		t.Fatalf("new lane, empty upstream = %v, %q; want NULL", err, read(id))
	}

	id = settled("clear")
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := q.ClearProviderLane(ctx, id); err != nil || read(id) != "" {
		t.Errorf("ClearProviderLane = %v, upstream %q; want NULL", err, read(id))
	}

	id = settled("settle")
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if out, err := q.SettleInstrumental(ctx, id, InstrumentalTelemetry{}, OwnedByWorker); err != nil || out != Settled || read(id) != "" {
		t.Errorf("SettleInstrumental = %v, %v, upstream %q; want settled, NULL", out, err, read(id))
	}

	id = settled("unsettle")
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET provider_lane = NULL WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := q.UnsettleInstrumental(ctx, id); err != nil || !ok || read(id) != "" {
		t.Errorf("UnsettleInstrumental = %v, %v, upstream %q; want NULL", ok, err, read(id))
	}

	id = settled("reopen")
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, id, time.Now()); err != nil || !ok {
		t.Fatalf("reopen = %v, %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if read(id) != "" {
		t.Errorf("ReopenDoneRowTx kept upstream %q", read(id))
	}

	// ResetInstrumental keeps provider_lane, so it keeps upstream.
	id = settled("reset")
	if n, err := q.ResetInstrumental(ctx, id); err != nil || n == 0 || read(id) != "lyricfind" {
		t.Errorf("ResetInstrumental = %d, %v, upstream %q; want it kept (lane is kept)", n, err, read(id))
	}
}
