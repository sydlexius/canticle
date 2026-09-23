package worker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
)

// newStampRig is the slice-3b rig over a row an ORDINARY fetch will process:
// real SQLite, the real LRCWriter on a temp library, fakes only at the lanes.
// mode is output.word_sync_mode as commands.wordSyncSwitches maps it; prior
// seeds a verdict an earlier completion left on the row.
func newStampRig(t *testing.T, primary, secondary *fakeFetcher, mode, prior string) (*recheckRig, *Worker) {
	t.Helper()
	rig, w := newRecheckRig(t, primary, secondary, true)
	lw := w.writer.(*lyrics.LRCWriter)
	lw.SetWordSync(mode == "inline" || mode == "both")
	lw.SetWordSyncCompanion(mode == "sidecar" || mode == "both")
	if prior != "" {
		if err := rig.q.SetWordTimingState(context.Background(), rig.id, prior, 1, time.Time{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rig.db.Exec(`UPDATE work_queue SET status = 'pending' WHERE id = ?`, rig.id); err != nil {
		t.Fatal(err)
	}
	return rig, w
}

func (r *recheckRig) elrc() string { return strings.TrimSuffix(r.lrc, ".lrc") + ".elrc" }

// TestOrdinaryStamp_Verdicts pins the slice-4 contracts: served only when the
// writer's own predicate held AND the words landed on disk; absent only on the
// aggregate; nothing for a non-synced outcome, a non-word lane, or mode off,
// and nothing (clearing a prior verdict) whenever the answer is not decided.
func TestOrdinaryStamp_Verdicts(t *testing.T) {
	unqualified := recheckSong("word line", false, models.WordAnswerServed)
	unqualified.WordTimings = []models.WordTiming{{Line: 0, Text: "zzz", StartMS: 10000, EndMS: 10500}}
	demotable := recheckSong("overrun", true, models.WordAnswerServed)
	demotable.Subtitles.Lines = append(demotable.Subtitles.Lines, guardLine(120, "overrun"))
	noMatch := &fakeFetcher{err: petitlyrics.ErrNoMatch}
	cases := []struct {
		name, mode, prior  string
		primary, secondary *fakeFetcher
		soleLane, foreign  bool
		mirrorFirst        bool // a second output path, listed FIRST, holding the foreign .elrc
		innertube          *fakeFetcher
		want               string
	}{
		{name: "served, companion landed", mode: "sidecar", primary: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, want: queue.WordTimingServed},
		{name: "served, inline markers", mode: "inline", primary: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, want: queue.WordTimingServed},
		// planCompanion never writes over a foreign file: the words did not land.
		{name: "foreign companion blocks landing", mode: "sidecar", foreign: true, primary: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}},
		// served needs the words at EVERY output path, not just the last one.
		{name: "foreign companion at the first of two paths", mode: "sidecar", foreign: true, mirrorFirst: true, primary: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}},
		// The lane said served, but the writer's a2 check refuses the words: not
		// served, and not absent either (the lane has words), so a recheck decides.
		{name: "words fail HasQualifyingWords", mode: "sidecar", prior: queue.WordTimingServed, primary: &fakeFetcher{song: unqualified}},
		{name: "inline, words fail HasQualifyingWords", mode: "inline", soleLane: true, primary: &fakeFetcher{song: unqualified}},
		{name: "absent, the only word lane", mode: "sidecar", soleLane: true, primary: &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}, want: queue.WordTimingAbsent},
		{name: "absent, every word lane answered", mode: "sidecar", primary: &fakeFetcher{err: musixmatch.ErrNotFound},
			secondary: &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}, want: queue.WordTimingAbsent},
		// petitlyrics was never asked, so the lane's own absent is not the aggregate.
		{name: "absent, another word lane unasked", mode: "sidecar", primary: &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}},
		{name: "unknown", mode: "sidecar", soleLane: true, primary: &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerUnknown)}},
		{name: "innertube winner", mode: "sidecar", prior: queue.WordTimingAbsent, primary: &fakeFetcher{err: musixmatch.ErrNotFound},
			innertube: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}},
		{name: "word_sync_mode off", mode: "off", soleLane: true, primary: &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}},
		{name: "off, absent answer", mode: "off", soleLane: true, primary: &fakeFetcher{song: recheckSong("line only", false, models.WordAnswerAbsent)}},
		{name: "txt outcome clears a stale absent", mode: "sidecar", prior: queue.WordTimingAbsent, soleLane: true, primary: &fakeFetcher{song: models.Song{
			Track: models.Track{ArtistName: "Synthetic Artist"}, Lyrics: models.Lyrics{LyricsBody: "plain words"}, WordAnswer: models.WordAnswerAbsent}}},
		{name: "instrumental outcome", mode: "sidecar", prior: queue.WordTimingServed, soleLane: true, primary: &fakeFetcher{song: models.Song{
			Track: models.Track{Instrumental: 1}, WordAnswer: models.WordAnswerAbsent}}},
		{name: "mis_synced demotion", mode: "sidecar", soleLane: true, primary: &fakeFetcher{song: demotable}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.secondary == nil {
				tc.secondary = noMatch
			}
			rig, w := newStampRig(t, tc.primary, tc.secondary, tc.mode, tc.prior)
			switch {
			case tc.soleLane:
				w.SetFallbackProviders()
			case tc.innertube != nil:
				w.SetFallbackProviders(providers.New(providers.InnerTube, tc.innertube))
			}
			foreign := []byte("[00:10.00]<00:10.00>someone else's\n")
			foreignAt := rig.elrc()
			if tc.mirrorFirst {
				lib := filepath.Dir(rig.lrc)
				mirror := filepath.Join(lib, "mirror")
				if err := os.Mkdir(mirror, 0o755); err != nil {
					t.Fatal(err)
				}
				foreignAt = filepath.Join(mirror, "track.elrc")
				paths := fmt.Sprintf(`[{"outdir":%q,"filename":"track.lrc"},{"outdir":%q,"filename":"track.lrc"}]`, mirror, lib)
				if _, err := rig.db.Exec(`UPDATE work_queue SET output_paths = ? WHERE id = ?`, paths, rig.id); err != nil {
					t.Fatal(err)
				}
			}
			if tc.foreign {
				if err := os.WriteFile(foreignAt, foreign, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			row := rig.recheckRow(t)
			if row.status != "done" || row.state != tc.want {
				t.Fatalf("row = %+v; want done with word_timing_state %q", row, tc.want)
			}
			if tc.want != "" && (row.generation != w.wordGeneration() || !row.hasChecked) {
				t.Fatalf("row = %+v; want generation %d and checked_at stamped", row, w.wordGeneration())
			}
			if tc.want == "" && (row.generation != 0 || row.hasChecked) {
				t.Fatalf("row = %+v; an unstamped row must carry no generation or checked_at", row)
			}
			if tc.foreign {
				if got, _ := os.ReadFile(foreignAt); string(got) != string(foreign) {
					t.Fatalf("foreign .elrc = %q; want untouched", got)
				}
			}
		})
	}
}

// TestOrdinaryStamp_LandedMeansOnDisk cross-checks served against the disk:
// the companion carrying word markers exists exactly when the row says served.
func TestOrdinaryStamp_LandedMeansOnDisk(t *testing.T) {
	rig, w := newStampRig(t, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, nil, "sidecar", "")
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	got, err := os.ReadFile(rig.elrc())
	if err != nil || !strings.Contains(string(got), "<00:10.00>") {
		t.Fatalf(".elrc = %q, %v; want the word markers on disk", got, err)
	}
	if rig.recheckRow(t).state != queue.WordTimingServed {
		t.Fatal("companion landed but the row is not served")
	}
}

// failingWordQueue fails only the word stamp, over the real DBQueue.
type failingWordQueue struct{ *queue.DBQueue }

func (failingWordQueue) SetWordTimingState(context.Context, int64, string, int64, time.Time) error {
	return errors.New("injected word stamp failure")
}

// TestOrdinaryStamp_FailureIsNonFatal: a lost stamp leaves the row NULL (a
// recheck candidate) and never costs the written result.
func TestOrdinaryStamp_FailureIsNonFatal(t *testing.T) {
	rig, w := newStampRig(t, &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}, nil, "sidecar", "")
	w.queue = failingWordQueue{rig.q}
	w.consecutiveFailures = 2
	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	row := rig.recheckRow(t)
	if row.status != "done" || row.state != "" || row.attempts != 0 || w.consecutiveFailures != 0 {
		t.Fatalf("row = %+v, failures %d; want an ordinary done completion, no verdict", row, w.consecutiveFailures)
	}
	if got, _ := os.ReadFile(rig.lrc); !strings.Contains(string(got), "word line") {
		t.Fatalf(".lrc = %s; want the result written", got)
	}
}

// TestWordRecheck_Metrics pins the section-8 decision: a recheck records no
// provider_outcomes hit (an absent one has no symmetric miss, and lane_attempts
// already excludes rechecks), and provider_lane moves only after the write, so
// it can never name a lane whose [source:] is not on disk.
func TestWordRecheck_Metrics(t *testing.T) {
	for _, writeFails := range []bool{false, true} {
		primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
		rig, w := newRecheckRig(t, primary, nil, false)
		lib := filepath.Dir(rig.lrc)
		if writeFails {
			if err := os.Chmod(lib, 0o555); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(lib, 0o755) })
		}
		if err := w.RunOnce(context.Background()); err != nil {
			t.Fatalf("RunOnce (write fails %v): %v", writeFails, err)
		}
		var hits int
		var lane string
		if err := rig.db.QueryRow(`SELECT (SELECT COUNT(*) FROM provider_outcomes), COALESCE(provider_lane, '')
		   FROM work_queue WHERE id = ?`, rig.id).Scan(&hits, &lane); err != nil {
			t.Fatal(err)
		}
		wantLane := providers.Musixmatch
		if writeFails {
			wantLane = ""
		}
		if hits != 0 || lane != wantLane {
			t.Fatalf("write fails %v: provider_outcomes rows %d, provider_lane %q; want 0 and %q", writeFails, hits, lane, wantLane)
		}
	}
}

// TestOrdinaryStamp_EarlySettlesClearAStaleVerdict: the detector-instrumental
// and guard-reject settles return before stampWordTiming, so each must drop a
// prior served/absent itself; neither leaves a verdict about an earlier file.
func TestOrdinaryStamp_EarlySettlesClearAStaleVerdict(t *testing.T) {
	for _, tc := range []struct {
		name, prior, outcome string
		setup                func(*Worker)
	}{
		{"detector instrumental", queue.WordTimingServed, "instrumental", func(w *Worker) {
			w.SetFallbackProviders()
			w.EnableAudioDetector(&fakeDetector{instrumental: true, version: "9.9.9"})
			w.SetInstrumentalDetectionDefault(true)
		}},
		{"guard reject", queue.WordTimingAbsent, outcomeTypeRejected, func(w *Worker) { w.EnableGuard(rejectAllGuard{reason: "script"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			primary := &fakeFetcher{song: recheckSong("word line", true, models.WordAnswerServed)}
			if tc.outcome == "instrumental" {
				primary = &fakeFetcher{err: musixmatch.ErrNotFound}
			}
			rig, w := newStampRig(t, primary, nil, "sidecar", tc.prior)
			tc.setup(w)
			if err := w.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			var outcome string
			if err := rig.db.QueryRow(`SELECT COALESCE(outcome_type, '') FROM work_queue WHERE id = ?`, rig.id).Scan(&outcome); err != nil {
				t.Fatal(err)
			}
			if row := rig.recheckRow(t); row.status != "done" || outcome != tc.outcome || row.state != "" || row.generation != 0 || row.hasChecked {
				t.Fatalf("row = %+v, outcome %q; want done %s with the prior %q cleared", row, outcome, tc.outcome, tc.prior)
			}
		})
	}
}
