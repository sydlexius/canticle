package petitlyrics

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

// A revoked clientAppId does NOT produce a 401. The API keeps answering: HTTP
// 200, well-formed XML, zero songs. The client maps that to ErrNotFound, which
// is byte-identical to what a genuine miss looks like.
//
// So a total lane outage would present as "petitlyrics suddenly has none of my
// tracks", indefinitely, with the lane appearing healthy and returning honest
// misses. That is the failure #607 exists to make diagnosable, and it is why a
// 401 branch alone is not sufficient cover.
//
// The signal is CONSECUTIVE, not cumulative: a real dry spell is common on a
// fallback lane that only sees what the primary provider missed, so any single
// non-zero response has to clear the count.

// emptyResponse is a well-formed API envelope carrying no songs -- exactly what
// a revoked application id is reported to return.
const emptyResponse = `<?xml version="1.0" encoding="UTF-8"?><response><songs></songs></response>`

func serveEmpty() http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		_, _ = w.Write([]byte(emptyResponse))
	}
}

func TestZeroResults_BelowThresholdStaysNotFound(t *testing.T) {
	c, _ := newTestClient(t, serveEmpty())

	// One short of the threshold: still indistinguishable from bad luck, so the
	// client must not escalate.
	for i := 0; i < ZeroResultThreshold-1; i++ {
		_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("lookup %d: err = %v; want ErrNotFound below the threshold", i, err)
		}
		if errors.Is(err, ErrProviderUnavailable) {
			t.Fatalf("lookup %d escalated early; %d consecutive misses is a plausible "+
				"dry spell on a fallback lane and must not be reported as an outage", i, i+1)
		}
	}
}

func TestZeroResults_CrossingThresholdSurfacesSentinel(t *testing.T) {
	c, _ := newTestClient(t, serveEmpty())

	var err error
	for i := 0; i < ZeroResultThreshold; i++ {
		_, err = c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
	}
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("after %d consecutive zero-result lookups err = %v; want "+
			"ErrProviderUnavailable. Without it a dead clientAppId is "+
			"indistinguishable from a library the provider simply lacks.",
			ZeroResultThreshold, err)
	}
	// It must ALSO still satisfy ErrNotFound, so every existing caller that
	// treats a miss as benign keeps working unchanged.
	if !errors.Is(err, ErrNotFound) {
		t.Error("ErrProviderUnavailable must still satisfy errors.Is(_, ErrNotFound): " +
			"callers that bucket a miss as benign must not break on the escalation")
	}
}

func TestZeroResults_SuccessMidRunResets(t *testing.T) {
	// Serve empties until told otherwise, then one real hit, then empties again.
	// The counter must restart from zero after the hit.
	hit := false
	c, _ := newTestClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/xml; charset=utf-8")
		if hit {
			body, err := os.ReadFile(filepath.Join("testdata", "type1_unsynced.xml"))
			if err != nil {
				t.Errorf("read fixture: %v", err)
				return
			}
			_, _ = w.Write(body)
			return
		}
		_, _ = w.Write([]byte(emptyResponse))
	})

	for i := 0; i < ZeroResultThreshold-1; i++ {
		_, _ = c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
	}

	hit = true
	if _, err := c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"}); err != nil {
		t.Fatalf("the reset lookup should have succeeded: %v", err)
	}
	hit = false

	// One more empty. If the reset worked this is count 1, nowhere near the
	// threshold; if it did not, this is the crossing.
	_, err := c.FindLyrics(context.Background(), models.Track{TrackName: "t", ArtistName: "a"})
	if errors.Is(err, ErrProviderUnavailable) {
		t.Error("a successful lookup did not reset the consecutive-zero counter. " +
			"The signal is CONSECUTIVE: a provider that answered once is not down, " +
			"and a cumulative counter would eventually escalate on any long-lived " +
			"client no matter how healthy.")
	}
}
