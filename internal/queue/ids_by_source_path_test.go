package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// IDsBySourcePaths is the file-to-row seam the revalidate CLI stamps through
// (#1082): it must find a settled row by its audio path, return nothing for an
// unknown path, and never return a row the worker is mid-write on.
func TestIDsBySourcePaths(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))

	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Song"},
		SourcePath: "/lib/a.mp3",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	// Pending is not processing, so it resolves.
	ids, err := q.IDsBySourcePaths(ctx, []string{"/lib/a.mp3"})
	if err != nil {
		t.Fatalf("IDsBySourcePaths: %v", err)
	}
	if len(ids) != 1 || ids[0] != item.ID {
		t.Errorf("ids = %v, want [%d]", ids, item.ID)
	}

	if ids, err = q.IDsBySourcePaths(ctx, []string{"/lib/other.mp3"}); err != nil || len(ids) != 0 {
		t.Errorf("unknown path: ids = %v err = %v, want empty", ids, err)
	}

	// Dequeue moves the row to processing: the worker owns it now.
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if ids, err = q.IDsBySourcePaths(ctx, []string{"/lib/a.mp3"}); err != nil || len(ids) != 0 {
		t.Errorf("processing row: ids = %v err = %v, want empty", ids, err)
	}

}

// TestIDsBySourcePathsMatchesAnyCandidate: the row names one of several
// same-stem copies, so any candidate must find it; an empty list is a no-op.
func TestIDsBySourcePathsMatchesAnyCandidate(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Song"},
		SourcePath: "/lib/a.mp3",
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ids, err := q.IDsBySourcePaths(ctx, []string{"/lib/a.flac", "/lib/a.mp3", "/lib/a.MP3"})
	if err != nil || len(ids) != 1 || ids[0] != item.ID {
		t.Errorf("ids = %v err = %v, want [%d]", ids, err, item.ID)
	}
	if ids, err = q.IDsBySourcePaths(ctx, nil); err != nil || len(ids) != 0 {
		t.Errorf("empty list: ids = %v err = %v, want empty", ids, err)
	}
}
