package worker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

const eventsDay = "2026-10-06"

var eventsT0 = time.Date(2026, 10, 6, 23, 30, 0, 0, time.UTC)

// newEventsRig is the #1207 cache-lane rig (real SQLite, real LRCWriter) with
// the daily counters wired to the real queue and the clock pinned (#1301).
func newEventsRig(t *testing.T, primary *fakeFetcher, word bool) (*cacheLaneRig, *Worker) {
	t.Helper()
	rig, w := newCacheLaneRig(t, primary)
	w.SetSourceEventRecorder(w.queue.(*queue.DBQueue))
	w.setClock(func() time.Time { return eventsT0 })
	if word {
		w.writer.(*lyrics.LRCWriter).SetWordSyncCompanion(true)
	}
	return rig, w
}

func eventsOf(t *testing.T, d *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := d.Query(`SELECT day, lane, event, count FROM source_event_daily`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string]int64{}
	for rows.Next() {
		var day, lane, event string
		var n int64
		if err := rows.Scan(&day, &lane, &event, &n); err != nil {
			t.Fatal(err)
		}
		got[day+"|"+lane+"|"+event] = n
	}
	return got
}

func assertEvents(t *testing.T, d *sql.DB, want map[string]int64) {
	t.Helper()
	got := eventsOf(t, d)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("source events = %v, want %v", got, want)
	}
}

// assertHitsMatchProviderOutcomes is the #1301 AC: per lane, the daily hit and
// miss counts equal provider_outcomes over the same completions.
func assertHitsMatchProviderOutcomes(t *testing.T, d *sql.DB) {
	t.Helper()
	// Read both sides fully before comparing: the pool may hold one connection.
	got := eventsOf(t, d)
	type outcome struct {
		lane         string
		hits, misses int64
	}
	var outcomes []outcome
	rows, err := d.Query(`SELECT lane, hits, misses FROM provider_outcomes`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var o outcome
		if err := rows.Scan(&o.lane, &o.hits, &o.misses); err != nil {
			t.Fatal(err)
		}
		outcomes = append(outcomes, o)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) == 0 {
		t.Fatal("no provider_outcomes rows: the comparison is vacuous")
	}
	for _, o := range outcomes {
		if gh, gm := got[eventsDay+"|"+o.lane+"|hit"], got[eventsDay+"|"+o.lane+"|miss"]; gh != o.hits || gm != o.misses {
			t.Errorf("lane %s: daily hit/miss = %d/%d, provider_outcomes = %d/%d", o.lane, gh, gm, o.hits, o.misses)
		}
	}
}

// TestSourceEvents_OrdinaryCompletion: a fresh fetch counts one hit and its
// delivered type for the winning lane, and matches provider_outcomes.
func TestSourceEvents_OrdinaryCompletion(t *testing.T) {
	tr := models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title"}
	instr := models.Track{ArtistName: "Synthetic Artist", TrackName: "Synthetic Title", Instrumental: 1}
	for _, c := range []struct {
		name, event string
		song        models.Song
		word        bool
	}{
		{"word", "word", recheckSong("word line", true, models.WordAnswerServed), true},
		{"line", "line", fallthroughSong(90, "line words"), false},
		{"unsynced", "unsynced", models.Song{Track: tr, Lyrics: models.Lyrics{LyricsBody: "plain words"}}, false},
		{"instrumental", "instrumental", models.Song{Track: instr}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r, w := newEventsRig(t, &fakeFetcher{song: c.song}, c.word)
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			assertEvents(t, r.db, map[string]int64{
				eventsDay + "|musixmatch|hit":        1,
				eventsDay + "|musixmatch|" + c.event: 1,
			})
			assertHitsMatchProviderOutcomes(t, r.db)
		})
	}
}

// TestSourceEvents_MissCountsEveryLane: a benign miss counts one miss per
// active lane, equal to provider_outcomes, and delivers nothing.
func TestSourceEvents_MissCountsEveryLane(t *testing.T) {
	r, w := newEventsRig(t, &fakeFetcher{err: musixmatch.ErrNotFound}, false)
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertEvents(t, r.db, map[string]int64{
		eventsDay + "|musixmatch|miss":  1,
		eventsDay + "|petitlyrics|miss": 1,
	})
	assertHitsMatchProviderOutcomes(t, r.db)
}

// TestSourceEvents_CacheHitCountsNothing: a cache-served completion adds no hit
// (provider_outcomes does not either) and no delivered type.
func TestSourceEvents_CacheHitCountsNothing(t *testing.T) {
	r, w := newEventsRig(t, &fakeFetcher{song: fallthroughSong(90, "line words")}, false)
	ctx := context.Background()
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	before := eventsOf(t, r.db)
	if err := os.Remove(r.lrc); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, r.id); err != nil {
		t.Fatal(err)
	}
	if err := w.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got := eventsOf(t, r.db); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Fatalf("events after a cache hit = %v, want unchanged %v", got, before)
	}
	assertHitsMatchProviderOutcomes(t, r.db)
}

// TestSourceEvents_UpgradeTrip: a trip that lands counts its delivered type; a
// kept write and a settle that lands nothing count a hit but no delivered type.
func TestSourceEvents_UpgradeTrip(t *testing.T) {
	hit := eventsDay + "|musixmatch|hit"
	instr := models.Song{Track: models.Track{ArtistName: "A", TrackName: "T", Instrumental: 1}}
	for _, c := range []struct {
		name  string
		song  models.Song
		force bool
		want  map[string]int64
	}{
		{"lands", fallthroughSong(90, "new synced lyric"), false, map[string]int64{hit: 1, eventsDay + "|musixmatch|line": 1}},
		{"kept write", instr, true, map[string]int64{hit: 1}},
		{"settle lands nothing", fallthroughSong(400, "another recording"), false, map[string]int64{hit: 1}},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newUpgradeRig(t, &fakeFetcher{song: c.song})
			r.w.SetSourceEventRecorder(r.q)
			r.w.setClock(func() time.Time { return eventsT0 })
			r.lw.SetForceOverwrite(c.force)
			r.run(t)
			assertEvents(t, r.db, c.want)
			if c.name == "lands" {
				assertHitsMatchProviderOutcomes(t, r.db)
			}
		})
	}
}

// TestSourceEvents_WordRecheckLands: a landed recheck counts the delivered tier
// for the serving lane and NO hit (provider_outcomes is untouched by a recheck).
func TestSourceEvents_WordRecheckLands(t *testing.T) {
	t.Run("lands", func(t *testing.T) {
		rig, w := newRecheckRig(t, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, nil, false)
		w.SetSourceEventRecorder(rig.q)
		w.setClock(func() time.Time { return eventsT0 })
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		assertEvents(t, rig.db, map[string]int64{eventsDay + "|musixmatch|word": 1})
		var n int
		if err := rig.db.QueryRow(`SELECT COUNT(*) FROM provider_outcomes`).Scan(&n); err != nil || n != 0 {
			t.Fatalf("provider_outcomes rows = %d, %v; a recheck must not touch it", n, err)
		}
	})
}

type failingSourceEvents struct{}

func (failingSourceEvents) RecordSourceEvent(context.Context, time.Time, string, string) error {
	return errors.New("injected counter failure")
}

// TestSourceEvents_WriteFailureLeavesCompletionIntact: a failing counter write
// is logged and never fails the completion.
func TestSourceEvents_WriteFailureLeavesCompletionIntact(t *testing.T) {
	r, w := newEventsRig(t, &fakeFetcher{song: fallthroughSong(90, "line words")}, false)
	w.SetSourceEventRecorder(failingSourceEvents{})
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	var status, outcome string
	if err := r.db.QueryRow(`SELECT status, outcome_type FROM work_queue WHERE id = ?`, r.id).Scan(&status, &outcome); err != nil {
		t.Fatal(err)
	}
	if status != "done" || outcome != "synced" {
		t.Fatalf("row = %s/%s, want done/synced", status, outcome)
	}
	assertEvents(t, r.db, map[string]int64{})
}
