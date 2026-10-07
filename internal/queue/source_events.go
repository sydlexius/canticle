package queue

import (
	"context"
	"fmt"
	"time"
)

// Event names in source_event_daily.event (#1301): hit/miss mirror
// provider_outcomes; the rest are delivered types.
const (
	SourceEventHit          = "hit"
	SourceEventMiss         = "miss"
	SourceEventWord         = SyncTierWord
	SourceEventLine         = SyncTierLine
	SourceEventUnsynced     = SyncTierUnsynced
	SourceEventInstrumental = "instrumental"
)

// validSourceEvent mirrors the table's CHECK constraint.
func validSourceEvent(event string) bool {
	switch event {
	case SourceEventHit, SourceEventMiss, SourceEventWord, SourceEventLine, SourceEventUnsynced, SourceEventInstrumental:
		return true
	}
	return false
}

// SourceEventDay is the UTC day (YYYY-MM-DD) of t.
func SourceEventDay(t time.Time) string {
	return t.UTC().Format("2006-01-02")
}

// RecordSourceEvent adds one to the (UTC day of at, lane, event) counter. The
// caller supplies the instant (the worker's clock). An empty lane is a no-op,
// matching RecordProviderHit; an unknown event is refused.
func (q *DBQueue) RecordSourceEvent(ctx context.Context, at time.Time, lane, event string) error {
	if lane == "" {
		return nil
	}
	if !validSourceEvent(event) {
		return fmt.Errorf("queue: record source event for %q: invalid event %q", lane, event)
	}
	_, err := q.db.ExecContext(ctx,
		`INSERT INTO source_event_daily(day, lane, event, count) VALUES(?, ?, ?, 1)
         ON CONFLICT(day, lane, event) DO UPDATE SET count = count + 1`,
		SourceEventDay(at), lane, event,
	)
	if err != nil {
		return fmt.Errorf("queue: record source event %q for %q: %w", event, lane, err)
	}
	return nil
}
