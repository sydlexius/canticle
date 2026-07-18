package orchestrator

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func syncedSong() models.Song {
	return models.Song{
		Track:     models.Track{ArtistName: "A", TrackName: "T"},
		Subtitles: models.Synced{Lines: []models.Lines{{Text: "la la"}}},
	}
}

func unsyncedSong() models.Song {
	return models.Song{
		Track:  models.Track{ArtistName: "A", TrackName: "T"},
		Lyrics: models.Lyrics{LyricsBody: "la la"},
	}
}

func instrumentalSong() models.Song {
	return models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}}
}

func emptySong() models.Song {
	return models.Song{Track: models.Track{ArtistName: "A", TrackName: "T"}}
}

func TestQualityOf(t *testing.T) {
	tests := []struct {
		name string
		song models.Song
		want Quality
	}{
		{"synced", syncedSong(), QualitySynced},
		{"unsynced", unsyncedSong(), QualityUnsynced},
		{"instrumental", instrumentalSong(), QualityInstrumental},
		{"none", emptySong(), QualityNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := QualityOf(tt.song); got != tt.want {
				t.Fatalf("QualityOf(%s) = %v; want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestQualityOrdering(t *testing.T) {
	if QualitySynced <= QualityUnsynced || QualityUnsynced <= QualityInstrumental || QualityInstrumental <= QualityNone {
		t.Fatalf("quality ordering broken: synced=%d unsynced=%d instrumental=%d none=%d",
			QualitySynced, QualityUnsynced, QualityInstrumental, QualityNone)
	}
}

// acceptGuard accepts iff accept is true and Enabled reports true.
type acceptGuard struct {
	enabled bool
	accept  bool
}

func (g acceptGuard) Enabled() bool { return g.enabled }
func (g acceptGuard) Accept(models.Song) (bool, string) {
	if g.accept {
		return true, ""
	}
	return false, "rejected"
}

func TestIsSuitable(t *testing.T) {
	enabledAccept := acceptGuard{enabled: true, accept: true}
	enabledReject := acceptGuard{enabled: true, accept: false}
	disabled := acceptGuard{enabled: false}

	tests := []struct {
		name  string
		song  models.Song
		guard ScriptGuard
		want  bool
	}{
		{"synced passes nil guard", syncedSong(), nil, true},
		{"unsynced passes nil guard", unsyncedSong(), nil, true},
		{"instrumental not suitable", instrumentalSong(), nil, false},
		{"none not suitable", emptySong(), nil, false},
		{"synced passes disabled guard", syncedSong(), disabled, true},
		{"synced passes enabled accepting guard", syncedSong(), enabledAccept, true},
		{"synced rejected by guard", syncedSong(), enabledReject, false},
		{"unsynced rejected by guard", unsyncedSong(), enabledReject, false},
		{"instrumental rejected even if guard accepts", instrumentalSong(), enabledAccept, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSuitable(tt.song, tt.guard); got != tt.want {
				t.Fatalf("IsSuitable(%s) = %v; want %v", tt.name, got, tt.want)
			}
		})
	}
}
