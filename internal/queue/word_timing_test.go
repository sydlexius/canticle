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
		shared bool // also link the row to library 2
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
		{name: "shared row, other library filter", shared: true, opts: WordRecheckOptions{LibraryIDs: []int64{2}}},
		{name: "shared row, no filter", shared: true, want: true},
		{name: "shared row, both libraries", shared: true, opts: WordRecheckOptions{LibraryIDs: []int64{1, 2}}, want: true},
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
			if tc.shared {
				mustExec(t, dbh, `INSERT INTO libraries (id, path, name) VALUES (2, '/n', 'lib2')`)
				mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (2, 2, '/n/x.flac', 'done')`)
				mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, 2)`, id)
			}
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
	mustExec(t, dbh, `UPDATE work_queue SET priority = 3, attempts = 2, last_error = 'x', miss_count = 4, refused_waits = 2,
        next_attempt_at = '2026-01-03T00:00:00Z', word_timing_state = 'absent', word_timing_generation = 6 WHERE id = ?`, done)

	var reported []WordRecheckPrior
	prior, err := q.MarkWordRecheckQueued(ctx, []int64{done, busy, 12345}, WordRecheckOptions{}, func(p WordRecheckPrior) error {
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
	var prio, attempts, miss, waits int
	if err := dbh.QueryRow(`SELECT status, priority, attempts, miss_count, last_error, next_attempt_at, word_timing_state, refused_waits
        FROM work_queue WHERE id = ?`, done).Scan(&status, &prio, &attempts, &miss, &lastErr, &next, &state, &waits); err != nil {
		t.Fatalf("read flipped: %v", err)
	}
	if status != "deferred" || prio != PriorityMiss || attempts != 0 || miss != 4 || lastErr != "" || waits != 0 ||
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

// TestMarkWordRecheckQueuedRevalidatesPredicate: a listed candidate settled
// (served, or absent at the current generation) before the flip is skipped.
func TestMarkWordRecheckQueuedRevalidatesPredicate(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	opts := WordRecheckOptions{Generation: 7}
	served := seedWordCandidate(t, dbh, "served")
	absent := seedWordCandidate(t, dbh, "absent")
	ids, err := q.ListWordRecheckCandidates(ctx, opts)
	if err != nil || len(ids) != 2 {
		t.Fatalf("list = (%v, %v); want both rows", ids, err)
	}
	if errS, errA := q.SetWordTimingState(ctx, served, WordTimingServed, 7, time.Time{}),
		q.SetWordTimingState(ctx, absent, WordTimingAbsent, 7, time.Time{}); errS != nil || errA != nil {
		t.Fatalf("settle: %v, %v", errS, errA)
	}
	if prior, err := q.MarkWordRecheckQueued(ctx, ids, opts, nil); err != nil || len(prior) != 0 {
		t.Fatalf("flip = (%+v, %v); want nothing flipped (both rows settled since the list)", prior, err)
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
			if _, err := q.MarkWordRecheckQueued(ctx, []int64{first, second}, WordRecheckOptions{}, report); err == nil {
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
	if _, err := q.MarkWordRecheckQueued(ctx, []int64{id}, WordRecheckOptions{}, nil); err != nil {
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
	inflight := seedWordCandidate(t, dbh, "inflight")
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, inflight)
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	for _, s := range []struct {
		id    int64
		state string
		gen   int64
	}{{absent, WordTimingAbsent, 9}, {stale, WordTimingAbsent, 8}, {served, WordTimingServed, 9}, {inflight, WordTimingAbsent, 9}} {
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

// TestReEnqueueKeepsProcessingRecheckIntact: a colliding Enqueue must not move
// a recheck row the worker holds; its settle is guarded on 'queued'.
func TestReEnqueueKeepsProcessingRecheckIntact(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	item := completeSynced(t, q, "Song", "/music/song.flac")
	if _, err := q.MarkWordRecheckQueued(ctx, []int64{item.ID}, WordRecheckOptions{}, nil); err != nil {
		t.Fatalf("flip: %v", err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, item.ID)
	read := func() (st, state, src, outs, done string) {
		if err := dbh.QueryRow(`SELECT status, COALESCE(word_timing_state, ''), source_path, COALESCE(output_paths, ''), COALESCE(completed_at, '')
            FROM work_queue WHERE id = ?`, item.ID).Scan(&st, &state, &src, &outs, &done); err != nil {
			t.Fatalf("read: %v", err)
		}
		return
	}
	s0, w0, p0, o0, c0 := read()
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:       models.Track{ArtistName: "Artist", TrackName: "Song"},
		SourcePath:  "/music/moved.flac",
		OutputPaths: []models.OutputPath{{Outdir: "moved", Filename: "b.lrc"}},
	}, PriorityWebhook); err != nil {
		t.Fatalf("re-enqueue: %v", err)
	}
	if s, w, p, o, c := read(); s != s0 || w != WordTimingQueued || w != w0 || p != p0 || o != o0 || c != c0 {
		t.Fatalf("after collision (%s,%s,%s,%q,%q); want (%s,%s,%s,%q,%q)", s, w, p, o, c, s0, w0, p0, o0, c0)
	}
}

// scanInputs is what the scan enqueuer sends for key ("A","t"): it always
// carries the scan_result it just reserved.
func scanInputs(t *testing.T, dbh *sql.DB) models.Inputs {
	t.Helper()
	mustExec(t, dbh, `INSERT OR IGNORE INTO libraries (id, name, path) VALUES (1, 'l', '/m')`)
	var srID int64
	if err := dbh.QueryRow(`INSERT INTO scan_results (library_id, file_path, artist, title, artist_key, title_key, outdir, filename, status)
        VALUES (1, '/m/x.flac', 'A', 't', 'a', 't', '/m', 'x.lrc', 'processing') RETURNING id`).Scan(&srID); err != nil {
		t.Fatalf("seed scan_result: %v", err)
	}
	return models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "t"}, SourcePath: "/m/x.flac",
		Outdir: "/m", Filename: "x.lrc", ScanResultID: srID, FromScan: true}
}

// TestWebhookCollisionKeepsWordRecheckRow (#1039 review I1): a Lidarr webhook
// (no scan_result) colliding with a flipped row keeps recheck mode and its
// paths, exactly as the same webhook leaves a plain 'done' row untouched, so
// the ordinary path can never replace the settled .lrc with a .txt.
func TestWebhookCollisionKeepsWordRecheckRow(t *testing.T) {
	for _, flip := range []bool{false, true} {
		ctx := context.Background()
		dbh := openQueueTestDB(t)
		q := NewDBQueue(dbh)
		id := seedWordCandidate(t, dbh, "t")
		mustExec(t, dbh, `UPDATE work_queue SET output_paths = '[{"outdir":"/m","filename":"x.lrc"}]' WHERE id = ?`, id)
		if flip {
			if _, err := q.MarkWordRecheckQueued(ctx, []int64{id}, WordRecheckOptions{}, nil); err != nil {
				t.Fatalf("flip: %v", err)
			}
		}
		read := func() (row string) {
			if err := dbh.QueryRow(`SELECT status || '|' || COALESCE(word_timing_state, '') || '|' || output_paths || '|' ||
                COALESCE(completed_at, '') || '|' || COALESCE(outcome_type, '') FROM work_queue WHERE id = ?`, id).Scan(&row); err != nil {
				t.Fatalf("read: %v", err)
			}
			return row
		}
		before := read()
		if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "t"}, SourcePath: "/m/x.flac",
			OutputPaths: []models.OutputPath{{Outdir: "/other", Filename: "dup.lrc"}}}, PriorityWebhook); err != nil {
			t.Fatalf("webhook enqueue: %v", err)
		}
		if after := read(); after != before {
			t.Fatalf("flip=%v: webhook collision moved the row %q -> %q", flip, before, after)
		}
	}
}

// TestScanCollisionReopensWordRecheckRow (#1039 review I2/M2): a scan collision
// reopens a flipped row the way ReopenDoneRowTx does -- no stale settle columns
// (so it is not an instrumental-backfill candidate), scan priority, not
// PriorityMiss -- and it dequeues as an ordinary fetch of the scan's paths.
func TestScanCollisionReopensWordRecheckRow(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	q.SetRandomized(false)
	id := seedWordCandidate(t, dbh, "t")
	mustExec(t, dbh, `UPDATE work_queue SET provider_lane = 'musixmatch' WHERE id = ?`, id)
	if _, err := q.MarkWordRecheckQueued(ctx, []int64{id}, WordRecheckOptions{}, nil); err != nil {
		t.Fatalf("flip: %v", err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET refused_waits = 2, last_error = 'lane throttled' WHERE id = ?`, id)
	if _, err := q.Enqueue(ctx, scanInputs(t, dbh), PriorityScan); err != nil {
		t.Fatalf("scan enqueue: %v", err)
	}
	var row string
	if err := dbh.QueryRow(`SELECT status || '|' || priority || '|' || refused_waits || '|' || last_error || '|' ||
        COALESCE(outcome_type, '') || COALESCE(timing_outcome, '') || COALESCE(provider_lane, '') || COALESCE(completed_at, '')
        FROM work_queue WHERE id = ?`, id).Scan(&row); err != nil {
		t.Fatalf("read: %v", err)
	}
	if row != "pending|0|0||" {
		t.Fatalf("reopened row = %q; want pending|0|0|| (settle columns cleared)", row)
	}
	if got, err := q.ListUnclassified(ctx, ListUnclassifiedOptions{GlobalDetectDefault: true}); err != nil || len(got) != 0 {
		t.Fatalf("ListUnclassified = (%d, %v); want 0 -- a reopened row is no backfill candidate", len(got), err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil || item.ID != id || item.WordTimingState != "" || item.Inputs.Filename != "x.lrc" {
		t.Fatalf("Dequeue = (%d, %q, %q, %v); want (%d, \"\", x.lrc)", item.ID, item.WordTimingState, item.Inputs.Filename, err, id)
	}
}

// TestReopenPathsLeaveWordRecheckMode (#1039): every reopen path turns a
// 'queued' row into an ordinary fetch (the dequeued item no longer carries
// 'queued', which is the worker's only recheck switch), and leaves a served
// verdict and its generation alone. Each case starts from the status the path
// is guarded on.
func TestReopenPathsLeaveWordRecheckMode(t *testing.T) {
	cases := []struct {
		name  string
		setup string // extra SET clause putting the row in the path's precondition
		run   func(ctx context.Context, t *testing.T, q *DBQueue, dbh *sql.DB, id int64)
	}{
		{"Retry", `status = 'failed'`, func(ctx context.Context, t *testing.T, q *DBQueue, _ *sql.DB, id int64) {
			if _, err := q.Retry(ctx, id); err != nil {
				t.Fatalf("Retry: %v", err)
			}
		}},
		{"RecheckRetired", `status = 'unavailable', last_error = '` + missLimitReachedError + `'`, func(ctx context.Context, t *testing.T, q *DBQueue, _ *sql.DB, _ int64) {
			if n, err := q.RecheckRetired(ctx, nil); err != nil || n != 1 {
				t.Fatalf("RecheckRetired = (%d, %v); want 1", n, err)
			}
		}},
		{"ResetInstrumental", `status = 'done', instrumental_result = 1`, func(ctx context.Context, t *testing.T, q *DBQueue, _ *sql.DB, id int64) {
			if n, err := q.ResetInstrumental(ctx, id); err != nil || n != 1 {
				t.Fatalf("ResetInstrumental = (%d, %v); want 1", n, err)
			}
		}},
		{"UnsettleInstrumental", `status = 'done', instrumental_result = 1`, func(ctx context.Context, t *testing.T, q *DBQueue, _ *sql.DB, id int64) {
			if ok, err := q.UnsettleInstrumental(ctx, id); err != nil || !ok {
				t.Fatalf("UnsettleInstrumental = (%v, %v); want true", ok, err)
			}
		}},
		// A flipped row is 'deferred'; a served row is 'done'.
		{"ReopenDoneRowTx", `status = CASE WHEN word_timing_state = 'queued' THEN 'deferred' ELSE 'done' END`, func(ctx context.Context, t *testing.T, _ *DBQueue, dbh *sql.DB, id int64) {
			tx, err := dbh.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			ok, err := ReopenDoneRowTx(ctx, tx, id, time.Now().UTC())
			if err != nil || !ok {
				t.Fatalf("ReopenDoneRowTx = (%v, %v); want true", ok, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}},
		// Only a SCAN collision reopens (#1039 review I1); see
		// TestWebhookCollisionKeepsWordRecheckRow for the webhook side.
		{"Enqueue scan collision", `status = 'deferred'`, func(ctx context.Context, t *testing.T, q *DBQueue, dbh *sql.DB, id int64) {
			item, err := q.Enqueue(ctx, scanInputs(t, dbh), PriorityScan)
			if err != nil || item.ID != id {
				t.Fatalf("Enqueue = (%d, %v); want the existing row %d", item.ID, err, id)
			}
		}},
	}
	for _, tc := range cases {
		for _, state := range []string{WordTimingQueued, WordTimingServed} {
			t.Run(tc.name+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				dbh := openQueueTestDB(t)
				q := NewDBQueue(dbh)
				q.SetRandomized(false)
				id := seedWordCandidate(t, dbh, "t")
				mustExec(t, dbh, `UPDATE work_queue SET word_timing_state = ?, word_timing_generation = 3, next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, state, id)
				mustExec(t, dbh, `UPDATE work_queue SET `+tc.setup+` WHERE id = ?`, id)
				tc.run(ctx, t, q, dbh, id)
				want := state
				if state == WordTimingQueued {
					want = ""
				}
				var got sql.NullString
				var gen sql.NullInt64
				if err := dbh.QueryRow(`SELECT word_timing_state, word_timing_generation FROM work_queue WHERE id = ?`, id).Scan(&got, &gen); err != nil {
					t.Fatalf("read: %v", err)
				}
				if got.String != want || gen.Int64 != 3 {
					t.Fatalf("after reopen state=%q gen=%d; want %q gen 3", got.String, gen.Int64, want)
				}
				item, err := q.Dequeue(ctx)
				if err != nil || item.ID != id || item.WordTimingState != want {
					t.Fatalf("Dequeue = (%d, %q, %v); want (%d, %q)", item.ID, item.WordTimingState, err, id, want)
				}
			})
		}
	}
}

// TestReopenDoneRowTxWidenedGuardIsNarrow: the widened guard admits only a
// parked recheck row, never one the worker holds nor an ordinary deferred row.
func TestReopenDoneRowTxWidenedGuardIsNarrow(t *testing.T) {
	for _, set := range []string{
		`status = 'processing', word_timing_state = 'queued'`,
		`status = 'deferred', word_timing_state = NULL`,
		`status = 'deferred', word_timing_state = 'absent'`,
	} {
		t.Run(set, func(t *testing.T) {
			ctx := context.Background()
			dbh := openQueueTestDB(t)
			id := seedWordCandidate(t, dbh, "t")
			mustExec(t, dbh, `UPDATE work_queue SET `+set+` WHERE id = ?`, id)
			tx, err := dbh.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			if ok, err := ReopenDoneRowTx(ctx, tx, id, time.Now().UTC()); err != nil || ok {
				t.Fatalf("ReopenDoneRowTx = (%v, %v); want (false, nil)", ok, err)
			}
		})
	}
}

// TestWordRecheckSettleAndDefer covers the worker's two recheck transitions:
// served moves completed_at, absent keeps it, a defer stays 'queued' and due
// later, no counter moves, and each refuses a row that is not a processing
// recheck row (an ordinary processing row included).
func TestWordRecheckSettleAndDefer(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	now := time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC)
	q.now = func() time.Time { return now }
	claim := func(key string, queued bool) int64 {
		id := seedWordCandidate(t, dbh, key)
		state := any(nil)
		if queued {
			state = WordTimingQueued
		}
		mustExec(t, dbh, `UPDATE work_queue SET status = 'processing', miss_count = 3, attempts = 1, word_timing_state = ? WHERE id = ?`, state, id)
		return id
	}
	var checked, lastErr string
	read := func(id int64) (status, state, completed, next string, gen sql.NullInt64, miss, attempts int) {
		t.Helper()
		if err := dbh.QueryRow(`SELECT status, COALESCE(word_timing_state, ''), completed_at, next_attempt_at,
            word_timing_generation, miss_count, attempts, COALESCE(word_timing_checked_at, ''), last_error FROM work_queue WHERE id = ?`, id).Scan(
			&status, &state, &completed, &next, &gen, &miss, &attempts, &checked, &lastErr); err != nil {
			t.Fatalf("read %d: %v", id, err)
		}
		return
	}
	served, absent, deferred, ordinary := claim("s", true), claim("a", true), claim("d", true), claim("o", false)
	// Another path (identityrepair, purgeprovenance) re-pended the served row's
	// scan result while it was queued; the settle is what re-settles it.
	mustExec(t, dbh, `INSERT INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
	mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (1, 1, '/m/x.flac', 'pending')`)
	mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, 1)`, served)
	// A prior lane attempt per row: no transition may add, change, or drop one.
	laneRows := func() string {
		t.Helper()
		var s string
		if err := dbh.QueryRow(`SELECT COALESCE(group_concat(queue_id || ':' || lane || ':' || hit || ':' || attempted_at, ','), '')
            FROM (SELECT * FROM lane_attempts ORDER BY queue_id, lane)`).Scan(&s); err != nil {
			t.Fatalf("read lane_attempts: %v", err)
		}
		return s
	}
	for _, id := range []int64{served, absent, deferred} {
		mustExec(t, dbh, `INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at) VALUES (?, 'musixmatch', 0, '2026-01-10T00:00:00Z')`, id)
	}
	lane0 := laneRows()
	if err := q.SettleWordRecheck(ctx, served, WordTimingServed, 9); err != nil {
		t.Fatalf("settle served: %v", err)
	}
	if err := q.SettleWordRecheck(ctx, absent, WordTimingAbsent, 9); err != nil {
		t.Fatalf("settle absent: %v", err)
	}
	if released, err := q.DeferWordRecheck(ctx, deferred, time.Hour, 1, "throttled"); err != nil || released {
		t.Fatalf("defer = (%v, %v); want parked", released, err)
	}
	if got := laneRows(); got != lane0 {
		t.Fatalf("lane_attempts after settle/defer = %q; want unchanged %q", got, lane0)
	}
	if st, s, c, _, g, m, a := read(served); st != "done" || s != WordTimingServed || c != formatTime(now) || g.Int64 != 9 || m != 3 || a != 1 {
		t.Fatalf("served = (%s,%s,%s,%v,%d,%d)", st, s, c, g, m, a)
	}
	if checked != formatTime(now) {
		t.Fatalf("served word_timing_checked_at = %q; want %q", checked, formatTime(now))
	}
	var srStatus string
	if err := dbh.QueryRow(`SELECT status FROM scan_results WHERE id = 1`).Scan(&srStatus); err != nil || srStatus != "done" {
		t.Fatalf("linked scan_results status = (%q, %v); want done", srStatus, err)
	}
	if st, s, c, _, g, m, a := read(absent); st != "done" || s != WordTimingAbsent || c != "2026-01-10T00:00:00Z" || g.Int64 != 9 || m != 3 || a != 1 {
		t.Fatalf("absent = (%s,%s,%s,%v,%d,%d); completed_at must not move", st, s, c, g, m, a)
	}
	if checked != formatTime(now) {
		t.Fatalf("absent word_timing_checked_at = %q; want %q", checked, formatTime(now))
	}
	// RecheckDeferred must not cancel the defer's back-off, nor count the row.
	if n, err := q.CountRecheckDeferred(ctx, nil); err != nil || n != 0 {
		t.Fatalf("CountRecheckDeferred = (%d, %v); want (0, nil)", n, err)
	}
	if n, err := q.RecheckDeferred(ctx, nil); err != nil || n != 0 {
		t.Fatalf("RecheckDeferred = (%d, %v); want (0, nil)", n, err)
	}
	if st, s, _, n, g, m, a := read(deferred); st != "deferred" || s != WordTimingQueued || n != formatTime(now.Add(time.Hour)) || g.Valid || m != 3 || a != 1 {
		t.Fatalf("deferred = (%s,%s,%s,%v,%d,%d)", st, s, n, g, m, a)
	}
	if lastErr != "throttled" {
		t.Fatalf("deferred last_error = %q; want throttled", lastErr)
	}
	deferErr := func(id int64) error { _, err := q.DeferWordRecheck(ctx, id, time.Hour, 1, "x"); return err }
	for name, err := range map[string]error{
		"settle ordinary": q.SettleWordRecheck(ctx, ordinary, WordTimingAbsent, 9),
		"defer ordinary":  deferErr(ordinary),
		"settle settled":  q.SettleWordRecheck(ctx, served, WordTimingServed, 9),
		// Right state, wrong status: only the status half of the guard refuses.
		"settle deferred": q.SettleWordRecheck(ctx, deferred, WordTimingAbsent, 9),
		"defer deferred":  deferErr(deferred),
	} {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("%s = %v; want sql.ErrNoRows", name, err)
		}
	}
	if err := q.SettleWordRecheck(ctx, deferred, WordTimingQueued, 9); err == nil {
		t.Fatal("settle with state queued succeeded; want refusal")
	}
	// The wait budget (maxWaits=1) is spent: the next unanswered defer un-flips
	// the row to done with no verdict, completed_at and counters untouched.
	mustExec(t, dbh, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, deferred)
	if released, err := q.DeferWordRecheck(ctx, deferred, time.Hour, 1, "again"); err != nil || !released {
		t.Fatalf("defer past cap = (%v, %v); want released", released, err)
	}
	if st, s, c, _, g, m, a := read(deferred); st != "done" || s != "" || c != "2026-01-10T00:00:00Z" || g.Valid || m != 3 || a != 1 || lastErr != "" {
		t.Fatalf("released = (%s,%q,%s,%v,%d,%d,%q); want done, no verdict, completed_at kept", st, s, c, g, m, a, lastErr)
	}
}

// TestClearWordTimingState (#982 slice 4): a served/absent verdict is dropped
// with its generation and checked_at; 'queued' and an unexamined row are not
// touched.
func TestClearWordTimingState(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	served := seedWordCandidate(t, dbh, "served")
	queued := seedWordCandidate(t, dbh, "queued")
	if err := q.SetWordTimingState(ctx, served, WordTimingServed, 9, time.Time{}); err != nil {
		t.Fatal(err)
	}
	mustExec(t, dbh, `UPDATE work_queue SET word_timing_state = 'queued' WHERE id = ?`, queued)
	for _, id := range []int64{served, queued} {
		if err := q.ClearWordTimingState(ctx, id); err != nil {
			t.Fatalf("clear %d: %v", id, err)
		}
	}
	for id, want := range map[int64]string{served: "", queued: WordTimingQueued} {
		var state string
		var gen, checked sql.NullString
		if err := dbh.QueryRow(`SELECT COALESCE(word_timing_state, ''), word_timing_generation, word_timing_checked_at FROM work_queue WHERE id = ?`,
			id).Scan(&state, &gen, &checked); err != nil {
			t.Fatal(err)
		}
		if state != want || (want == "" && (gen.Valid || checked.Valid)) {
			t.Fatalf("row %d = (%q, %v, %v); want %q", id, state, gen, checked, want)
		}
	}
	_ = dbh.Close()
	if err := q.ClearWordTimingState(ctx, served); err == nil {
		t.Fatal("ClearWordTimingState on a closed db returned nil")
	}
}
