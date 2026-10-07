package worker

import (
	"errors"
	"strings"

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
		switch {
		// A refusal, plain or inside a PartialFailureError (#1372). A stale
		// client version is not one in either shape.
		case orchestrator.IsRefusal(cause):
			return queue.FailureThrottle
		case localLock(cause): // failsig calls it transient, but it is not the network
			return queue.FailureOther
		case failsig.Classify(failsig.Normalize(cause.Error())) == failsig.Transient:
			return queue.FailureNetwork
		}
	}
	return queue.FailureOther
}

// localLock reports SQLite lock contention on the local database. It reads the
// text, a superset of db.IsSQLiteBusy: several worker paths render the cause
// with %v, which drops the driver's typed error.
func localLock(cause error) bool {
	s := strings.ToLower(cause.Error())
	return strings.Contains(s, "database is locked") || strings.Contains(s, "sqlite_busy")
}

// classed returns cause carrying its failureClass, for queue.Fail and Defer.
func classed(cause error) error {
	return queue.WithFailureClass(cause, failureClass(cause))
}
