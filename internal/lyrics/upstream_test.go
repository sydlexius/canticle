package lyrics

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestRecordedUpstream pins the four arms of the shared tag/row rule (#1297).
func TestRecordedUpstream(t *testing.T) {
	for _, tc := range []struct {
		name string
		song models.Song
		want string
	}{
		{"no lane", models.Song{Upstream: "lyricfind"}, ""},
		{"detector lane", models.Song{WinningLane: DetectorLaneName, Upstream: "lyricfind"}, ""},
		{"detector version", models.Song{WinningLane: "innertube", DetectorVersion: "1.0", Upstream: "lyricfind"}, ""},
		{"plain lane", models.Song{WinningLane: "innertube", Upstream: "lyricfind"}, "lyricfind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RecordedUpstream(tc.song); got != tc.want {
				t.Errorf("RecordedUpstream = %q; want %q", got, tc.want)
			}
		})
	}
}
