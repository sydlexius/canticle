package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
)

func wordStateAt(t *testing.T, ctx context.Context, sqlDB *sql.DB, sourcePath string) (status, state string) {
	t.Helper()
	var s sql.NullString
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT status, word_timing_state FROM work_queue WHERE source_path = ?`, sourcePath).Scan(&status, &s); err != nil {
		t.Fatalf("read work_queue: %v", err)
	}
	return status, s.String
}

// A word-recheck row (#982, 'deferred' + 'queued') whose source is gone for
// good is retired KEEPING 'queued' (#1039 review M1): as 'done' it is never
// dequeued, and 'queued' is what keeps the recheck candidate predicate from
// re-flipping a gone source on every later apply.
func TestSweep_RetiredWordRecheckRowIsNotACandidate(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistGone", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "deferred", "", "")
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET word_timing_state = 'queued',
        outcome_type = 'synced', timing_outcome = 'ok' WHERE source_path = ?`, gone); err != nil {
		t.Fatalf("stamp state: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	sweepExact(t, ctx, sqlDB)
	if st, ws := wordStateAt(t, ctx, sqlDB, gone); st != "done" || ws != queue.WordTimingQueued {
		t.Fatalf("retired row = (%q,%q); want (done, queued)", st, ws)
	}
	if n, err := queue.NewDBQueue(sqlDB).CountWordRecheckCandidates(ctx, queue.WordRecheckOptions{}); err != nil || n != 0 {
		t.Fatalf("CountWordRecheckCandidates = (%d, %v); want 0 -- a gone source is not re-flippable", n, err)
	}
}

// A resurrecting relink reopens the row for an ordinary fetch, so the 'queued'
// state a retirement keeps on a 'done' row is cleared and the dequeued item
// takes the ordinary path.
func TestSweep_ResurrectionClearsWordRecheckQueued(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")
	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	sweepExact(t, ctx, sqlDB)
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET word_timing_state = 'queued' WHERE source_path = ?`, gone); err != nil {
		t.Fatalf("stamp legacy state: %v", err)
	}
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
	if res := sweepExact(t, ctx, sqlDB); len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d; want 1", len(res.Relinked))
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Dequeue(ctx)
	if err != nil || item.Inputs.SourcePath != moved || item.WordTimingState != "" {
		t.Fatalf("Dequeue = (%q, %q, %v); want (%q, \"\") -- an ordinary fetch", item.Inputs.SourcePath, item.WordTimingState, err, moved)
	}
}
