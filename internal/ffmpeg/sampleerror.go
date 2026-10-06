package ffmpeg

import (
	"errors"
	"fmt"
	"os/exec"
)

// ErrSampleFailed is matched (errors.Is) by an error SampleError builds when
// ffmpeg RAN and could not decode the file. A caller uses it to tell a per-file
// defect (retrying the same bytes fails identically) from an outage (classifier
// down, context canceled, host trouble) that a later cycle may clear (#1149).
var ErrSampleFailed = errors.New("ffmpeg could not sample the audio file")

// sampleError keeps SampleError's rendered text byte-for-byte while adding the
// ErrSampleFailed match alongside the wrapped cause for a decode failure.
type sampleError struct {
	msg    string
	cause  error
	decode bool
}

func (e *sampleError) Error() string { return e.msg }
func (e *sampleError) Unwrap() []error {
	if e.decode {
		return []error{e.cause, ErrSampleFailed}
	}
	return []error{e.cause}
}

// isDecodeFailure reports whether err is a sampling process that started and
// exited on its own with a non-zero status: the one shape that says the FILE is
// the problem. Everything else is the host's and must stay retryable (#1149):
// a start failure (*os.PathError, exec.ErrNotFound), a signal (an OOM kill, a
// cgroup limit, an operator kill), exit 126/127 (the nice/ionice wrappers'
// "could not execute ffmpeg"), and 255 (ffmpeg's exit after catching a
// termination signal).
func isDecodeFailure(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ProcessState == nil || !exitErr.Exited() {
		return false
	}
	switch exitErr.ExitCode() {
	case 0, 126, 127, 255:
		return false
	}
	return true
}

// SampleError builds the error returned when an ffmpeg sample invocation
// fails, for a subsystem-prefixed caller (e.g. "verification", "detector").
// It applies BoundOutput to the captured subprocess output internally, so a
// caller cannot forget the size bound that keeps a corrupt input's unbounded
// stderr out of work_queue.last_error (#731). That one IS structural: there is
// no path through this function to an unbounded rendering.
//
// The audio file path must NOT be passed as part of output or folded into
// subsystem -- but note that is a CALLER CONTRACT, not something the signature
// enforces: a path folded into either string still renders. It is pinned by a
// test at each call site rather than by the type system.
//
// Why the contract: this error is persisted to work_queue.last_error, which is
// rendered into the Failure Analysis report, and last_error has leaked to a
// report surface before (#431). For a caller whose failure lands in
// status='failed' it travels further still -- CountFailuresByReason
// (internal/queue) selects only that status and groups last_error verbatim into
// the mxlrcgo_queue_failures{reason=...} Prometheus label, scraped and retained
// off-host. That is the verification caller today; the detector lane defers
// instead (its failures are wrapped in ErrLaneOutage), so it does not currently
// reach the metric. The contract is written for the wider surface deliberately:
// a caller's status disposition is not this function's to know, and a lane that
// starts failing rather than deferring must not silently widen a leak. The path
// belongs in the caller's slog.Warn, never here.
func SampleError(subsystem string, err error, output string) error {
	msg := fmt.Sprintf("%s: sample audio with ffmpeg: %v: %s", subsystem, err, BoundOutput(output))
	return &sampleError{msg: msg, cause: err, decode: isDecodeFailure(err)}
}
