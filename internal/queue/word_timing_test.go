package queue

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// seedWordCandidate inserts a row that satisfies every candidate predicate:
// done, synced, timing ok, a source path, never examined, completed 2026-01-10.
func seedWordCandidate(t *testing.T, dbh *sql.DB, key string) int64 {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(
		`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status,
             outcome_type, timing_outcome, completed_at)
         VALUES ('A', ?, 'a', ?, '/m/x.flac', 'done', 'synced', 'ok', '2026-01-10T00:00:00Z')
         RETURNING id`, key, key).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", key, err)
	}
	return id
}

func mustExec(t *testing.T, dbh *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := dbh.Exec(q, args...); err != nil {
		t.Fatalf("exec %q: %v", q, err)
	}
}

// TestWordRecheckCandidatePredicates starts every case from one full candidate,
// changes exactly one thing, and asserts count and list agree on the result.
func TestWordRecheckCandidatePredicates(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	const gen = 7
	absentChecked := func(d string) string {
		return `UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = 7, word_timing_checked_at = '` + d + `T00:00:00Z'`
	}
	cases := []struct {
		name   string
		mutate string
		opts   WordRecheckOptions
		want   bool
	}{
		{name: "baseline candidate", want: true},
		{name: "status processing", mutate: `UPDATE work_queue SET status = 'processing'`},
		{name: "outcome unsynced", mutate: `UPDATE work_queue SET outcome_type = 'unsynced'`},
		{name: "timing categorical", mutate: `UPDATE work_queue SET timing_outcome = 'categorical'`},
		{name: "timing mis_synced", mutate: `UPDATE work_queue SET timing_outcome = 'mis_synced'`},
		{name: "timing null still admitted", mutate: `UPDATE work_queue SET timing_outcome = NULL`, want: true},
		{name: "blank source_path", mutate: `UPDATE work_queue SET source_path = '  '`},
		{name: "state queued", mutate: `UPDATE work_queue SET word_timing_state = 'queued'`},
		{name: "state served", mutate: `UPDATE work_queue SET word_timing_state = 'served', word_timing_generation = 1`},
		{name: "absent current generation", mutate: `UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = 7`},
		{name: "absent stale generation", mutate: `UPDATE work_queue SET word_timing_state = 'absent', word_timing_generation = 6`, want: true},
		{name: "absent null generation", mutate: `UPDATE work_queue SET word_timing_state = 'absent'`, want: true},
		{name: "absent current gen checked before recheck cutoff", mutate: absentChecked("2026-01-05"), opts: WordRecheckOptions{RecheckAbsentBefore: day(6)}, want: true},
		{name: "absent current gen checked at recheck cutoff (strict)", mutate: absentChecked("2026-01-06"), opts: WordRecheckOptions{RecheckAbsentBefore: day(6)}},
		{name: "recheck cutoff never re-admits served", mutate: `UPDATE work_queue SET word_timing_state = 'served', word_timing_generation = 7, word_timing_checked_at = '2026-01-01T00:00:00Z'`, opts: WordRecheckOptions{RecheckAbsentBefore: day(6)}},
		{name: "completed before cutoff", opts: WordRecheckOptions{CompletedBefore: day(11)}, want: true},
		{name: "completed at cutoff (strict)", opts: WordRecheckOptions{CompletedBefore: day(10)}},
		{name: "no completed_at under a cutoff", mutate: `UPDATE work_queue SET completed_at = NULL`, opts: WordRecheckOptions{CompletedBefore: day(11)}},
		{name: "other library", opts: WordRecheckOptions{LibraryIDs: []int64{99}}},
		{name: "linked library", opts: WordRecheckOptions{LibraryIDs: []int64{99, 1}}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbh := openQueueTestDB(t)
			q := NewDBQueue(dbh)
			id := seedWordCandidate(t, dbh, "t")
			mustExec(t, dbh, `INSERT INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
			mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (1, 1, '/m/x.flac', 'done')`)
			mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, 1)`, id)
			if tc.mutate != "" {
				mustExec(t, dbh, tc.mutate)
			}
			tc.opts.Generation = gen
			n, err := q.CountWordRecheckCandidates(ctx, tc.opts)
			if err != nil {
				t.Fatalf("count: %v", err)
			}
			ids, err := q.ListWordRecheckCandidates(ctx, tc.opts)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if n != len(ids) {
				t.Fatalf("count %d != list len %d", n, len(ids))
			}
			if got := n == 1; got != tc.want {
				t.Fatalf("candidate = %v; want %v", got, tc.want)
			}
		})
	}
}

// TestWordRecheckListOrderAndLimit pins oldest-first and the limit, which the
// count ignores.
func TestWordRecheckListOrderAndLimit(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	newer := seedWordCandidate(t, dbh, "newer")
	older := seedWordCandidate(t, dbh, "older")
	seedWordCandidate(t, dbh, "third")
	mustExec(t, dbh, `UPDATE work_queue SET completed_at = '2025-12-01T00:00:00Z' WHERE id = ?`, older)
	ids, err := q.ListWordRecheckCandidates(ctx, WordRecheckOptions{Limit: 2})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 2 || ids[0] != older || ids[1] != newer {
		t.Fatalf("ids = %v; want [%d %d]", ids, older, newer)
	}
	if n, err := q.CountWordRecheckCandidates(ctx, WordRecheckOptions{Limit: 2}); err != nil || n != 3 {
		t.Fatalf("count = %d, %v; want 3 (limit ignored)", n, err)
	}
}

// TestMarkWordRecheckQueuedOnlyFlipsDone covers the done-only guard, the prior
// state returned for the backup, and the flipped row's shape.
func TestMarkWordRecheckQueuedOnlyFlipsDone(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	done := seedWordCandidate(t, dbh, "done")
	busy := seedWordCandidate(t, dbh, "busy")
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, busy)
	mustExec(t, dbh, `UPDATE work_queue SET priority = 3, attempts = 2, last_error = 'x', miss_count = 4,
        next_attempt_at = '2026-01-03T00:00:00Z', word_timing_state = 'absent', word_timing_generation = 6 WHERE id = ?`, done)

	var reported []WordRecheckPrior
	prior, err := q.MarkWordRecheckQueued(ctx, []int64{done, busy, 12345}, func(p WordRecheckPrior) error {
		reported = append(reported, p)
		return nil
	})
	if err != nil {
		t.Fatalf("flip: %v", err)
	}
	if len(reported) != 1 || reported[0] != prior[0] {
		t.Fatalf("reported = %+v; want exactly the returned prior", reported)
	}
	if len(prior) != 1 || prior[0].ID != done || prior[0].Status != "done" || prior[0].Priority != 3 ||
		prior[0].NextAttemptAt != "2026-01-03T00:00:00Z" ||
		prior[0].Attempts != 2 || prior[0].LastError != "x" ||
		prior[0].WordTimingState == nil || *prior[0].WordTimingState != WordTimingAbsent ||
		prior[0].WordTimingGeneration == nil || *prior[0].WordTimingGeneration != 6 {
		t.Fatalf("prior = %+v; want only the done row's pre-flip state", prior)
	}
	var status, state, next, lastErr string
	var prio, attempts, miss int
	if err := dbh.QueryRow(`SELECT status, priority, attempts, miss_count, last_error, next_attempt_at, word_timing_state
        FROM work_queue WHERE id = ?`, done).Scan(&status, &prio, &attempts, &miss, &lastErr, &next, &state); err != nil {
		t.Fatalf("read flipped: %v", err)
	}
	if status != "deferred" || prio != PriorityMiss || attempts != 0 || miss != 4 || lastErr != "" ||
		next != formatTime(now) || state != WordTimingQueued {
		t.Fatalf("flipped = (%s,%d,%d,%d,%q,%s,%s)", status, prio, attempts, miss, lastErr, next, state)
	}
	if err := dbh.QueryRow(`SELECT status, COALESCE(word_timing_state, '') FROM work_queue WHERE id = ?`, busy).Scan(&status, &state); err != nil {
		t.Fatalf("read busy: %v", err)
	}
	if status != "processing" || state != "" {
		t.Fatalf("processing row changed to (%s, %q)", status, state)
	}
	mustExec(t, dbh, `DELETE FROM work_queue WHERE id = ?`, busy)
	item, err := q.Dequeue(ctx)
	if err != nil || item.ID != done || item.WordTimingState != WordTimingQueued {
		t.Fatalf("Dequeue = (%d, %q, %v); want (%d, queued)", item.ID, item.WordTimingState, err, done)
	}
}

// TestMarkWordRecheckQueuedRollsBackOnError proves the batch is one
// transaction: a failed UPDATE or a failed backup report on a later id undoes
// the earlier flip.
func TestMarkWordRecheckQueuedRollsBackOnError(t *testing.T) {
	for _, viaReport := range []bool{false, true} {
		t.Run(strconv.FormatBool(viaReport), func(t *testing.T) {
			ctx := context.Background()
			dbh := openQueueTestDB(t)
			q := NewDBQueue(dbh)
			first := seedWordCandidate(t, dbh, "first")
			second := seedWordCandidate(t, dbh, "second")
			var report func(WordRecheckPrior) error
			if viaReport {
				report = func(p WordRecheckPrior) error {
					if p.ID == second {
						return errors.New("backup full")
					}
					return nil
				}
			} else {
				mustExec(t, dbh, `CREATE TRIGGER fail_second BEFORE UPDATE OF word_timing_state ON work_queue
                    WHEN NEW.id = `+strconv.FormatInt(second, 10)+` BEGIN SELECT RAISE(ABORT, 'boom'); END`)
			}
			if _, err := q.MarkWordRecheckQueued(ctx, []int64{first, second}, report); err == nil {
				t.Fatal("flip succeeded; want an error")
			}
			var n int
			if err := dbh.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE status = 'done' AND word_timing_state IS NULL`).Scan(&n); err != nil || n != 2 {
				t.Fatalf("unflipped rows after rollback = (%d, %v); want 2", n, err)
			}
		})
	}
}

// TestWordRecheckQueuedHiddenFromDeferredSweeps: a flipped row is a settled
// synced row, not a provider miss, so no sweep over 'deferred' may classify,
// settle, cancel or re-judge it; only the worker's Dequeue claims it.
func TestWordRecheckQueuedHiddenFromDeferredSweeps(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	id := seedWordCandidate(t, dbh, "flip")
	mustExec(t, dbh, `INSERT INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
	mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (1, 1, '/m/x.flac', 'done')`)
	mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, 1)`, id)
	if _, err := q.MarkWordRecheckQueued(ctx, []int64{id}, nil); err != nil {
		t.Fatalf("flip: %v", err)
	}
	// Give it every column the instrumental sweeps key on, and no timing verdict.
	mustExec(t, dbh, `UPDATE work_queue SET detect_instrumental = 1, instrumental_result = 0, music_sum = 1,
        vocal_peak = 0.1, speech_mean = 0.1, vocal_class = 'Singing', timing_outcome = NULL WHERE id = ?`, id)
	lib := int64(1)
	checks := map[string]func() (int, error){
		"ListVocalGateRejections": func() (int, error) {
			r, err := q.ListVocalGateRejections(ctx, ListVocalGateRejectionsOptions{})
			return len(r), err
		},
		"ResetInstrumentalToUnclassified": func() (int, error) {
			ok, err := q.ResetInstrumentalToUnclassified(ctx, id)
			return map[bool]int{true: 1}[ok], err
		},
		"StampUnclassifiedMiss": func() (int, error) {
			mustExec(t, dbh, `UPDATE work_queue SET instrumental_result = NULL WHERE id = ?`, id)
			ok, err := q.StampUnclassifiedMiss(ctx, id, InstrumentalTelemetry{})
			return map[bool]int{true: 1}[ok], err
		},
		"ListUnclassified": func() (int, error) {
			r, err := q.ListUnclassified(ctx, ListUnclassifiedOptions{})
			return len(r), err
		},
		"CountUnclassified":  func() (int, error) { return q.CountUnclassified(ctx, nil, true) },
		"ListTimingBacklog":  func() (int, error) { r, err := q.ListTimingBacklog(ctx, TimingBacklogOptions{}); return len(r), err },
		"CountTimingBacklog": func() (int, error) { return q.CountTimingBacklog(ctx) },
		"SettleInstrumental": func() (int, error) {
			o, err := q.SettleInstrumental(ctx, id, InstrumentalTelemetry{}, OwnedByBackfill)
			return map[bool]int{true: 1}[o == Settled], err
		},
		"CountCancelByLibrary": func() (int, error) { d, u, err := q.CountCancelByLibrary(ctx, lib); return int(d + u), err },
		"Cleanup": func() (int, error) {
			n, err := q.Cleanup(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "flip"}})
			return int(n), err
		},
	}
	for _, name := range []string{"ListVocalGateRejections", "ResetInstrumentalToUnclassified", "StampUnclassifiedMiss",
		"ListUnclassified", "CountUnclassified", "ListTimingBacklog", "CountTimingBacklog", "SettleInstrumental",
		"CountCancelByLibrary", "Cleanup"} {
		if n, err := checks[name](); err != nil || n != 0 {
			t.Errorf("%s saw the flipped row: (%d, %v); want (0, nil)", name, n, err)
		}
	}
	if item, err := q.Dequeue(ctx); err != nil || item.ID != id {
		t.Fatalf("Dequeue = (%d, %v); want the flipped row %d", item.ID, err, id)
	}
}

// TestSetWordTimingStateRoundTrip covers the settle stamp, the #1007 reader's
// generation match, and the vocabulary guard.
func TestSetWordTimingStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	absent := seedWordCandidate(t, dbh, "absent")
	stale := seedWordCandidate(t, dbh, "stale")
	served := seedWordCandidate(t, dbh, "served")
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []struct {
		id    int64
		state string
		gen   int64
	}{{absent, WordTimingAbsent, 9}, {stale, WordTimingAbsent, 8}, {served, WordTimingServed, 9}} {
		if err := q.SetWordTimingState(ctx, s.id, s.state, s.gen, at); err != nil {
			t.Fatalf("settle %d: %v", s.id, err)
		}
	}
	var state, checked string
	var gen int
	if err := dbh.QueryRow(`SELECT word_timing_state, word_timing_generation, word_timing_checked_at FROM work_queue WHERE id = ?`,
		served).Scan(&state, &gen, &checked); err != nil {
		t.Fatalf("read: %v", err)
	}
	if state != WordTimingServed || gen != 9 || checked != formatTime(at) {
		t.Fatalf("served row = (%s, %d, %s)", state, gen, checked)
	}
	ids, err := q.ListWordTimingAbsent(ctx, 9, 0)
	if err != nil {
		t.Fatalf("absent list: %v", err)
	}
	if len(ids) != 1 || ids[0] != absent {
		t.Fatalf("absent ids = %v; want [%d]", ids, absent)
	}
	if err := q.SetWordTimingState(ctx, absent, WordTimingQueued, 9, at); err == nil {
		t.Fatal("SetWordTimingState accepted 'queued'; only served/absent are verdicts")
	}
}

// TestReEnqueueLeavesDoneRowDone characterizes why #982 is queue-driven: once a
// work_queue row is 'done', a re-scan's Enqueue (what --upgrade/ForceStatus
// ultimately produces, since ForceStatus only resets scan_results) keeps it
// 'done', so the worker never sees it again.
func TestReEnqueueLeavesDoneRowDone(t *testing.T) {
	ctx := context.Background()
	q := NewDBQueue(openQueueTestDB(t))
	item := completeSynced(t, q, "Song", "/music/song.flac")
	again, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Artist", TrackName: "Song"},
		Outdir:     "out",
		Filename:   "a.lrc",
		SourcePath: "/music/song.flac",
	}, PriorityWebhook)
	if err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if again.ID != item.ID || again.Status != "done" {
		t.Fatalf("re-enqueued row = (%d, %s); want (%d, done)", again.ID, again.Status, item.ID)
	}
	if _, err := q.Dequeue(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("Dequeue err = %v; want sql.ErrNoRows (done row not claimable)", err)
	}
}
