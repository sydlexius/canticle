package worker

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// recheckRig is a worker over REAL SQLite and a REAL writer on a temp library
// holding one settled synced row with its .lrc on disk. Fakes stand only at
// the provider-lane boundary: a Musixmatch-named primary and a petitlyrics
// fallback, both word-capable.
type recheckRig struct {
	fallthroughRig
	lrc      string
	original []byte
	mtime    time.Time
}

func recheckSong(text string, words bool, answer models.WordAnswer) models.Song {
	s := fallthroughSong(90, text)
	s.WordAnswer = answer
	if words {
		s.WordTimings = []models.WordTiming{{Line: 0, Text: text, StartMS: 10000, EndMS: 10500}}
	}
	return s
}

// newRecheckRig settles the row as an ordinary synced fetch would, then flips
// it with MarkWordRecheckQueued unless ordinary is set.
func newRecheckRig(t *testing.T, primary, secondary *fakeFetcher, ordinary bool) (*recheckRig, *Worker) {
	t.Helper()
	if secondary == nil {
		secondary = &fakeFetcher{err: petitlyrics.ErrNoMatch}
	}
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	lib := t.TempDir()
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:      models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"},
		Outdir:     lib,
		Filename:   "track.lrc",
		SourcePath: filepath.Join(lib, "track.flac"),
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	writer := lyrics.NewLRCWriter(lib)
	settled := fallthroughSong(90, "settled line")
	settled.AudioDurationSeconds = fallthroughFileSeconds
	if err := writer.WriteLRC(settled, "track.lrc", lib); err != nil {
		t.Fatalf("seed .lrc: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("dequeue: %v", err)
	}
	if err := q.SetOutcomeType(ctx, item.ID, "synced"); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !ordinary {
		if flipped, err := q.MarkWordRecheckQueued(ctx, []int64{item.ID}, queue.WordRecheckOptions{}, nil); err != nil || len(flipped) != 1 {
			t.Fatalf("flip = %d, %v; want 1 row", len(flipped), err)
		}
	}
	rig := &recheckRig{fallthroughRig: fallthroughRig{db: sqlDB, q: q, id: item.ID}, lrc: filepath.Join(lib, "track.lrc")}
	rig.mtime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(rig.lrc, rig.mtime, rig.mtime); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	if rig.original, err = os.ReadFile(rig.lrc); err != nil {
		t.Fatalf("read seed: %v", err)
	}
	rig.cache = cache.New(sqlDB)
	w := New(q, rig.cache, primary, writer)
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, secondary))
	w.SetProviderRecorder(q)
	w.SetMetadataReader((&fakeMetadataReader{meta: scanner.AudioMetadata{TrackLength: fallthroughFileSeconds}}).read)
	return rig, w
}

type recheckRow struct {
	status, state, lastError        string
	generation                      int64
	attempts, missCount, laneRows   int
	nextAttemptAfterNow, hasChecked bool
}

func (r *recheckRig) recheckRow(t *testing.T) recheckRow {
	t.Helper()
	var row recheckRow
	var state, checked, next string
	var gen *int64
	if err := r.db.QueryRowContext(context.Background(),
		`SELECT status, COALESCE(word_timing_state, ''), word_timing_generation, COALESCE(word_timing_checked_at, ''),
		        attempts, miss_count, last_error, next_attempt_at,
		        (SELECT COUNT(*) FROM lane_attempts WHERE queue_id = work_queue.id)
		   FROM work_queue WHERE id = ?`, r.id).Scan(&row.status, &state, &gen, &checked,
		&row.attempts, &row.missCount, &row.lastError, &next, &row.laneRows); err != nil {
		t.Fatalf("read row: %v", err)
	}
	row.state, row.hasChecked = state, checked != ""
	if gen != nil {
		row.generation = *gen
	}
	at, err := time.Parse(time.RFC3339Nano, next)
	row.nextAttemptAfterNow = err == nil && at.After(time.Now())
	return row
}

// assertUntouched is the no-downgrade invariant: the settled .lrc keeps its
// bytes AND its mtime, and no .txt appears beside it.
func (r *recheckRig) assertUntouched(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(r.lrc)
	if err != nil {
		t.Fatalf("settled .lrc gone: %v", err)
	}
	if !bytes.Equal(got, r.original) {
		t.Fatalf(".lrc rewritten:\n%s\nwant:\n%s", got, r.original)
	}
	if fi, err := os.Stat(r.lrc); err != nil || !fi.ModTime().Equal(r.mtime) {
		t.Fatalf(".lrc mtime changed: %v, %v", fi, err)
	}
	if _, err := os.Stat(r.lrc[:len(r.lrc)-len(".lrc")] + ".txt"); !os.IsNotExist(err) {
		t.Fatalf(".txt sidecar appeared: %v", err)
	}
}

func wantGeneration() int64 {
	return providers.WordGeneration([]string{providers.Musixmatch, providers.PetitLyrics})
}

func TestWordRecheck_ServedWritesWordResult(t *testing.T) {
	primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	secondary := &fakeFetcher{err: petitlyrics.ErrNoMatch}
	rig, w := newRecheckRig(t, primary, secondary, false)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := os.ReadFile(rig.lrc)
	if !bytes.Contains(got, []byte("word line")) {
		t.Fatalf(".lrc = %s; want the word result's cues", got)
	}
	if secondary.calls != 0 {
		t.Fatalf("petitlyrics calls = %d; a word result must end the dispatch", secondary.calls)
	}
	row := rig.recheckRow(t)
	if row.status != "done" || row.state != queue.WordTimingServed || row.generation != wantGeneration() || !row.hasChecked {
		t.Fatalf("row = %+v; want done/served under the current word generation", row)
	}
	if row.missCount != 0 || row.attempts != 0 || row.laneRows != 0 {
		t.Fatalf("row counters = %+v; want miss 0, attempts 0, no lane_attempts row", row)
	}
	if song, ok := rig.cached(t); !ok || len(song.WordTimings) == 0 {
		t.Fatalf("cache = %+v, %v; want the word result stored", song, ok)
	}
}

// TestWordRecheck_NoWordsNeverDowngrades is the settle table's absent rows and
// the downgrade trap: whatever non-word answer the lanes give, the settled
// .lrc is untouched and no miss counter or lane_attempts row moves.
func TestWordRecheck_NoWordsNeverDowngrades(t *testing.T) {
	unqualified := recheckSong("word line", false, models.WordAnswerServed)
	unqualified.WordTimings = []models.WordTiming{{Line: 0, Text: "zzz", StartMS: 10000, EndMS: 10500}}
	cases := []struct {
		name    string
		primary *fakeFetcher
		setup   func(*Worker)
	}{
		{"line-synced, no words", &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}, nil},
		// Words the writer's a2 check refuses are not words (review C1).
		{"words that do not reconstruct the cue", &fakeFetcher{song: unqualified}, nil},
		{"script guard rejects", &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)},
			func(w *Worker) { w.EnableGuard(&fakeGuard{enabled: true}) }},
		{"verifier rejects", &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, func(w *Worker) {
			w.EnableVerification(&fakeVerifier{results: []verificationResult{{accepted: false}}}, 1)
			w.verifyBelowConfidence = 2
		}},
		{"unsynced result", &fakeFetcher{song: models.Song{Track: models.Track{ArtistName: "Synthetic Artist"},
			Lyrics: models.Lyrics{LyricsBody: "plain words"}, WordAnswer: models.WordAnswerAbsent}}, nil},
		{"instrumental result", &fakeFetcher{song: models.Song{Track: models.Track{Instrumental: 1}, WordAnswer: models.WordAnswerAbsent}}, nil},
		{"no match on every lane", &fakeFetcher{err: musixmatch.ErrNotFound}, nil},
		{"words the guard demotes (held)", &fakeFetcher{song: func() models.Song {
			s := recheckSong("overrun", true, models.WordAnswerServed)
			s.Subtitles.Lines = append(s.Subtitles.Lines, guardLine(120, "overrun"))
			return s
		}()}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig, w := newRecheckRig(t, tc.primary, &fakeFetcher{err: petitlyrics.ErrNoMatch}, false)
			// An owned word companion from an earlier fetch must survive (C1).
			elrc := rig.lrc[:len(rig.lrc)-len(".lrc")] + ".elrc"
			companion := []byte("[by:canticle]\n[00:10.00]<00:10.00>settled <00:11.00>line\n")
			if err := os.WriteFile(elrc, companion, 0o644); err != nil {
				t.Fatal(err)
			}
			w.writer.(*lyrics.LRCWriter).SetWordSyncCompanion(true)
			if tc.setup != nil {
				tc.setup(w)
			}
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			rig.assertUntouched(t)
			if got, err := os.ReadFile(elrc); err != nil || !bytes.Equal(got, companion) {
				t.Fatalf("owned .elrc = %q, %v; want untouched", got, err)
			}
			row := rig.recheckRow(t)
			if row.status != "done" || row.state != queue.WordTimingAbsent || row.generation != wantGeneration() {
				t.Fatalf("row = %+v; want done/absent under the current word generation", row)
			}
			if row.missCount != 0 || row.attempts != 0 || row.laneRows != 0 {
				t.Fatalf("row counters = %+v; absent must move no miss/attempt counter and write no lane_attempts", row)
			}
		})
	}
}

// TestWordRecheck_UnansweredStaysQueued: a result that does not answer the
// word question (unknown), or a throttled lane, re-parks the row still
// 'queued' with a delay; never absent, never a counter.
func TestWordRecheck_UnansweredStaysQueued(t *testing.T) {
	held := recheckSong("overrun", true, models.WordAnswerServed)
	held.Subtitles.Lines = append(held.Subtitles.Lines, guardLine(120, "overrun"))
	words := recheckSong("word line", true, models.WordAnswerServed)
	cases := []struct {
		name               string
		primary, secondary *fakeFetcher
		verifyErr          bool
		wantErr            error
		wantFailures       int
	}{
		// A healthy round-trip is not a failure: it must clear, never feed, the
		// worker-wide backoff (review I2).
		{"unknown word answer", &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerUnknown)}, nil, false, nil, 0},
		{"throttled lane", &fakeFetcher{err: musixmatch.ErrRateLimited}, nil, false, errThrottled, 0},
		// Held words while another word lane did not answer (plan 2.4 row 4).
		{"held words, other lane throttled", &fakeFetcher{song: held}, &fakeFetcher{err: petitlyrics.ErrRateLimited}, false, nil, 0},
		{"verifier error", &fakeFetcher{song: words}, nil, true, nil, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.secondary == nil {
				tc.secondary = &fakeFetcher{err: petitlyrics.ErrNoMatch}
			}
			rig, w := newRecheckRig(t, tc.primary, tc.secondary, false)
			if tc.verifyErr {
				w.EnableVerification(&fakeVerifier{results: []verificationResult{{err: errors.New("sidecar down")}}}, 1)
				w.verifyBelowConfidence = 2
			}
			w.consecutiveFailures = 2
			if err := w.RunOnce(context.Background()); err != tc.wantErr {
				t.Fatalf("RunOnce = %v; want %v", err, tc.wantErr)
			}
			if w.consecutiveFailures != tc.wantFailures {
				t.Fatalf("consecutiveFailures = %d; want %d", w.consecutiveFailures, tc.wantFailures)
			}
			rig.assertUntouched(t)
			row := rig.recheckRow(t)
			if row.status != "deferred" || row.state != queue.WordTimingQueued || !row.nextAttemptAfterNow {
				t.Fatalf("row = %+v; want deferred, still queued, due later", row)
			}
			if row.missCount != 0 || row.attempts != 0 || row.laneRows != 0 {
				t.Fatalf("row counters = %+v; an unanswered recheck must move no counter", row)
			}
		})
	}
}

// TestWordRecheck_LaneServedKeepsProvenance: recheck never serves from the
// cache (an entry has no lane or fetch time, review I1), so the rewritten .lrc
// names its lane and fetched_at is stamped; and the row's ordinary per-track
// lane_attempts history is left exactly as it was (review I4, #282).
func TestWordRecheck_LaneServedKeepsProvenance(t *testing.T) {
	primary := &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}
	rig, w := newRecheckRig(t, primary, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, false)
	entry, err := encodeSong(recheckSong("cached line", true, models.WordAnswerUnknown))
	if err != nil {
		t.Fatal(err)
	}
	if err := rig.cache.Store(context.Background(), "Synthetic Artist", "Synthetic Title",
		normalize.DurationBucket(fallthroughFileSeconds), entry); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`INSERT INTO lane_attempts(queue_id, lane, hit, attempted_at) VALUES(?, 'musixmatch', 1, '2020-01-01T00:00:00Z')`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, _ := os.ReadFile(rig.lrc)
	if primary.calls != 1 || !bytes.Contains(got, []byte("[source:petitlyrics]")) || !bytes.Contains(got, []byte("[fetched:")) {
		t.Fatalf("lane calls %d, .lrc:\n%s\nwant the lane asked and [source:petitlyrics] + [fetched:]", primary.calls, got)
	}
	var fetched, lanes string
	if err := rig.db.QueryRow(`SELECT COALESCE(fetched_at, 'NULL'), (SELECT group_concat(lane || ':' || hit) FROM lane_attempts WHERE queue_id = work_queue.id)
           FROM work_queue WHERE id = ?`, rig.id).Scan(&fetched, &lanes); err != nil {
		t.Fatal(err)
	}
	if fetched == "NULL" || lanes != "musixmatch:1" {
		t.Fatalf("fetched_at %s, lane_attempts %q; want stamped, and only the prior musixmatch:1", fetched, lanes)
	}
}

// TestWordRecheck_LanesAndBreakers pins contracts 6/7: a non-word lane
// (innertube) is never dispatched, and a recheck throttle opens the SAME
// breaker the ordinary dispatch consults.
func TestWordRecheck_LanesAndBreakers(t *testing.T) {
	rig, w := newRecheckRig(t, &fakeFetcher{err: musixmatch.ErrRateLimited}, nil, false)
	it := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
	w.SetFallbackProviders(providers.New(providers.PetitLyrics, &fakeFetcher{err: petitlyrics.ErrNoMatch}), providers.New(providers.InnerTube, it))
	if err := w.RunOnce(context.Background()); err != errThrottled {
		t.Fatalf("RunOnce = %v; want errThrottled", err)
	}
	if it.calls != 0 || rig.recheckRow(t).state != queue.WordTimingQueued {
		t.Fatalf("innertube calls = %d, row %+v; want 0 and still queued", it.calls, rig.recheckRow(t))
	}
	if w.lane != w.lanes[0] || w.lanes[0].Breaker().Allow() != circuit.StateOpen {
		t.Fatal("recheck throttle did not open the ordinary Musixmatch breaker")
	}
}

// TestWordRecheck_WaitBudgetUnflips (review I3): once the wait budget is
// spent, an unanswered row returns to done with NO verdict (never absent) and
// completed_at untouched, so it stops rechecking and stays a candidate.
func TestWordRecheck_WaitBudgetUnflips(t *testing.T) {
	rig, w := newRecheckRig(t, &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerUnknown)}, nil, false)
	var before string
	if err := rig.db.QueryRow(`UPDATE work_queue SET refused_waits = ? WHERE id = ? RETURNING completed_at`, maxWordRecheckWaits, rig.id).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	rig.assertUntouched(t)
	var after string
	_ = rig.db.QueryRow(`SELECT completed_at FROM work_queue WHERE id = ?`, rig.id).Scan(&after)
	if row := rig.recheckRow(t); row.status != "done" || row.state != "" || row.generation != 0 || after != before {
		t.Fatalf("row = %+v, completed_at %s -> %s; want done, no verdict, completed_at kept", row, before, after)
	}
}

// TestWordRecheck_OrdinaryRowUnchanged characterizes the ordinary path, which
// recheck mode must not alter: a row that was never flipped still takes the
// ordinary write (an unsynced result replaces the .lrc, the pinned writer
// contract) and never gains a word verdict.
func TestWordRecheck_OrdinaryRowUnchanged(t *testing.T) {
	primary := &fakeFetcher{song: models.Song{Track: models.Track{ArtistName: "Synthetic Artist"},
		Lyrics: models.Lyrics{LyricsBody: "plain words"}, WordAnswer: models.WordAnswerAbsent}}
	rig, w := newRecheckRig(t, primary, &fakeFetcher{err: petitlyrics.ErrNoMatch}, true)
	// An ordinary done row is never dequeued; requeue it as a scan would.
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if _, err := os.Stat(rig.lrc); !os.IsNotExist(err) {
		t.Fatalf("ordinary unsynced write left the .lrc: %v", err)
	}
	if row := rig.recheckRow(t); row.status != "done" || row.state != "" || row.laneRows != 1 {
		t.Fatalf("row = %+v; want an ordinary done completion with no word verdict", row)
	}
}
