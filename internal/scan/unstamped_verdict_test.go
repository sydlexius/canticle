package scan_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// TestForcedScanLeavesAnUnstampedVerdictUnstamped (#825) is the review
// scenario on real SQLite: a done + categorical row stamped 0 (a CLI scan wrote
// it), and a forced (--update/--upgrade) serve scan that resets its file to
// pending. The 0 stamp is not suppressed, so the scan enqueues; that enqueue
// re-evaluates nothing (the done row stays done and nothing becomes
// claimable), so it must not stamp the current generation either, or a later
// scan would suppress on a verdict no current lane produced.
func TestForcedScanLeavesAnUnstampedVerdictUnstamped(t *testing.T) {
	ctx := context.Background()
	dbh := openTestDB(t)
	root := t.TempDir()
	lib, err := library.New(dbh).Add(ctx, root, "M", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	path := writeTestAudioFile(t, root, "a.flac")
	repo := scan.New(dbh)
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{FilePath: path, Status: scan.StatusPending,
		Track: models.Track{ArtistName: "A", TrackName: "T"}, Outdir: root, Filename: "a.lrc"}},
		scan.UpsertOptions{ForceStatus: true}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := dbh.Exec(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status,
             providers_version, timing_outcome, overrun_magnitude, overrun_ratio)
         VALUES ('A', 'T', 'a', 't', ?, 'done', 0, 'categorical', 300, 4.0)`, path); err != nil {
		t.Fatalf("seed work_queue: %v", err)
	}

	q := queue.NewDBQueue(dbh)
	q.SetProvidersVersion(5)
	counted := &countingQueue{WorkQueue: q}
	e := scan.Enqueuer{Results: repo, Cache: cache.New(dbh), Queue: counted, Priority: queue.PriorityScan,
		Timing: scan.TimingVerdicts{Reader: q}, ProvidersVersion: 5}
	if _, _, err := e.EnqueuePending(ctx, lib); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if counted.calls != 1 {
		t.Fatalf("Enqueue calls = %d; want 1 (setup: an unstamped verdict is not suppressed)", counted.calls)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue err = %v; want sql.ErrNoRows (the enqueue must not reopen the settled row)", err)
	}
	outcome, version, _, found, err := q.LookupTiming(ctx, "A", "T")
	if err != nil || !found || outcome != "categorical" || version != 0 {
		t.Fatalf("verdict = (%q, generation %d, found %v, err %v); want (categorical, generation 0)", outcome, version, found, err)
	}
}
