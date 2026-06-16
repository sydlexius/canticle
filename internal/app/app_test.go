package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
)

type fakeFetcher struct {
	song  models.Song
	err   error
	calls []models.Track
}

func (f *fakeFetcher) FindLyrics(ctx context.Context, track models.Track) (models.Song, error) {
	if err := ctx.Err(); err != nil {
		return models.Song{}, err
	}
	f.calls = append(f.calls, track)
	if f.err != nil {
		return models.Song{}, f.err
	}
	return f.song, nil
}

func TestRunFetchesAndWritesSyncedLyrics(t *testing.T) {
	t.Parallel()

	outdir := t.TempDir()
	track := models.Track{
		ArtistName: "Test Artist",
		TrackName:  "Test Song",
	}
	fetcher := &fakeFetcher{
		song: models.Song{
			Track: models.Track{
				ArtistName:  "Test Artist",
				TrackName:   "Test Song",
				AlbumName:   "Test Album",
				TrackLength: 123,
			},
			Subtitles: models.Synced{
				Lines: []models.Lines{
					{
						Text: "first line",
						Time: models.Time{Minutes: 0, Seconds: 1, Hundredths: 23},
					},
					{
						Text: "second line",
						Time: models.Time{Minutes: 0, Seconds: 2, Hundredths: 34},
					},
				},
			},
		},
	}
	inputs := queue.NewInputsQueue()
	inputs.Push(models.Inputs{
		Track:    track,
		Outdir:   outdir,
		Filename: "test-song.lrc",
	})

	a := NewApp(fetcher, lyrics.NewLRCWriter(), inputs, 0, "multi")
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	if len(fetcher.calls) != 1 {
		t.Fatalf("FindLyrics() calls = %d, want 1", len(fetcher.calls))
	}
	if fetcher.calls[0] != track {
		t.Fatalf("FindLyrics() track = %#v, want %#v", fetcher.calls[0], track)
	}

	got, err := os.ReadFile(filepath.Join(outdir, "test-song.lrc"))
	if err != nil {
		t.Fatalf("reading LRC output: %v", err)
	}
	want := strings.Join([]string{
		"[by:mxlrcgo-svc]",
		"[ar:Test Artist]",
		"[ti:Test Song]",
		"[al:Test Album]",
		"[length:02:03]",
		"[ve:dev]",
		"[00:01.23]first line",
		"[00:02.34]second line",
		"",
	}, "\n")
	if string(got) != want {
		t.Fatalf("LRC output = %q, want %q", got, want)
	}
}

func TestRunWritesFailedFileOnFetchFailure(t *testing.T) {
	tmp := t.TempDir()
	t.Chdir(tmp)

	track := models.Track{
		ArtistName: "Failure Artist",
		TrackName:  "Failure Song",
	}
	inputs := queue.NewInputsQueue()
	inputs.Push(models.Inputs{Track: track, Outdir: filepath.Join(tmp, "lyrics")})
	fetcher := &fakeFetcher{err: errors.New("fake fetch failed")}

	a := NewApp(fetcher, lyrics.NewLRCWriter(), inputs, 0, "multi")
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(tmp, "*_failed.txt"))
	if err != nil {
		t.Fatalf("globbing failed file: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("failed file count = %d, want 1: %v", len(matches), matches)
	}

	got, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("reading failed file: %v", err)
	}
	if string(got) != "Failure Artist,Failure Song\n" {
		t.Fatalf("failed file content = %q", got)
	}
}

func TestRunBenignMissDoesNotBackOff(t *testing.T) {
	for name, sentinel := range map[string]error{
		"not found": musixmatch.ErrNotFound,
		"no lyrics": musixmatch.ErrNoLyrics,
	} {
		t.Run(name, func(t *testing.T) {
			inputs := queue.NewInputsQueue()
			inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}})
			inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Two"}})
			inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Three"}})
			fetcher := &fakeFetcher{err: fmt.Errorf("lookup: %w", sentinel)}

			a := NewApp(fetcher, lyrics.NewLRCWriter(), inputs, 0, "dir")
			a.baseBackoff = time.Second
			a.maxBackoff = time.Hour
			var sleeps []time.Duration
			a.sleep = func(_ context.Context, d time.Duration) bool {
				sleeps = append(sleeps, d)
				return true
			}

			if err := a.Run(context.Background()); err != nil {
				t.Fatalf("Run() error = %v", err)
			}

			if len(sleeps) != 0 {
				t.Fatalf("sleeps = %v; want none (benign misses must not trip the backoff timer)", sleeps)
			}
			if a.failureCount != 0 {
				t.Fatalf("failureCount = %d; want 0 (benign misses must not increment the failure counter)", a.failureCount)
			}
			if a.failed.Len() != 3 {
				t.Fatalf("failed bucket = %d; want 3 (benign misses are still reported as not-fetched)", a.failed.Len())
			}
		})
	}
}

func TestRunBacksOffGeometricallyAfterFetchFailures(t *testing.T) {
	inputs := queue.NewInputsQueue()
	inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "One"}})
	inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Two"}})
	inputs.Push(models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Three"}})
	fetcher := &fakeFetcher{err: errors.New("rate limited")}

	a := NewApp(fetcher, lyrics.NewLRCWriter(), inputs, 0, "dir")
	a.baseBackoff = time.Second
	a.maxBackoff = time.Hour
	var sleeps []time.Duration
	a.sleep = func(_ context.Context, d time.Duration) bool {
		sleeps = append(sleeps, d)
		return true
	}

	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("Run() error = %v", err)
	}

	want := []time.Duration{time.Second, 2 * time.Second}
	if len(sleeps) != len(want) {
		t.Fatalf("sleep count = %d, want %d: %v", len(sleeps), len(want), sleeps)
	}
	for i := range want {
		if sleeps[i] != want[i] {
			t.Fatalf("sleep[%d] = %s, want %s", i, sleeps[i], want[i])
		}
	}
}
