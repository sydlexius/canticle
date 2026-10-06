package worker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// colonOutdir and colonName hold ": " (the separator failsig splits on) so a
// path carried by the wrap would corrupt the normalized label (#1336).
const (
	colonOutdir = "/lib/Artist: The Band/Album: Live"
	colonName   = "01. Intro: Part One.lrc"
)

func assertNoPathInCause(t *testing.T, cause error) {
	t.Helper()
	if cause == nil {
		t.Fatal("no cause recorded")
	}
	for name, got := range map[string]string{"last_error": cause.Error(), "label": failsig.Normalize(cause.Error())} {
		for _, frag := range []string{"Artist", "The Band", "Album", "Live", "Intro", "Part One", ".lrc", "/lib"} {
			if strings.Contains(got, frag) {
				t.Errorf("%s %q carries path fragment %q", name, got, frag)
			}
		}
	}
}

func TestWriteFailureWrapOmitsOutputPath(t *testing.T) {
	track := models.Track{ArtistName: "A", TrackName: "T"}
	in := models.Inputs{Track: track, Outdir: colonOutdir, Filename: colonName}
	writer := &fakeWriter{err: errors.New("disk full")}

	t.Run("ordinary", func(t *testing.T) {
		q := &fakeQueue{items: []queue.WorkItem{{ID: 7, Inputs: in}}}
		fetcher := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "ok"}}}
		w := New(q, &fakeCache{}, fetcher, writer)
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		causes := append(append([]error{}, q.failCauses...), q.deferCauses...)
		if len(causes) != 1 {
			t.Fatalf("causes = %v; want exactly one", causes)
		}
		assertNoPathInCause(t, causes[0])
	})

	t.Run("detector instrumental", func(t *testing.T) {
		in := in
		in.SourcePath = "/music/silent.flac"
		q := &fakeQueue{items: []queue.WorkItem{{ID: 8, Inputs: in}}}
		w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, writer)
		w.EnableAudioDetector(&fakeDetector{instrumental: true})
		w.SetInstrumentalDetectionDefault(true)
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce: %v", err)
		}
		if len(q.deferCauses) != 1 {
			t.Fatalf("deferCauses = %v; want exactly one", q.deferCauses)
		}
		assertNoPathInCause(t, q.deferCauses[0])
	})
}
