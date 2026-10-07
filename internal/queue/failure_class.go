package queue

import (
	"errors"
	"strings"
)

// FailureClass is the category of a row's recorded failure, stored in
// work_queue.failure_class by the statement that writes last_error (#1285,
// migration 067). The values are the Work Queue Reason filter's keys.
type FailureClass string

// The failure classes. FailureNone is never passed by a caller: it is what a
// message of only whitespace stores.
const (
	FailureNone     FailureClass = "none"
	FailureWrite    FailureClass = "write"    // writing or reading a file failed
	FailureThrottle FailureClass = "throttle" // rate limited, refused, or the lane is parked
	FailureNetwork  FailureClass = "network"  // a server or network error
	FailureMiss     FailureClass = "miss"     // the provider answered: nothing usable
	FailureOther    FailureClass = "other"
)

// classedError carries a FailureClass alongside an unchanged error.
type classedError struct {
	error
	class FailureClass
}

func (e classedError) Unwrap() error { return e.error }

// WithFailureClass attaches class to err for Fail and Defer; message and chain are unchanged.
func WithFailureClass(err error, class FailureClass) error {
	if err == nil {
		return nil
	}
	return classedError{err, class}
}

// FailureClassOf returns the outermost class attached to err, or "" for none.
func FailureClassOf(err error) FailureClass {
	var ce classedError
	if errors.As(err, &ce) {
		return ce.class
	}
	return ""
}

// storedClass is the failure_class written beside message as last_error: NULL
// for an empty message or a missing class (the filter then reads the row as
// "none" or "other"), FailureNone for a blank message.
func storedClass(message string, class FailureClass) any {
	switch {
	case message == "" || class == "":
		return nil
	case strings.TrimSpace(message) == "":
		return string(FailureNone)
	}
	return string(class)
}
