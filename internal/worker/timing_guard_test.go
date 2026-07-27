package worker

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
)

// guardLine builds a text-bearing cue at a whole second.
func guardLine(sec int, text string) models.Lines {
	return models.Lines{
		Text: text,
		Time: models.Time{Total: float64(sec), Minutes: sec / 60, Seconds: sec % 60},
	}
}

// TestOutcomeTypeFromSong_ReflectsTimingGuardDecision: once the accept-time
// guard (#439) can override the content-type gate, outcome_type must record
// what LANDED, not what was planned. A row stamped "synced" whose .lrc was
// refused is exactly the enqueue-time-plan drift #379 removed.
func TestOutcomeTypeFromSong_ReflectsTimingGuardDecision(t *testing.T) {
	tests := []struct {
		name string
		song models.Song
		want string
	}{
		{
			name: "MisSynced result is recorded as the unsynced .txt it became",
			song: models.Song{
				Track:                models.Track{ArtistName: "A", TrackName: "T"},
				Subtitles:            models.Synced{Lines: []models.Lines{guardLine(10, "a"), guardLine(120, "b")}},
				AudioDurationSeconds: 100,
			},
			want: "unsynced",
		},
		{
			name: "quarantined result wrote nothing, so there is no outcome to classify",
			song: models.Song{
				Track:                models.Track{ArtistName: "A", TrackName: "T"},
				Subtitles:            models.Synced{Lines: []models.Lines{guardLine(400, "a")}},
				AudioDurationSeconds: 100,
			},
			want: "",
		},
		{
			name: "compliant synced result is still synced",
			song: models.Song{
				Track:                models.Track{ArtistName: "A", TrackName: "T"},
				Subtitles:            models.Synced{Lines: []models.Lines{guardLine(90, "a")}},
				AudioDurationSeconds: 100,
			},
			want: "synced",
		},
		{
			name: "trailing decorative marker must not demote the record either",
			song: models.Song{
				Track:                models.Track{ArtistName: "A", TrackName: "T"},
				Subtitles:            models.Synced{Lines: []models.Lines{guardLine(90, "a"), guardLine(400, "♪")}},
				AudioDurationSeconds: 100,
			},
			want: "synced",
		},
		{
			name: "unknown duration fails open and stays synced",
			song: models.Song{
				Track:     models.Track{ArtistName: "A", TrackName: "T"},
				Subtitles: models.Synced{Lines: []models.Lines{guardLine(400, "a")}},
			},
			want: "synced",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := outcomeTypeFromSong(tc.song); got != tc.want {
				t.Fatalf("outcomeTypeFromSong = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestRunOnce_StampsAudioDurationForTheGuard pins the wiring the guard depends
// on: the worker must hand the writer the AUDIO FILE's duration, not the
// provider's catalog length off song.Track. Those are the same number whenever
// the lyric was timed against the catalog value, which is precisely when the
// comparison is circular and the guard sees nothing.
func TestRunOnce_StampsAudioDurationForTheGuard(t *testing.T) {
	const fileDuration = 180
	// The provider payload claims a much longer recording; the file's tags say
	// 180s. A cue at 300s fits the provider's claim and grossly overruns the
	// file, so only a guard fed the FILE duration rejects it.
	song := models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T", TrackLength: 320},
		Subtitles: models.Synced{Lines: []models.Lines{
			guardLine(10, "a"), guardLine(300, "b"),
		}},
	}

	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	dw := &durationCapturingWriter{}
	w := New(q, &fakeCache{}, &fakeFetcher{song: song}, dw)
	w.SetRecordingEnrichmentDefault(true)
	w.SetMetadataReader((&fakeMetadataReader{
		meta: scanner.AudioMetadata{TrackLength: fileDuration},
	}).read)

	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if got := dw.seen; got != fileDuration {
		t.Fatalf("writer saw AudioDurationSeconds = %d; want the file duration %d "+
			"(a provider-length comparison is circular and never rejects)", got, fileDuration)
	}
	// And the recorded verdict must agree with what the guard enforced.
	rec, ok := q.timingOutcomes[1]
	if !ok {
		t.Fatal("no timing outcome stamped")
	}
	if rec.Outcome != "categorical" {
		t.Errorf("timing_outcome = %q; want categorical (300s cue vs 180s file)", rec.Outcome)
	}
}

// durationCapturingWriter records the AudioDurationSeconds the worker stamped.
type durationCapturingWriter struct {
	seen int
}

func (d *durationCapturingWriter) WriteLRC(song models.Song, _ string, _ string) error {
	d.seen = song.AudioDurationSeconds
	return nil
}

// TestRunOnce_StampedOutcomeUsesTheGuardFallbackDuration pins that the durable
// record is evaluated against the SAME duration the guard enforced on, including
// on the Track.TrackLength fallback path.
//
// The regression it guards: guardDurationSeconds falls back to the catalog
// length when the file duration is unknown, but the stamp site was passed the
// raw AudioDurationSeconds. A song with no file duration and a known catalog
// length was therefore quarantined by the guard while the row recorded
// unknown_duration -- the durable record contradicting the decision it exists to
// document. Re-deriving from "the same inputs" is only safe when both sides
// actually share them.
func TestRunOnce_StampedOutcomeUsesTheGuardFallbackDuration(t *testing.T) {
	// No metadata reader is set, so AudioDurationSeconds stays 0 and only the
	// catalog length is available. A 900s cue grossly overruns it.
	song := models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T", TrackLength: 200},
		Subtitles: models.Synced{Lines: []models.Lines{
			guardLine(10, "a"), guardLine(900, "b"),
		}},
	}

	q := &fakeQueue{items: []queue.WorkItem{queuedItem("/library/track.flac")}}
	w := New(q, &fakeCache{}, &fakeFetcher{song: song}, &durationCapturingWriter{})

	if err := w.RunOnce(t.Context()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	rec, ok := q.timingOutcomes[1]
	if !ok {
		t.Fatal("no timing outcome stamped")
	}
	if rec.Outcome == "unknown_duration" {
		t.Fatalf("timing_outcome = %q, but the guard judged this song against the "+
			"catalog-length fallback and refused it; the record must not claim "+
			"no judgment was possible", rec.Outcome)
	}
	if rec.Outcome != "categorical" {
		t.Errorf("timing_outcome = %q; want categorical (900s cue vs 200s fallback)", rec.Outcome)
	}
}
