package reports

import (
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/tablesort"
)

// planOf is the EXPLAIN QUERY PLAN detail lines for the real bucket query.
func planOf(t *testing.T, top TopRung, f BucketFilter) string {
	t.Helper()
	d := seedLibraryRows(t)
	q, args, err := bucketQuery(BucketFinished, top, f, BucketSpec(BucketFinished).Default, tablesort.Cursor{}, 50)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := d.Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		out = append(out, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(out, "\n")
}

// TestWordChipAddsNoAccessPath pins #1405: the Word-synced chip is a pure
// row-level predicate, so Finished with word=1 reads the table exactly as
// Finished does without it, under both rungs.
func TestWordChipAddsNoAccessPath(t *testing.T) {
	for _, top := range []TopRung{TopRungWord, TopRungLine} {
		if base, word := planOf(t, top, BucketFilter{}), planOf(t, top, BucketFilter{Word: true}); base != word {
			t.Errorf("rung %v: plan changed by the word chip\nbase:\n%s\nword:\n%s", top, base, word)
		}
	}
}
