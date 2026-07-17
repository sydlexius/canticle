package orchestrator

import (
	"errors"
	"fmt"
	"testing"

	"github.com/doxazo-net/canticle/internal/musixmatch"
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
