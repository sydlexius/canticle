package db

import (
	"context"
	"strings"
	"testing"
)

// Migration 046 strips the detector lane's wrapper chain from work_queue rows
// already stored (#790). Asserted against the REAL migration via openAtVersion
// -- see the comment on that helper in migration_038_test.go for why a
// hand-executed copy of the UPDATE proves nothing.
//
// WHAT THE STORED SHAPE IS. The pre-#790 lane built its error as
//
//	fmt.Errorf("detector request failed: %w", errors.Join(ErrLaneOutage, cause))
//
// and errors.Join renders its members newline-separated, so the persisted value
// opens with exactly "detector request failed: orchestrator: lane outage\n"
// before the cause. That is 51 characters of wrapper naming two subsystems and
// no problem.
//
// WHY A MIGRATION AND NOT ONLY THE CODE FIX. A deferred row keeps its last_error
// until it eventually succeeds, and a row whose source file is permanently gone
// never will -- so the code fix alone leaves the symptom in place on exactly the
// installs that hit it. Same reasoning as migration 043.
func TestMigration046StripsDetectorLaneWrapper(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 45) // the state a real deployment upgrades FROM

	const wrapper = "detector request failed: orchestrator: lane outage\n"

	// The two production shapes named in #790: a moved file and a corrupt one.
	movedCause := "detector: sample audio with ffmpeg: exit status 254: No such file or directory"
	corruptCause := "detector: sample audio with ffmpeg: exit status 69: Header missing"

	// Rows that must be left byte-identical.
	notReady := "detector not ready: orchestrator: lane not ready (starting up)\ndetector: dial failed"
	verification := "worker: verify lyrics: verification: sample audio with ffmpeg: exit status 3: boom"
	ordinary := "musixmatch: no results found"
	// The wrapper with NOTHING after it. Stripping would leave an empty
	// last_error, which reports.FailureAnalysis normalizes to 'unknown' -- i.e.
	// indistinguishable from "no error recorded". Must be left alone.
	wrapperOnly := "detector request failed: orchestrator: lane outage\n"
	// The wrapper text NOT at the start. The predicate is anchored, so an
	// occurrence anywhere else must not trigger a rewrite.
	notAnchored := "worker: wrapped: detector request failed: orchestrator: lane outage\ncause"

	insert := `INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, updated_at)
	           VALUES (?, ?, ?, 'a', 't', '/x.mp3', 'deferred', ?, '2026-08-27T00:00:00Z')`
	for _, r := range []struct {
		id  int64
		key string
		val string
	}{
		{901, "k1", wrapper + movedCause},
		{902, "k2", wrapper + corruptCause},
		{903, "k3", notReady},
		{904, "k4", verification},
		{905, "k5", ordinary},
		{906, "k6", wrapperOnly},
		{907, "k7", notAnchored},
	} {
		if _, err := dbh.ExecContext(ctx, insert, r.id, r.key, r.key, r.val); err != nil {
			t.Fatalf("insert %d: %v", r.id, err)
		}
	}

	if _, err := provider.UpTo(ctx, 46); err != nil {
		t.Fatalf("migrate to 46: %v", err)
	}

	get := func(id int64) string {
		var s string
		if err := dbh.QueryRowContext(ctx, `SELECT last_error FROM work_queue WHERE id = ?`, id).Scan(&s); err != nil {
			t.Fatalf("select %d: %v", id, err)
		}
		return s
	}

	// THE REWRITTEN ROWS. The cause survives intact; only the wrapper goes.
	if got := get(901); got != movedCause {
		t.Errorf("moved-file row:\n got %q\nwant %q", got, movedCause)
	}
	if got := get(902); got != corruptCause {
		t.Errorf("corrupt-file row:\n got %q\nwant %q", got, corruptCause)
	}
	for _, id := range []int64{901, 902} {
		if strings.Contains(get(id), "orchestrator:") {
			t.Errorf("row %d still carries the sentinel text an operator should never read", id)
		}
		if strings.Contains(get(id), "detector request failed") {
			t.Errorf("row %d still carries the wrapper prefix", id)
		}
	}

	// THE UNTOUCHED ROWS. Each is a distinct way an over-broad predicate goes
	// wrong, which is why they are asserted individually rather than as a set.
	for _, tc := range []struct {
		id   int64
		want string
		why  string
	}{
		{903, notReady, "the not-ready sentinel is a different lane outcome and its text never reaches last_error"},
		{904, verification, "verification does not go through the detector lane and has no orchestrator wrapper"},
		{905, ordinary, "an ordinary provider miss must come through byte-identical"},
		{906, wrapperOnly, "stripping to empty would read as 'no error recorded' rather than as a failure"},
		{907, notAnchored, "the predicate is anchored to the START; a mid-string occurrence is not this bug"},
	} {
		if got := get(tc.id); got != tc.want {
			t.Errorf("row %d was altered (%s):\n got %q\nwant %q", tc.id, tc.why, got, tc.want)
		}
	}
}

// The migration must be RE-RUNNABLE independently of goose's version tracking: a
// stripped row no longer matches the anchored predicate, so applying the
// statement twice is a no-op. This matters because the value is idempotent by
// CONSTRUCTION rather than by bookkeeping -- if the predicate ever matched its
// own output, a re-run would eat a second 51 characters off every row, silently
// truncating the cause.
func TestMigration046IsIdempotent(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 45)

	const wrapper = "detector request failed: orchestrator: lane outage\n"
	cause := "detector: sample audio with ffmpeg: exit status 69: Header missing"

	if _, err := dbh.ExecContext(ctx,
		`INSERT INTO work_queue (id, artist_key, title_key, artist, title, source_path, status, last_error, updated_at)
		 VALUES (910, 'k', 'k', 'a', 't', '/x.mp3', 'deferred', ?, '2026-08-27T00:00:00Z')`,
		wrapper+cause); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := provider.UpTo(ctx, 46); err != nil {
		t.Fatalf("migrate to 46: %v", err)
	}

	var afterFirst string
	if err := dbh.QueryRowContext(ctx, `SELECT last_error FROM work_queue WHERE id = 910`).Scan(&afterFirst); err != nil {
		t.Fatalf("select: %v", err)
	}
	if afterFirst != cause {
		t.Fatalf("first application:\n got %q\nwant %q", afterFirst, cause)
	}

	// Re-run the statement itself, bypassing goose's applied-version record.
	if _, err := dbh.ExecContext(ctx, migration046UpStatement(t)); err != nil {
		t.Fatalf("re-run: %v", err)
	}
	var afterSecond string
	if err := dbh.QueryRowContext(ctx, `SELECT last_error FROM work_queue WHERE id = 910`).Scan(&afterSecond); err != nil {
		t.Fatalf("select after re-run: %v", err)
	}
	if afterSecond != afterFirst {
		t.Errorf("re-running the migration changed the value again:\n got %q\nwant %q", afterSecond, afterFirst)
	}
}

// migration046UpStatement returns the Up statement of migration 046, READ FROM
// THE EMBEDDED MIGRATION FILE at run time.
//
// WHY NOT A HAND-COPY, WHICH IS WHAT THIS REPLACED. The idempotence test above
// needs to apply the statement a SECOND time, after goose has already recorded
// 046 as applied and will not re-run it. The first version of this file
// satisfied that by pasting the UPDATE into a const -- and nothing forced the
// paste to match the file. Gutting the const to `SELECT 1;` left the whole
// package green, because a no-op statement trivially leaves the value
// unchanged, which is the idempotence test's only claim.
//
// A guard test was then added to compare the copy against the file. It worked,
// but it could only constrain the SPAN IT COPIED: strings.Contains is satisfied
// by a SUPERSET, so a second UPDATE appended to the Up block -- precisely the
// "if a second wrapping site were ever added" case the migration's own comment
// anticipates -- passed the guard silently.
//
// Extracting at run time removes the failure mode instead of policing it. There
// is no second copy to drift, no marker list to keep in sync, and an appended
// statement is executed by this test exactly as goose would execute it. The
// reason the test needed the statement was to OBTAIN it, never to RETYPE it.
func migration046UpStatement(t *testing.T) string {
	t.Helper()
	b, err := migrations.ReadFile("migrations/046_work_queue_collapse_lane_wrapper.sql")
	if err != nil {
		t.Fatalf("read migration file: %v", err)
	}
	body := string(b)

	// Bound the Up block explicitly rather than scanning for the first SQL-ish
	// line: the Down block is also a valid statement, and silently applying it
	// would make the idempotence test assert nothing.
	up := strings.Index(body, "-- +goose Up")
	down := strings.Index(body, "-- +goose Down")
	if up < 0 || down < 0 || down < up {
		t.Fatalf("migration file does not have the expected goose Up/Down structure")
	}
	block := body[up:down]

	begin := strings.Index(block, "-- +goose StatementBegin")
	end := strings.Index(block, "-- +goose StatementEnd")
	if begin < 0 || end < 0 || end < begin {
		t.Fatalf("Up block does not have the expected StatementBegin/StatementEnd markers")
	}
	stmt := block[begin+len("-- +goose StatementBegin") : end]

	// Strip the leading comment lines. The file is mostly rationale, and a
	// comment carried into the exec is harmless but makes a failure unreadable.
	var kept []string
	for _, line := range strings.Split(stmt, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		kept = append(kept, line)
	}
	out := strings.TrimSpace(strings.Join(kept, "\n"))

	// A statement that does not look like the migration means the extraction
	// broke, not that the migration changed -- fail loudly rather than applying
	// something unintended and reporting a green idempotence result.
	if !strings.HasPrefix(out, "UPDATE work_queue") {
		t.Fatalf("extracted statement does not start with UPDATE work_queue:\n%s", out)
	}
	return out
}
