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
	heads, all := renderTableRows(t, c)
	return heads, all[0]
}

// renderTableRows renders c and returns its header texts and the cells of
// every body row that carries as many cells as there are headers.
func renderTableRows(t *testing.T, c templ.Component) (heads []string, rows [][]string) {
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
			rows = append(rows, row)
		}
	}
	if len(rows) == 0 {
		t.Fatalf("no body row matching %d headers in %q", len(heads), out)
	}
	return heads, rows
}

// assertArtistAlbumTitle checks the header order Artist, Album, Title (adjacent,
// in that order) and that the row's cell under each header holds that field.
func assertArtistAlbumTitle(t *testing.T, heads, cells []string, album string) {
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
	if cells[a] != tcArtist || cells[b] != album || cells[c] != tcTitle {
		t.Errorf("cells %v: want artist %q, album %q, title %q under their headers", cells, tcArtist, album, tcTitle)
	}
}

// Header labels of the three track tables, in column order.
var (
	recentLabels = []string{"Artist", "Album", "Title", "Result", "Detail", "Source", "Completed"}
	instrLabels  = []string{"Artist", "Album", "Title", "ID", "File", "Detect requested"}
	reviewLabels = []string{"Artist", "Album", "Title", "Outcome", "Overrun (s)", "Ratio", "Evaluated", "Lyrics"}
)

// plainCols is an unsorted header row of the given labels.
func plainCols(labels ...string) []SortHeaderView {
	out := make([]SortHeaderView, len(labels))
	for i, l := range labels {
		out[i] = SortHeaderView{Label: l}
	}
	return out
}

// Fixture values shared by the track-column tests.
const (
	tcArtist = "Test Artist"
	tcAlbum  = "Test Album"
	tcTitle  = "Test Title"
)

// TestTrackTablesArtistAlbumTitle pins that each track table shows artist, album and title in that order under matching headers.
func TestTrackTablesArtistAlbumTitle(t *testing.T) {
	tests := []struct {
		name string
		c    templ.Component
	}{
		{"queue bucket", QueuePage("v", QueueView{Columns: tcQueueColumns(), Rows: []QueueRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}}, nil, false, false)},
		{"dashboard recent", dashRecentOutcomes([]RecentOutcomeRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"attention", attentionTable([]AttentionRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "none")},
		{"up next", dashUpNext(nil, []UpNextRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "h", "e")},
		{"reports recent", tableRecentOutcomes(plainCols(recentLabels...), []RecentOutcomeRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"reports instrumentals", tableInstrumentals(plainCols(instrLabels...), []InstrumentalRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}})},
		{"failure group", FailureGroupRows(FailureGroupView{Rows: []FailureItemRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}})},
		{"review queue", tableReviewQueue(plainCols(reviewLabels...), []ReviewQueueRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			heads, cells := renderTable(t, tt.c)
			assertArtistAlbumTitle(t, heads, cells, tcAlbum)
		})
	}
}

// TestUpNextInFlightRow covers the in-flight rows of the Up Next table, which
// are a separate loop from the queued rows. Both kinds are asserted, every row.
func TestUpNextInFlightRow(t *testing.T) {
	c := dashUpNext(
		[]InFlightRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}, {Artist: tcArtist, Title: tcTitle}},
		[]UpNextRow{{Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}, "h", "e")
	heads, rows := renderTableRows(t, c)
	if len(rows) != 3 {
		t.Fatalf("got %d body rows, want 3 (2 in flight, 1 queued)", len(rows))
	}
	assertArtistAlbumTitle(t, heads, rows[0], tcAlbum)
	assertArtistAlbumTitle(t, heads, rows[1], "-")
	assertArtistAlbumTitle(t, heads, rows[2], tcAlbum)
}

// TestInstrumentalsArtistLeads pins that Artist, Album, Title are the FIRST
// three columns of the Instrumentals table, with the row ID after them. (Up
// Next's "#" is a queue position, not a track attribute, and stays first.)
func TestInstrumentalsArtistLeads(t *testing.T) {
	heads, cells := renderTable(t, tableInstrumentals(plainCols(instrLabels...), []InstrumentalRow{{ID: "7", Artist: tcArtist, Album: tcAlbum, Title: tcTitle}}))
	if len(heads) < 4 || heads[0] != "Artist" || heads[1] != "Album" || heads[2] != "Title" || heads[3] != "ID" {
		t.Fatalf("headers %v: want Artist, Album, Title, ID leading", heads)
	}
	if cells[0] != tcArtist || cells[3] != "7" {
		t.Errorf("cells %v: want artist first and ID 7 fourth", cells)
	}
}

// TestTrackTablesEmptyAlbumIsDash pins that an empty album renders as a dash in every track table.
func TestTrackTablesEmptyAlbumIsDash(t *testing.T) {
	tests := []struct {
		name string
		c    templ.Component
	}{
		{"queue bucket", QueuePage("v", QueueView{Columns: tcQueueColumns(), Rows: []QueueRow{{Artist: tcArtist, Title: tcTitle}}}, nil, false, false)},
		{"dashboard recent", dashRecentOutcomes([]RecentOutcomeRow{{Artist: tcArtist, Title: tcTitle}})},
		{"attention", attentionTable([]AttentionRow{{Artist: tcArtist, Title: tcTitle}}, "none")},
		{"up next", dashUpNext(nil, []UpNextRow{{Artist: tcArtist, Title: tcTitle}}, "h", "e")},
		{"reports recent", tableRecentOutcomes(plainCols(recentLabels...), []RecentOutcomeRow{{Artist: tcArtist, Title: tcTitle}})},
		{"reports instrumentals", tableInstrumentals(plainCols(instrLabels...), []InstrumentalRow{{Artist: tcArtist, Title: tcTitle}})},
		{"failure group", FailureGroupRows(FailureGroupView{Rows: []FailureItemRow{{Artist: tcArtist, Title: tcTitle}}})},
		{"review queue", tableReviewQueue(plainCols(reviewLabels...), []ReviewQueueRow{{Artist: tcArtist, Title: tcTitle}}, "")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			heads, cells := renderTable(t, tt.c)
			assertArtistAlbumTitle(t, heads, cells, "-")
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

// tcQueueColumns is the queue table's header row as the handler builds it.
func tcQueueColumns() []SortHeaderView {
	var out []SortHeaderView
	for _, l := range []string{"Artist", "Album", "Title", "Status", "Reason", "Next attempt", "Misses", "Attempts", "Updated", "Libraries", "Lyrics"} {
		out = append(out, SortHeaderView{Label: l})
	}
	return out
}
