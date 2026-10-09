package web

import (
	"context"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/reports"
)

// TestSourcesRowSumsWithLaneHealth is the production-path twin of
// TestSourcesRowSumsToFinishedPlusSettled (#1439): serve attaches lane health,
// so the tiles come from providerTilesWithHealth. The health list is partial
// (musixmatch and a Local detector) while done rows exist for lanes outside it
// (petitlyrics, innertube), so the done-only tiles must still make the row add
// up to Finished + Settled under both rungs.
func TestSourcesRowSumsWithLaneHealth(t *testing.T) {
	health := func() []orchestrator.LaneState {
		return []orchestrator.LaneState{
			{Provider: providers.Musixmatch, State: orchestrator.LaneStateClosed},
			{Provider: "detector", Local: true, State: orchestrator.LaneStateClosed},
		}
	}
	for _, top := range []reports.TopRung{reports.TopRungWord, reports.TopRungLine} {
		sqlDB := openReportsTestDB(t)
		seedSourceUnits(t, sqlDB)
		repo := reports.New(sqlDB, reports.WithLineTopRung(top == reports.TopRungLine))
		u := NewUI(config.Config{}, "v-test", WithReports(repo))
		u.AttachLaneHealth(health)
		view, err := u.buildDashboardView(httptest.NewRequest("GET", "/dashboard", nil))
		if err != nil {
			t.Fatal(err)
		}
		qs, err := repo.QueueSummary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sum, labels := 0, map[string]bool{}
		for _, tile := range view.ProviderTiles {
			n, err := strconv.Atoi(tile.Value)
			if err != nil {
				t.Fatalf("tile %q value %q is not a result count: %v", tile.Label, tile.Value, err)
			}
			sum += n
			labels[tile.Label] = true
		}
		if want := int(qs.Finished + qs.SettledUpgradable); sum != want {
			t.Errorf("top %v: sources + Unattributed = %d, want Finished + Settled = %d", top, sum, want)
		}
		if !labels["Unattributed"] {
			t.Errorf("top %v: no Unattributed tile", top)
		}
	}
}

// TestSourcesNoteAndUnattributedTitle pins two #1439 template details on the
// rendered dashboard: the explanatory note appears only when tiles exist, and
// the Unattributed tile (a track list, not a source page) does not carry the
// by-source hover text.
func TestSourcesNoteAndUnattributedTitle(t *testing.T) {
	const note = "add up to Finished + Settled"
	const sourceTitle = `title="Results by type for this source"`

	empty := getDashboard(t, laneHealthTestUI(t, nil, func() []orchestrator.LaneState { return nil }))
	if !strings.Contains(empty, "No lyrics source data yet") {
		t.Fatal("empty database: empty state missing")
	}
	if strings.Contains(empty, note) {
		t.Error("the Lyrics Sources note renders with no tiles")
	}

	sqlDB := openReportsTestDB(t)
	seedSourceUnits(t, sqlDB)
	mux := newReportsUIServer(t, sqlDB)
	_, dash := getSource(t, mux, "/dashboard")
	if !strings.Contains(dash, note) {
		t.Error("the Lyrics Sources note is missing when tiles exist")
	}
	m := unattributedTileRE.FindStringSubmatch(dash)
	if m == nil {
		t.Fatal("dashboard has no linked Unattributed tile")
	}
	start := strings.Index(dash, `href="`+unattributedQueueHref+`"`)
	tag := dash[start : start+strings.Index(dash[start:], ">")]
	if strings.Contains(tag, sourceTitle) || !strings.Contains(tag, `title="Open the finished tracks`) {
		t.Errorf("Unattributed tile has the wrong hover text: %s", tag)
	}
	if !strings.Contains(dash, `href="/sources/musixmatch"`) || !strings.Contains(dash, sourceTitle) {
		t.Error("a source tile lost its by-type hover text")
	}
}

// TestEmptyLaneTileHasLabelAndNoLink: done rows with provider_lane = ” (not
// NULL) get a named, unlinked tile on both tile paths, and the row still sums.
func TestEmptyLaneTileHasLabelAndNoLink(t *testing.T) {
	for _, withHealth := range []bool{false, true} {
		sqlDB := openReportsTestDB(t)
		seedSourceUnits(t, sqlDB)
		if _, err := sqlDB.ExecContext(context.Background(), `UPDATE work_queue SET provider_lane = '' WHERE title = 'u1'`); err != nil {
			t.Fatal(err)
		}
		repo := reports.New(sqlDB)
		u := NewUI(config.Config{}, "v-test", WithReports(repo))
		if withHealth {
			u.AttachLaneHealth(func() []orchestrator.LaneState {
				return []orchestrator.LaneState{{Provider: providers.Musixmatch, State: orchestrator.LaneStateClosed}}
			})
		}
		view, err := u.buildDashboardView(httptest.NewRequest("GET", "/dashboard", nil))
		if err != nil {
			t.Fatal(err)
		}
		qs, err := repo.QueueSummary(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		sum, found := 0, false
		for _, tile := range view.ProviderTiles {
			n, _ := strconv.Atoi(tile.Value)
			sum += n
			if tile.Label == emptyLaneLabel {
				found = true
				if tile.Href != "" {
					t.Errorf("health=%v: empty-lane tile links to %q", withHealth, tile.Href)
				}
			}
			if tile.Label == "" {
				t.Errorf("health=%v: a tile has a blank label", withHealth)
			}
		}
		if !found {
			t.Errorf("health=%v: no %q tile", withHealth, emptyLaneLabel)
		}
		if want := int(qs.Finished + qs.SettledUpgradable); sum != want {
			t.Errorf("health=%v: sum %d, want %d", withHealth, sum, want)
		}
	}
}
