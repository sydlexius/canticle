package reports_test

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
)

// seedChipRows inserts done rows that each match or just miss one chip
// predicate. Returns the db for edits.
func seedChipRows(t *testing.T) *reports.Repo {
	t.Helper()
	d := openTestDB(t)
	for _, w := range []workItem{
		{artist: "A", title: "line", status: "done", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "line-edited", status: "done", outcomeType: "synced", syncTier: "line"},
		{artist: "A", title: "line-recheck", status: "done", outcomeType: "synced", syncTier: "line", wordTimingState: "queued"},
		{artist: "A", title: "line-retired", status: "done", outcomeType: "synced", syncTier: "line", lastError: queue.UnresolvableGoneError},
		{artist: "A", title: "word", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "word-edited", status: "done", outcomeType: "synced", syncTier: "word"},
		{artist: "A", title: "word-missync", status: "done", outcomeType: "synced", syncTier: "word", timingOutcome: "mis_synced"},
		{artist: "A", title: "line-missync", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "mis_synced"},
		{artist: "A", title: "line-cat", status: "done", outcomeType: "synced", syncTier: "line", timingOutcome: "categorical"},
		// A line tier stamp on a NON-synced outcome: the line chip must still miss it.
		{artist: "A", title: "line-unsynced-outcome", status: "done", outcomeType: "unsynced", syncTier: "line"},
		{artist: "A", title: "plain-txt", status: "done", outcomeType: "unsynced"},
		{artist: "A", title: "txt-edited", status: "done", outcomeType: "unsynced"},
	} {
		insertWorkItem(t, d, w)
	}
	for _, title := range []string{"line-edited", "word-edited", "txt-edited"} {
		if _, err := d.Exec(`UPDATE work_queue SET lyric_edited_at = '2026-01-01T00:00:00Z' WHERE title = ?`, title); err != nil {
			t.Fatal(err)
		}
	}
	return reports.New(d)
}

// chipTitles lists every title the filter returns on bucket b, sorted.
func chipTitles(t *testing.T, repo *reports.Repo, b reports.Bucket, f reports.BucketFilter) string {
	t.Helper()
	var out []string
	sp := reports.BucketSpec(b)
	rows, err := repo.ListBucketFiltered(context.Background(), b, f, sp.Default, tablesort.Cursor{}, reports.MaxBucketLimit)
	if err != nil {
		t.Fatalf("ListBucketFiltered(%s): %v", b, err)
	}
	for _, r := range rows {
		out = append(out, r.Title)
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestBucketChipsReturnExactlyTheirPredicate(t *testing.T) {
	repo := seedChipRows(t)
	const settledAll = "line,line-cat,line-edited,line-missync,line-recheck,line-retired,line-unsynced-outcome,plain-txt,txt-edited,word-missync"
	cases := []struct {
		name   string
		bucket reports.Bucket
		f      reports.BucketFilter
		want   string
	}{
		{"settled none", reports.BucketSettled, reports.BucketFilter{}, settledAll},
		{"finished none", reports.BucketFinished, reports.BucketFilter{}, "word,word-edited"},
		{"settled line", reports.BucketSettled, reports.BucketFilter{Tier: reports.TierLine}, "line,line-edited"},
		{"settled edited", reports.BucketSettled, reports.BucketFilter{Edited: true}, "line-edited,txt-edited"},
		{"finished edited", reports.BucketFinished, reports.BucketFilter{Edited: true}, "word-edited"},
		{"settled missync", reports.BucketSettled, reports.BucketFilter{MisSynced: true}, "line-missync,word-missync"},
		{"settled line+edited", reports.BucketSettled, reports.BucketFilter{Tier: reports.TierLine, Edited: true}, "line-edited"},
		{"settled line+missync is empty (shared tier predicate excludes mis_synced)", reports.BucketSettled, reports.BucketFilter{Tier: reports.TierLine, MisSynced: true}, ""},
		{"settled edited+missync", reports.BucketSettled, reports.BucketFilter{Edited: true, MisSynced: true}, ""},
		{"unknown tier is no filter", reports.BucketSettled, reports.BucketFilter{Tier: "bogus"}, settledAll},
		{"word is not a tier", reports.BucketSettled, reports.BucketFilter{Tier: "word"}, settledAll},
	}
	for _, tc := range cases {
		if got := chipTitles(t, repo, tc.bucket, tc.f); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The line chip must select exactly the rows the dashboard counts as line-synced:
// the same shared predicate, read through ResultsBreakdown. Finished is the
// dashboard's word-synced tile.
func TestBucketChipsAgreeWithDashboardCounts(t *testing.T) {
	repo := seedChipRows(t)
	rb, err := repo.ResultsBreakdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := chipTitles(t, repo, reports.BucketSettled, reports.BucketFilter{Tier: reports.TierLine})
	if n := int64(len(strings.Split(got, ","))); got == "" || n != rb.LineSynced {
		t.Errorf("line chip lists %d rows (%q), dashboard counts %d", n, got, rb.LineSynced)
	}
	got = chipTitles(t, repo, reports.BucketFinished, reports.BucketFilter{})
	if n := int64(len(strings.Split(got, ","))); got == "" || n != rb.WordSynced {
		t.Errorf("finished lists %d rows (%q), dashboard counts %d", n, got, rb.WordSynced)
	}
}

// Paging through a chip + search + sort view returns every matching row once.
func TestBucketChipsPagingWalksEveryRowOnce(t *testing.T) {
	d := openTestDB(t)
	want := map[string]bool{}
	for i := 0; i < 23; i++ {
		title := fmt.Sprintf("t%02d", i)
		tier := "line"
		if i%3 == 0 {
			tier = "word"
		}
		insertWorkItem(t, d, workItem{artist: "walk", title: title, status: "done", outcomeType: "synced", syncTier: tier})
		if i%2 == 0 {
			if _, err := d.Exec(`UPDATE work_queue SET lyric_edited_at = '2026-01-01T00:00:00Z' WHERE title = ?`, title); err != nil {
				t.Fatal(err)
			}
			if tier == "line" {
				want[title] = true
			}
		}
	}
	repo := reports.New(d)
	f := reports.BucketFilter{Query: "walk", Tier: reports.TierLine, Edited: true}
	sp := reports.BucketSpec(reports.BucketSettled)
	o := sp.Resolve(tablesort.KeyTitle, "desc")
	got := map[string]int{}
	var cur tablesort.Cursor
	for page := 0; page < 20; page++ {
		rows, err := repo.ListBucketFiltered(context.Background(), reports.BucketSettled, f, o, cur, 4)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rows {
			got[r.Title]++
		}
		if len(rows) < 4 {
			break
		}
		last := rows[len(rows)-1]
		cur = tablesort.Cursor{ID: last.ID, Val: last.SortVal}
	}
	if len(got) != len(want) {
		t.Fatalf("walk returned %d distinct rows, want %d: %v", len(got), len(want), got)
	}
	for title, n := range got {
		if n != 1 || !want[title] {
			t.Errorf("row %s seen %d times (expected=%v)", title, n, want[title])
		}
	}
}

func TestBucketChipsPerBucket(t *testing.T) {
	want := map[reports.Bucket][]reports.Chip{
		reports.BucketFinished: {reports.ChipEdited},
		reports.BucketSettled:  {reports.ChipLineSynced, reports.ChipEdited, reports.ChipMissynced},
	}
	for _, b := range reports.Buckets() {
		if got := fmt.Sprint(reports.BucketChips(b)); got != fmt.Sprint(want[b]) {
			t.Errorf("BucketChips(%s) = %s, want %v", b, got, want[b])
		}
		if wantAny := len(want[b]) > 0; reports.ChipBucket(b) != wantAny {
			t.Errorf("ChipBucket(%s) = %v, want %v", b, !wantAny, wantAny)
		}
		for _, c := range []reports.Chip{reports.ChipLineSynced, reports.ChipEdited, reports.ChipMissynced} {
			wantHas := false
			for _, w := range want[b] {
				wantHas = wantHas || w == c
			}
			if reports.HasChip(b, c) != wantHas {
				t.Errorf("HasChip(%s, %s) = %v, want %v", b, c, !wantHas, wantHas)
			}
		}
	}
}

func TestValidTier(t *testing.T) {
	if !reports.ValidTier("line") || reports.ValidTier("word") || reports.ValidTier("") || reports.ValidTier("x") {
		t.Error("ValidTier vocabulary wrong: only line is a tier chip")
	}
}
