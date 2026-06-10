package commands

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
)

type probeFetcher struct {
	song models.Song
	err  error
}

func (p probeFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return p.song, p.err
}

func TestFetchProbe_PrintsMatchedMetadataAndPreview(t *testing.T) {
	f := probeFetcher{song: models.Song{
		Track:     models.Track{ArtistName: "Real Artist", TrackName: "Real Title", AlbumName: "Real Album"},
		Subtitles: models.Synced{Lines: []models.Lines{{Text: "line one"}, {Text: "line two"}}},
	}}
	var buf bytes.Buffer
	code := fetchProbe(context.Background(), &buf,
		models.Track{ArtistName: "q artist", TrackName: "q title", AlbumName: "q album"}, f)
	if code != 0 {
		t.Fatalf("code=%d want 0", code)
	}
	out := buf.String()
	for _, want := range []string{
		`artist="q artist"`,
		`title="q title"`,
		`album="q album"`,
		`matched: artist="Real Artist"`,
		"synced_lines=2",
		"line one",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("probe output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestRunFetchProbe_EchoesRecordingIdentifiers drives the public fetch --probe
// path so the runFetch probe branch (which builds the models.Track from the new
// --isrc/--duration/--spotify-id flags) is exercised end to end. The injected
// fetcher is a stub; the assertion targets the query echo, which is printed from
// the input track before the provider is called.
func TestRunFetchProbe_EchoesRecordingIdentifiers(t *testing.T) {
	// runFetch calls initLogging, which replaces the global slog default logger.
	// Restore it so this test does not leak logging state into TestApplyLogLevel.
	prevLogger := slog.Default()
	t.Cleanup(func() {
		slog.SetDefault(prevLogger)
		slog.SetLogLoggerLevel(slog.LevelInfo)
	})
	cfg, _ := commandsTestEnv(t)
	deps := Deps{NewFetcher: func(string) musixmatch.Fetcher {
		return probeFetcher{song: models.Song{Track: models.Track{ArtistName: "A", TrackName: "B"}}}
	}}
	var out bytes.Buffer
	code := Run(context.Background(), []string{
		"fetch", "--probe",
		"--isrc", "USENC1234567",
		"--duration", "215",
		"--spotify-id", "abc123xyz",
		"--token", "dummy",
		"--config", cfg,
		"Some Artist,Some Title",
	}, &out, deps)
	if code != 0 {
		t.Fatalf("Run fetch --probe = %d; want 0\n--- output ---\n%s", code, out.String())
	}
	got := out.String()
	for _, want := range []string{`isrc="USENC1234567"`, "duration=215", `spotify_id="abc123xyz"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("probe output missing %q\n--- output ---\n%s", want, got)
		}
	}
}

func TestParseArtistTitle(t *testing.T) {
	tests := []struct {
		in         string
		wantArtist string
		wantTitle  string
	}{
		{"Adele,Hello", "Adele", "Hello"},
		{" Lady Gaga , Shallow ", "Lady Gaga", "Shallow"},
		{"a,b,c", "a", "b,c"},
		{"ArtistOnly", "ArtistOnly", ""},
	}
	for _, tt := range tests {
		gotArtist, gotTitle := parseArtistTitle(tt.in)
		if gotArtist != tt.wantArtist || gotTitle != tt.wantTitle {
			t.Errorf("parseArtistTitle(%q) = (%q, %q); want (%q, %q)", tt.in, gotArtist, gotTitle, tt.wantArtist, tt.wantTitle)
		}
	}
}

func TestFetchProbe_UnsyncedPreviewFallback(t *testing.T) {
	f := probeFetcher{song: models.Song{
		Track:  models.Track{ArtistName: "A", TrackName: "B"},
		Lyrics: models.Lyrics{LyricsBody: "first unsynced line\n\nsecond unsynced line\n"},
	}}
	var buf bytes.Buffer
	if code := fetchProbe(context.Background(), &buf, models.Track{ArtistName: "A", TrackName: "B"}, f); code != 0 {
		t.Fatalf("code=%d want 0", code)
	}
	out := buf.String()
	for _, want := range []string{"unsynced=true", "synced_lines=0", "first unsynced line", "second unsynced line"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q\n%s", want, out)
		}
	}
}

func TestFetchProbe_MissPrintsReason(t *testing.T) {
	f := probeFetcher{err: errors.New("no results found")}
	var buf bytes.Buffer
	code := fetchProbe(context.Background(), &buf, models.Track{ArtistName: "a", TrackName: "b"}, f)
	if code != 0 {
		t.Fatalf("code=%d want 0", code)
	}
	if !strings.Contains(buf.String(), "MISS") {
		t.Fatalf("want MISS in output, got %q", buf.String())
	}
}
