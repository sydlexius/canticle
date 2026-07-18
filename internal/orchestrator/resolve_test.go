package orchestrator

import (
	"context"
	"testing"
	"time"

	"github.com/doxazo-net/canticle/internal/circuit"
	"github.com/doxazo-net/canticle/internal/models"
)

// resolveStubProvider is a minimal providers.LyricsProvider for lane tests.
type resolveStubProvider struct {
	name string
	song models.Song
	err  error
	got  models.Track
}

func (s *resolveStubProvider) Name() string { return s.name }
func (s *resolveStubProvider) FindLyrics(_ context.Context, t models.Track) (models.Song, error) {
	s.got = t
	return s.song, s.err
}

func TestProviderLane_IgnoresSourcePath(t *testing.T) {
	p := &resolveStubProvider{name: "stub", song: models.Song{Track: models.Track{TrackName: "x"}}}
	lane := NewProviderLane(p, circuit.New(60*time.Second, 30*time.Minute))
	got, err := lane.FindLyrics(context.Background(), models.Track{TrackName: "x"}, "/music/x.flac")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got.Track.TrackName != "x" {
		t.Fatalf("song = %+v", got)
	}
	if lane.Name() != "stub" {
		t.Fatalf("name = %q", lane.Name())
	}
}
