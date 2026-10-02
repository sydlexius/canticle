package templates

import (
	"bytes"
	"context"
	"html"
	"regexp"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

var (
	thRE  = regexp.MustCompile(`(?s)<th[^>]*>(.*?)</th>`)
	trRE  = regexp.MustCompile(`(?s)<tr[^>]*>(.*?)</tr>`)
	tdRE  = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	tagRE = regexp.MustCompile(`(?s)<[^>]*>`)
)

func cellText(s string) string {
	return strings.TrimSpace(html.UnescapeString(tagRE.ReplaceAllString(s, "")))
}

// renderTable renders c and returns its header texts and the cells of the first
// body row that carries as many cells as there are headers.
func renderTable(t *testing.T, c templ.Component) (heads, cells []string) {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, m := range thRE.FindAllStringSubmatch(out, -1) {
		heads = append(heads, cellText(m[1]))
	}
	for _, r := range trRE.FindAllStringSubmatch(out, -1) {
		var row []string
		for _, m := range tdRE.FindAllStringSubmatch(r[1], -1) {
			row = append(row, cellText(m[1]))
		}
		if len(heads) > 0 && len(row) == len(heads) {
			return heads, row
		}
	}
	t.Fatalf("no body row matching %d headers in %q", len(heads), out)
	return nil, nil
}

// assertArtistAlbumTitle checks the header order Artist, Album, Title (adjacent,
// in that order) and that the row's cell under each header holds that field.
func assertArtistAlbumTitle(t *testing.T, heads, cells []string, artist, album, title string) {
	t.Helper()
	idx := func(h string) int {
		for i, x := range heads {
			if x == h {
				return i
			}
		}
		t.Fatalf("header %q missing in %v", h, heads)
		return -1
	}
	a, b, c := idx("Artist"), idx("Album"), idx("Title")
	if b != a+1 || c != b+1 {
		t.Fatalf("headers %v: want Artist, Album, Title adjacent in that order", heads)
	}
	if cells[a] != artist || cells[b] != album || cells[c] != title {
		t.Errorf("cells %v: want artist %q, album %q, title %q under their headers", cells, artist, album, title)
	}
}

const (
	tcArtist = "Test Artist"
	tcAlbum  = "Test Album"
	tcTitle  = "Test Title"
)

func TestTrackTablesArtistAlbumTitle(t *testing.T) {
	tests := []struct {
		name string
		c    templ.Component
	}{
		{"queue bucket", QueuePage("v", QueueView{Rows: []QueueRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}}, nil, false, false)},
		{"dashboard recent", dashRecentOutcomes([]RecentOutcomeRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"attention", attentionTable([]AttentionRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "none")},
		{"up next", dashUpNext(nil, []UpNextRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "h", "e")},
		{"reports recent", tableRecentOutcomes([]RecentOutcomeRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"reports instrumentals", tableInstrumentals([]InstrumentalRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"failure group", FailureGroupRows(FailureGroupView{Rows: []FailureItemRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}})},
		{"review queue", tableReviewQueue([]ReviewQueueRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			heads, cells := renderTable(t, tt.c)
			assertArtistAlbumTitle(t, heads, cells, tcArtist, tcAlbum, tcTitle)
		})
	}
}

func TestTrackTablesEmptyAlbumIsDash(t *testing.T) {
	tests := []struct {
		name string
		c    templ.Component
	}{
		{"queue bucket", QueuePage("v", QueueView{Rows: []QueueRow{{Artist: tcArtist, Title: tcTitle}}}, nil, false, false)},
		{"dashboard recent", dashRecentOutcomes([]RecentOutcomeRow{{Artist: tcArtist, Title: tcTitle}})},
		{"attention", attentionTable([]AttentionRow{{Artist: tcArtist, Title: tcTitle}}, "none")},
		{"up next", dashUpNext(nil, []UpNextRow{{Artist: tcArtist, Title: tcTitle}}, "h", "e")},
		{"reports instrumentals", tableInstrumentals([]InstrumentalRow{{Artist: tcArtist, Title: tcTitle}})},
		{"review queue", tableReviewQueue([]ReviewQueueRow{{Artist: tcArtist, Title: tcTitle}})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			heads, cells := renderTable(t, tt.c)
			assertArtistAlbumTitle(t, heads, cells, tcArtist, "-", tcTitle)
		})
	}
}

func TestAlbumText(t *testing.T) {
	if got := AlbumText(""); got != "-" {
		t.Errorf("empty album = %q, want dash", got)
	}
	if got := AlbumText("X"); got != "X" {
		t.Errorf("album = %q, want X", got)
	}
}
