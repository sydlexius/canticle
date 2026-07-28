package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// LookupTiming is the READ side migration 034's columns never had: the worker
// has stamped timing_outcome since #440, but nothing consumed it, so a
// Categorical verdict (which writes no sidecar) was re-enqueued and re-fetched
// on every scan (#679). These exercise it against a real SQLite database rather
// than a fake, since the lookup is a query and its keying is the thing at risk.
func TestLookupTimingReturnsStoredVerdict(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.SetProvidersVersion(7)

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
		Outdir:   "out",
		Filename: "a.lrc",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.SetTimingOutcome(ctx, item.ID, TimingRecord{Outcome: "categorical"}); err != nil {
		t.Fatalf("SetTimingOutcome: %v", err)
	}

	outcome, version, found, err := q.LookupTiming(ctx, "Artist", "Song")
	if err != nil {
		t.Fatalf("LookupTiming: %v", err)
	}
	if !found {
		t.Fatal("found = false; want true for a row carrying a verdict")
	}
	if outcome != "categorical" {
		t.Errorf("outcome = %q; want categorical", outcome)
	}
	if version != 7 {
		t.Errorf("providersVersion = %d; want 7 -- losing it would break expiry", version)
	}
}

// The lookup keys on the NORMALIZED artist/title, exactly as work_queue itself
// is keyed. A caller passes the raw tag values, so a lookup that did not
// normalize would miss every row whose tags differ in case, spacing, or
// Unicode form -- silently disabling the suppression for those tracks.
func TestLookupTimingNormalizesTheKey(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Hello", TrackName: "World"},
		Outdir:   "out",
		Filename: "a.lrc",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.SetTimingOutcome(ctx, item.ID, TimingRecord{Outcome: "categorical"}); err != nil {
		t.Fatalf("SetTimingOutcome: %v", err)
	}

	// Same track, differently spelled by the caller.
	outcome, _, found, err := q.LookupTiming(ctx, "  HELLO  ", " world ")
	if err != nil {
		t.Fatalf("LookupTiming: %v", err)
	}
	if !found || outcome != "categorical" {
		t.Fatalf("found=%v outcome=%q; want the verdict to be found through normalization", found, outcome)
	}
}

// A row with no verdict yet reports NOT FOUND, identically to no row at all: to
// a caller both mean "no verdict on record", and collapsing them keeps the
// caller from distinguishing two states it treats alike.
func TestLookupTimingRowWithoutVerdictIsNotFound(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:    models.Track{ArtistName: "Artist", TrackName: "Unjudged"},
		Outdir:   "out",
		Filename: "a.lrc",
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	_, _, found, err := q.LookupTiming(ctx, "Artist", "Unjudged")
	if err != nil {
		t.Fatalf("LookupTiming: %v", err)
	}
	if found {
		t.Fatal("found = true for a row that has never been judged; want false")
	}
}

// An absent track is the ordinary never-fetched case: not found, and NOT an
// error. Returning an error here would make the caller fail open on every
// unqueued track, which is every track on a first scan.
func TestLookupTimingAbsentTrackIsNotAnError(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	_, _, found, err := q.LookupTiming(ctx, "Nobody", "Nothing")
	if err != nil {
		t.Fatalf("LookupTiming on an absent track returned an error: %v", err)
	}
	if found {
		t.Fatal("found = true for a track that was never enqueued; want false")
	}
}
