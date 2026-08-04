package db

import (
	"context"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/ffmpeg"
)

// Migration 043 bounds oversized work_queue.last_error values already stored
// (#731). Asserted against the REAL migration via openAtVersion -- see the
// comment on that helper in migration_038_test.go for why a hand-executed copy
// of the UPDATE proves nothing.
//
// The behaviors that matter are the ones a careless bound gets wrong: an
// oversized value must lose its middle while keeping BOTH ends (a head-only
// truncation discards the terminating line that carries the actual cause), and
// an ordinary value must come through byte-identical rather than being reformatted
// by a statement that matched more rows than it should.
func TestMigration043BoundsOnlyOversizedLastErrors(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 42) // the state a real deployment upgrades FROM

	const (
		firstLine = "FIRSTLINE decoder opened"
		lastLine  = "LASTLINE decode error rate exceeds maximum"
	)
	oversized := firstLine + "\n" + strings.Repeat("Header missing\n", 20000) + lastLine
	ordinary := "musixmatch: no results found"
	// Exactly at the threshold: the WHERE clause is strictly greater-than, so this
	// must NOT be touched. An off-by-one that used >= would reformat it.
	atCap := strings.Repeat("z", 4096)

	insert := `INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, updated_at)
	           VALUES (?, ?, ?, 'a', 't', '/x.mp3', 'deferred', ?, '2026-08-04T00:00:00Z')`
	for _, r := range []struct {
		id  int64
		key string
		val any
	}{
		{801, "k1", oversized},
		{802, "k2", ordinary},
		{803, "k3", atCap},
		// No NULL case: work_queue.last_error is NOT NULL, so the schema already
		// rules it out and an inserted NULL fails the constraint rather than
		// reaching the migration.
	} {
		if _, err := dbh.ExecContext(ctx, insert, r.id, r.key, r.key, r.val); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	if _, err := provider.UpTo(ctx, 43); err != nil {
		t.Fatalf("migrate to 43: %v", err)
	}

	get := func(id int64) string {
		var s string
		if err := dbh.QueryRowContext(ctx, `SELECT last_error FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("select %d: %v", id, err)
		}
		return s
	}

	got := get(801)
	if len(got) > 4096 {
		t.Errorf("oversized value still exceeds the cap: %d bytes", len(got))
	}
	if !strings.Contains(got, firstLine) {
		t.Error("bound dropped the head, which names the failing decoder")
	}
	if !strings.Contains(got, lastLine) {
		t.Error("bound dropped the tail, which carries the terminating cause")
	}
	if !strings.Contains(got, "omitted") {
		t.Error("bounded value does not announce its elision, so it reads as complete")
	}

	if got := get(802); got != ordinary {
		t.Errorf("ordinary value was altered:\n got %q\nwant %q", got, ordinary)
	}
	if got := get(803); got != atCap {
		t.Errorf("value exactly at the cap was altered (off-by-one in the WHERE clause): got %d chars", len(got))
	}
}

// The migration reimplements ffmpeg.BoundOutput's policy in SQL because SQLite
// cannot call into Go. That makes the two independently editable and therefore
// able to drift: raising the Go cap while leaving the migration at 4096 would
// leave the database enforcing a stricter rule than the code, silently.
//
// This pins them together. It asserts the CEILING rather than exact equality --
// the SQL marker's length varies slightly with the magnitude of the omitted
// count, and SQLite counts characters where Go counts bytes, so the SQL result
// can land marginally under. Under is the safe direction for a size bound; over
// is the regression worth failing on.
func TestMigration043BoundMatchesBoundOutput(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 42)

	oversized := "HEAD marker\n" + strings.Repeat("noise\n", 50000) + "TAIL marker"

	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, updated_at)
		 VALUES (811, 'p1', 'p1', 'a', 't', '/x.mp3', 'deferred', ?, '2026-08-04T00:00:00Z')`,
		oversized); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := provider.UpTo(ctx, 43); err != nil {
		t.Fatalf("migrate to 43: %v", err)
	}

	var sqlBounded string
	if err := dbh.QueryRowContext(ctx, `SELECT last_error FROM work_queue WHERE id = 811`).Scan(&sqlBounded); err != nil {
		t.Fatalf("select: %v", err)
	}
	goBounded := ffmpeg.BoundOutput(oversized)

	if len(sqlBounded) > len(goBounded) {
		t.Errorf("the migration's bound is LOOSER than ffmpeg.BoundOutput -- the two have drifted:\n"+
			"  SQL kept %d bytes, Go kept %d bytes", len(sqlBounded), len(goBounded))
	}
	// Both must preserve the same ends, or they disagree about WHICH bytes matter
	// rather than merely how many.
	for _, want := range []string{"HEAD marker", "TAIL marker"} {
		if !strings.Contains(sqlBounded, want) {
			t.Errorf("SQL bound dropped %q, which ffmpeg.BoundOutput retains", want)
		}
		if !strings.Contains(goBounded, want) {
			t.Errorf("ffmpeg.BoundOutput dropped %q, which the SQL bound retains", want)
		}
	}
}
