package ffmpeg

import (
	"errors"
	"fmt"
)

// ErrAudioMissing is the sentinel returned when a sidecar was asked to sample an
// audio file that is not on disk. It is a DIFFERENT CONDITION from a sampling
// failure and must not be reported as one: a missing file is a path problem
// (the file moved, or the queue row is stale) with a path remedy, while a
// sampling failure means ffmpeg reached the file and could not decode it.
//
// Both previously surfaced to the operator as the same "detector request
// failed" prose, which named the wrong subsystem for the missing-file case and
// would send someone reading the detector sidecar's logs for a file-location
// bug (#790).
//
// WHY A STRUCTURAL CHECK RATHER THAN PARSING ffmpeg. Two alternatives were
// considered and rejected:
//
//   - ffmpeg's EXIT CODE. 254 was observed for this case on one build, but it
//     is not a documented stable API and nothing pins it across versions.
//   - ffmpeg's DIAGNOSTIC TEXT ("No such file or directory"). It is
//     locale-dependent, so the discriminator would silently stop working under
//     a non-English locale -- failing back to the misleading message this
//     exists to remove, in an environment nobody tests.
//
// os.Stat is the authority for "the file is gone", which is exactly what
// internal/prune already relies on for the same question.
//
// This lives in internal/ffmpeg rather than in a sidecar package because BOTH
// sidecars need it: internal/detector and internal/verification are siblings,
// so a sentinel in either would force the other to import it. internal/ffmpeg
// is the existing common ancestor -- the same reasoning that put SampleError
// here (#796).
var ErrAudioMissing = errors.New("audio file is missing")

// MissingAudioError builds the error a subsystem returns when the audio file it
// was asked to sample does not exist, matchable via errors.Is(err,
// ErrAudioMissing).
//
// The audioPath parameter is deliberately ACCEPTED AND NOT RENDERED. It is
// present so every call site reads as though the path is being reported, and so
// the compiler requires the caller to have it in hand -- but the rendered string
// carries only the subsystem and the condition. Same contract as SampleError,
// for the same reason: this value is persisted to work_queue.last_error, which
// reaches the Failure Analysis report and (for a failing rather than deferring
// caller) the externally scraped mxlrcgo_queue_failures{reason=...} label. A
// library path is private metadata and last_error has leaked to a report
// surface before (#431). The path belongs in the caller's slog.Warn.
//
// Unlike SampleError's caller contract, this one IS structural: there is no
// parameter through which a path can reach the output.
func MissingAudioError(subsystem string, audioPath string) error {
	_ = audioPath // deliberately unrendered; see the doc comment above
	return fmt.Errorf("%s: %w", subsystem, ErrAudioMissing)
}
