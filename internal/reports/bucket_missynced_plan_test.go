package reports

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/tablesort"
)

func explainPlan(t *testing.T, d *sql.DB, query string, args ...any) []string {
	t.Helper()
	rows, err := d.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var plans []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plans = append(plans, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plans
}

// The real page query (bucketQuery, as ListBucketFiltered runs it) with the
// mis-synced chip on must never scan work_queue whole. It does NOT pin the
// partial index: measured without ANALYZE (production never runs it) on 20,000
// rows, the default sort (updated desc) and next_attempt asc walk the status
// index in order and filter, while every other sort and direction SEARCHes
// idx_work_queue_missynced; after ANALYZE every sort does. No Settled
// mis-synced COUNT is issued anywhere, so none is pinned. The upgrade sweep's
// arm is the pinned partial-index user (queue.TestUpgradeMissyncedArmUsesPartialIndex).
func TestSettledMissyncedPagePlanIsIndexed(t *testing.T) {
	d := seedLibraryRows(t)
	query, args, err := bucketQuery(BucketSettled, BucketFilter{MisSynced: true}, BucketSpec(BucketSettled).Default, tablesort.Cursor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	plans := explainPlan(t, d, query, args...)
	t.Logf("page plan: %q", plans)
	var wq []string
	for _, p := range plans {
		if strings.Contains(p, "work_queue") {
			wq = append(wq, p)
		}
	}
	if len(wq) != 1 || !strings.HasPrefix(wq[0], "SEARCH work_queue USING INDEX idx_work_queue_") || strings.Contains(wq[0], "SCAN") {
		t.Fatalf("settled mis-synced page plan = %q, want one indexed SEARCH of work_queue", plans)
	}
}
