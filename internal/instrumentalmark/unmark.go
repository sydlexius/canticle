package instrumentalmark

import (
	"context"
	"fmt"
	"sync"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/sidecar"
)

// rowLocks is a keyed mutex: one lock per work item id, created on demand and
// dropped when the last holder leaves. The zero value is ready to use.
type rowLocks struct {
	mu sync.Mutex
	m  map[int64]*rowLock
}

type rowLock struct {
	mu sync.Mutex
	n  int
}

// lock blocks until id's lock is held and returns its release. It does not
// honor context cancellation: the holders run short local operations.
func (r *rowLocks) lock(id int64) func() {
	r.mu.Lock()
	if r.m == nil {
		r.m = make(map[int64]*rowLock)
	}
	l := r.m[id]
	if l == nil {
		l = &rowLock{}
		r.m[id] = l
	}
	l.n++
	r.mu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		r.mu.Lock()
		l.n--
		if l.n == 0 {
			delete(r.m, id)
		}
		r.mu.Unlock()
	}
}

// Unmark withdraws a hand-made instrumental mark from work_queue row id and
// re-queues the row, so the track is fetched, upgraded and rechecked like any
// other. Order, so that no failure re-queues a row while its manual marker is
// still on disk (the writer refuses to replace a manual marker, so a worker
// fetching such a row could never write its result): (1) refuse a missing or
// in-flight row; (2) a row that is not marked is a no-op (OutcomeNotMarked),
// even if a manual marker file sits on disk; (3) inventory every lyric file
// beside the audio with Mark's inventory (confined to the row's library root;
// a symlinked lyric file is refused with ErrSymlinkedSidecar); (4) hand each to
// opts.Report (OpUnmark) BEFORE anything changes; (5) remove each that is a
// manual marker, the writer re-checking immediately before the unlink that it
// still is one, so a .txt that became real lyrics since is left alone; (6) one
// transaction clears the mark, invalidates the track's cache entries and
// re-queues the row.
//
// After an unmark the track is fetched like any other, so a lyric file left
// beside it (real lyrics the operator wrote over the marker) can be replaced by
// the fetch; that is why every lyric file is backed up, not only the markers.
// Only manual markers are removed; a real lyric file is backed up and left.
//
// Partial failures: a backup failure leaves row and files untouched. A removal
// failure after some markers were removed leaves the row marked with some or
// none of its markers (a protected row, nothing is re-queued); calling Unmark
// again removes the rest and finishes, and calling Mark again rewrites the
// markers. A failure in step 6 leaves the row marked with no marker; the same
// two retries recover it. Because the row stays marked until every marker is
// gone, the dangerous state (an unmarked, queued row beside a manual marker)
// is reachable only if a marker appears after step 5, from another process.
//
// Unmark does NOT restore the lyrics Mark backed up: the row goes back through
// the queue and is fetched afresh. Restoring from the mark's JSONL record
// stays an operator action. Result.FilesBackedUp counts every file backed up
// (and in a dry run, the ones that would be).
//
// Mark and Unmark on the same row, through the same Marker, are serialized by
// a per-row lock, so within one process a concurrent pair cannot leave a marker
// beside an unmarked row. The lock does not span processes: a CLI process and
// the serve process each hold their own Marker, so between processes only the
// re-checks apply (Mark's ErrMarkWithdrawn re-reads, Unmark's pre-unlink check).
func (m *Marker) Unmark(ctx context.Context, id int64, opts Options) (Result, error) {
	defer m.locks.lock(id)()
	t, err := m.load(ctx, id)
	if err != nil {
		return Result{}, err
	}
	if t.status == queue.StatusProcessing {
		return Result{}, queue.ErrManualInstrumentalInFlight
	}
	if !t.marked {
		return Result{Outcome: OutcomeNotMarked}, nil
	}
	dirs, err := m.resolveDirs(ctx, id, &t)
	if err != nil {
		return Result{}, err
	}
	files, _, err := inventory(t, dirs, true)
	if err != nil {
		return Result{}, err
	}
	if opts.DryRun {
		return Result{Outcome: OutcomeDryRun, FilesBackedUp: len(files)}, nil
	}
	if len(files) > 0 && opts.Report == nil {
		return Result{}, ErrNoBackupSink
	}
	reported := make(map[string]bool)
	for _, p := range files {
		if rerr := backup(OpUnmark, id, p, opts.Report, reported); rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: backup of work item %d failed, nothing changed: %w", id, rerr)
		}
	}
	removed := 0
	for _, p := range files {
		if sidecar.KindOf(p) != sidecar.KindUnsynced {
			continue
		}
		ok, rerr := m.w.RemoveManualMarker(p)
		if rerr != nil {
			return Result{}, fmt.Errorf("instrumentalmark: work item %d is still marked and %d markers were removed; retry the unmark to finish: %w",
				id, removed, stripPath(rerr))
		}
		if ok {
			removed++
		}
	}
	changed, err := m.unmark(ctx, id, t.keys)
	if err != nil {
		return Result{}, fmt.Errorf("instrumentalmark: work item %d is still marked with %d markers removed; retry the unmark to finish: %w", id, removed, err)
	}
	if !changed {
		return Result{Outcome: OutcomeNotMarked}, nil
	}
	return Result{Outcome: OutcomeUnmarked, FilesBackedUp: len(reported)}, nil
}
