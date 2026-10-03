package reports_test

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/reports"
	"github.com/sydlexius/canticle/internal/tablesort"
)

func TestBucketSpecDefaults(t *testing.T) {
	soonest := tablesort.Order{Key: tablesort.KeyNextAttempt}
	newest := tablesort.Order{Key: tablesort.KeyUpdated, Desc: true}
	want := map[reports.Bucket]tablesort.Order{
		reports.BucketPending: soonest, reports.BucketDeferred: soonest,
		reports.BucketFailed: newest, reports.BucketUnavailable: newest,
		reports.BucketFinished: newest, reports.BucketSettled: newest, reports.BucketProcessing: newest,
	}
	for _, b := range reports.Buckets() {
		if got := reports.BucketSpec(b).Default; got != want[b] {
			t.Errorf("%s default = %+v, want %+v", b, got, want[b])
		}
	}
	// Reason, Libraries and Lyrics are not sortable.
	if _, ok := reports.BucketSpec(reports.BucketFailed).Columns["reason"]; ok {
		t.Error("reason must not be sortable")
	}
}

func TestListBucketFilteredRejectsUnknownSort(t *testing.T) {
	repo := reports.New(openTestDB(t))
	_, err := repo.ListBucketFiltered(context.Background(), reports.BucketPending, reports.BucketFilter{},
		tablesort.Order{Key: "id;drop"}, tablesort.Cursor{}, 10)
	if err == nil {
		t.Fatal("unknown sort key accepted by the query layer")
	}
}

// The default sort of every bucket must be served by an index (migration 059):
// a temp B-tree means each "Show more" re-sorts the whole bucket. The predicate
// is the bucket's real one (reports.BucketPredicateForTest), so Finished and
// Settled are judged on the SQL ListBucketFiltered actually runs.
func TestDefaultSortsAvoidTempBTree(t *testing.T) {
	d := openTestDB(t)
	for _, b := range reports.Buckets() {
		sp := reports.BucketSpec(b)
		o := sp.Default
		where, args := sp.Keyset(o, tablesort.Cursor{ID: 5, Val: "t2026"})
		rows, err := d.Query("EXPLAIN QUERY PLAN SELECT id FROM work_queue WHERE ("+reports.BucketPredicateForTest(b)+")"+where+" ORDER BY "+sp.OrderBy(o)+" LIMIT 51", args...)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(detail, "TEMP B-TREE") {
				t.Errorf("%s: default sort needs a temp B-tree: %s", b, detail)
			}
		}
		_ = rows.Close()
	}
}
