package worker

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"testing"

	"github.com/sydlexius/canticle/internal/lyricblock"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
)

// blockBody blocks song's body for the QUERY identity (artist, title).
func blockBody(t *testing.T, d *sql.DB, artist, title string, song models.Song) *lyricblock.Store {
	t.Helper()
	s := lyricblock.NewStore(d, nil)
	if _, err := s.Add(context.Background(), d, lyricblock.Block{ArtistKey: artist, TitleKey: title, Fingerprint: lyricblock.SongFingerprints(song)[0]}); err != nil {
		t.Fatal(err)
	}
	return s
}

func rowStatus(t *testing.T, d *sql.DB, id int64) (status string) {
	t.Helper()
	if err := d.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func assertNotWritten(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s exists (%v); a blocked result must write nothing", path, err)
	}
}

// Ordinary fetch: every answer is blocked; the row is deferred (no hot loop, no
// failure backoff) and nothing is written.
func TestBlocked_OrdinaryFetch_AllBlockedDefers(t *testing.T) {
	song := fallthroughSong(90, "wrong words")
	rig, w := newCacheLaneRig(t, &fakeFetcher{song: song})
	w.SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", song))
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	assertNotWritten(t, rig.lrc)
	if status := rowStatus(t, rig.db, rig.id); status != queue.StatusDeferred {
		t.Fatalf("status = %q; want %q (an all-blocked row is deferred, not settled, in this slice)", status, queue.StatusDeferred)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; a blocked result is not a provider failure", w.consecutiveFailures)
	}
	assertNoLaneMiss(t, rig.db, rig.id)
}

// assertNoLaneMiss: a blocked answer is not a provider miss, so neither
// provider_outcomes nor lane_attempts may carry one (#1394).
func assertNoLaneMiss(t *testing.T, d *sql.DB, id int64) {
	t.Helper()
	var misses, rows int
	if err := d.QueryRow(`SELECT COALESCE(SUM(misses), 0) FROM provider_outcomes`).Scan(&misses); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT COUNT(*) FROM lane_attempts WHERE queue_id = ?`, id).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if misses != 0 || rows != 0 {
		t.Fatalf("provider_outcomes misses = %d, lane_attempts rows = %d; a blocked answer must charge neither", misses, rows)
	}
}

// A blocked lane in a SUCCESSFUL dispatch is not attributed as a miss either.
func TestBlocked_FallThrough_BlockedLaneIsNotAMiss(t *testing.T) {
	blocked := fallthroughSong(90, "wrong words")
	rig, w := newFallthroughRig(t, &fakeFetcher{song: blocked}, &fakeFetcher{song: fallthroughSong(90, "right words")})
	w.SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", blocked))
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if status, lane, _ := rig.row(t); status != queue.StatusDone || lane != providers.PetitLyrics {
		t.Fatalf("row = (%q, %q); want done under the lane with the different body", status, lane)
	}
	if got := rig.attempts(t); len(got) != 1 || !got[providers.PetitLyrics] {
		t.Fatalf("lane_attempts = %v; want only the winning lane (the blocked lane is not a miss)", got)
	}
	var misses int
	if err := rig.db.QueryRow(`SELECT COALESCE(SUM(misses), 0) FROM provider_outcomes`).Scan(&misses); err != nil || misses != 0 {
		t.Fatalf("provider_outcomes misses = %d (%v); want 0", misses, err)
	}
}

// One lane's answer is blocked and the other lane did not answer (auth failure,
// breaker open): just THIS row is parked through the bounded wait, the drain pass
// continues, and no lane miss is charged. With the budget spent the row takes the
// all-blocked deferral and nothing is written.
func TestBlocked_UntriedSiblingParksTheRowOnly(t *testing.T) {
	ctx := context.Background()
	blocked := fallthroughSong(90, "wrong words")
	rig, w := newFallthroughRig(t, byTitleFetcher{
		"Synthetic Title": blocked,
		"Other Title":     fallthroughSong(90, "right words"),
	}, &fakeFetcher{err: musixmatch.ErrUnauthorized})
	w.SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", blocked))
	row2, err := rig.q.Enqueue(ctx, models.Inputs{
		Track:  models.Track{ArtistName: "Other Artist", TrackName: "Other Title"},
		Outdir: "/out", Filename: "other.lrc", SourcePath: "/library/other.flac",
	}, queue.PriorityScan)
	if err != nil {
		t.Fatal(err)
	}
	w.consecutiveFailures = 2
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("pass 1 = %v; want nil (the drain pass must not idle on a blocked row)", err)
	}
	var waits, attempts, misses int
	read := func() {
		if err := rig.db.QueryRow(`SELECT refused_waits, attempts, miss_count FROM work_queue WHERE id = ?`, rig.id).Scan(&waits, &attempts, &misses); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if status, _, _ := rig.row(t); status != queue.StatusDeferred || waits != 1 || attempts != 0 || misses != 0 {
		t.Fatalf("row 1 = (%q, waits %d, attempts %d, miss_count %d); want parked: (deferred, 1, 0, 0)", status, waits, attempts, misses)
	}
	if w.consecutiveFailures != 0 {
		t.Fatalf("consecutiveFailures = %d; want 0", w.consecutiveFailures)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatalf("pass 2: %v", err)
	}
	if s2 := rowStatus(t, rig.db, row2.ID); s2 != queue.StatusDone {
		t.Fatalf("row 2 status = %q; want done (the blocked row must not starve it)", s2)
	}
	for i := 2; i <= maxRefusedWaits+1; i++ {
		if _, err := rig.db.Exec(`UPDATE work_queue SET next_attempt_at = '2000-01-01T00:00:00Z' WHERE id = ?`, rig.id); err != nil {
			t.Fatal(err)
		}
		if err := w.RunOnce(ctx); err != nil {
			t.Fatalf("pass %d: %v", i+1, err)
		}
	}
	if status := rowStatus(t, rig.db, rig.id); status != queue.StatusDeferred {
		t.Fatalf("row 1 after the budget = %q; want the all-blocked deferral, never done", status)
	}
	for _, s := range rig.writer.songs {
		if s.Subtitles.Lines[0].Text == "wrong words" {
			t.Fatalf("the blocked body was written: %+v", s)
		}
	}
	assertNoLaneMiss(t, rig.db, rig.id)
}

// Word recheck, block hit at DISPATCH level (checker on the worker only): a
// blocked word result is not a word answer, so the recheck is deferred (still
// queued), never settled absent; this also fails if wordOrchestrator loses its
// checker (#1394).
func TestBlocked_WordRecheck_DispatchBlockDefers(t *testing.T) {
	song := recheckSong("word line", true, models.WordAnswerServed)
	rig, w := newRecheckRig(t, &fakeFetcher{song: song}, nil, false)
	w.SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", song))
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.assertUntouched(t)
	if row := rig.recheckRow(t); row.state != "queued" {
		t.Fatalf("word_timing_state = %q; want queued (a blocked result must not settle the recheck absent)", row.state)
	}
}

// One identity everywhere: for a track whose album artist differs from its track
// artist, the row's stored keys, the worker's dispatch and the scan enqueuer all
// ask the block store under the same (artist_key, title_key) (#1394).
func TestBlocked_RowIdentityIsTheSameForWorkerScanAndRow(t *testing.T) {
	ctx := context.Background()
	track := models.Track{ArtistName: "Track Artist feat. Guest", AlbumArtist: "Album Artist", TrackName: "Song"}
	blocked := fallthroughSong(90, "wrong words")
	fetch := &fakeFetcher{song: blocked}
	rig, w := newCacheLaneRigFor(t, fetch, track)

	var artistKey, titleKey string
	if err := rig.db.QueryRow(`SELECT artist_key, title_key FROM work_queue WHERE id = ?`, rig.id).Scan(&artistKey, &titleKey); err != nil {
		t.Fatal(err)
	}
	if wa, wt := queue.IdentityKeys(track); artistKey != wa || titleKey != wt || artistKey == normalize.NormalizeKey("Album Artist") {
		t.Fatalf("row keys = (%q, %q); want queue.IdentityKeys(%v) = (%q, %q), not the album artist", artistKey, titleKey, track, wa, wt)
	}
	store := blockBody(t, rig.db, artistKey, titleKey, blocked) // keyed by the ROW's stored keys

	// Worker: the dispatch asks under the row identity, so the body is refused.
	w.SetBlockChecker(store)
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	assertNotWritten(t, rig.lrc)

	// Scan enqueuer: the same scanned track, a cached copy of the blocked body.
	if err := rig.cache.Store(ctx, track.ArtistName, track.TrackName, 0, mustJSON(t, blocked)); err != nil {
		t.Fatal(err)
	}
	enq := &countingQueue{}
	e := scan.Enqueuer{Results: &oneScanResult{track: track}, Cache: rig.cache, Queue: enq, Priority: 5, Blocks: store}
	if n, hits, err := e.EnqueuePending(ctx, models.Library{ID: 1}); err != nil || n != 1 || hits != 0 {
		t.Fatalf("EnqueuePending = (%d, %d, %v); want the blocked cache entry to read as a miss (1, 0)", n, hits, err)
	}
	if enq.n != 1 {
		t.Fatalf("enqueued %d rows", enq.n)
	}
}

type countingQueue struct{ n int }

func (c *countingQueue) Enqueue(context.Context, models.Inputs, int) (queue.WorkItem, error) {
	c.n++
	return queue.WorkItem{}, nil
}

type oneScanResult struct{ track models.Track }

func (o *oneScanResult) ListPendingByLibrary(context.Context, int64) ([]models.ScanResult, error) {
	return []models.ScanResult{{ID: 1, FilePath: "/music/a.flac", Track: o.track}}, nil
}
func (o *oneScanResult) SetStatus(context.Context, []int64, string) error { return nil }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Cache hit: a blocked cache entry reads as a miss, so the lanes are asked and
// a different body is accepted.
func TestBlocked_CacheEntryReadsAsMiss(t *testing.T) {
	first := fallthroughSong(90, "first words")
	primary := &fakeFetcher{song: first}
	rig, w := newCacheLaneRig(t, primary)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", first))
	if err := os.Remove(rig.lrc); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	primary.song = fallthroughSong(90, "other words")
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if primary.calls != 2 {
		t.Fatalf("provider calls = %d; want 2 (the blocked cache entry must read as a miss)", primary.calls)
	}
	if b, err := os.ReadFile(rig.lrc); err != nil || !bytes.Contains(b, []byte("other words")) || bytes.Contains(b, []byte("first words")) {
		t.Fatalf("sidecar = %q, %v; want the different body", b, err)
	}
}

// Cache hit, writer backstop alone: the cache serves it, the writer refuses it.
func TestBlocked_CacheHit_WriterBackstop(t *testing.T) {
	song := fallthroughSong(90, "first words")
	rig, w := newCacheLaneRig(t, &fakeFetcher{song: song})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	w.writer.(*lyrics.LRCWriter).SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", song))
	if err := os.Remove(rig.lrc); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertNotWritten(t, rig.lrc)
	if status := rowStatus(t, rig.db, rig.id); status == "done" {
		t.Fatalf("status = %q after a refused write", status)
	}
}

// Upgrade trip: the backstop refuses the blocked re-fetch; file and row record
// are untouched. The identity is the ROW's raw track artist, not the resolved album artist.
func TestBlocked_UpgradeTrip_KeepsFileRecord(t *testing.T) {
	song := fallthroughSong(90, "wrong words")
	r := newUpgradeRig(t, &fakeFetcher{song: song})
	r.lw.SetBlockChecker(blockBody(t, r.db, "Track Artist feat. X", "Song", song)) // the ROW identity, not the album artist
	r.run(t)
	r.kept(t)
}

// Word recheck: the backstop refuses a blocked word result; the .lrc is untouched.
func TestBlocked_WordRecheck_KeepsSettledLRC(t *testing.T) {
	song := recheckSong("word line", true, models.WordAnswerServed)
	rig, w := newRecheckRig(t, &fakeFetcher{song: song}, nil, false)
	w.writer.(*lyrics.LRCWriter).SetBlockChecker(blockBody(t, rig.db, "Synthetic Artist", "Synthetic Title", song))
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	rig.assertUntouched(t)
	if row := rig.recheckRow(t); row.state != "queued" {
		t.Fatalf("word_timing_state = %q; want the row still queued after a refused write", row.state)
	}
}
