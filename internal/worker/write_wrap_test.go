package worker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

// colonOutdir and colonName hold ": " (the separator failsig splits on) so a
// path carried by the wrap would corrupt the normalized label (#1336).
const (
	colonOutdir = "Artist: The Band/Album: Live"
	colonName   = "01. Intro: Part One.lrc"
)

func assertNoPathInCause(t *testing.T, cause error) {
	t.Helper()
	if cause == nil {
		t.Fatal("no cause recorded")
	}
	for name, got := range map[string]string{"last_error": cause.Error(), "label": failsig.Normalize(cause.Error())} {
		for _, frag := range []string{"Artist", "The Band", "Album", "Live", "Intro", "Part One", ".lrc", ".tmp"} {
			if strings.Contains(got, frag) {
				t.Errorf("%s %q carries path fragment %q", name, got, frag)
			}
		}
	}
}

// writeErrorsWithPaths returns the writer failures that embed the library path:
// real OS errors (alone and nested in a writer-style wrap) and the refusal
// error of the real confined lyrics writer.
func writeErrorsWithPaths(t *testing.T) map[string]error {
	t.Helper()
	full := "/lib/" + colonOutdir + "/" + colonName
	root := t.TempDir()
	refused := lyrics.NewLRCWriter(root).WriteLRC(
		models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}, Lyrics: models.Lyrics{LyricsBody: "x"}},
		colonName, root+"/"+colonOutdir)
	if refused == nil {
		t.Fatal("expected the lyrics writer to refuse a missing output dir")
	}
	return map[string]error{
		"plain":          errors.New("disk full"),
		"lyrics refusal": refused,
		"path error":     &fs.PathError{Op: "open", Path: full + ".tmp", Err: syscall.ENOSPC},
		"wrapped path":   fmt.Errorf("creating temp file: %w", &fs.PathError{Op: "open", Path: full, Err: syscall.EACCES}),
		"link error":     &os.LinkError{Op: "rename", Old: full + ".tmp", New: full, Err: syscall.EROFS},
	}
}

func TestWriteFailureWrapOmitsOutputPath(t *testing.T) {
	track := models.Track{ArtistName: "A", TrackName: "T"}
	in := models.Inputs{Track: track, Outdir: "/lib/" + colonOutdir, Filename: colonName}
	for name, werr := range writeErrorsWithPaths(t) {
		t.Run(name, func(t *testing.T) {
			writer := &fakeWriter{err: werr}

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
		})
	}
}

// The word-recheck write wrap shares the helper; pin it directly since its rig
// is heavy.
func TestScrubWritePathsKeepsChain(t *testing.T) {
	pe := &fs.PathError{Op: "open", Path: colonOutdir + "/" + colonName, Err: syscall.ENOSPC}
	got := scrubWritePaths(fmt.Errorf("creating temp file: %w", pe))
	if want := "creating temp file: open: no space left on device"; got.Error() != want {
		t.Errorf("message = %q, want %q", got.Error(), want)
	}
	if !errors.Is(got, syscall.ENOSPC) {
		t.Error("errno lost from the Unwrap chain")
	}
	if scrubWritePaths(nil) != nil {
		t.Error("nil must stay nil")
	}
}
