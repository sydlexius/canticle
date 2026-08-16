package web

import (
	"io/fs"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
	static "github.com/sydlexius/canticle/web/static"
)

func TestLaneMark(t *testing.T) {
	tests := []struct {
		name string
		lane string
		want string
	}{
		{"detector lane gets the authored glyph", "detector", markInstrumentalDetector},
		{"musixmatch gets the vendored provider mark", "musixmatch", markMusixmatch},
		{"petitlyrics has no mark yet and degrades to text", "petitlyrics", markNone},
		{"unmapped lane degrades rather than rendering a broken mark", "somefuturelane", markNone},
		{"empty lane has no mark", "", markNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := laneMark(tt.lane); got != tt.want {
				t.Errorf("laneMark(%q) = %q; want %q", tt.lane, got, tt.want)
			}
		})
	}
}

// TestLaneMarkAssetsAreEmbedded is the test that would actually catch a broken
// mark in production. laneMark returning "musixmatch" proves nothing about
// whether an asset exists at the path the template requests -- the UI is served
// from an embedded FS, so a file present in the working tree but missing from
// the embed (or renamed under it) yields a 404 and a broken image at runtime,
// with every Go test still green.
//
// Asserting against static.FS rather than the filesystem is the whole point: it
// is the bytes the binary actually serves.
func TestLaneMarkAssetsAreEmbedded(t *testing.T) {
	// Each entry pairs a mark token with the path its template branch requests.
	// Keep in sync with the providerMark calls in lanemark.templ; a token with a
	// vendored asset and no entry here is untested, not proven absent.
	assets := map[string]string{
		markMusixmatch: "img/lanes/musixmatch.svg",
	}

	for token, path := range assets {
		t.Run(token, func(t *testing.T) {
			b, err := fs.ReadFile(static.FS, path)
			if err != nil {
				t.Fatalf("mark %q asset %q is not in the embedded FS: %v", token, path, err)
			}
			if len(b) == 0 {
				t.Fatalf("mark %q asset %q is embedded but empty", token, path)
			}
			if !strings.Contains(string(b), "<svg") {
				t.Errorf("mark %q asset %q does not look like an SVG", token, path)
			}
		})
	}
}

// TestBuildProviderTilesAppliesLaneMark covers the call site rather than the
// helper, mirroring TestBuildProviderTilesAppliesLaneLabel. TestLaneMark alone
// would still pass if laneMark were dropped from dashboard.go entirely, since it
// exercises the function in isolation; this asserts the token reaches the tile.
func TestBuildProviderTilesAppliesLaneMark(t *testing.T) {
	tiles := buildProviderTiles([]reports.ProviderEffectiveness{
		{Lane: "detector", Hits: 3, Misses: 1, HitRate: 0.75},
		{Lane: "musixmatch", Hits: 1, Misses: 1, HitRate: 0.5},
		{Lane: "petitlyrics", Hits: 1, Misses: 1, HitRate: 0.5},
	})
	if len(tiles) != 3 {
		t.Fatalf("buildProviderTiles returned %d tiles; want 3", len(tiles))
	}
	want := []string{markInstrumentalDetector, markMusixmatch, markNone}
	for i, w := range want {
		if tiles[i].LabelMark != w {
			t.Errorf("tiles[%d].LabelMark = %q; want %q", i, tiles[i].LabelMark, w)
		}
	}
	// The unmarked lane must still carry its name: the degrade is "no mark", not
	// "no lane". A blank label here would be a gap in the row rather than a
	// deliberate text-only cell.
	if tiles[2].Label == "" {
		t.Error("petitlyrics tile has no label; an unmarked lane must still render its name")
	}
}

// TestBuildRecentRowsAppliesLaneMark covers the second dashboard call site. The
// two build functions are independent, so a mark wired into tiles alone would
// leave the recent-outcomes table unmarked while every tile test passed.
func TestBuildRecentRowsAppliesLaneMark(t *testing.T) {
	rows := buildRecentRows([]reports.RecentOutcome{
		{Artist: "A", Title: "T", ProviderLane: "musixmatch"},
		{Artist: "B", Title: "U", ProviderLane: "petitlyrics"},
	}, nil)
	if len(rows) != 2 {
		t.Fatalf("buildRecentRows returned %d rows; want 2", len(rows))
	}
	if rows[0].LaneMark != markMusixmatch {
		t.Errorf("rows[0].LaneMark = %q; want %q", rows[0].LaneMark, markMusixmatch)
	}
	if rows[1].LaneMark != markNone {
		t.Errorf("rows[1].LaneMark = %q; want %q (no mark sourced yet)", rows[1].LaneMark, markNone)
	}
}
