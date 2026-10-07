package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
)

// seedLinkRow inserts one work_queue row with the columns the result and chip
// predicates read.
func seedLinkRow(t *testing.T, db *sql.DB, title, status, outcome, tier, timing, wordState, lastErr, edited string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, sync_tier,
                                 timing_outcome, word_timing_state, last_error, lyric_edited_at, source_path)
         VALUES ('A', ?, 'a', ?, 'Album', ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), ?, NULLIF(?, ''), '/x/'||?||'.flac')`,
		title, strings.ToLower(title), status, outcome, tier, timing, wordState, lastErr, edited, title)
	if err != nil {
		t.Fatal(err)
	}
}

func seedLinkPopulation(t *testing.T, db *sql.DB) {
	t.Helper()
	seedLinkRow(t, db, "w1", "done", "synced", "word", "", "", "", "")
	seedLinkRow(t, db, "w2", "done", "synced", "word", "", "", "", "x")
	seedLinkRow(t, db, "w3-missync", "done", "synced", "word", "mis_synced", "", "", "")
	seedLinkRow(t, db, "l1", "done", "synced", "line", "", "", "", "")
	seedLinkRow(t, db, "l2", "done", "synced", "line", "", "", "", "x")
	seedLinkRow(t, db, "l3-recheck", "done", "synced", "line", "", "queued", "", "")
	seedLinkRow(t, db, "retired", "done", "synced", "word", "", "", queue.UnresolvableGoneError, "")
	seedLinkRow(t, db, "u1", "done", "unsynced", "", "", "", "", "")
	seedLinkRow(t, db, "i1", "done", "instrumental", "", "", "", "", "")
	seedLinkRow(t, db, "t1", "done", "synced", "", "", "", "", "")
	seedLinkRow(t, db, "p1", "pending", "", "", "", "", "", "")
	seedLinkRow(t, db, "f1", "failed", "", "", "", "", "boom", "")
	// A hand-marked instrumental is Finished under every rung (#1405).
	seedLinkRow(t, db, "m1", "done", "instrumental", "", "", "", "", "")
	if _, err := db.ExecContext(context.Background(),
		`UPDATE work_queue SET manual_instrumental_at = '2026-08-16T05:00:00Z' WHERE title = 'm1'`); err != nil {
		t.Fatal(err)
	}
}

// listFromHref follows a dashboard/report link the way the browser does: the
// path names the bucket, the query goes through parseQueueViewState, and the
// rows come from the same ListBucketFiltered the page handler calls.
func listFromHref(t *testing.T, repo *reports.Repo, top reports.TopRung, href string) int {
	t.Helper()
	u, err := url.Parse(href)
	if err != nil {
		t.Fatal(err)
	}
	bucket, err := reports.ParseBucket(strings.TrimPrefix(u.Path, "/queue/"))
	if err != nil {
		t.Fatalf("href %q: %v", href, err)
	}
	state, err := parseQueueViewState(u.Query(), bucket, top)
	if err != nil {
		t.Fatal(err)
	}
	spec := reports.BucketSpec(bucket)
	order := spec.Resolve(state.Sort, state.Dir)
	cursor, _ := spec.DecodeCursor(order, state.After)
	// Follow the keyset cursor the way the pager does, so a population above
	// one page is counted whole rather than truncated at MaxBucketLimit.
	total := 0
	for {
		rows, err := repo.ListBucketFiltered(context.Background(), bucket, state.filter(), order, cursor, reports.MaxBucketLimit)
		if err != nil {
			t.Fatal(err)
		}
		total += len(rows)
		if len(rows) < reports.MaxBucketLimit {
			return total
		}
		last := rows[len(rows)-1]
		cursor = tablesort.Cursor{ID: last.ID, Val: last.SortVal}
	}
}

// TestResultTilesLinkToEqualPopulations pins #1237: every Results tile that
// links to the Work Queue shows exactly as many rows there as its own count, under
// both top-rung modes, and the tiles that cannot be linked exactly stay plain.
func TestResultTilesLinkToEqualPopulations(t *testing.T) {
	cases := []struct {
		name   string
		top    reports.TopRung
		linked map[string]string // tile label -> href
	}{
		{"word rung", reports.TopRungWord, map[string]string{
			// Finished also holds hand-marked instrumentals (#1405): Word-synced stays unlinked.
			"Line-synced": "/queue/settled?tier=line",
		}},
		{"line rung", reports.TopRungLine, map[string]string{
			// Finished = word + line there and no word chip exists: Word-synced stays unlinked.
			"Line-synced": "/queue/finished?tier=line",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sqlDB := openReportsTestDB(t)
			seedLinkPopulation(t, sqlDB)
			repo := reports.New(sqlDB, reports.WithLineTopRung(tc.top == reports.TopRungLine))
			b, err := repo.ResultsBreakdown(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, tile := range buildResultsTiles(b) {
				if tile.Href == "" {
					continue
				}
				got[tile.Label] = tile.Href
				if n := listFromHref(t, repo, tc.top, tile.Href); strconv.Itoa(n) != tile.Value || n == 0 {
					t.Errorf("tile %q count %s but %s lists %d rows", tile.Label, tile.Value, tile.Href, n)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.linked) {
				t.Errorf("linked tiles = %v, want %v", got, tc.linked)
			}
		})
	}
}

// TestReviewQueueLinks pins the review-queue report's links (#1237): the player
// link only on a row whose RECORDED tier is a settled word/line one (the file
// may still be gone; nothing stats it), none on a demoted, quarantined,
// retired, recheck-queued, unsettled or unsynced one, and a Mis-synced Work
// Queue link that shows the settled mis-synced rows, a subset of the report
// (categorical and unsettled rows are not in it), under both rungs.
func TestReviewQueueLinks(t *testing.T) {
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		t.Run(fmt.Sprint(top), func(t *testing.T) {
			sqlDB := openReportsTestDB(t)
			// Previewable: tier still recorded.
			seedLinkRow(t, sqlDB, "kept", "done", "synced", "line", "mis_synced", "", "", "")
			seedLinkRow(t, sqlDB, "kept-word", "done", "synced", "word", "mis_synced", "", "", "")
			// Not previewable: each trips exactly one clause of previewableFilePredicate.
			seedLinkRow(t, sqlDB, "demoted", "done", "synced", "", "mis_synced", "", "", "")
			seedLinkRow(t, sqlDB, "quarantined", "done", "", "", "categorical", "", "", "")
			seedLinkRow(t, sqlDB, "retired", "done", "synced", "line", "mis_synced", "", queue.UnresolvableGoneError, "")
			seedLinkRow(t, sqlDB, "recheck", "done", "synced", "line", "mis_synced", "queued", "", "")
			seedLinkRow(t, sqlDB, "unsettled", "pending", "synced", "line", "mis_synced", "", "", "")
			seedLinkRow(t, sqlDB, "unsynced", "done", "unsynced", "line", "mis_synced", "", "", "")
			ids := map[string]int64{}
			for _, title := range []string{"kept", "kept-word"} {
				var id int64
				if err := sqlDB.QueryRow(`SELECT id FROM work_queue WHERE title = ?`, title).Scan(&id); err != nil {
					t.Fatal(err)
				}
				ids[title] = id
			}
			repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
			mux := http.NewServeMux()
			NewUI(config.Config{}, "v-test", WithReports(repo)).Register(mux)

			body := getFragment(t, mux, "review-queue").Body.String()
			if n := strings.Count(body, `href="/preview/`); n != len(ids) {
				t.Errorf("want %d player links; got %d in:\n%s", len(ids), n, body)
			}
			for title, id := range ids {
				if !strings.Contains(body, `href="/preview/`+strconv.FormatInt(id, 10)+`"`) {
					t.Errorf("no player link for %q (row %d)", title, id)
				}
			}
			const link = `href="/queue/settled?missync=1"`
			if !strings.Contains(body, link) {
				t.Fatalf("report missing the Mis-synced queue link; body:\n%s", body)
			}
			// The view lists done mis_synced rows only: the report's pending and
			// categorical rows are not in it.
			report, err := repo.ReviewQueue(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			const want = 6 // the 8 report rows minus the categorical and the pending one
			if n := listFromHref(t, repo, top, "/queue/settled?missync=1"); n != want || n >= len(report) {
				t.Errorf("Mis-synced view lists %d rows, want %d, a strict subset of the report's %d", n, want, len(report))
			}
		})
	}
}
