package worker

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/queue"
)

// TestOutageLatchedMissReleasesWithoutAMiss pins the ROW side of #1195 review
// F1. While the petitlyrics outage is latched, a zero-result answer must take
// the same path ErrProviderUnavailable took before the rearm: the row is
// released untouched, never deferred with a miss charged toward RetireMiss and
// never failed. Read as an ordinary miss, every lookup on a revoked credential
// would march a real track toward retirement.
func TestOutageLatchedMissReleasesWithoutAMiss(t *testing.T) {
	for name, sentinel := range map[string]error{
		"confirmed outage": petitlyrics.ErrProviderUnavailable,
		"latched outage":   petitlyrics.ErrOutageLatched,
	} {
		t.Run(name, func(t *testing.T) {
			q := &fakeQueue{items: []queue.WorkItem{{ID: 1, Inputs: models.Inputs{
				Track: models.Track{ArtistName: "Artist", TrackName: "Title"}, Outdir: "out", Filename: "a.lrc"}}}}
			w := New(q, &fakeCache{}, &fakeFetcher{err: fmt.Errorf("upstream: %w", sentinel)}, &fakeWriter{})

			if err := w.RunOnce(context.Background()); !errors.Is(err, errThrottled) {
				t.Fatalf("RunOnce = %v; want errThrottled (released, no verdict)", err)
			}
			if len(q.released) != 1 || len(q.deferred) != 0 || len(q.failed) != 0 || len(q.retired) != 0 {
				t.Errorf("released=%v deferred=%v failed=%v retired=%v; want only a release",
					q.released, q.deferred, q.failed, q.retired)
			}
		})
	}
}
