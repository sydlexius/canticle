package orchestrator

// laneOutage carries the ErrLaneOutage sentinel WITHOUT rendering its text.
//
// THE PROBLEM IT SOLVES (#790). The lane previously returned
//
//	fmt.Errorf("detector request failed: %w", errors.Join(ErrLaneOutage, cause))
//
// which renders every layer: the "detector request failed: " prefix, then the
// sentinel's own "orchestrator: lane outage", and only then the cause. That is
// roughly 80 characters of wrapper before anything actionable appears, and this
// string is persisted verbatim to work_queue.last_error and rendered into the
// Failure Analysis report. Each layer named a SUBSYSTEM; none named the PROBLEM.
//
// It was also actively misleading, which is the sharper half. A row whose audio
// file had merely MOVED presented as "detector request failed" -- naming the
// detector sidecar for what is a path problem, and sending an operator to read
// the wrong logs.
//
// WHY NOT JUST CHANGE ErrLaneOutage's TEXT. The sentinel is matched by
// errors.Is in detectorClassifier and ClassifyOutcome, never by its text, so its
// wording is free to change -- but it is a PACKAGE-LEVEL name describing an
// internal concept ("orchestrator: lane outage"), and the operator-facing
// string wants to describe the CAUSE. Those are different jobs. Keeping the
// sentinel's text for the classifier's identity while letting the cause own the
// message serves both without either compromising.
//
// WHY NOT errors.Join ALONE. Join renders BOTH errors, newline-separated, which
// puts the sentinel text back into the message.
//
// Unwrap() returns both errors, so errors.Is reaches the sentinel AND the cause
// exactly as the previous errors.Join did. Only the rendering changes.
type laneOutage struct {
	cause error
}

// Error renders the CAUSE alone. The sentinel is matchable but invisible, which
// is the entire point: the reason column should open with the actionable fact.
func (e *laneOutage) Error() string {
	if e.cause == nil {
		// Defensive: no caller builds this with a nil cause today. An empty
		// string here would be worse than the old verbosity -- downstream an
		// empty last_error is normalized to 'unknown' by reports.FailureAnalysis
		// and is indistinguishable from "no error was recorded".
		return ErrLaneOutage.Error()
	}
	return e.cause.Error()
}

// Unwrap returns both the sentinel and the cause so errors.Is finds either.
// This is the multi-error Unwrap form (Go 1.20+), the same shape errors.Join
// produces -- so every existing errors.Is call site behaves identically.
//
// ONE DELIBERATE DIFFERENCE FROM A PLAIN %w WRAP: the SINGULAR errors.Unwrap()
// returns nil for a multi-error, as it did for the errors.Join value this
// replaced. Verified: no caller in this repo uses the singular form on a lane
// error. A future caller that needs the cause should use errors.Is/errors.As,
// which traverse both branches.
func (e *laneOutage) Unwrap() []error {
	if e.cause == nil {
		return []error{ErrLaneOutage}
	}
	return []error{ErrLaneOutage, e.cause}
}

// newLaneOutage wraps a lane's genuine backend failure so it stays classifiable
// as ErrLaneOutage while reading as the cause itself.
func newLaneOutage(cause error) error {
	return &laneOutage{cause: cause}
}

// Compile-time assertion that the multi-error Unwrap shape is what errors.Is
// consumes. A single-error Unwrap() error method would silently satisfy neither
// this interface nor the sentinel matching, and the failure would surface only
// as a misclassified queue row.
var _ interface{ Unwrap() []error } = (*laneOutage)(nil)
