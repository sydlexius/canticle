package web

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/reports"
)

// seedSourceUnits stamps provider_lane on the link population and records
// lookups that the OLD tile unit (hits per lane) would have counted: hits on rows
// that are not done, a hit later replaced by another lane, and a lane with done
// rows but no lookups at all.
func seedSourceUnits(t *testing.T, db *sql.DB) {
	t.Helper()
	seedLinkPopulation(t, db)
	lanes := map[string]string{
		"w1": providers.Musixmatch, "w2": providers.Musixmatch, "l1": providers.Musixmatch,
		"w3-missync": providers.PetitLyrics, "l2": providers.PetitLyrics, "retired": providers.PetitLyrics,
		"u1": providers.InnerTube,
		"t1": "detector", // done rows, a lane with no tile of its own until it has done rows
	}
	for title, lane := range lanes {
		if _, err := db.ExecContext(context.Background(), `UPDATE work_queue SET provider_lane = ? WHERE title = ?`, lane, title); err != nil {
			t.Fatal(err)
		}
	}
	hit := func(title, lane string, h int) {
		t.Helper()
		if _, err := db.ExecContext(context.Background(),
			`INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at)
             SELECT id, ?, ?, '2026-06-18T00:00:00Z' FROM work_queue WHERE title = ?`, lane, h, title); err != nil {
			t.Fatal(err)
		}
	}
	hit("p1", providers.Musixmatch, 1) // a hit on a queued row
	hit("f1", providers.Musixmatch, 1) // a hit on an errored row
	hit("w1", providers.PetitLyrics, 1)
	hit("w1", providers.Musixmatch, 1) // w1 is served by musixmatch; petitlyrics hit was replaced
	hit("u1", providers.PetitLyrics, 0)
}

// TestSourcesRowSumsToFinishedPlusSettled pins #1439: every number the Lyrics
// Sources row presents as a result count is done rows by provider_lane, so the
// tiles plus Unattributed equal Finished + Settled under both rungs, even
// though the lookup hits (which the old tile showed) do not.
func TestSourcesRowSumsToFinishedPlusSettled(t *testing.T) {
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		sqlDB := openReportsTestDB(t)
		seedSourceUnits(t, sqlDB)
		repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
		u := NewUI(config.Config{}, "v-test", WithReports(repo))
		view, err := u.buildDashboardView(httptest.NewRequest("GET", "/dashboard", nil))
		if err != nil {
			t.Fatal(err)
		}
		qs, err := repo.QueueSummary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sum, unattributed := 0, 0
		for _, tile := range view.ProviderTiles {
			n, err := strconv.Atoi(tile.Value)
			if err != nil {
				t.Fatalf("tile %q value %q is not a result count: %v", tile.Label, tile.Value, err)
			}
			sum += n
			if tile.Label == "Unattributed" {
				unattributed = n
			}
		}
		if want := int(qs.Finished + qs.SettledUpgradable); sum != want {
			t.Errorf("top %v: sources + Unattributed = %d, want Finished + Settled = %d", top, sum, want)
		}
		if unattributed == 0 {
			t.Errorf("top %v: seed must include Unattributed rows", top)
		}
		// The seed makes the old unit visibly wrong: lane hits exceed what the lanes finished.
		pe, err := repo.ProviderEffectiveness(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		var hits int64
		for _, p := range pe {
			hits += p.Hits
		}
		if int(hits)+unattributed == sum {
			t.Errorf("top %v: seed does not separate lookup hits (%d) from done rows", top, hits)
		}
	}
}

// TestUnattributedTileLinksToEqualPopulation pins #1439 as
// TestResultTilesLinkToEqualPopulations pins #1237: the Unattributed tile links
// to a Work Queue list holding exactly the tile's count, under both rungs, and
// the queue Source filter offers it.
func TestUnattributedTileLinksToEqualPopulation(t *testing.T) {
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		sqlDB := openReportsTestDB(t)
		seedSourceUnits(t, sqlDB)
		repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
		d, err := repo.DoneByLane(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		tile, ok := buildUnattributedTile(d.Unattributed)
		if !ok || tile.Href == "" {
			t.Fatalf("top %v: no linked Unattributed tile for %d rows", top, d.Unattributed)
		}
		// A queued and an errored row also have no lane; only the done bucket
		// keeps the list equal to the tile.
		if n := listFromHref(t, repo, top, tile.Href); strconv.Itoa(n) != tile.Value || n == 0 {
			t.Errorf("top %v: tile count %s but %s lists %d rows", top, tile.Value, tile.Href, n)
		}
	}
	var offered bool
	for _, o := range buildLaneOptions("") {
		if o.Value == reports.LaneUnattributed && o.Label == "Unattributed" {
			offered = true
		}
	}
	if !offered {
		t.Error("Source filter does not offer Unattributed")
	}
	s, err := parseQueueViewState(map[string][]string{"lane": {reports.LaneUnattributed}}, reports.BucketDone, reports.TopRungWord)
	if err != nil || s.Lane != reports.LaneUnattributed {
		t.Errorf("lane=- parsed to %q, %v", s.Lane, err)
	}
}
