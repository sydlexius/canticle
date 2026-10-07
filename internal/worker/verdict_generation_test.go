package worker

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// countingEnqueuer counts the enqueues a scan pass makes.
type countingEnqueuer struct {
	q     *queue.DBQueue
	calls int
}

func (c *countingEnqueuer) Enqueue(ctx context.Context, inputs models.Inputs, priority int) (queue.WorkItem, error) {
	c.calls++
	return c.q.Enqueue(ctx, inputs, priority)
}

// TestRunOnce_CategoricalVerdictCarriesItsGeneration is #825 end to end on
// real SQLite: a row first written at generation 0 (a CLI scan) is fetched by
// a serve worker at generation 5 and settles categorical. The verdict must
// record the generation its lanes answered under (#1386), and a later scan
// under that generation must then suppress the track rather than reserve and
// re-enqueue it.
func TestRunOnce_CategoricalVerdictCarriesItsGeneration(t *testing.T) {
	ctx := context.Background()
	primary := &fakeFetcher{song: fallthroughSong(400, "wrong recording")}
	secondary := &fakeFetcher{err: musixmatch.ErrNotFound}
	rig, w := newFallthroughRig(t, primary, secondary) // enqueued at generation 0
	rig.q.SetProvidersVersion(5)
	w.SetProvidersVersion(5)

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	outcome, version, _, found, err := rig.q.LookupTiming(ctx, "Synthetic Artist", "Synthetic Title")
	if err != nil || !found || outcome != "categorical" || version != 5 {
		t.Fatalf("verdict = (%q, generation %d, found %v, err %v); want (categorical, generation 5)", outcome, version, found, err)
	}

	root := t.TempDir()
	lib, err := library.New(rig.db).Add(ctx, root, "M", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("add library: %v", err)
	}
	repo := scan.New(rig.db)
	if err := repo.Upsert(ctx, lib.ID, []models.ScanResult{{FilePath: filepath.Join(root, "track.flac"),
		Track: models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"}, Outdir: root, Filename: "track.lrc"}},
		scan.UpsertOptions{}); err != nil {
		t.Fatalf("upsert scan result: %v", err)
	}
	counted := &countingEnqueuer{q: rig.q}
	e := scan.Enqueuer{Results: repo, Cache: rig.cache, Queue: counted, Priority: queue.PriorityScan,
		Timing: scan.TimingVerdicts{Reader: rig.q}, ProvidersVersion: 5}
	if _, _, err := e.EnqueuePending(ctx, lib); err != nil {
		t.Fatalf("EnqueuePending: %v", err)
	}
	if counted.calls != 0 {
		t.Fatalf("Enqueue calls = %d; want 0 (the verdict reached at this generation must suppress the track)", counted.calls)
	}
}

// TestRunOnce_CacheHitVerdictKeepsTheStamp: a cache hit judges a stored lyric
// whose fetch generation was never recorded, so its verdict keeps the row's
// stamp. With a configured generation a hit happens only when the stamp
// already matches, so the unconfigured worker (generation 0) is where writing
// one would show: it would erase the row's recorded generation 7.
func TestRunOnce_CacheHitVerdictKeepsTheStamp(t *testing.T) {
	ctx := context.Background()
	primary := &fakeFetcher{song: fallthroughSong(90, "fresh")}
	secondary := &fakeFetcher{err: musixmatch.ErrNotFound}
	rig, w := newFallthroughRig(t, primary, secondary)
	if _, err := rig.db.ExecContext(ctx, `UPDATE work_queue SET providers_version = 7 WHERE id = ?`, rig.id); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}
	cached, err := encodeSong(fallthroughSong(90, "cached"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := rig.cache.Store(ctx, "Synthetic Artist", "Synthetic Title",
		normalize.DurationBucket(fallthroughFileSeconds), cached); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if primary.calls != 0 {
		t.Fatalf("primary calls = %d; want 0 (setup: the verdict must come from the cache)", primary.calls)
	}
	outcome, version, _, found, err := rig.q.LookupTiming(ctx, "Synthetic Artist", "Synthetic Title")
	if err != nil || !found || outcome != "ok" || version != 7 {
		t.Fatalf("verdict = (%q, generation %d, found %v, err %v); want (ok, generation 7): a cache hit names no generation", outcome, version, found, err)
	}
}
