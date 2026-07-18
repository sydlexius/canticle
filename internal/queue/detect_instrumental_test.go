package queue

import (
	"context"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

func boolPtr(b bool) *bool { return &b }

// TestDBQueue_DetectInstrumentalRoundTrip verifies the per-item detect_instrumental
// decision is stamped on insert and read back, with NULL (nil) preserved.
func TestDBQueue_DetectInstrumentalRoundTrip(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		value *bool
	}{
		{"on", boolPtr(true)},
		{"off", boolPtr(false)},
		{"unset", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewDBQueue(openQueueTestDB(t))
			item, err := q.Enqueue(ctx, models.Inputs{
				Track:              models.Track{ArtistName: "Artist", TrackName: "Title"},
				Outdir:             "out",
				Filename:           "a.lrc",
				DetectInstrumental: tc.value,
			}, 1)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			switch {
			case tc.value == nil && item.DetectInstrumental != nil:
				t.Errorf("DetectInstrumental = %v; want nil", *item.DetectInstrumental)
			case tc.value != nil && item.DetectInstrumental == nil:
				t.Errorf("DetectInstrumental = nil; want %v", *tc.value)
			case tc.value != nil && *item.DetectInstrumental != *tc.value:
				t.Errorf("DetectInstrumental = %v; want %v", *item.DetectInstrumental, *tc.value)
			}
		})
	}
}

// TestDBQueue_DetectInstrumentalStampOnInsertOnly verifies the value is written
// only on the initial insert; an ON CONFLICT refresh of the same key leaves it
// unchanged (mirrors providers_version).
func TestDBQueue_DetectInstrumentalStampOnInsertOnly(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	q.now = func() time.Time { return time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC) }

	first, err := q.Enqueue(ctx, models.Inputs{
		Track:              models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:             "out",
		Filename:           "a.lrc",
		DetectInstrumental: boolPtr(true),
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue first: %v", err)
	}
	// Re-enqueue the same normalized key with a conflicting decision.
	second, err := q.Enqueue(ctx, models.Inputs{
		Track:              models.Track{ArtistName: "artist", TrackName: "title"},
		Outdir:             "out",
		Filename:           "a.lrc",
		DetectInstrumental: boolPtr(false),
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue conflict: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("conflict produced new row %d; want %d", second.ID, first.ID)
	}
	if second.DetectInstrumental == nil || *second.DetectInstrumental != true {
		t.Errorf("DetectInstrumental = %v; want the original true (stamp-on-insert only)", second.DetectInstrumental)
	}
}
