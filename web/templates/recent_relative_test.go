package templates

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/a-h/templ"
)

func renderToString(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.String()
}

func relativeRow() RecentOutcomeRow {
	return RecentOutcomeRow{
		Artist: "A", Title: "T", Result: "Synced",
		CompletedAt:         "2026-10-03 11:55 UTC",
		CompletedAtISO:      "2026-10-03T11:55:00Z",
		CompletedAtRelative: "5 min ago",
	}
}

// The Dashboard cell shows the relative label in a <time> that carries the
// machine-readable datetime and the exact time as its tooltip (#1263).
func TestDashRecentOutcomesRelativeCell(t *testing.T) {
	out := renderToString(t, dashRecentOutcomes([]RecentOutcomeRow{relativeRow()}))
	want := `<time datetime="2026-10-03T11:55:00Z" title="2026-10-03 11:55 UTC" data-tz="pending">5 min ago</time>`
	if !strings.Contains(out, want) {
		t.Errorf("dashboard cell missing %s in:\n%s", want, out)
	}
	if strings.Contains(out, ">2026-10-03 11:55 UTC<") {
		t.Error("dashboard still renders the absolute time as visible text")
	}
}

// A row with no completion time keeps the placeholder, no <time> element.
func TestDashRecentOutcomesUnknownTime(t *testing.T) {
	r := relativeRow()
	r.CompletedAt, r.CompletedAtISO, r.CompletedAtRelative = "-", "", "-"
	out := renderToString(t, dashRecentOutcomes([]RecentOutcomeRow{r}))
	if strings.Contains(out, "<time") || strings.Contains(out, "ago") {
		t.Errorf("unknown time rendered a <time> or relative text:\n%s", out)
	}
}

// A row with a timestamp but no relative label (the fallback guard) renders
// the plain absolute text and never a <time> or the relative field.
func TestDashRecentOutcomesNoRelativeFallsBack(t *testing.T) {
	r := relativeRow()
	r.CompletedAtRelative = ""
	out := renderToString(t, dashRecentOutcomes([]RecentOutcomeRow{r}))
	if !strings.Contains(out, ">2026-10-03 11:55 UTC<") {
		t.Errorf("fallback lost the absolute text:\n%s", out)
	}
	if strings.Contains(out, "<time") || strings.Contains(out, "datetime=") {
		t.Errorf("fallback rendered a <time>:\n%s", out)
	}
}

// The fallback branch must print only CompletedAt: a label set alongside an
// empty ISO (no machine-readable time) is not shown either.
func TestDashRecentOutcomesRelativeNeedsISO(t *testing.T) {
	r := relativeRow()
	r.CompletedAtISO = ""
	out := renderToString(t, dashRecentOutcomes([]RecentOutcomeRow{r}))
	if strings.Contains(out, "5 min ago") || strings.Contains(out, "<time") {
		t.Errorf("relative label leaked without an ISO time:\n%s", out)
	}
}

// Reports shares RecentOutcomeRow but must keep the absolute timestamp even
// when the relative field is populated.
func TestReportsRecentOutcomesStaysAbsolute(t *testing.T) {
	out := renderToString(t, tableRecentOutcomes(plainCols(recentLabels...), []RecentOutcomeRow{relativeRow()}))
	if !strings.Contains(out, ">2026-10-03 11:55 UTC<") {
		t.Errorf("reports lost its absolute timestamp:\n%s", out)
	}
	if strings.Contains(out, "5 min ago") || strings.Contains(out, "<time") {
		t.Errorf("reports rendered relative time:\n%s", out)
	}
}
