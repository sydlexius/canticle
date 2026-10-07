package queue

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// TestEnqueueNeverStampsAnUnevaluatedVerdict (#825): a settled categorical row
// stamped 0 (first seen by a CLI scan) keeps that stamp through a later
// enqueue under a real generation, scan or webhook. The enqueue re-evaluates
// nothing, so stamping the current generation would make the #679 suppression
// and the worker's cache bypass trust a verdict no current lane produced.
func TestEnqueueNeverStampsAnUnevaluatedVerdict(t *testing.T) {
	for _, tc := range []struct {
		name     string
		priority int
		fromScan bool
	}{
		{"scan enqueue", PriorityScan, true},
		{"webhook enqueue", PriorityWebhook, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q := NewDBQueue(openQueueTestDB(t))
			inputs := models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}, Outdir: "out", Filename: "a.lrc"}
			if _, err := q.Enqueue(ctx, inputs, PriorityScan); err != nil { // CLI scan: generation 0
				t.Fatalf("Enqueue: %v", err)
			}
			claimed, err := q.Dequeue(ctx)
			if err != nil {
				t.Fatalf("Dequeue: %v", err)
			}
			if err := q.SetTimingOutcome(ctx, claimed.ID, TimingRecord{Outcome: "categorical", Source: TimingSourceFetch}); err != nil {
				t.Fatalf("SetTimingOutcome: %v", err)
			}
			if err := q.Complete(ctx, claimed.ID); err != nil {
				t.Fatalf("Complete: %v", err)
			}

			q.SetProvidersVersion(5)
			inputs.FromScan = tc.fromScan
			item, err := q.Enqueue(ctx, inputs, tc.priority)
			if err != nil {
				t.Fatalf("re-Enqueue: %v", err)
			}
			outcome, version, _, found, err := q.LookupTiming(ctx, "Artist", "Song")
			if err != nil || !found {
				t.Fatalf("LookupTiming found=%v err=%v", found, err)
			}
			if item.Status != StatusDone || outcome != "categorical" || version != 0 {
				t.Fatalf("row = (%s, %s, generation %d); want (done, categorical, generation 0): an enqueue must not stamp a verdict it did not reach",
					item.Status, outcome, version)
			}
		})
	}
}

// TestTimingOutcomeWritesGenerationOnlyWithAVerdict (#825, #1386): the verdict's
// generation lands in the same UPDATE as the verdict, a nil Generation keeps the
// stored stamp (the sweep's and revalidate's case), and no verdict writes no
// generation. Both writers, so a caller of the guarded one is not silently
// ignored.
func TestTimingOutcomeWritesGenerationOnlyWithAVerdict(t *testing.T) {
	gen := func(v int) *int { return &v }
	for _, tc := range []struct {
		name    string
		idle    bool
		rec     TimingRecord
		want    int
		outcome string
	}{
		{"verdict with generation", false, TimingRecord{Outcome: "categorical", Generation: gen(9)}, 9, "categorical"},
		{"verdict with unknown generation", false, TimingRecord{Outcome: "ok", Generation: gen(0)}, 0, "ok"},
		{"verdict without generation keeps the stamp", false, TimingRecord{Outcome: "categorical"}, 7, "categorical"},
		{"no verdict writes no generation", false, TimingRecord{Generation: gen(9)}, 7, ""},
		{"guarded writer, verdict with generation", true, TimingRecord{Outcome: "mis_synced", Generation: gen(9)}, 9, "mis_synced"},
		{"guarded writer, verdict without generation", true, TimingRecord{Outcome: "mis_synced"}, 7, "mis_synced"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			q := NewDBQueue(openQueueTestDB(t))
			q.SetProvidersVersion(7)
			item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Song"}}, PriorityScan)
			if err != nil {
				t.Fatalf("Enqueue: %v", err)
			}
			if tc.idle {
				if _, err := q.SetTimingOutcomeIfIdle(ctx, item.ID, tc.rec); err != nil {
					t.Fatalf("SetTimingOutcomeIfIdle: %v", err)
				}
			} else if err := q.SetTimingOutcome(ctx, item.ID, tc.rec); err != nil {
				t.Fatalf("SetTimingOutcome: %v", err)
			}
			var version int
			var outcome string
			if err := q.db.QueryRowContext(ctx, `SELECT providers_version, COALESCE(timing_outcome, '') FROM work_queue WHERE id = ?`,
				item.ID).Scan(&version, &outcome); err != nil {
				t.Fatalf("read row: %v", err)
			}
			if version != tc.want || outcome != tc.outcome {
				t.Fatalf("row = (generation %d, verdict %q); want (generation %d, verdict %q)", version, outcome, tc.want, tc.outcome)
			}
		})
	}
}
