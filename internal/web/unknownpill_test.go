package web

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/web/templates"
)

// TestDashboardUnknownPillExplainsItself pins #654 review F1/F2: the unknown
// pill carries the row's Detail as its title and an accessible name, and the
// categorical row keeps its lane; a synced pill carries neither attribute.
func TestDashboardUnknownPillExplainsItself(t *testing.T) {
	rows := buildRecentRows([]reports.RecentOutcome{
		{Artist: "A", Title: "legacy", Result: reports.ResultUnknown, Detail: reports.LegacyNoOutcomeDetail},
		{Artist: "A", Title: "quarantined", Result: reports.ResultUnknown, ProviderLane: "musixmatch", Detail: "timing refused: categorical"},
		{Artist: "A", Title: "plain", Result: reports.ResultSynced, ProviderLane: "musixmatch"},
	}, nil)
	var sb strings.Builder
	view := templates.DashboardView{RecentRows: rows}
	if err := templates.DashboardPage("test", nil, view, false, false).Render(context.Background(), &sb); err != nil {
		t.Fatalf("render: %v", err)
	}
	html := sb.String()
	for _, want := range []string{
		`title="` + reports.LegacyNoOutcomeDetail + `"`,
		`title="timing refused: categorical"`,
		`aria-label="no outcome recorded"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("dashboard HTML missing %s", want)
		}
	}
	if n := strings.Count(html, `aria-label="no outcome recorded"`); n != 2 {
		t.Errorf("aria-label count = %d; want 2 (one per unknown row)", n)
	}
	if rows[1].Lane == "" || rows[0].Lane != "" {
		t.Errorf("lane cells: legacy=%q quarantined=%q", rows[0].Lane, rows[1].Lane)
	}
}
