package reports_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
)

// TestRecentOutcomesSortedKeepsRowSetAndDefault pins #1260: the default order
// equals RecentOutcomes, and a sort re-orders the newest `limit` rows without
// ever swapping in an older one.
func TestRecentOutcomesSortedKeepsRowSetAndDefault(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	// "A-oldest" sorts first by title but completed earliest: outside limit 3.
	insertWorkItem(t, sqlDB, workItem{artist: "A", title: "A-oldest", status: "done", completedAt: "2026-08-01T00:00:00Z", outcomeType: "synced"})
	// Completion order (newest first) is Mu, Zeta, Beta, which differs from the
	// alphabetical order, so a title sort that ignored its Order would fail.
	for i, title := range []string{"Beta", "Zeta", "Mu"} {
		insertWorkItem(t, sqlDB, workItem{artist: "A", title: title, status: "done",
			completedAt: fmt.Sprintf("2026-08-0%dT00:00:00Z", i+2), outcomeType: "synced"})
	}
	const limit = 3
	plain, err := repo.RecentOutcomes(ctx, limit)
	if err != nil {
		t.Fatal(err)
	}
	def, err := repo.RecentOutcomesSorted(ctx, limit, reports.RecentOutcomesSpec.Default)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plain, def) {
		t.Errorf("default sorted order differs from RecentOutcomes:\n%v\n%v", plain, def)
	}
	byTitle, err := repo.RecentOutcomesSorted(ctx, limit, tablesort.Order{Key: tablesort.KeyTitle})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range byTitle {
		got = append(got, o.Title)
	}
	if want := []string{"Beta", "Mu", "Zeta"}; !reflect.DeepEqual(got, want) {
		t.Errorf("title asc = %v, want %v (A-oldest is outside the newest %d)", got, want, limit)
	}
}

// RecentOutcomesSorted's default is pinned against a LITERAL order, not against
// a query built from the same spec: newest completion first, a completed_at tie
// broken by the higher id, a NULL completed_at last.
func TestRecentOutcomesDefaultOrderIsLiteral(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	for _, w := range []workItem{
		{title: "tie-low-id", completedAt: "2026-08-05T00:00:00Z"},
		{title: "tie-high-id", completedAt: "2026-08-05T00:00:00Z"},
		{title: "older", completedAt: "2026-08-03T00:00:00Z"},
		{title: "never-completed"},
	} {
		w.artist, w.status, w.outcomeType = "A", "done", "synced"
		insertWorkItem(t, sqlDB, w)
	}
	want := []string{"tie-high-id", "tie-low-id", "older", "never-completed"}
	plain, err := repo.RecentOutcomes(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	sorted, err := repo.RecentOutcomesSorted(ctx, 10, reports.RecentOutcomesSpec.Default)
	if err != nil {
		t.Fatal(err)
	}
	for name, rows := range map[string][]reports.RecentOutcome{"RecentOutcomes": plain, "RecentOutcomesSorted": sorted} {
		var got []string
		for _, r := range rows {
			got = append(got, r.Title)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s default = %v, want %v", name, got, want)
		}
	}
}

// TestInstrumentalInventoryFileTieBreak pins sr.id as the last tie-break: one
// track with two linked files lists them in scan_results id order (z.flac was
// indexed first), under the default order and under a title sort that ties.
func TestInstrumentalInventoryFileTieBreak(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := reports.New(sqlDB)
	libID := insertLibrary(t, sqlDB)
	wq := insertWorkItem(t, sqlDB, workItem{artist: "A", title: "T", status: "done", instrumentalResult: 1})
	first := insertScanResult(t, sqlDB, libID, "/music/z.flac")
	second := insertScanResult(t, sqlDB, libID, "/music/a.flac")
	linkScanResult(t, sqlDB, wq, second)
	linkScanResult(t, sqlDB, wq, first)
	want := []string{"/music/z.flac", "/music/a.flac"}
	for _, o := range []tablesort.Order{{}, {Key: tablesort.KeyTitle}, {Key: tablesort.KeyTitle, Desc: true}} {
		rows, err := repo.InstrumentalInventorySorted(ctx, o)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, r := range rows {
			got = append(got, r.FilePath)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("order %+v = %v, want %v", o, got, want)
		}
	}
}
