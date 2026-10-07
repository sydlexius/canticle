package web

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"
)

func seedEvent(t *testing.T, db *sql.DB, daysAgo int, lane, event string, n int) {
	t.Helper()
	day := time.Now().UTC().AddDate(0, 0, -daysAgo).Format("2006-01-02")
	if _, err := db.ExecContext(context.Background(),
		`INSERT INTO source_event_daily(day, lane, event, count) VALUES(?, ?, ?, ?)`, day, lane, event, n); err != nil {
		t.Fatal(err)
	}
}

// TestSourceTrend (#1302): ranges, fallback, empty state, series attributes.
func TestSourceTrend(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedSources(t, sqlDB)
	seedEvent(t, sqlDB, 0, "musixmatch", "hit", 3)
	seedEvent(t, sqlDB, 0, "musixmatch", "miss", 1)
	seedEvent(t, sqlDB, 0, "musixmatch", "word", 2)
	seedEvent(t, sqlDB, 20, "musixmatch", "miss", 2)
	seedEvent(t, sqlDB, 60, "musixmatch", "hit", 1)
	mux := newReportsUIServer(t, sqlDB)

	rows := func(body string) int { return strings.Count(body, `<td class="mx-source-num">`) / 7 }
	for _, tc := range []struct {
		name, query string
		days        int
	}{
		{"default", "", 30}, {"7", "?range=7", 7}, {"30", "?range=30", 30}, {"90", "?range=90", 90},
		{"unknown", "?range=5", 30}, {"nonnumeric", "?range=abc", 30}, {"repeated", "?range=7&range=90", 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getSource(t, mux, "/sources/musixmatch"+tc.query)
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if got := rows(body); got != tc.days {
				t.Errorf("table rows = %d, want %d", got, tc.days)
			}
			cur := `href="/sources/musixmatch?range=` + map[int]string{7: "7", 30: "30", 90: "90"}[tc.days] + `" aria-current="page"`
			if !strings.Contains(body, cur) {
				t.Errorf("current range link missing: %s", cur)
			}
			for _, d := range []string{"7", "30", "90"} {
				if !strings.Contains(body, `href="/sources/musixmatch?range=`+d+`"`) {
					t.Errorf("range link %s missing", d)
				}
			}
			if !strings.Contains(body, "UTC") || !strings.Contains(body, `data-chart-type="stacked-bar"`) || !strings.Contains(body, `data-chart-type="line"`) {
				t.Error("UTC note or chart canvases missing")
			}
		})
	}

	_, body := getSource(t, mux, "/sources/musixmatch?range=7")
	today := time.Now().UTC().Format("2006-01-02")
	for _, want := range []string{
		`data-chart-series="[{&#34;label&#34;:&#34;Hit rate (%)&#34;,&#34;data&#34;:[null,null,null,null,null,null,75]}]"`,
		`&#34;label&#34;:&#34;Word-synced&#34;,&#34;data&#34;:[0,0,0,0,0,0,2]`,
		`&#34;label&#34;:&#34;Instrumental&#34;`,
		`<td>` + today + `</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("range=7 body missing %q", want)
		}
	}
	// 20 days ago is outside 7 but inside 30: that day is 0%, real, not a gap.
	if _, b30 := getSource(t, mux, "/sources/musixmatch?range=30"); !strings.Contains(b30, `[null,null,null,null,null,null,null,null,null,0,`) {
		t.Error("range=30: an all-miss day must be 0 and its neighbors gaps")
	}

	t.Run("no history yet", func(t *testing.T) {
		code, body := getSource(t, mux, "/sources/petitlyrics")
		if code != http.StatusOK || !strings.Contains(body, "No history yet") || strings.Contains(body, "data-chart-series") {
			t.Errorf("status=%d, want 200 with the no-history note and no trend chart", code)
		}
	})
	t.Run("unattributed has no trend", func(t *testing.T) {
		_, body := getSource(t, mux, unattributedPath)
		if !strings.Contains(body, "not recorded for unattributed") || strings.Contains(body, "data-chart-series") {
			t.Error("unattributed page must show the note, no chart")
		}
	})
	t.Run("history but no lookups in range", func(t *testing.T) {
		seedEvent(t, sqlDB, 80, "innertube", "hit", 1)
		_, body := getSource(t, mux, "/sources/innertube?range=7")
		if !strings.Contains(body, "No lookups recorded in this range") || !strings.Contains(body, "Nothing delivered in this range") || strings.Contains(body, "data-chart-series") {
			t.Error("a source with only old history must show per-chart empty text in a short range")
		}
	})
}
