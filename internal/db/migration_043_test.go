package db

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

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

// The migration reimplements BoundOutput's policy in SQL because SQLite cannot
// call into Go, so the two are independently editable and can drift.
//
// BOTH SIDES ARE PINNED TO A HARD LITERAL, not to each other. An earlier version
// of this test asserted only `len(sql) > len(go)`, which is one-sided and
// therefore vacuous in the direction that actually happens: raising
// maxCapturedOutput makes the Go result BIGGER, so the comparison is satisfied
// MORE easily and the test passes under the exact drift its comment claimed to
// catch (verified -- setting the constant to 1048576 left this green). A test
// that passes under the drift it names is worse than none: it is a false
// assurance a future editor will trust.
//
// The literal 4096 is the contract both implementations must satisfy. The two
// are NOT required to produce identical output -- the SQL cut is deliberately
// more conservative, since it must assume worst-case 4-byte runes to stay
// byte-bounded without splitting one. What must hold for both is: at or under
// the byte cap, and both diagnostic ends retained.
func TestMigration043BoundMatchesBoundOutput(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 42)

	// ASCII and multi-byte. The multi-byte case is the one that caught the real
	// defect: a character-counting migration bounded 12,000 bytes to "4,096"
	// characters that were 8,162 bytes, and let a 3,000-character/12,000-byte
	// value through untouched because length() never saw it as oversized.
	cases := []struct {
		id        int64
		key       string
		oversized string
	}{
		{811, "p1", "HEAD marker\n" + strings.Repeat("noise\n", 50000) + "TAIL marker"},
		{812, "p2", "HEAD marker\n" + strings.Repeat("é", 200000) + "\nTAIL marker"},
		{813, "p3", "HEAD marker\n" + strings.Repeat("😀", 100000) + "\nTAIL marker"},
		// Under the character threshold but far OVER the byte threshold: the exact
		// shape that evaded the first draft's WHERE clause entirely.
		{814, "p4", "HEAD marker\n" + strings.Repeat("😀", 3000) + "\nTAIL marker"},
	}
	for _, c := range cases {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, updated_at)
			 VALUES (?, ?, ?, 'a', 't', '/x.mp3', 'deferred', ?, '2026-08-04T00:00:00Z')`,
			c.id, c.key, c.key, c.oversized); err != nil {
			t.Fatalf("insert %d: %v", c.id, err)
		}
	}
	if _, err := provider.UpTo(ctx, 43); err != nil {
		t.Fatalf("migrate to 43: %v", err)
	}

	for _, c := range cases {
		var sqlBounded string
		if err := dbh.QueryRowContext(ctx,
			`SELECT last_error FROM work_queue WHERE id = ?`, c.id).Scan(&sqlBounded); err != nil {
			t.Fatalf("select %d: %v", c.id, err)
		}
		goBounded := ffmpeg.BoundOutput(c.oversized)

		if len(sqlBounded) > 4096 {
			t.Errorf("id %d: the migration left %d BYTES, over the 4096 cap", c.id, len(sqlBounded))
		}
		if len(goBounded) > 4096 {
			t.Errorf("id %d: ffmpeg.BoundOutput left %d BYTES, over the 4096 cap", c.id, len(goBounded))
		}
		if !utf8.ValidString(sqlBounded) {
			t.Errorf("id %d: the migration produced invalid UTF-8", c.id)
		}
		// Both must preserve the same ends, or they disagree about WHICH bytes
		// matter rather than merely how many.
		for _, want := range []string{"HEAD marker", "TAIL marker"} {
			if !strings.Contains(sqlBounded, want) {
				t.Errorf("id %d: SQL bound dropped %q", c.id, want)
			}
			if !strings.Contains(goBounded, want) {
				t.Errorf("id %d: ffmpeg.BoundOutput dropped %q", c.id, want)
			}
		}
	}
}
