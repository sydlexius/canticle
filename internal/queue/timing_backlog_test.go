package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// ListTimingBacklog is the query the serve-mode timing sweep (#443) drains. It
// is what makes the sweep incremental: a row leaves the backlog by being
// stamped, so convergence is a property of the PREDICATE rather than something
// the loop has to maintain. These run against real SQLite, since the keying and
// the NULL semantics are the thing at risk.

// completeSynced drives a row to the state the sweep's population requires: a
// completed synced outcome carrying a source path, with no timing verdict yet.
func completeSynced(t *testing.T, q *DBQueue, title, sourcePath string) WorkItem {
	t.Helper()
	ctx := context.Background()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: title},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: sourcePath,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue(%s): %v", title, err)
	}
	// Complete requires the row to be in 'processing', so it must be dequeued
	// first -- the same path the worker takes. Every previously seeded row is
	// already 'done', so this claims the row just enqueued.
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue(%s): %v", title, err)
	}
	if claimed.ID != item.ID {
		t.Fatalf("Dequeue(%s) claimed id %d; want %d", title, claimed.ID, item.ID)
	}
	if err := q.SetOutcomeType(ctx, item.ID, "synced"); err != nil {
		t.Fatalf("SetOutcomeType(%s): %v", title, err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete(%s): %v", title, err)
	}
	return item
}

// TestListTimingBacklogSelectsUnstampedSyncedRows verifies the core population:
// a completed synced row with no timing verdict is in the backlog.
func TestListTimingBacklogSelectsUnstampedSyncedRows(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	want := completeSynced(t, q, "Song", "/music/song.flac")

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d; want 1", len(got))
	}
	if got[0].ID != want.ID {
		t.Errorf("ID = %d; want %d", got[0].ID, want.ID)
	}
	if got[0].Inputs.SourcePath != "/music/song.flac" {
		t.Errorf("SourcePath = %q; want the audio path (the sidecar is derived from it)", got[0].Inputs.SourcePath)
	}
}

// TestListTimingBacklogExcludesStampedRows is the convergence property itself:
// once a row carries a verdict it leaves the backlog, so a drained install
// returns an empty batch forever rather than re-judging the same files.
//
// It covers 'ok' specifically. A compliant file is the overwhelming majority of
// a library, so if 'ok' did not retire a row, the sweep would re-read nearly
// every .lrc on every cycle -- the exact array-thrashing this design exists to
// avoid, and a bug that would be invisible in a small test corpus.
func TestListTimingBacklogExcludesStampedRows(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	for _, outcome := range []string{"ok", "mis_synced", "categorical", "unknown_duration", "degenerate"} {
		item := completeSynced(t, q, outcome, "/music/"+outcome+".flac")
		if err := q.SetTimingOutcome(ctx, item.ID, TimingRecord{Outcome: outcome}); err != nil {
			t.Fatalf("SetTimingOutcome(%s): %v", outcome, err)
		}
	}

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0 -- every stamped row must retire, or the sweep never converges", len(got))
	}
}

// TestListTimingBacklogExcludesNonSyncedOutcomes verifies only synced rows are
// candidates. An unsynced .txt or an instrumental marker has no line timing to
// judge, so including them would spend the batch budget on files with no
// possible verdict and starve the rows that do.
func TestListTimingBacklogExcludesNonSyncedOutcomes(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	for _, outcome := range []string{"unsynced", "instrumental"} {
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:      models.Track{ArtistName: "Artist", TrackName: outcome},
			Outdir:     "out",
			Filename:   "a.lrc",
			SourcePath: "/music/" + outcome + ".flac",
		}, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, derr := q.Dequeue(ctx); derr != nil {
			t.Fatalf("Dequeue: %v", derr)
		}
		if err := q.SetOutcomeType(ctx, item.ID, outcome); err != nil {
			t.Fatalf("SetOutcomeType: %v", err)
		}
		if err := q.Complete(ctx, item.ID); err != nil {
			t.Fatalf("Complete: %v", err)
		}
	}

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0 (no line timing exists to judge)", len(got))
	}
}

// TestListTimingBacklogExcludesNullOutcomeType verifies a legacy row whose
// outcome was never recorded is NOT swept. Migration 024 states such rows are
// not reliably reconstructable -- output_paths holds the stale enqueue-time
// .lrc, not what was written -- so treating NULL as "probably synced" would
// point the sweep at files that may not exist or may not be .lrc at all.
//
// This is the conservative reading, and it is deliberate: those rows are
// reachable by the `revalidate` CLI, which walks the filesystem and therefore
// judges what is ACTUALLY on disk rather than what a NULL column implies.
func TestListTimingBacklogExcludesNullOutcomeType(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Legacy"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: "/music/legacy.flac",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, derr := q.Dequeue(ctx); derr != nil {
		t.Fatalf("Dequeue: %v", derr)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0 (a NULL outcome_type is not evidence of a .lrc)", len(got))
	}
}

// TestListTimingBacklogRequiresSourcePath verifies a row with no source path is
// excluded. The sidecar is DERIVED from the audio path, so a blank source_path
// leaves nothing to judge; including it would cost a batch slot per cycle
// forever, since nothing about the row ever changes to retire it.
func TestListTimingBacklogRequiresSourcePath(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	completeSynced(t, q, "NoSource", "")

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len = %d; want 0 (no audio path to derive a sidecar from)", len(got))
	}
}

// TestListTimingBacklogExcludesInFlightRows verifies a row still being processed
// is not swept. The worker is about to stamp its own verdict and may still be
// writing the sidecar; judging it mid-write races the writer over the same file.
func TestListTimingBacklogExcludesInFlightRows(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "InFlight"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: "/music/inflight.flac",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if err := q.SetOutcomeType(ctx, item.ID, "synced"); err != nil {
		t.Fatalf("SetOutcomeType: %v", err)
	}
	// Left in 'processing' by dequeuing without completing.
	if _, derr := q.Dequeue(ctx); derr != nil {
		t.Fatalf("Dequeue: %v", derr)
	}

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 10})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	for _, g := range got {
		if g.ID == item.ID {
			t.Error("an in-flight row was returned; the worker may still be writing its sidecar")
		}
	}
}

// TestListTimingBacklogHonorsLimit verifies the batch budget is enforced in SQL.
// Enforcing it in Go would still read every backlog row per cycle, which on a
// large unswept library is the cost the budget exists to bound.
func TestListTimingBacklogHonorsLimit(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	for _, name := range []string{"One", "Two", "Three", "Four", "Five"} {
		completeSynced(t, q, name, "/music/"+name+".flac")
	}

	got, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{Limit: 2})
	if err != nil {
		t.Fatalf("ListTimingBacklog: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("len = %d; want 2 (the limit must bound the query, not the caller)", len(got))
	}
}

// TestCountTimingBacklogMatchesTheListPopulation verifies the count applies the
// SAME eligibility as the list. A count over a wider population would overstate
// the backlog and make the sweep's progress logging a lie.
func TestCountTimingBacklogMatchesTheListPopulation(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	completeSynced(t, q, "Eligible", "/music/a.flac")
	completeSynced(t, q, "NoSource", "")
	stamped := completeSynced(t, q, "Stamped", "/music/b.flac")
	if err := q.SetTimingOutcome(ctx, stamped.ID, TimingRecord{Outcome: "ok"}); err != nil {
		t.Fatalf("SetTimingOutcome: %v", err)
	}

	n, err := q.CountTimingBacklog(ctx)
	if err != nil {
		t.Fatalf("CountTimingBacklog: %v", err)
	}
	if n != 1 {
		t.Errorf("CountTimingBacklog = %d; want 1 (only the eligible row)", n)
	}
}
