package reports_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/reports"
)

// insertKeyed inserts a pending row whose keys are stamped the way the app
// stamps them (normalize.NormalizeKey), which insertWorkItem's raw keys are not.
func insertKeyed(t *testing.T, db *sql.DB, artist, title string) int64 {
	t.Helper()
	res, err := db.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status)
         VALUES (?, ?, ?, ?, '', 'pending')`,
		artist, title, normalize.NormalizeKey(artist), normalize.NormalizeKey(title))
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func searchTitles(t *testing.T, repo *reports.Repo, q string) []string {
	t.Helper()
	rows, err := repo.ListBucketFiltered(context.Background(), reports.BucketPending,
		reports.BucketFilter{Query: q}, 0, reports.MaxBucketLimit)
	if err != nil {
		t.Fatalf("ListBucketFiltered(%q): %v", q, err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Title)
	}
	return out
}

func sameSet(got []string, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]bool{}
	for _, g := range got {
		m[g] = true
	}
	for _, w := range want {
		if !m[w] {
			return false
		}
	}
	return true
}

func TestListBucketFilteredSearch(t *testing.T) {
	db := openTestDB(t)
	insertKeyed(t, db, "Invented Artist", "Quiet Harbor")
	insertKeyed(t, db, "Other Band", "Invented Song")
	insertKeyed(t, db, "Other Band", "Plain Title")
	insertKeyed(t, db, "Björk Stand-in", "Café Tune")
	insertKeyed(t, db, "Discount", "100% Done")
	insertKeyed(t, db, "Discount", "Snake_case")
	insertKeyed(t, db, "Discount", "Plain Title Two")
	insertKeyed(t, db, "Slash", `back\slash`)
	repo := reports.New(db)

	cases := []struct {
		name string
		q    string
		want []string
	}{
		{"by artist", "invented artist", []string{"Quiet Harbor"}},
		{"by title", "harbor", []string{"Quiet Harbor"}},
		{"artist or title", "invented", []string{"Quiet Harbor", "Invented Song"}},
		{"case-insensitive", "QUIET hArBoR", []string{"Quiet Harbor"}},
		{"non-ascii query matches stored key", "björk", []string{"Café Tune"}},
		{"unaccented query matches accented row", "bjork", []string{"Café Tune"}},
		{"accented query on accented title", "CAFÉ", []string{"Café Tune"}},
		{"literal percent", "100%", []string{"100% Done"}},
		{"literal underscore", "snake_c", []string{"Snake_case"}},
		{"underscore is not a one-char wildcard", "snake?case", nil},
		{"bare percent is not match-all", "%", []string{"100% Done"}},
		{"literal backslash", `back\s`, []string{`back\slash`}},
		{"no match", "zzzz-nothing", nil},
		{"blank query is no filter", "   ", []string{"Quiet Harbor", "Invented Song", "Plain Title", "Café Tune", "100% Done", "Snake_case", "Plain Title Two", `back\slash`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := searchTitles(t, repo, c.q); !sameSet(got, c.want...) {
				t.Errorf("query %q = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

func TestListBucketFilteredPagesAcrossBoundary(t *testing.T) {
	db := openTestDB(t)
	// Interleave matches and non-matches so a page boundary falls between them.
	var want []int64
	for i := 0; i < 10; i++ {
		want = append(want, insertKeyed(t, db, "Needle Act", fmt.Sprintf("match %02d", i)))
		insertKeyed(t, db, "Haystack", fmt.Sprintf("other %02d", i))
	}
	repo := reports.New(db)
	var got []int64
	var after int64
	for pages := 0; pages < 20; pages++ {
		page, err := repo.ListBucketFiltered(context.Background(), reports.BucketPending,
			reports.BucketFilter{Query: "NEEDLE"}, after, 4)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			got = append(got, r.ID)
		}
		if len(page) < 4 {
			break
		}
		after = page[len(page)-1].ID
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("paged ids = %v, want %v", got, want)
	}
}

func TestListBucketFilteredRespectsBucket(t *testing.T) {
	db := openTestDB(t)
	insertWorkItem(t, db, workItem{artist: "needle", title: "t1", status: "failed"})
	insertKeyed(t, db, "needle", "t2")
	repo := reports.New(db)
	if got := searchTitles(t, repo, "needle"); !sameSet(got, "t2") {
		t.Errorf("search leaked across buckets: %v", got)
	}
}
