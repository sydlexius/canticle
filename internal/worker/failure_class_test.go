package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
)

// The class comes from what the worker knows about the cause (#1285): the write
// step's own class, the orchestrator's outcome class, then failsig's verdict.
func TestFailureClassMapping(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  queue.FailureClass
	}{
		{"no cause", nil, ""},
		{"a write failure, whatever its wording", fmt.Errorf("y: %w", scrubWritePaths(musixmatch.ErrNotFound)), queue.FailureWrite},
		{"provider miss", fmt.Errorf("lane a: %w", musixmatch.ErrNotFound), queue.FailureMiss},
		{"no word answer", fmt.Errorf("recheck: %w", errNoWordAnswer), queue.FailureMiss},
		{"rate limited", fmt.Errorf("lane a: %w", musixmatch.ErrRateLimited), queue.FailureThrottle},
		{"breaker open", orchestrator.ErrLaneUnavailable, queue.FailureThrottle},
		{"timing-refused, lane untried", &orchestrator.RefusedUntriedError{Lane: "a", Cause: "x"}, queue.FailureThrottle},
		{"refused with 403", fmt.Errorf("lane a: %w", musixmatch.ErrForbidden), queue.FailureThrottle},
		{"failing lane now open", &orchestrator.PartialFailureError{Err: errors.New("lane a: stale client")}, queue.FailureThrottle},
		{"detector outage", orchestrator.ErrLaneOutage, queue.FailureNetwork},
		{"transport", errors.New("lane a: transport error: dial tcp 192.0.2.1:443: i/o timeout"), queue.FailureNetwork},
		{"anything else", errors.New("worker: settle guard-rejected item 5: constraint failed"), queue.FailureOther},
	} {
		if got := failureClass(tc.cause); got != tc.want {
			t.Errorf("%s: failureClass = %q, want %q", tc.name, got, tc.want)
		}
	}
	if classed(nil) != nil {
		t.Error("classed(nil) is not nil")
	}
}

// The worker hands the queue a classed cause at its Fail and Defer writes.
func TestWorkerFailureWritesCarryTheClass(t *testing.T) {
	track := models.Track{ArtistName: "A", TrackName: "T"}
	item := queue.WorkItem{ID: 7, Inputs: models.Inputs{Track: track, Outdir: "out", Filename: "a.lrc"}}
	found := &fakeFetcher{song: models.Song{Track: track, Lyrics: models.Lyrics{LyricsBody: "ok"}}}
	for _, tc := range []struct {
		name    string
		fetcher *fakeFetcher
		writer  *fakeWriter
		want    queue.FailureClass
	}{
		// "timeout" in the message: the write step's class must win over failsig's.
		{"write failure", found, &fakeWriter{err: errors.New("i/o timeout")}, queue.FailureWrite},
		{"fetch failure", &fakeFetcher{err: errors.New("lane a: connection refused")}, &fakeWriter{}, queue.FailureNetwork},
		{"benign miss", &fakeFetcher{err: musixmatch.ErrNotFound}, &fakeWriter{}, queue.FailureMiss},
	} {
		q := &fakeQueue{items: []queue.WorkItem{item}}
		if err := New(q, &fakeCache{}, tc.fetcher, tc.writer).RunOnce(context.Background()); err != nil {
			t.Fatalf("%s: RunOnce: %v", tc.name, err)
		}
		causes := append(append([]error{}, q.failCauses...), q.deferCauses...)
		if len(causes) != 1 || queue.FailureClassOf(causes[0]) != tc.want {
			t.Errorf("%s: causes %v carry class %q, want one cause classed %q", tc.name, causes, queue.FailureClassOf(errors.Join(causes...)), tc.want)
		}
	}
}
