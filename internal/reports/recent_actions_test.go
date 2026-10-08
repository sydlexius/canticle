package reports

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/tablesort"
)

// TestRecentOutcomesCarryActionState pins #1433: each Recent row carries its
// work_queue id and the three flags the Actions column reads, on both statements,
// while the order and the limit stay what they were (newest completion first).
func TestRecentOutcomesCarryActionState(t *testing.T) {
	d := openBlockedDB(t)
	type seed struct {
		title, status, outcome string
		manual, block          bool
		wantLyric              bool
	}
	seeds := []seed{ // oldest first; completed_at ascends with the index
		{title: "Syn", status: "done", outcome: "synced", wantLyric: true},
		{title: "Uns", status: "done", outcome: "unsynced", wantLyric: true},
		{title: "Exh", status: "unavailable", outcome: ""},
		{title: "Man", status: "done", outcome: "instrumental", manual: true},
		{title: "Blo", status: "done", outcome: "blocked", block: true},
		{title: "Ref", status: "done", outcome: "synced", block: true, wantLyric: true},
	}
	ids := map[string]int64{}
	for i, s := range seeds {
		id := seedBlockedRow(t, d, "Band", s.title, s.status, s.outcome, "")
		ids[s.title] = id
		stamp := fmt.Sprintf("2026-08-16T05:%02d:00Z", i)
		q := `UPDATE work_queue SET completed_at = ?`
		if s.manual {
			q += `, manual_instrumental_at = '2026-01-01T00:00:00Z'`
		}
		if _, err := d.Exec(q+` WHERE id = ?`, stamp, id); err != nil {
			t.Fatal(err)
		}
		if s.block {
			seedBlock(t, d, "band", strings.ToLower(s.title), "fp")
		}
	}
	// A pending row has no outcome yet and is never listed.
	seedBlockedRow(t, d, "Band", "Pen", "pending", "", "")

	repo := New(d)
	ctx := context.Background()
	check := func(name string, got []RecentOutcome, wantTitles []string) {
		t.Helper()
		if len(got) != len(wantTitles) {
			t.Fatalf("%s: %d rows, want %d", name, len(got), len(wantTitles))
		}
		for i, o := range got {
			if o.Title != wantTitles[i] {
				t.Errorf("%s: row %d = %s, want %s", name, i, o.Title, wantTitles[i])
			}
			var s seed
			for _, c := range seeds {
				if c.title == o.Title {
					s = c
				}
			}
			if o.ID != ids[o.Title] {
				t.Errorf("%s %s: ID = %d, want %d", name, o.Title, o.ID, ids[o.Title])
			}
			if o.HasLyric != s.wantLyric || o.ManualInstrumental != s.manual || o.Blocked != s.block {
				t.Errorf("%s %s: HasLyric/Manual/Blocked = %v/%v/%v, want %v/%v/%v", name, o.Title,
					o.HasLyric, o.ManualInstrumental, o.Blocked, s.wantLyric, s.manual, s.block)
			}
		}
	}
	newestFirst := []string{"Ref", "Blo", "Man", "Exh", "Uns", "Syn"}
	all, err := repo.RecentOutcomes(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	check("RecentOutcomes", all, newestFirst)
	// The limit still bounds the newest rows, not an arbitrary subset.
	top, err := repo.RecentOutcomes(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	check("RecentOutcomes limit 3", top, newestFirst[:3])

	sorted, err := repo.RecentOutcomesSorted(ctx, 50, tablesort.Order{Key: tablesort.KeyTitle})
	if err != nil {
		t.Fatal(err)
	}
	check("RecentOutcomesSorted title", sorted, []string{"Blo", "Exh", "Man", "Ref", "Syn", "Uns"})
	// The inner limit picks the newest rows before the outer sort arranges them.
	limited, err := repo.RecentOutcomesSorted(ctx, 3, tablesort.Order{Key: tablesort.KeyTitle})
	if err != nil {
		t.Fatal(err)
	}
	check("RecentOutcomesSorted limit 3", limited, []string{"Blo", "Man", "Ref"})
}
