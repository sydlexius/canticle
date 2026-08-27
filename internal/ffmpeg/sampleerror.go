package ffmpeg

import "fmt"

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
	return fmt.Errorf("%s: sample audio with ffmpeg: %w: %s", subsystem, err, BoundOutput(output))
}
