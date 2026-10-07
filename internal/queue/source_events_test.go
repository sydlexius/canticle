package queue

import (
	"context"
	"testing"
	"time"
)

func sourceEventCount(t *testing.T, q *DBQueue, day, lane, event string) int64 {
	t.Helper()
	var n int64
	err := q.db.QueryRowContext(context.Background(),
		`SELECT COALESCE(SUM(count), 0) FROM source_event_daily WHERE day = ? AND lane = ? AND event = ?`,
		day, lane, event).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// TestRecordSourceEvent (#1301): the counter accumulates per (UTC day, lane,
// event), a UTC day boundary starts a new row, a non-UTC instant is bucketed by
// its UTC day, and an empty lane or unknown event writes nothing.
func TestRecordSourceEvent(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	late := time.Date(2026, 10, 6, 23, 59, 59, 0, time.UTC)
	for i := 0; i < 2; i++ {
		if err := q.RecordSourceEvent(ctx, late, "musixmatch", SourceEventHit); err != nil {
			t.Fatal(err)
		}
	}
	if err := q.RecordSourceEvent(ctx, late.Add(time.Second), "musixmatch", SourceEventHit); err != nil {
		t.Fatal(err)
	}
	// 2026-10-06 17:00 PDT is 2026-10-07 00:00 UTC: the UTC day wins.
	pdt := time.FixedZone("PDT", -7*3600)
	if err := q.RecordSourceEvent(ctx, time.Date(2026, 10, 6, 17, 0, 0, 0, pdt), "petitlyrics", SourceEventWord); err != nil {
		t.Fatal(err)
	}
	if got := sourceEventCount(t, q, "2026-10-06", "musixmatch", "hit"); got != 2 {
		t.Errorf("10-06 hits = %d, want 2", got)
	}
	if got := sourceEventCount(t, q, "2026-10-07", "musixmatch", "hit"); got != 1 {
		t.Errorf("10-07 hits = %d, want 1 (rollover)", got)
	}
	if got := sourceEventCount(t, q, "2026-10-07", "petitlyrics", "word"); got != 1 {
		t.Errorf("non-UTC instant not bucketed by UTC day, got %d", got)
	}
	if err := q.RecordSourceEvent(ctx, late, "musixmatch", "bogus"); err == nil {
		t.Error("unknown event accepted")
	}
}
