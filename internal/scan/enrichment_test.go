package scan_test

import (
	"context"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
)

// optsCapturingScanner records the ScanOptions passed to each ScanLibrary call so
// tests can assert the resolved per-library EnrichRecording value.
type optsCapturingScanner struct {
	results []models.ScanResult
	opts    []scanner.ScanOptions
}

func (c *optsCapturingScanner) ScanLibrary(_ context.Context, _ string, opts scanner.ScanOptions) ([]models.ScanResult, error) {
	c.opts = append(c.opts, opts)
	return c.results, nil
}

func boolPtr(b bool) *bool { return &b }

// TestScheduler_ResolvesEnrichRecording verifies the scheduler resolves the
// per-library enrichment toggle with precedence CLI override > per-library
// setting > global default, and stamps it onto the ScanOptions for the scan.
func TestScheduler_ResolvesEnrichRecording(t *testing.T) {
	cases := []struct {
		name          string
		override      *bool
		libSetting    *bool
		globalDefault bool
		want          bool
	}{
		{"cli override beats lib and global", boolPtr(false), boolPtr(true), true, false},
		{"lib setting beats global", nil, boolPtr(false), true, false},
		{"global default when lib unset", nil, nil, true, true},
		{"global default off", nil, nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &optsCapturingScanner{results: []models.ScanResult{{
				FilePath: "/music/a.mp3",
				Track:    models.Track{TrackName: "Title"},
			}}}
			s := scan.Scheduler{
				Libraries: fakeLibraries{libs: []models.Library{
					{ID: 7, Path: "/music", Name: "Music", EnrichRecording: tc.libSetting},
				}},
				Results:             &fakeResults{},
				Scanner:             cap,
				EnrichOverride:      tc.override,
				GlobalEnrichDefault: tc.globalDefault,
			}
			if err := s.RunOnce(context.Background()); err != nil {
				t.Fatalf("RunOnce: %v", err)
			}
			if len(cap.opts) != 1 {
				t.Fatalf("ScanLibrary calls = %d; want 1", len(cap.opts))
			}
			if got := cap.opts[0].EnrichRecording; got != tc.want {
				t.Errorf("resolved EnrichRecording = %v; want %v", got, tc.want)
			}
		})
	}
}
