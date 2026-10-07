package reports

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/tablesort"
)

func openBlockedDB(t *testing.T) *sql.DB {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "blocked.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// seedBlockedRow inserts a work_queue row whose identity keys are the lower-cased
// artist and title, and returns its id.
func seedBlockedRow(t *testing.T, d *sql.DB, artist, title, status, outcome, lastErr string) int64 {
	t.Helper()
	res, err := d.ExecContext(context.Background(),
		`INSERT INTO work_queue (artist, title, artist_key, title_key, album, status, outcome_type, last_error, completed_at)
         VALUES (?, ?, ?, ?, 'Album', ?, NULLIF(?, ''), ?, '2026-08-16T05:00:00Z')`,
		artist, title, strings.ToLower(artist), strings.ToLower(title), status, outcome, lastErr)
	if err != nil {
		t.Fatalf("insert work_queue: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func seedBlock(t *testing.T, d *sql.DB, artistKey, titleKey, fingerprint string) {
	t.Helper()
	if _, err := d.ExecContext(context.Background(),
		`INSERT INTO lyric_blocks (artist_key, title_key, fingerprint) VALUES (?, ?, ?)`,
		artistKey, titleKey, fingerprint); err != nil {
		t.Fatalf("insert lyric_blocks: %v", err)
	}
}

// TestBlockedClassIsItsOwnCount pins #1396: a blocked row is counted in the
// Results breakdown's own Blocked field, and neither the miss count
// (QueueSummary.Unavailable, the Recent outcomes miss class) nor Other moves.
func TestBlockedClassIsItsOwnCount(t *testing.T) {
	d := openBlockedDB(t)
	seedBlockedRow(t, d, "A", "blocked-1", "done", "blocked", "")
	seedBlockedRow(t, d, "A", "blocked-2", "done", "blocked", "")
	// A prune-retired row keeps its stale outcome but is Other, never Blocked.
	seedBlockedRow(t, d, "A", "blocked-retired", "done", "blocked", queue.UnresolvableGoneError)
	// Blocked-shaped but not settled: not a result.
	seedBlockedRow(t, d, "A", "blocked-pending", "pending", "blocked", "")
	seedBlockedRow(t, d, "A", "rejected", "done", "rejected", "")
	seedBlockedRow(t, d, "A", "legacy", "done", "", "")
	seedBlockedRow(t, d, "A", "word", "done", "unsynced", "")
	seedBlockedRow(t, d, "A", "miss", "unavailable", "", "miss limit reached")

	repo := New(d)
	ctx := context.Background()
	got, err := repo.ResultsBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := ResultsBreakdown{Blocked: 2, Unsynced: 1, Other: 3}
	if got != want {
		t.Errorf("ResultsBreakdown = %+v, want %+v", got, want)
	}
	qs, err := repo.QueueSummary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total() != qs.Done {
		t.Errorf("Results total %d != QueueSummary.Done %d", got.Total(), qs.Done)
	}
	if qs.Unavailable != 1 {
		t.Errorf("QueueSummary.Unavailable (miss count) = %d, want 1: blocked rows are not misses", qs.Unavailable)
	}

	recent, err := repo.RecentOutcomes(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[ResultClass]int{}
	for _, o := range recent {
		classes[o.Result]++
	}
	if classes[ResultBlocked] != 2 || classes[ResultMiss] != 1 || classes[ResultUnknown] != 1 {
		t.Errorf("recent classes = %v, want blocked 2, miss 1, unknown 1 (the legacy row)", classes)
	}

	// The per-source breakdown agrees with the tile.
	sb, err := repo.SourceBreakdown(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var blocked int64
	for _, s := range sb {
		blocked += s.Counts.Blocked
	}
	if blocked != 2 {
		t.Errorf("per-source Blocked total = %d, want 2", blocked)
	}
}

// TestBlockedBucketListsExactlyTheTileRows pins that the Blocked bucket is the
// tile's population: a done, non-retired blocked row, nothing else.
func TestBlockedBucketListsExactlyTheTileRows(t *testing.T) {
	d := openBlockedDB(t)
	seedBlockedRow(t, d, "A", "b1", "done", "blocked", "")
	seedBlockedRow(t, d, "A", "b2", "done", "blocked", "")
	seedBlockedRow(t, d, "A", "retired", "done", "blocked", queue.UnresolvableGoneError)
	seedBlockedRow(t, d, "A", "pending", "pending", "blocked", "")
	seedBlockedRow(t, d, "A", "txt", "done", "unsynced", "")
	repo := New(d)

	b, err := ParseBucket("blocked")
	if err != nil || b != BucketBlocked {
		t.Fatalf("ParseBucket(blocked) = %q, %v", b, err)
	}
	rows, err := repo.ListBucket(context.Background(), BucketBlocked, 0, MaxBucketLimit)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Title] = true
	}
	if len(rows) != 2 || !got["b1"] || !got["b2"] {
		t.Errorf("blocked bucket = %v, want exactly b1 and b2", got)
	}
	bd, err := repo.ResultsBreakdown(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(rows)) != bd.Blocked {
		t.Errorf("bucket lists %d rows, tile counts %d", len(rows), bd.Blocked)
	}
}

// TestBlockedFlagMatchesIdentity pins the Blocked bool on both row types: true
// for a track with a lyric_blocks row for its identity (however many), false
// without one, and a block for a different identity does not leak. Matching is
// on BOTH keys: the same artist with another title, and the same title under
// another artist, are not blocked.
func TestBlockedFlagMatchesIdentity(t *testing.T) {
	d := openBlockedDB(t)
	seedBlockedRow(t, d, "Band", "Song", "done", "blocked", "")
	seedBlockedRow(t, d, "Band", "Other Song", "done", "blocked", "")
	seedBlockedRow(t, d, "Other Band", "Song", "done", "blocked", "")
	seedBlockedRow(t, d, "Free", "Clear", "done", "unsynced", "")
	// Two fingerprints for one identity must not repeat the row.
	seedBlock(t, d, "band", "song", "fp-1")
	seedBlock(t, d, "band", "song", "fp-2")
	repo := New(d)
	ctx := context.Background()

	want := map[string]bool{"Song/Band": true, "Other Song/Band": false, "Song/Other Band": false, "Clear/Free": false}
	rows, err := repo.ListBucket(ctx, BucketBlocked, 0, MaxBucketLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("blocked bucket rows = %d, want 3 (a two-block identity must not repeat)", len(rows))
	}
	for _, r := range rows {
		if w := want[r.Title+"/"+r.Artist]; r.Blocked != w {
			t.Errorf("bucket row %s/%s Blocked = %v, want %v", r.Title, r.Artist, r.Blocked, w)
		}
	}
	settled, err := repo.ListBucket(ctx, BucketSettled, 0, MaxBucketLimit)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range settled {
		if w := want[r.Title+"/"+r.Artist]; r.Blocked != w {
			t.Errorf("settled row %s/%s Blocked = %v, want %v", r.Title, r.Artist, r.Blocked, w)
		}
	}

	for name, fetch := range map[string]func() ([]RecentOutcome, error){
		"RecentOutcomes": func() ([]RecentOutcome, error) { return repo.RecentOutcomes(ctx, 50) },
		"RecentOutcomesSorted": func() ([]RecentOutcome, error) {
			return repo.RecentOutcomesSorted(ctx, 50, tablesort.Order{Key: tablesort.KeyTitle})
		},
	} {
		recent, err := fetch()
		if err != nil {
			t.Fatal(err)
		}
		if len(recent) != 4 {
			t.Fatalf("%s: %d rows, want 4", name, len(recent))
		}
		for _, o := range recent {
			if w := want[o.Title+"/"+o.Artist]; o.Blocked != w {
				t.Errorf("%s: %s/%s Blocked = %v, want %v", name, o.Title, o.Artist, o.Blocked, w)
			}
		}
	}
}

// TestBlockedPredicateKeepsKeysetAndPlan pins that the EXISTS leaves the keyset
// cursor alone (a multi-page walk sees every row once, in order, although every
// row carries two blocks) and that the plan still drives work_queue the way it
// did without the column: the same scan/search line on work_queue, plus only
// the correlated lyric_blocks probe through its identity index.
func TestBlockedPredicateKeepsKeysetAndPlan(t *testing.T) {
	d := openBlockedDB(t)
	const n = 23
	for i := 0; i < n; i++ {
		title := fmt.Sprintf("t%02d", i)
		seedBlockedRow(t, d, "A", title, "done", "blocked", "")
		seedBlock(t, d, "a", title, "fp-1")
		seedBlock(t, d, "a", title, "fp-2")
	}
	repo := New(d)
	var got []int64
	var after int64
	for pages := 0; ; pages++ {
		if pages > n {
			t.Fatal("pagination did not terminate")
		}
		page, err := repo.ListBucket(context.Background(), BucketBlocked, after, 5)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page {
			if !r.Blocked {
				t.Errorf("row %d Blocked = false", r.ID)
			}
			got = append(got, r.ID)
		}
		if len(page) < 5 {
			break
		}
		after = page[len(page)-1].ID
	}
	if len(got) != n {
		t.Fatalf("paged %d rows, want %d: %v", len(got), n, got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("ids have a duplicate or gap at %d: %v", i, got)
		}
	}

	for _, bucket := range []Bucket{BucketBlocked, BucketSettled} {
		plan := func(sql string, args []any) []string {
			rows, err := d.Query("EXPLAIN QUERY PLAN "+sql, args...)
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
			return out
		}
		q, args, err := bucketQuery(bucket, TopRungWord, BucketFilter{}, tablesort.Order{}, tablesort.Cursor{ID: 5}, 5)
		if err != nil {
			t.Fatal(err)
		}
		with := plan(q, args)
		without := plan(strings.Replace(q, blockedExistsSQL+",", "0,", 1), args)
		t.Logf("%s plan with column:    %q", bucket, with)
		t.Logf("%s plan without column: %q", bucket, without)
		var outerWith, outerWithout []string
		for _, l := range with {
			if strings.HasPrefix(l, "SCAN work_queue") || strings.HasPrefix(l, "SEARCH work_queue") {
				outerWith = append(outerWith, l)
			}
		}
		for _, l := range without {
			if strings.HasPrefix(l, "SCAN work_queue") || strings.HasPrefix(l, "SEARCH work_queue") {
				outerWithout = append(outerWithout, l)
			}
		}
		if len(outerWith) == 0 || fmt.Sprint(outerWith) != fmt.Sprint(outerWithout) {
			t.Errorf("%s: work_queue access changed: %q, want %q", bucket, outerWith, outerWithout)
		}
		probed := false
		for _, l := range with {
			if strings.Contains(l, "lyric_blocks") {
				probed = true
				if !strings.Contains(l, "SEARCH") || !strings.Contains(l, "artist_key=?") || !strings.Contains(l, "title_key=?") {
					t.Errorf("%s: lyric_blocks probed as %q, want an index SEARCH on artist_key and title_key", bucket, l)
				}
			}
		}
		if !probed {
			t.Errorf("%s: plan never probes lyric_blocks: %q", bucket, with)
		}
	}
}
