package templates

import (
	"strings"
	"testing"
)

// TestChartDataHasData covers the all-zero/empty -> false and any-nonzero ->
// true logic that gates whether a chart renders.
func TestChartDataHasData(t *testing.T) {
	tests := []struct {
		name   string
		values []float64
		want   bool
	}{
		{"nil", nil, false},
		{"empty", []float64{}, false},
		{"all zero", []float64{0, 0, 0}, false},
		{"one nonzero", []float64{0, 0, 5}, true},
		{"negative counts as data", []float64{-1}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := ChartData{Values: tc.values}
			if got := c.HasData(); got != tc.want {
				t.Errorf("HasData() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChartDataJSON verifies the labels/values serialize to compact JSON arrays
// suitable for a canvas data attribute, and that empty series produce "[]".
func TestChartDataJSON(t *testing.T) {
	c := ChartData{
		Labels: []string{"Pending", "Done"},
		Values: []float64{1, 75.5},
	}
	if got, want := c.LabelsJSON(), `["Pending","Done"]`; got != want {
		t.Errorf("LabelsJSON() = %q, want %q", got, want)
	}
	if got, want := c.ValuesJSON(), `[1,75.5]`; got != want {
		t.Errorf("ValuesJSON() = %q, want %q", got, want)
	}

	empty := ChartData{}
	if got := empty.LabelsJSON(); got != "null" && got != "[]" {
		t.Errorf("LabelsJSON() for nil = %q, want null or []", got)
	}
	if got := empty.ValuesJSON(); got != "null" && got != "[]" {
		t.Errorf("ValuesJSON() for nil = %q, want null or []", got)
	}
}

// TestDashQueueTileTooltipUnavailable guards the #477 tile: its tooltip must be
// set (a missing case renders title=""), must not reuse Failed's copy, and must
// name the manual revival command, since nothing revives these rows on its own.
func TestDashQueueTileTooltipUnavailable(t *testing.T) {
	got := dashQueueTileTooltip("Unavailable")
	if got == "" {
		t.Fatal("Unavailable tooltip is empty")
	}
	if got == dashQueueTileTooltip("Failed") {
		t.Errorf("Unavailable tooltip %q duplicates Failed's", got)
	}
	if !strings.Contains(got, "queue recheck --retired") {
		t.Errorf("Unavailable tooltip %q does not name the manual revival command", got)
	}
}
