package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
)

func detectBoolPtr(b bool) *bool { return &b }

// TestEnqueuer_ResolvesDetectInstrumental verifies the enqueuer resolves the
// per-item detect decision with precedence CLI override > per-library setting >
// global default, and stamps it onto the enqueued Inputs.
func TestEnqueuer_ResolvesDetectInstrumental(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name          string
		override      *bool
		libSetting    *bool
		globalDefault bool
		want          bool
	}{
		{"cli beats lib and global", detectBoolPtr(false), detectBoolPtr(true), true, false},
		{"lib beats global", nil, detectBoolPtr(true), false, true},
		{"global default when lib unset", nil, nil, true, true},
		{"global default off", nil, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakePendingStore{results: []models.ScanResult{{
				ID:       2,
				FilePath: "/music/x.mp3",
				Track:    models.Track{ArtistName: "A", TrackName: "T"},
			}}}
			work := &fakeWorkQueue{}
			e := scan.Enqueuer{
				Results:             store,
				Cache:               fakeLyricsCache{},
				Queue:               work,
				Priority:            5,
				DetectOverride:      tc.override,
				GlobalDetectDefault: tc.globalDefault,
			}
			if _, _, err := e.EnqueuePending(ctx, models.Library{ID: 7, DetectInstrumental: tc.libSetting}); err != nil {
				t.Fatalf("EnqueuePending: %v", err)
			}
			if len(work.inputs) != 1 {
				t.Fatalf("enqueued inputs = %d; want 1", len(work.inputs))
			}
			got := work.inputs[0].DetectInstrumental
			if got == nil {
				t.Fatalf("DetectInstrumental = nil; want %v", tc.want)
			}
			if *got != tc.want {
				t.Errorf("DetectInstrumental = %v; want %v", *got, tc.want)
			}
		})
	}
}
