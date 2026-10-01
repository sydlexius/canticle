package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// LatestServedTrack is the durable evidence #1195 seeds a lane's liveness
// control from: the most recent row the lane was stamped onto, with a usable
// identity, and nothing from any other lane.
func TestLatestServedTrack(t *testing.T) {
	ctx := context.Background()
	sqlDB := openQueueTestDB(t)
	q := NewDBQueue(sqlDB)

	if _, found, err := q.LatestServedTrack(ctx, "petitlyrics"); err != nil || found {
		t.Fatalf("empty table: found=%v err=%v; want none, no error", found, err)
	}

	add := func(artist, albumArtist, title, lane, completed string) int64 {
		t.Helper()
		item, err := q.Enqueue(ctx, models.Inputs{
			Track:      models.Track{ArtistName: artist, AlbumArtist: albumArtist, TrackName: title, AlbumName: "Album"},
			SourcePath: "/lib/" + artist + title + ".mp3",
		}, 1)
		if err != nil {
			t.Fatalf("Enqueue: %v", err)
		}
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE work_queue SET provider_lane = ?, completed_at = ?, artist = ?, title = ?, status = 'done' WHERE id = ?`,
			lane, completed, artist, title, item.ID); err != nil {
			t.Fatalf("stamp row: %v", err)
		}
		return item.ID
	}
	add("Old", "", "Older Win", "petitlyrics", "2026-09-01T00:00:00Z")
	add("Mid", "Band", "Recent Win", "petitlyrics", "2026-09-20T00:00:00Z")
	add("Other", "", "Newest Other Lane", "musixmatch", "2026-09-30T00:00:00Z")
	add("   ", "", "Blank Artist", "petitlyrics", "2026-09-29T00:00:00Z")
	add("Someone", "", "  ", "petitlyrics", "2026-09-28T00:00:00Z")
	add("", "Various Artists", "Generic Album Artist", "petitlyrics", "2026-09-27T00:00:00Z")

	// A stamp outlives the win it recorded (#1195 review F2). Each of these is
	// NEWER than the real win and must lose to it, or the lane is seeded with a
	// track it demonstrably did not serve.
	settle := func(id int64, status, lastErr string) {
		t.Helper()
		if _, err := sqlDB.ExecContext(ctx,
			`UPDATE work_queue SET status = ?, last_error = ? WHERE id = ?`, status, lastErr, id); err != nil {
			t.Fatalf("settle row: %v", err)
		}
	}
	// purge-provenance keeps provider_lane, then the lane misses and RetireMiss
	// settles the row unavailable with a fresh completed_at.
	settle(add("Purged", "", "Purged Then Retired", "petitlyrics", "2026-09-29T12:00:00Z"),
		"unavailable", missLimitReachedError)
	// prune.retireUnresolvable settles 'done' with a non-empty last_error.
	settle(add("Gone", "", "Prune Retired", "petitlyrics", "2026-09-29T13:00:00Z"),
		"done", UnresolvableGoneError)
	// Purged and still waiting to be re-fetched.
	settle(add("Reset", "", "Purged Deferred", "petitlyrics", "2026-09-29T14:00:00Z"),
		"deferred", "")

	got, found, err := q.LatestServedTrack(ctx, "petitlyrics")
	if err != nil || !found {
		t.Fatalf("found=%v err=%v; want a row", found, err)
	}
	want := models.Track{ArtistName: "Band", TrackName: "Recent Win", AlbumName: "Album", AlbumArtist: "Band"}
	if got != want {
		t.Errorf("got %+v; want %+v (most recent petitlyrics row with a usable identity, artist resolved as the worker queries it)", got, want)
	}

	if _, found, err := q.LatestServedTrack(ctx, "innertube"); err != nil || found {
		t.Errorf("lane with no rows: found=%v err=%v; want none", found, err)
	}

	// The seed carries the stored identity untrimmed, exactly as the worker
	// queries it (ResolveArtist returns the track-artist fallback as-is), or a
	// padded tag makes the probe ask for a track the lane never served.
	add(" Padded ", "", "Padded Win ", "petitlyrics", "2026-09-30T06:00:00Z")
	got, found, err = q.LatestServedTrack(ctx, "petitlyrics")
	if err != nil || !found {
		t.Fatalf("padded: found=%v err=%v; want a row", found, err)
	}
	if got.ArtistName != " Padded " || got.TrackName != "Padded Win " {
		t.Errorf("padded: got artist %q title %q; want the stored values untrimmed", got.ArtistName, got.TrackName)
	}
}
