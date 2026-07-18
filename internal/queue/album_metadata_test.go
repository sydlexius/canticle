package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func TestDBQueue_EnqueueDequeueRoundTripsAlbumMetadata(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	in := models.Inputs{
		Track: models.Track{
			ArtistName:  "Lady Gaga feat. Bradley Cooper",
			TrackName:   "Shallow",
			AlbumName:   "A Star Is Born",
			AlbumArtist: "Lady Gaga",
		},
		Outdir:   "out",
		Filename: "shallow.lrc",
	}
	if _, err := q.Enqueue(ctx, in, PriorityScan); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if got.Inputs.Track.AlbumName != "A Star Is Born" {
		t.Errorf("AlbumName = %q; want %q", got.Inputs.Track.AlbumName, "A Star Is Born")
	}
	if got.Inputs.Track.AlbumArtist != "Lady Gaga" {
		t.Errorf("AlbumArtist = %q; want %q", got.Inputs.Track.AlbumArtist, "Lady Gaga")
	}
}
