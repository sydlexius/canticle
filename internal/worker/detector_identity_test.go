package worker

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// TestRunOnceDetectorInstrumentalCarriesRecordingIdentity is the worker half of
// #530. The queue stores no ISRC or duration, so the worker restores them from
// the file's tags before dispatch; a detector settle must hand that identity on
// to the song the writer sees and to the completion-provenance stamp (the row's
// isrc column, a fallback identity for prune). A duration alone is not asserted
// on the stamp (it has no column); it is asserted on the written song.
func TestRunOnceDetectorInstrumentalCarriesRecordingIdentity(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{{
		ID: 31,
		Inputs: models.Inputs{
			Track: models.Track{
				ArtistName:  "Fennel Quartet",
				TrackName:   "Slow Lantern",
				AlbumName:   "Quiet Rooms",
				AlbumArtist: "Various Fennel",
			},
			Outdir:     "out",
			Filename:   "slow-lantern.lrc",
			SourcePath: "/music/slow-lantern.flac",
		},
	}}}
	wr := &fakeWriter{}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, wr)
	w.EnableAudioDetector(&fakeDetector{instrumental: true})
	w.SetInstrumentalDetectionDefault(true)
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{
		TrackLength: 241, ISRC: "ZZXXX2600001", AlbumName: "Quiet Rooms",
	}}).read)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(q.completed) != 1 || q.completed[0] != 31 {
		t.Fatalf("completed = %v; want [31] (precondition)", q.completed)
	}
	if len(wr.songs) != 1 {
		t.Fatalf("writer saw %d songs; want 1", len(wr.songs))
	}
	got := wr.songs[0].Track
	if got.Instrumental != 1 || got.ISRC != "ZZXXX2600001" || got.TrackLength != 241 || got.AlbumArtist != "Various Fennel" {
		t.Errorf("written song track = %+v; want instrumental carrying ISRC, TrackLength 241 and AlbumArtist", got)
	}
	if prov := q.provenance[31]; prov.ISRC != "ZZXXX2600001" {
		t.Errorf("stamped ISRC = %q; want the file's ISRC", prov.ISRC)
	}
}
