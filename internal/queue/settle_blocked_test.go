package queue

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// blockedRow enqueues one row, gives it miss/attempt/lane-attempt history and
// claims it, so SettleBlocked meets a processing row with counters worth keeping.
func blockedRow(t *testing.T) (*DBQueue, int64, models.Inputs) {
	t.Helper()
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	in := models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: "/music/a.flac",
	}
	item, err := q.Enqueue(ctx, in, PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET miss_count = 4, attempts = 2, last_error = 'old', refused_waits = 2 WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.RecordLaneAttempts(ctx, item.ID, []models.LaneAttempt{{Lane: "musixmatch"}, {Lane: "petitlyrics"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	return q, item.ID, in
}

type counters struct {
	status, outcome string
	miss, attempts  int
	laneRows        int
}

func readCounters(t *testing.T, q *DBQueue, id int64) counters {
	t.Helper()
	var c counters
	var outcome sql.NullString
	if err := q.db.QueryRow(`SELECT status, outcome_type, miss_count, attempts FROM work_queue WHERE id = ?`, id).
		Scan(&c.status, &outcome, &c.miss, &c.attempts); err != nil {
		t.Fatal(err)
	}
	c.outcome = outcome.String
	if err := q.db.QueryRow(`SELECT COUNT(*) FROM lane_attempts WHERE queue_id = ?`, id).Scan(&c.laneRows); err != nil {
		t.Fatal(err)
	}
	return c
}

// A blocked row settles done/blocked in one statement and every counter is
// unchanged: the lanes answered, they did not miss (#1395).
func TestDBQueue_SettleBlocked_DoneAndCountersUnchanged(t *testing.T) {
	ctx := context.Background()
	q, id, _ := blockedRow(t)
	before := readCounters(t, q, id)

	outcome, err := q.SettleBlocked(ctx, id)
	if err != nil || outcome != Settled {
		t.Fatalf("SettleBlocked = (%v, %v); want Settled", outcome, err)
	}
	after := readCounters(t, q, id)
	if after.status != StatusDone || after.outcome != OutcomeBlocked {
		t.Fatalf("row = (%q, %q); want (done, blocked)", after.status, after.outcome)
	}
	if after.miss != before.miss || after.attempts != before.attempts || after.laneRows != before.laneRows {
		t.Fatalf("counters changed: miss %d->%d attempts %d->%d lane_attempts %d->%d",
			before.miss, after.miss, before.attempts, after.attempts, before.laneRows, after.laneRows)
	}
	if before.miss != 4 || before.attempts != 2 || before.laneRows != 2 {
		t.Fatalf("setup lost its history: %+v", before)
	}
	var lastErr string
	var waits int
	var completed sql.NullString
	if err := q.db.QueryRow(`SELECT last_error, refused_waits, completed_at FROM work_queue WHERE id = ?`, id).Scan(&lastErr, &waits, &completed); err != nil {
		t.Fatal(err)
	}
	if lastErr != "" || waits != 0 || !completed.Valid {
		t.Fatalf("last_error=%q refused_waits=%d completed_at=%v; want cleared, 0, set", lastErr, waits, completed)
	}
}

// A row the caller does not hold is refused and nothing is written.
func TestDBQueue_SettleBlocked_RefusesUnheldRow(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "T"}, Outdir: "o", Filename: "a.lrc"}, PriorityScan)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := q.SettleBlocked(ctx, item.ID)
	if err != nil || outcome == Settled {
		t.Fatalf("SettleBlocked on a pending row = (%v, %v); want a refusal", outcome, err)
	}
	if c := readCounters(t, q, item.ID); c.status != StatusPending || c.outcome != "" {
		t.Fatalf("row = %+v; a refused settle must write nothing", c)
	}
}

// Not retried on a timer: no later dequeue returns a blocked row, and no
// enqueue (scan or webhook), provider-generation change, categorical reopen
// request or upgrade sweep reopens it (#1395, #825).
func TestDBQueue_SettleBlocked_NotReopenedByTimeOrProviderChange(t *testing.T) {
	ctx := context.Background()
	q, id, in := blockedRow(t)
	if _, err := q.SettleBlocked(ctx, id); err != nil {
		t.Fatal(err)
	}

	year := time.Now().UTC().AddDate(1, 0, 0)
	q.now = func() time.Time { return year }
	if item, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue a year later = (%+v, %v); want no row", item, err)
	}

	// A provider-set change (#825) re-stamps nothing on a done row, and a scan
	// enqueue asking to reopen categorical rows does not match a blocked one.
	q.SetProvidersVersion(99)
	for _, tc := range []struct {
		name string
		prio int
		mod  func(*models.Inputs)
	}{
		{"scan", PriorityScan, func(i *models.Inputs) { i.FromScan = true; i.ReopenCategorical = true }},
		{"webhook", PriorityWebhook, func(i *models.Inputs) {}},
	} {
		inputs := in
		tc.mod(&inputs)
		if _, err := q.Enqueue(ctx, inputs, tc.prio); err != nil && !errors.Is(err, ErrCategoricalNotReopened) {
			t.Fatalf("%s enqueue: %v", tc.name, err)
		}
		if c := readCounters(t, q, id); c.status != StatusDone || c.outcome != OutcomeBlocked {
			t.Fatalf("after a %s enqueue under a new provider generation row = (%q, %q); want it still done/blocked", tc.name, c.status, c.outcome)
		}
	}
	if ids, err := q.ListUpgradeCandidates(ctx, year, 100); err != nil || len(ids) != 0 {
		t.Fatalf("ListUpgradeCandidates = (%v, %v); a blocked row is not an upgrade candidate", ids, err)
	}
}

// The one reopen: ReopenBlockedTx, for clearing a block (#1397). It reopens a
// row only while it is still blocked, clears the label, and the row dequeues.
func TestReopenBlockedTx(t *testing.T) {
	ctx := context.Background()
	reopen := func(q *DBQueue, a, ti string) bool {
		t.Helper()
		tx, err := q.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		ok, err := ReopenBlockedTx(ctx, tx, a, ti, time.Now().UTC())
		if err != nil {
			_ = tx.Rollback()
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		return ok
	}

	t.Run("blocked row reopens and dequeues", func(t *testing.T) {
		q, id, in := blockedRow(t)
		if _, err := q.SettleBlocked(ctx, id); err != nil {
			t.Fatal(err)
		}
		a, ti := IdentityKeys(in.Track)
		if !reopen(q, a, ti) {
			t.Fatal("ReopenBlockedTx = false; want the blocked row reopened")
		}
		if c := readCounters(t, q, id); c.status != StatusPending || c.outcome != "" {
			t.Fatalf("row = %+v; want pending with the label cleared", c)
		}
		if item, err := q.Dequeue(ctx); err != nil || item.ID != id {
			t.Fatalf("Dequeue = (%d, %v); want the reopened row %d", item.ID, err, id)
		}
	})

	t.Run("a row settled by something else is left alone", func(t *testing.T) {
		q, id, in := blockedRow(t)
		if _, err := q.SettleGuardRejected(ctx, id, ""); err != nil {
			t.Fatal(err)
		}
		a, ti := IdentityKeys(in.Track)
		if reopen(q, a, ti) {
			t.Fatal("ReopenBlockedTx reopened a rejected row")
		}
		if c := readCounters(t, q, id); c.status != StatusDone || c.outcome != "rejected" {
			t.Fatalf("row = %+v; want untouched", c)
		}
	})

	t.Run("no row for the identity", func(t *testing.T) {
		q := NewDBQueue(openQueueTestDB(t))
		if reopen(q, "nobody", "nothing") {
			t.Fatal("ReopenBlockedTx = true with no row")
		}
	})
}

// A blocked row has no file, so the settle clears every column describing a
// result IN the same statement: a stale lane would count the row as that lane's
// latest served track and attribute it in the source breakdown (#1395). The fixed
// outcome_detail is what Recent outcomes shows for it.
func TestDBQueue_SettleBlocked_ClearsLaneTierAndVerdicts(t *testing.T) {
	ctx := context.Background()
	q, id, _ := blockedRow(t)
	if _, err := q.db.ExecContext(ctx,
		`UPDATE work_queue SET provider_lane = 'petitlyrics', upstream = 'licensor', sync_tier = 'word',
             word_timing_state = 'served', word_timing_generation = 3, word_timing_checked_at = '2026-01-01T00:00:00Z',
             timing_outcome = 'ok', overrun_magnitude = 2.5, overrun_ratio = 1.1, evaluated_at = '2026-01-01T00:00:00Z'
         WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	// Control: the same stale lane on a done row IS served-track evidence, so the
	// absence asserted below comes from the settle and not from the fixture.
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = 'done', last_error = '', completed_at = '2026-01-02T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if track, found, err := q.LatestServedTrack(ctx, "petitlyrics"); err != nil || !found || track.TrackName != "Title" {
		t.Fatalf("setup: LatestServedTrack = (%+v, %v, %v); want the seeded row", track, found, err)
	}
	if _, err := q.db.ExecContext(ctx, `UPDATE work_queue SET status = ? WHERE id = ?`, StatusProcessing, id); err != nil {
		t.Fatal(err)
	}
	if outcome, err := q.SettleBlocked(ctx, id); err != nil || outcome != Settled {
		t.Fatalf("SettleBlocked = (%v, %v)", outcome, err)
	}
	for _, col := range []string{"provider_lane", "upstream", "sync_tier", "word_timing_state", "word_timing_generation",
		"word_timing_checked_at", "timing_outcome", "overrun_magnitude", "overrun_ratio", "evaluated_at"} {
		var v sql.NullString
		if err := q.db.QueryRowContext(ctx, `SELECT CAST(`+col+` AS TEXT) FROM work_queue WHERE id = ?`, id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		if v.Valid {
			t.Errorf("%s = %q after a blocked settle; want NULL", col, v.String)
		}
	}
	if track, found, err := q.LatestServedTrack(ctx, "petitlyrics"); err != nil || found {
		t.Fatalf("LatestServedTrack = (%+v, %v, %v); a blocked row must not count as the lane's latest served track", track, found, err)
	}
	var detail string
	if err := q.db.QueryRowContext(ctx, `SELECT outcome_detail FROM work_queue WHERE id = ?`, id).Scan(&detail); err != nil || detail != OutcomeDetailBlocked {
		t.Fatalf("outcome_detail = %q (%v); want %q", detail, err, OutcomeDetailBlocked)
	}
}
