package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// fakeMetadataReader stands in for scanner.ReadAudioMetadata so worker tests
// exercise the fetch-time refresh without binary audio fixtures.
type fakeMetadataReader struct {
	meta  scanner.AudioMetadata
	err   error
	calls int
	paths []string
}

func (r *fakeMetadataReader) read(path string) (scanner.AudioMetadata, error) {
	r.calls++
	r.paths = append(r.paths, path)
	if r.err != nil {
		return scanner.AudioMetadata{}, r.err
	}
	return r.meta, nil
}

// queuedItem builds a work item shaped like the queue path: album and
// album_artist survive the round trip through work_queue, duration and ISRC do
// not (#584).
func queuedItem(sourcePath string) queue.WorkItem {
	return queue.WorkItem{
		ID: 1,
		Inputs: models.Inputs{
			Track: models.Track{
				ArtistName: "Artist",
				TrackName:  "Title",
				AlbumName:  "Backfilled Album",
			},
			Outdir:     "/out",
			Filename:   "track.lrc",
			SourcePath: sourcePath,
		},
	}
}

func refreshFetcher() *fakeFetcher {
	return &fakeFetcher{song: models.Song{Lyrics: models.Lyrics{LyricsBody: "fresh lyrics"}}}
}

func newRefreshWorker(t *testing.T, q *fakeQueue, c *fakeCache, f *fakeFetcher, r *fakeMetadataReader) *Worker {
	t.Helper()
	w := New(q, c, f, &fakeWriter{})
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader(r.read)
	return w
}

// TestRefresh_SendsDurationAndISRCToProvider is the core parity assertion: the
// track handed to the provider carries the on-disk duration and ISRC even though
// work_queue stored neither.
func TestRefresh_SendsDurationAndISRCToProvider(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	f := refreshFetcher()
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180, ISRC: "USAB11800001", AlbumName: "Tag Album"}}

	w := newRefreshWorker(t, q, &fakeCache{}, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(f.tracks) != 1 {
		t.Fatalf("provider called %d times; want 1", len(f.tracks))
	}
	got := f.tracks[0]
	if got.TrackLength != 180 {
		t.Errorf("TrackLength = %d; want 180 (q_duration would be omitted)", got.TrackLength)
	}
	if got.ISRC != "USAB11800001" {
		t.Errorf("ISRC = %q; want USAB11800001 (track_isrc would be omitted)", got.ISRC)
	}
	if len(r.paths) != 1 || r.paths[0] != "/library/track.flac" {
		t.Errorf("read paths = %v; want [/library/track.flac]", r.paths)
	}
}

// TestRefresh_CacheBucketsAgree verifies the refresh happens at the single
// mutation point, so the lookup and store keys are the same non-zero bucket.
func TestRefresh_CacheBucketsAgree(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	c := &fakeCache{}
	f := refreshFetcher()
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180}}

	w := newRefreshWorker(t, q, c, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	want := normalize.DurationBucket(180)
	if want == 0 {
		t.Fatal("test setup error: expected a non-zero bucket for 180 seconds")
	}
	if len(c.lookupBuckets) != 1 || c.lookupBuckets[0] != want {
		t.Errorf("lookup buckets = %v; want [%d]", c.lookupBuckets, want)
	}
	if len(c.stores) != 1 || c.stores[0].bucket != want {
		t.Errorf("store buckets = %+v; want one entry with bucket %d", c.stores, want)
	}
}

// TestRefresh_DoesNotClobberAlbumWithEmptyTag locks in the fresh-when-present
// rule: a file with no album tag must not clear the album migration 032
// backfilled onto the row, because q_album is sent unconditionally.
func TestRefresh_DoesNotClobberAlbumWithEmptyTag(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	f := refreshFetcher()
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180}} // no album tag

	w := newRefreshWorker(t, q, &fakeCache{}, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(f.tracks) != 1 {
		t.Fatalf("provider called %d times; want 1", len(f.tracks))
	}
	if got := f.tracks[0].AlbumName; got != "Backfilled Album" {
		t.Errorf("AlbumName = %q; want %q", got, "Backfilled Album")
	}
}

// TestRefresh_ReadErrorIsNonFatal verifies a metadata read failure degrades to
// the enqueue-time identity instead of failing or deferring the item; a vanished
// file is prune's business, not the fetch path's.
func TestRefresh_ReadErrorIsNonFatal(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/gone.flac")}}
	f := refreshFetcher()
	r := &fakeMetadataReader{err: errors.New("open: no such file")}

	w := newRefreshWorker(t, q, &fakeCache{}, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if len(f.tracks) != 1 {
		t.Fatalf("provider called %d times; want 1 (the item must still be attempted)", len(f.tracks))
	}
	if got := f.tracks[0].TrackLength; got != 0 {
		t.Errorf("TrackLength = %d; want 0 (the enqueue-time value)", got)
	}
	if len(q.failed) != 0 {
		t.Errorf("failed = %v; want none (a metadata read failure must not fail the item)", q.failed)
	}
}

// TestRefresh_SkippedWhenSourcePathEmpty verifies no read is attempted when there
// is no file to read.
func TestRefresh_SkippedWhenSourcePathEmpty(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("")}}
	f := refreshFetcher()
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180}}

	w := newRefreshWorker(t, q, &fakeCache{}, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if r.calls != 0 {
		t.Errorf("reader called %d times; want 0", r.calls)
	}
}

// TestRefresh_SkippedWhenEnrichmentDisabled verifies the operator's global
// enrichment opt-out suppresses the fetch-time read entirely, keeping serve mode
// at CLI parity in that configuration rather than exceeding it.
func TestRefresh_SkippedWhenEnrichmentDisabled(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	f := refreshFetcher()
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180}}

	w := New(q, &fakeCache{}, f, &fakeWriter{})
	w.SetMetadataReader(r.read)
	w.SetRecordingEnrichmentDefault(false)

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if r.calls != 0 {
		t.Errorf("reader called %d times; want 0 when enrichment is disabled", r.calls)
	}
	if len(f.tracks) != 1 {
		t.Fatalf("provider called %d times; want 1", len(f.tracks))
	}
	if got := f.tracks[0].TrackLength; got != 0 {
		t.Errorf("TrackLength = %d; want 0", got)
	}
}

// TestRefresh_TimingOutcomeUsesFileDurationNotProviderLength pins the call site
// that stamps the timing verdict (#440). The unit tests call the helper with a
// hand-passed duration, so nothing there catches a regression that feeds the
// provider's catalog length instead of the audio file's -- the exact I2 bug.
// This drives the full RunOnce path: the metadata reader supplies a 180s file
// duration onto resolvedTrack, while the provider returns a song whose own
// Track.TrackLength is a much larger 600s and whose last cue at 300s overruns
// the FILE but sits comfortably inside the provider length. The verdict must be
// categorical (300 vs 180), proving the file duration reached the stamp; against
// the provider length it would read ok and this test fails.
func TestRefresh_TimingOutcomeUsesFileDurationNotProviderLength(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	f := &fakeFetcher{song: models.Song{
		Track: models.Track{TrackLength: 600}, // provider catalog length
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "a", Time: models.Time{Total: 10}},
			{Text: "b", Time: models.Time{Total: 300}}, // overruns 180, fits 600
		}},
	}}
	r := &fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: 180}} // file duration

	w := newRefreshWorker(t, q, &fakeCache{}, f, r)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rec, ok := q.timingOutcomes[1]
	if !ok {
		t.Fatal("no timing outcome stamped")
	}
	if rec.Outcome != "categorical" {
		t.Errorf("timing_outcome = %q; want categorical (300s cue vs 180s file). "+
			"ok would mean the provider's 600s length was used instead of the file duration", rec.Outcome)
	}
	if !rec.Measured {
		t.Errorf("Measured = false; want true (a real comparison against the 180s file happened)")
	}
	// magnitude is 300 - 180 = 120 against the file; against the provider it
	// would be negative.
	if rec.Magnitude != 120 {
		t.Errorf("overrun magnitude = %v; want 120 (300s - 180s file duration)", rec.Magnitude)
	}
}
