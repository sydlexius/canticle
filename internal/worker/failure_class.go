package worker

import (
	"errors"

	"github.com/sydlexius/canticle/internal/failsig"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/queue"
)

// failureClass is the class the queue records beside a failure's text (#1285).
// It owns no rules of its own: a class the failing step attached wins (the write
// loop), then orchestrator.ClassifyOutcome decides, and its catch-all
// OutcomeTransport (every error no provider sentinel names, so also a store or
// settle failure) is split by failsig.Classify into network and other.
func failureClass(cause error) queue.FailureClass {
	if c := queue.FailureClassOf(cause); c != "" {
		return c
	}
	if errors.Is(cause, errNoWordAnswer) {
		return queue.FailureMiss // a healthy round trip with no word data
	}
	switch orchestrator.ClassifyOutcome(cause) {
	case orchestrator.OutcomeSuccess:
		return ""
	case orchestrator.OutcomeBenignMiss:
		return queue.FailureMiss
	case orchestrator.OutcomeAuthRateLimit, orchestrator.OutcomeUnavailable, orchestrator.OutcomeLaneNotReady,
		orchestrator.OutcomeRefusedUntried:
		return queue.FailureThrottle
	case orchestrator.OutcomeLaneOutage:
		return queue.FailureNetwork
	case orchestrator.OutcomeTransport:
		var partial *orchestrator.PartialFailureError
		switch {
		case orchestrator.IsRefusal(cause), errors.As(cause, &partial): // refused, or its lane is now open (#1372)
			return queue.FailureThrottle
		case failsig.Classify(failsig.Normalize(cause.Error())) == failsig.Transient:
			return queue.FailureNetwork
		}
	}
	return queue.FailureOther
}

// classed returns cause carrying its failureClass, for queue.Fail and Defer.
func classed(cause error) error {
	return queue.WithFailureClass(cause, failureClass(cause))
}
