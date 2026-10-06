package lyrics

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// TestDecodeCachedSong_LaneDegradesSafely pins what a cache hit may restore
// (#1207): a legacy entry (encoded before the lane was stored) and an entry
// whose lane is not a built-in provider both decode laneless, never guessed and
// never written into [source:]; a known lane comes back canonical.
func TestDecodeCachedSong_LaneDegradesSafely(t *testing.T) {
	track := models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"}
	at := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	legacy, err := json.Marshal(models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "words"}, WinningLane: "musixmatch", FetchedAt: at})
	if err != nil {
		t.Fatal(err)
	}
	// withEnvelope adds envelope keys to the legacy encoding, so every case
	// carries a real decodable song.
	withEnvelope := func(extra map[string]any) string {
		var m map[string]any
		if err := json.Unmarshal(legacy, &m); err != nil {
			t.Fatal(err)
		}
		for k, v := range extra {
			m[k] = v
		}
		b, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct {
		name, raw, wantLane string
		wantFetched         time.Time
	}{
		{"legacy entry", string(legacy), "", time.Time{}},
		{"detector lane", withEnvelope(map[string]any{"Lane": DetectorLaneName, "Fetched": at}), "", at},
		{"unknown lane", withEnvelope(map[string]any{"Lane": "[source:evil]\n"}), "", time.Time{}},
		{"known lane, any case", withEnvelope(map[string]any{"Lane": " MusixMatch ", "Fetched": at}), "musixmatch", at},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeCachedSong(tc.raw, track)
			if got.WinningLane != tc.wantLane || !got.FetchedAt.Equal(tc.wantFetched) {
				t.Errorf("decoded lane %q fetched %v; want %q %v", got.WinningLane, got.FetchedAt, tc.wantLane, tc.wantFetched)
			}
		})
	}
}
