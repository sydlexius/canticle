package web

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/reports"
)

// trendNow is the pinned instant the trend tests run at.
var trendNow = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newTrendServer(t *testing.T, sqlDB *sql.DB, now time.Time) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewUI(config.Config{}, "v-test", WithReports(reports.New(sqlDB)), withClock(func() time.Time { return now })).Register(mux)
	return mux
}

func seedEvent(t *testing.T, db *sql.DB, daysAgo int, lane, event string, n int) {
	t.Helper()
	day := trendNow.AddDate(0, 0, -daysAgo).Format("2006-01-02")
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
	mux := newTrendServer(t, sqlDB, trendNow)

	rows := func(body string) int { return strings.Count(body, `<td class="mx-source-num">`) / 7 }
	for _, tc := range []struct {
		name, query string
		days        int
	}{
		{"default", "", 30}, {"7", "?range=7", 7}, {"30", "?range=30", 30}, {"90", "?range=90", 90},
		{"unknown", "?range=5", 30}, {"nonnumeric", "?range=abc", 30}, {"signed", "?range=%2B7", 30}, {"padded", "?range=007", 30},
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
	today := trendNow.Format("2006-01-02")
	for _, want := range []string{
		`data-chart-series="[{&#34;label&#34;:&#34;Hit rate (%)&#34;,&#34;data&#34;:[null,null,null,null,null,null,75]}]"`,
		`&#34;label&#34;:&#34;Word-synced&#34;,&#34;data&#34;:[0,0,0,0,0,0,2]`,
		`&#34;label&#34;:&#34;Instrumental&#34;`,
		`data-chart-detail="[`,
		`Hits 3  /  Misses 1`,
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

	t.Run("repeated range is rejected", func(t *testing.T) {
		if code, _ := getSource(t, mux, "/sources/musixmatch?range=7&range=90"); code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400", code)
		}
	})
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
	t.Run("all-miss source still gets its hit-rate chart", func(t *testing.T) {
		seedEvent(t, sqlDB, 0, "petitlyrics", "miss", 4)
		seedEvent(t, sqlDB, 1, "petitlyrics", "miss", 2)
		code, body := getSource(t, mux, "/sources/petitlyrics?range=7")
		if code != http.StatusOK {
			t.Fatalf("status = %d", code)
		}
		if !strings.Contains(body, `data-chart-series="[{&#34;label&#34;:&#34;Hit rate (%)&#34;,&#34;data&#34;:[null,null,null,null,null,0,0]}]"`) {
			t.Error("hit-rate series must be zeros (real 0%), not nulls")
		}
		if strings.Contains(body, "No lookups recorded in this range") {
			t.Error("false no-lookups sentence shown for a source with all-miss days")
		}
		if !strings.Contains(body, "Nothing delivered in this range") {
			t.Error("delivered-types chart must still be empty text when nothing landed")
		}
	})
}

// TestSourceTrendLastColumnIsUTCDay: a second before and after UTC midnight the
// last column is that UTC day, whatever the host zone.
func TestSourceTrendLastColumnIsUTCDay(t *testing.T) {
	sqlDB := openReportsTestDB(t)
	seedSources(t, sqlDB)
	seedEvent(t, sqlDB, 0, "musixmatch", "hit", 1)
	for _, tc := range []struct {
		now  time.Time
		want string
	}{
		{time.Date(2026, 10, 6, 23, 59, 59, 0, time.UTC), "2026-10-06"},
		{time.Date(2026, 10, 7, 0, 0, 1, 0, time.UTC), "2026-10-07"},
		{time.Date(2026, 10, 6, 23, 59, 59, 0, time.FixedZone("far", -11*3600)).Add(2 * time.Hour), "2026-10-07"},
	} {
		_, body := getSource(t, newTrendServer(t, sqlDB, tc.now), "/sources/musixmatch?range=7")
		i := strings.LastIndex(body, "<td>20")
		if i < 0 || !strings.HasPrefix(body[i:], "<td>"+tc.want+"</td>") {
			t.Errorf("now %s: last column is not %s", tc.now, tc.want)
		}
	}
}

// TestTrendTypeLabelsAreResultBuckets: the delivered-type series reuse the
// Results labels (chart color keys), in the worker's landing order.
func TestTrendTypeLabelsAreResultBuckets(t *testing.T) {
	want := []string{"Word-synced", "Line-synced", "Unsynced", "Instrumental"}
	if got := trendTypeLabels(); !slices.Equal(got, want) {
		t.Errorf("trendTypeLabels = %q, want %q", got, want)
	}
}
