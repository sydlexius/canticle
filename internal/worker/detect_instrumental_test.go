package worker

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

func boolPtr(b bool) *bool { return &b }

func detectItem(id int64, detect *bool) queue.WorkItem {
	return queue.WorkItem{
		ID: id,
		Inputs: models.Inputs{
			Track:      models.Track{ArtistName: "Composer", TrackName: "Interlude"},
			Outdir:     "out",
			Filename:   "interlude.lrc",
			SourcePath: "/music/interlude.flac",
		},
		DetectInstrumental: detect,
	}
}

// TestRunOnce_DetectItemFlagOffSkipsDetection verifies a per-item decision of
// "off" suppresses detection even when the global default is on.
func TestRunOnce_DetectItemFlagOffSkipsDetection(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(300, boolPtr(false))}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNoLyrics}, &fakeWriter{})
	w.EnableAudioDetector(det)
	w.SetInstrumentalDetectionDefault(true) // global on, but item says off

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 0 {
		t.Errorf("detector calls = %v; want none (item opted out)", det.calls)
	}
	if len(q.deferred) != 1 {
		t.Errorf("deferred = %v; want the item deferred as a normal miss", q.deferred)
	}
}

// TestRunOnce_DetectItemFlagOnOverridesDefaultOff verifies a per-item decision of
// "on" runs detection even when the global default is off.
func TestRunOnce_DetectItemFlagOnOverridesDefaultOff(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(301, boolPtr(true))}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{})
	w.EnableAudioDetector(det)
	// global default left false on purpose

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 1 {
		t.Errorf("detector calls = %v; want 1 (item opted in)", det.calls)
	}
	if len(q.completed) != 1 || q.completed[0] != 301 {
		t.Errorf("completed = %v; want [301] (instrumental marker)", q.completed)
	}
}

// TestRunOnce_DetectNilFallsBackToDefaultOff verifies a NULL (nil) per-item
// decision falls back to the global default (here off), preserving the behavior
// of pre-existing rows.
func TestRunOnce_DetectNilFallsBackToDefaultOff(t *testing.T) {
	q := &fakeQueue{items: []queue.WorkItem{detectItem(302, nil)}}
	det := &fakeDetector{instrumental: true}
	w := New(q, &fakeCache{}, &fakeFetcher{err: musixmatch.ErrNoLyrics}, &fakeWriter{})
	w.EnableAudioDetector(det)
	// global default false: nil item must not detect

	if err := w.RunOnce(context.Background()); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if len(det.calls) != 0 {
		t.Errorf("detector calls = %v; want none (nil falls back to default off)", det.calls)
	}
}

// TestDetectInstrumental_WantedButNoClassifierLoudSkips verifies that when an
// item requests detection but no classifier is configured, the worker logs an
// error (loud-skip, no silent no-op) and returns (false, nil).
func TestDetectInstrumental_WantedButNoClassifierLoudSkips(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	w := New(&fakeQueue{}, &fakeCache{}, &fakeFetcher{}, &fakeWriter{})
	// no EnableAudioDetector: classifier unconfigured
	res, ran, err := w.detectInstrumental(context.Background(), detectItem(303, boolPtr(true)))
	if err != nil {
		t.Fatalf("detectInstrumental err = %v; want nil (non-fatal loud-skip)", err)
	}
	if res.Instrumental {
		t.Error("instrumental = true; want false when no classifier configured")
	}
	if ran {
		t.Error("ran = true; want false when no classifier configured (loud-skip path)")
	}
	logged := buf.String()
	if !strings.Contains(logged, "level=ERROR") || !strings.Contains(logged, "no classifier") {
		t.Errorf("expected an ERROR loud-skip log mentioning the missing classifier; got: %q", logged)
	}
}
