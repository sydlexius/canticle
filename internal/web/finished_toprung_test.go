package web

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
)

// TestFinishedPagesFollowTopRung pins #1275 on the rendered pages: with word
// sync off (reports.TopRungLine) the line-synced row counts as Finished on the
// dashboard and the Queue page, the Finished listing holds it and offers the
// Line-synced chip, and the descriptions say line sync is the best result.
// With word sync on, the same rows render as before.
func TestFinishedPagesFollowTopRung(t *testing.T) {
	cases := []struct {
		name                          string
		lineTop                       bool
		finished, settled             string
		blurb, finishedHint           string
		lineOnFinished, lineOnSettled bool
		finishedTip, lineResultTip    string
	}{
		{"word sync on", false, "2", "3",
			"Tracks with word-synced lyrics, the best result there is.",
			"read-only: they have word timing", false, true,
			"The only terminal state", "It may still be upgraded."},
		{"word sync off", true, "3", "2",
			"Tracks with line- or word-synced lyrics, the best result with word sync off.",
			"line-synced ones can also have their timing edited", true, false,
			"Word sync is off, so line-synced is the best result", "the best result with word sync off"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB := openReportsTestDB(t)
			seedFinishedSplitRows(t, sqlDB)
			mux := http.NewServeMux()
			repo := reports.New(sqlDB, reports.WithLineTopRung(tc.lineTop))
			NewUI(config.Config{}, "v-test", WithReports(repo)).Register(mux)

			dash := getPath(t, mux, "/dashboard").Body.String()
			for label, want := range map[string]string{"Finished": tc.finished, "Settled (upgradable)": tc.settled} {
				tile := regexp.MustCompile(`<span class="mx-dash-tile-label">` + regexp.QuoteMeta(label) + `</span>\s*<span class="mx-dash-tile-value">(\d+)</span>`)
				if m := tile.FindStringSubmatch(dash); m == nil || m[1] != want {
					t.Errorf("dashboard %q tile = %v, want %s", label, m, want)
				}
			}
			for _, tip := range []string{tc.finishedTip, tc.lineResultTip} {
				if !strings.Contains(dash, tip) {
					t.Errorf("dashboard missing tooltip text %q", tip)
				}
			}

			index := getPath(t, mux, "/queue").Body.String()
			row := regexp.MustCompile(`href="/queue/finished">Finished</a></td>\s*<td class="mx-cell-mono">(\d+)</td>`)
			if m := row.FindStringSubmatch(index); m == nil || m[1] != tc.finished {
				t.Errorf("queue page Finished row = %v, want %s", m, tc.finished)
			}
			if !strings.Contains(index, tc.finishedHint) {
				t.Errorf("queue page missing Finished hint %q", tc.finishedHint)
			}
			settledEditHint := strings.Contains(index, "Line-synced tracks here can be previewed and their timing edited.")
			if settledEditHint != tc.lineOnSettled {
				t.Errorf("queue page Settled edit hint present = %v, want %v", settledEditHint, tc.lineOnSettled)
			}

			fin := getPath(t, mux, "/queue/finished").Body.String()
			if !strings.Contains(fin, tc.blurb) {
				t.Errorf("Finished page missing blurb %q", tc.blurb)
			}
			if got := strconv.Itoa(strings.Count(fin, `href="/preview/`)); got != tc.finished {
				t.Errorf("Finished page lists %s previewable rows, want %s", got, tc.finished)
			}
			if got := strings.Contains(fin, "Line-synced (editable)"); got != tc.lineOnFinished {
				t.Errorf("Finished page offers Line-synced chip = %v, want %v", got, tc.lineOnFinished)
			}
			set := getPath(t, mux, "/queue/settled").Body.String()
			if got := strings.Contains(set, "Line-synced (editable)"); got != tc.lineOnSettled {
				t.Errorf("Settled page offers Line-synced chip = %v, want %v", got, tc.lineOnSettled)
			}
		})
	}
}
