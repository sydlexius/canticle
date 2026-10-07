package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// A row first seen by a CLI scan carries providers_version = 0 (#825). A later
// serve-mode re-enqueue under a real generation must adopt it, or the #679
// suppression can never match; a settled row must adopt it too, since that is
// exactly the row the suppression guards.
func TestEnqueueAdoptsGenerationOnlyFromUnknownStamp(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}, Outdir: "out", Filename: "a.lrc"}

	first, err := q.Enqueue(ctx, inputs, 1) // CLI scan: generation 0
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if first.ProvidersVersion != 0 {
		t.Fatalf("setup: ProvidersVersion = %d; want 0", first.ProvidersVersion)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.SetTimingOutcome(ctx, claimed.ID, TimingRecord{Outcome: "categorical"}); err != nil {
		t.Fatalf("SetTimingOutcome: %v", err)
	}
	if err := q.Complete(ctx, claimed.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	q.SetProvidersVersion(5) // serve mode, real provider set
	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}
	_, version, _, found, err := q.LookupTiming(ctx, "Artist", "Song")
	if err != nil || !found {
		t.Fatalf("LookupTiming found=%v err=%v", found, err)
	}
	if version != 5 {
		t.Fatalf("providers_version = %d; want 5 (unknown stamp must adopt the current generation)", version)
	}

	// A known stamp is never refreshed: that would defeat the expiry.
	q.SetProvidersVersion(9)
	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("third Enqueue: %v", err)
	}
	_, version, _, _, _ = q.LookupTiming(ctx, "Artist", "Song")
	if version != 5 {
		t.Fatalf("providers_version = %d; want 5 (a known stamp must not be refreshed)", version)
	}
}

// Adoption is a property of the stamp, not of the row's status: a pending row
// first seen at generation 0 adopts the current generation on re-enqueue too.
func TestEnqueueAdoptsGenerationOnPendingRow(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)
	inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}, Outdir: "out", Filename: "a.lrc"}

	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	q.SetProvidersVersion(5)
	if _, err := q.Enqueue(ctx, inputs, 1); err != nil {
		t.Fatalf("re-Enqueue: %v", err)
	}
	var status string
	var version int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, providers_version FROM work_queue WHERE artist = 'Artist' AND title = 'Song'`,
	).Scan(&status, &version); err != nil {
		t.Fatalf("select row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("status = %q; want pending (setup)", status)
	}
	if version != 5 {
		t.Fatalf("providers_version = %d; want 5 (a pending row must adopt too)", version)
	}
}
