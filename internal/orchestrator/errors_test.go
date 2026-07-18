package orchestrator

import (
	"errors"
	"fmt"
	"testing"

	"github.com/sydlexius/canticle/internal/musixmatch"
)

func TestClassifyOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want OutcomeClass
	}{
		{"nil is success", nil, OutcomeSuccess},
		{"lane unavailable", ErrLaneUnavailable, OutcomeUnavailable},
		{"token renewal is auth", fmt.Errorf("x: %w", musixmatch.ErrTokenRenewalRequired), OutcomeAuthRateLimit},
		{"unauthorized is auth", fmt.Errorf("x: %w", musixmatch.ErrUnauthorized), OutcomeAuthRateLimit},
		{"rate limited is auth", fmt.Errorf("x: %w", musixmatch.ErrRateLimited), OutcomeAuthRateLimit},
		{"not found is benign miss", fmt.Errorf("x: %w", musixmatch.ErrNotFound), OutcomeBenignMiss},
		{"no lyrics is benign miss", fmt.Errorf("x: %w", musixmatch.ErrNoLyrics), OutcomeBenignMiss},
		{"truncated is benign miss", fmt.Errorf("x: %w", musixmatch.ErrTruncatedResponse), OutcomeBenignMiss},
		{"generic error is transport", errors.New("connection refused"), OutcomeTransport},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyOutcome(tt.err); got != tt.want {
				t.Fatalf("ClassifyOutcome(%v) = %v; want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestOutcomePrecedence(t *testing.T) {
	// Higher precedence wins. auth/rate-limit > transport > benign-miss.
	// Unavailable is treated at rate-limit level for queue purposes.
	if OutcomeAuthRateLimit.precedence() <= OutcomeTransport.precedence() {
		t.Fatal("auth/rate-limit must outrank transport")
	}
	if OutcomeTransport.precedence() <= OutcomeBenignMiss.precedence() {
		t.Fatal("transport must outrank benign miss")
	}
	if OutcomeUnavailable.precedence() < OutcomeAuthRateLimit.precedence() {
		t.Fatal("all-unavailable must rank at least at rate-limit for queue purposes")
	}
	if OutcomeSuccess.precedence() != 0 {
		t.Fatal("success carries no error precedence")
	}
}

func TestErrLaneUnavailableIsSentinel(t *testing.T) {
	if !errors.Is(fmt.Errorf("wrapped: %w", ErrLaneUnavailable), ErrLaneUnavailable) {
		t.Fatal("ErrLaneUnavailable must be matchable through wrapping")
	}
}

func TestClassifyOutcome_LaneSentinels(t *testing.T) {
	if got := ClassifyOutcome(ErrLaneBenignMiss); got != OutcomeBenignMiss {
		t.Errorf("benign-miss sentinel = %v, want OutcomeBenignMiss", got)
	}
	if got := ClassifyOutcome(ErrLaneOutage); got != OutcomeLaneOutage {
		t.Errorf("outage sentinel = %v, want OutcomeLaneOutage", got)
	}
}

// TestOutcomeLaneOutage_RanksBelowTransport pins the precedence relationship
// that keeps a non-provider lane's outage from masking a provider's own
// failure. A lane outage must outrank a clean benign miss (we genuinely learned
// nothing from that lane) but must stay strictly below OutcomeTransport, so
// that when a detector outage and a provider transport error are both in play
// the PROVIDER error is the one dispatchResult surfaces - and the worker then
// fails-with-backoff instead of downgrading to a benign miss.
func TestOutcomeLaneOutage_RanksBelowTransport(t *testing.T) {
	if OutcomeLaneOutage.precedence() <= OutcomeBenignMiss.precedence() {
		t.Errorf("lane outage (%d) must outrank a benign miss (%d)",
			OutcomeLaneOutage.precedence(), OutcomeBenignMiss.precedence())
	}
	if OutcomeLaneOutage.precedence() >= OutcomeTransport.precedence() {
		t.Errorf("lane outage (%d) must rank BELOW transport (%d), else a detector outage can mask a provider failure",
			OutcomeLaneOutage.precedence(), OutcomeTransport.precedence())
	}
}

// TestRankErr_ProviderTransportBeatsDetectorOutage is the regression test for
// the masking bug directly: a front-ordered detector reports its outage FIRST,
// then a provider reports a transport error. rankErr keeps the first reporter
// at equal precedence, so under the old shared-OutcomeTransport classification
// the outage won and the worker swallowed the provider failure. The provider
// error must win.
func TestRankErr_ProviderTransportBeatsDetectorOutage(t *testing.T) {
	transport := errors.New("provider: connection reset")
	var r dispatchResult
	r.rankErr(ErrLaneOutage, ClassifyOutcome(ErrLaneOutage))
	r.rankErr(transport, ClassifyOutcome(transport))

	if !errors.Is(r.topErr, transport) {
		t.Fatalf("provider transport error must outrank a detector outage, got %v", r.topErr)
	}
	if errors.Is(r.topErr, ErrLaneOutage) {
		t.Fatal("a detector outage must not be the surfaced error when a provider also failed")
	}
}
