package reports_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/sydlexius/canticle/internal/detectorbackfill"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/reports"
)

// seedSourceFixture covers every type, the unattributed and detector groups, a
// multiplexing lane with two licensors plus a NULL one, and rows the remediation
// guard, retirement and non-done status must keep out of a type or the total.
func seedSourceFixture(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	mx, it := providers.Musixmatch, providers.InnerTube
	type row struct {
		w        workItem
		upstream any
	}
	rows := []row{
		{w: workItem{title: "mx-word", providerLane: mx, outcomeType: "synced", syncTier: "word"}},
		{w: workItem{title: "mx-line", providerLane: mx, outcomeType: "synced", syncTier: "line"}},
		{w: workItem{title: "mx-line-remediated", providerLane: mx, outcomeType: "synced", syncTier: "line", timingOutcome: "mis_synced"}},
		{w: workItem{title: "mx-unsynced", providerLane: mx, outcomeType: "unsynced"}},
		{w: workItem{title: "mx-instr", providerLane: mx, outcomeType: "instrumental"}},
		{w: workItem{title: "mx-retired", providerLane: mx, outcomeType: "synced", syncTier: "word", lastError: queue.UnresolvableGoneError}},
		{w: workItem{title: "it-a-line", providerLane: it, outcomeType: "synced", syncTier: "line"}, upstream: "Musixmatch"},
		{w: workItem{title: "it-a-word", providerLane: it, outcomeType: "synced", syncTier: "word"}, upstream: "Musixmatch"},
		{w: workItem{title: "it-b-line", providerLane: it, outcomeType: "synced", syncTier: "line"}, upstream: "LyricFind"},
		{w: workItem{title: "it-null", providerLane: it, outcomeType: "unsynced"}},
		{w: workItem{title: "it-null2", providerLane: it, outcomeType: "synced"}},
		{w: workItem{title: "det", providerLane: detectorbackfill.LaneName, outcomeType: "instrumental"}},
		{w: workItem{title: "none-line", outcomeType: "synced", syncTier: "line"}},
		{w: workItem{title: "none-legacy"}},
		// Not done: must never appear.
		{w: workItem{title: "pending", providerLane: mx, outcomeType: "synced", syncTier: "word", status: "pending"}},
	}
	for _, r := range rows {
		if r.w.status == "" {
			r.w.status = "done"
		}
		r.w.artist = "A"
		id := insertWorkItem(t, sqlDB, r.w)
		if r.upstream != nil {
			if _, err := sqlDB.Exec(`UPDATE work_queue SET upstream = ? WHERE id = ?`, r.upstream, id); err != nil {
				t.Fatalf("set upstream: %v", err)
			}
		}
	}
}

func findSource(t *testing.T, got []reports.SourceBreakdown, lane string, unattributed bool) reports.SourceBreakdown {
	t.Helper()
	for _, s := range got {
		if s.Lane == lane && s.Unattributed == unattributed {
			return s
		}
	}
	t.Fatalf("source %q (unattributed=%v) missing from %+v", lane, unattributed, got)
	return reports.SourceBreakdown{}
}

func TestSourceBreakdownGroupsByTypeAndUpstream(t *testing.T) {
	sqlDB := openTestDB(t)
	seedSourceFixture(t, sqlDB)
	got, err := reports.New(sqlDB).SourceBreakdown(context.Background())
	if err != nil {
		t.Fatalf("SourceBreakdown: %v", err)
	}

	if len(got) != 4 {
		t.Fatalf("want 4 groups (musixmatch, innertube, detector, unattributed), got %d: %+v", len(got), got)
	}
	if !got[len(got)-1].Unattributed {
		t.Errorf("unattributed group must sort last: %+v", got)
	}

	mx := findSource(t, got, providers.Musixmatch, false)
	// The mis_synced line row is tier unknown; the retired row is other.
	wantMX := reports.TypeCounts{WordSynced: 1, LineSynced: 1, Unsynced: 1, Instrumental: 1, TierUnknown: 1, Other: 1}
	if mx.Counts != wantMX {
		t.Errorf("musixmatch counts = %+v, want %+v", mx.Counts, wantMX)
	}
	if mx.Upstreams != nil {
		t.Errorf("a single-source lane must carry no upstream split: %+v", mx.Upstreams)
	}

	it := findSource(t, got, providers.InnerTube, false)
	wantIT := reports.TypeCounts{WordSynced: 1, LineSynced: 2, Unsynced: 1, TierUnknown: 1}
	if it.Counts != wantIT {
		t.Errorf("innertube counts = %+v, want %+v", it.Counts, wantIT)
	}
	if len(it.Upstreams) != 3 {
		t.Fatalf("innertube upstreams = %+v, want 3", it.Upstreams)
	}
	// Ordered by total desc, not-recorded last: Musixmatch (2), LyricFind (1), NULL (2).
	if it.Upstreams[0].Upstream != "Musixmatch" || it.Upstreams[0].Counts != (reports.TypeCounts{WordSynced: 1, LineSynced: 1}) {
		t.Errorf("upstream[0] = %+v", it.Upstreams[0])
	}
	if it.Upstreams[1].Upstream != "LyricFind" || it.Upstreams[1].Counts != (reports.TypeCounts{LineSynced: 1}) {
		t.Errorf("upstream[1] = %+v", it.Upstreams[1])
	}
	if !it.Upstreams[2].NotRecorded || it.Upstreams[2].Upstream != "" ||
		it.Upstreams[2].Counts != (reports.TypeCounts{Unsynced: 1, TierUnknown: 1}) {
		t.Errorf("upstream[2] (not recorded) = %+v", it.Upstreams[2])
	}

	det := findSource(t, got, detectorbackfill.LaneName, false)
	if det.Counts != (reports.TypeCounts{Instrumental: 1}) {
		t.Errorf("detector counts = %+v", det.Counts)
	}
	un := findSource(t, got, "", true)
	if un.Counts != (reports.TypeCounts{LineSynced: 1, Other: 1}) {
		t.Errorf("unattributed counts = %+v", un.Counts)
	}
}

func TestSourceBreakdownSumsAndAgreesWithDashboard(t *testing.T) {
	sqlDB := openTestDB(t)
	seedSourceFixture(t, sqlDB)
	ctx := context.Background()
	repo := reports.New(sqlDB)
	got, err := repo.SourceBreakdown(ctx)
	if err != nil {
		t.Fatalf("SourceBreakdown: %v", err)
	}
	summary, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatalf("QueueSummary: %v", err)
	}
	rb, err := repo.ResultsBreakdown(ctx)
	if err != nil {
		t.Fatalf("ResultsBreakdown: %v", err)
	}

	var all reports.TypeCounts
	for _, s := range got {
		all.WordSynced += s.Counts.WordSynced
		all.LineSynced += s.Counts.LineSynced
		all.Unsynced += s.Counts.Unsynced
		all.Instrumental += s.Counts.Instrumental
		all.TierUnknown += s.Counts.TierUnknown
		all.Other += s.Counts.Other
		if len(s.Upstreams) > 0 {
			var sum int64
			for _, u := range s.Upstreams {
				sum += u.Counts.Total()
			}
			if sum != s.Counts.Total() {
				t.Errorf("%s: upstream totals %d != source total %d", s.Lane, sum, s.Counts.Total())
			}
		}
	}
	if all.Total() != summary.Done {
		t.Errorf("source totals sum to %d, QueueSummary.Done = %d", all.Total(), summary.Done)
	}
	want := reports.TypeCounts{
		WordSynced: rb.WordSynced, LineSynced: rb.LineSynced, Unsynced: rb.Unsynced,
		Instrumental: rb.Instrumental, TierUnknown: rb.SyncedTierUnknown, Other: rb.Other,
	}
	if all != want {
		t.Errorf("per-type sums over sources = %+v, ResultsBreakdown = %+v", all, want)
	}
}

// TestSourceBreakdownTieBreakOrder pins the ordering when totals are equal:
// sources fall back to lane ascending and licensors to name ascending. There
// are more than 12 lanes with mixed totals because sort.Slice is a stable
// insertion sort at or below that size and would keep SQL's alphabetical order,
// hiding a missing tie-break; the licensors come out of a map, so eight make a
// missing tie-break near certain to show.
func TestSourceBreakdownTieBreakOrder(t *testing.T) {
	sqlDB := openTestDB(t)
	it := providers.InnerTube
	ups := []string{"Up-h", "Up-g", "Up-f", "Up-e", "Up-d", "Up-c", "Up-b", "Up-a"}
	for c := 'p'; c >= 'c'; c-- {
		for n := 0; n < 1+int(c)%3; n++ {
			insertWorkItem(t, sqlDB, workItem{title: string(c) + string(rune('a'+n)), artist: "A", status: "done", providerLane: "lane-" + string(c), outcomeType: "unsynced"})
		}
	}
	for _, u := range ups {
		id := insertWorkItem(t, sqlDB, workItem{title: "it-" + u, artist: "A", status: "done", providerLane: it, outcomeType: "unsynced"})
		if _, err := sqlDB.Exec(`UPDATE work_queue SET upstream = ? WHERE id = ?`, u, id); err != nil {
			t.Fatalf("set upstream: %v", err)
		}
	}
	got, err := reports.New(sqlDB).SourceBreakdown(context.Background())
	if err != nil {
		t.Fatalf("SourceBreakdown: %v", err)
	}
	for i := 1; i < len(got); i++ {
		if a, b := got[i-1], got[i]; a.Counts.Total() < b.Counts.Total() || (a.Counts.Total() == b.Counts.Total() && a.Lane >= b.Lane) {
			t.Fatalf("equal-total sources must order by lane ascending, got %q before %q", got[i-1].Lane, got[i].Lane)
		}
	}
	for _, s := range got {
		if s.Lane != it {
			continue
		}
		for i := 1; i < len(s.Upstreams); i++ {
			if s.Upstreams[i-1].Upstream >= s.Upstreams[i].Upstream {
				t.Fatalf("equal-total upstreams must order by name ascending, got %q before %q", s.Upstreams[i-1].Upstream, s.Upstreams[i].Upstream)
			}
		}
	}
}

// TestDoneByLaneMatchesBreakdown pins #1422: the count the
// dashboard tile reads equals the SourceBreakdown unattributed group total (the
// figure the unattributed page shows), over attributed, unattributed, blocked
// and not-done rows, and a database with none is zero.
func TestDoneByLaneMatchesBreakdown(t *testing.T) {
	ctx := context.Background()
	empty := reports.New(openTestDB(t))
	if d, err := empty.DoneByLane(ctx); err != nil || d.Unattributed != 0 || len(d.ByLane) != 0 {
		t.Fatalf("empty db: %+v, err = %v, want zero", d, err)
	}

	sqlDB := openTestDB(t)
	seedSourceFixture(t, sqlDB)
	for _, w := range []workItem{
		{title: "none-blocked", outcomeType: "blocked", status: "done"},
		{title: "none-pending", outcomeType: "synced", syncTier: "line", status: "pending"},
		{title: "none-failed", status: "failed"},
		{title: "lane-blocked", providerLane: providers.Musixmatch, outcomeType: "blocked", status: "done"},
	} {
		w.artist = "A"
		insertWorkItem(t, sqlDB, w)
	}
	repo := reports.New(sqlDB)
	done, err := repo.DoneByLane(ctx)
	if err != nil {
		t.Fatalf("DoneByLane: %v", err)
	}
	got := done.Unattributed
	// none-line, none-legacy and none-blocked: the pending/failed and the
	// attributed blocked rows must not count.
	if got != 3 {
		t.Errorf("DoneByLane Unattributed = %d, want 3", got)
	}
	groups, err := repo.SourceBreakdown(ctx)
	if err != nil {
		t.Fatalf("SourceBreakdown: %v", err)
	}
	if want := findSource(t, groups, "", true).Counts.Total(); got != want {
		t.Errorf("count %d != SourceBreakdown unattributed total %d", got, want)
	}
	for _, g := range groups {
		if g.Unattributed {
			continue
		}
		if done.ByLane[g.Lane] != g.Counts.Total() {
			t.Errorf("DoneByLane[%q] = %d, SourceBreakdown total = %d", g.Lane, done.ByLane[g.Lane], g.Counts.Total())
		}
	}
	if len(done.ByLane) != len(groups)-1 {
		t.Errorf("DoneByLane has %d lanes, SourceBreakdown has %d attributed groups", len(done.ByLane), len(groups)-1)
	}
}
