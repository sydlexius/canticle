package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/providers"
)

// detectorLaneName is the lane name a detector-backed lane reports.
const detectorLaneName = "detector"

// notReadyBound caps how many consecutive "not ready" (dial-refused, never
// reached) results a detector lane emits before escalating to a genuine outage.
// A sidecar that is merely booting comes up within a few work cycles; one that
// has never been reachable is misconfigured or dead, and must trip the breaker
// so the lane stops re-spending a rate-limited provider call every cycle (#567).
// This is an attempt count, not a wall-clock sleep (which the issue forbids).
const notReadyBound = 5

// NewDetectorLane builds an orchestrator lane over the audio detector and its
// dedicated breaker. The resolve func runs the 3-gate over the work item's audio
// path: a gate-positive verdict returns a terminal-suitable instrumental Song
// carrying detector telemetry; a gate-negative verdict (or an empty path)
// returns ErrLaneBenignMiss so the providers run; a detector call failure
// returns ErrLaneOutage so the breaker trips.
//
// pacer is optional (nil is valid, and every existing call site predating #550
// passed none): when non-nil it is credited on a gate-positive settle so the
// detector's throughput helps ease the SHARED provider pacer's ratchet back
// down (see the local/pacer split below). Callers should pass the SAME pacer
// instance the primary provider lane uses (Lane.Pacer() on that lane), not a
// detector-owned one -- the detector has no throttle concept of its own; it is
// only ever crediting recovery of the provider's adaptive interval.
func NewDetectorLane(d detector.Detector, breaker *circuit.Breaker, pacer providers.AdaptivePacer) *Lane {
	// everReached records whether the lane has ever completed a detector call
	// since this process booted; notReadyStreak counts consecutive dial failures
	// while it has not. Together they distinguish a boot race (release, no trip)
	// from a die-after-up or dead-from-boot sidecar (outage, trip) -- see #567.
	// These are lane-global across work items, captured in the resolve closure.
	var everReached atomic.Bool
	var notReadyStreak atomic.Uint32
	return &Lane{
		name:        detectorLaneName,
		breaker:     breaker,
		classifyErr: detectorClassifier,
		// local=true: the detector settles a track from local audio analysis, no
		// outbound provider request, so an item settled here must not SPEND the
		// provider-request pacing budget (#534) -- Lane.FindLyrics never calls
		// pace() for this lane regardless of pacer below.
		local: true,
		// pacer decision (#550): a local lane SHOULD still CREDIT decay. Spending
		// and crediting are separable: local=true controls only whether pace() is
		// paid (it is not, for any lane -- pace() is invoked exclusively inside the
		// musixmatch client's own FindLyrics, never by Lane), while pacer here
		// controls only whether Lane.notifySuccess() fires OnSuccess on a genuine
		// settle. A detector settle proves the work item was disposed of without
		// needing a provider round-trip at all, so crediting it costs nothing (no
		// extra request is made to earn the credit) and helps the ratchet unwind
		// faster when detector settles make up a large share of traffic, exactly
		// the measured production gap (8 of 52 settles in the starved window were
		// detector instrumentals, per #492's pacer_decay_test.go). The next local
		// lane added to this package should make the same call explicitly rather
		// than silently inheriting whatever this constructor happens to do.
		pacer: pacer,
		resolve: func(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
			// An empty sourcePath means instrumental detection is disabled for this
			// item (e.g. no audio path on the work item); the detector must never be
			// invoked in that case, so this is checked before calling Detect.
			if sourcePath == "" {
				return models.Song{}, ErrLaneBenignMiss
			}
			res, err := d.Detect(ctx, sourcePath)
			if err != nil {
				// A dial-level failure (sidecar unreachable) is a startup race ONLY
				// until the lane has reached the sidecar once since boot, and only
				// for the first notReadyBound attempts. Beyond that -- or after any
				// prior success -- an unreachable sidecar is a genuine outage that
				// must trip the breaker (#567). Any non-dial detector error (a
				// non-2xx status, an ffmpeg sampling failure) is always an outage.
				//
				// Join rather than %v so BOTH the sentinel and the detector's own
				// cause stay matchable with errors.Is: the classifier keys on the
				// sentinel, while callers and logs need the underlying failure.
				if errors.Is(err, detector.ErrClassifierNotReady) &&
					!everReached.Load() && notReadyStreak.Load() < notReadyBound {
					notReadyStreak.Add(1)
					return models.Song{}, fmt.Errorf("detector not ready: %w", errors.Join(ErrLaneNotReady, err))
				}
				// newLaneOutage rather than the fmt.Errorf/errors.Join pair used
				// above (#790): this error is persisted verbatim to
				// work_queue.last_error and rendered into the Failure Analysis
				// report, where the old form opened with ~80 characters of wrapper
				// ("detector request failed: " plus the sentinel's own
				// "orchestrator: lane outage") before the actionable cause. The
				// sentinel stays matchable via errors.Is -- only its TEXT stops
				// rendering. The not-ready branch above keeps the joined form
				// deliberately: it releases the item without recording a failure,
				// so its text never reaches last_error.
				return models.Song{}, newLaneOutage(err)
			}
			everReached.Store(true)
			notReadyStreak.Store(0)
			if !res.Instrumental {
				return models.Song{}, ErrLaneBenignMiss
			}
			// Only these identity fields are carried, deliberately: this mirrors the
			// inline miss-branch path in the worker that this lane replaces, so the
			// settled song is byte-identical to today's. Note it therefore also
			// inherits that path's limitation of dropping the remaining
			// models.Track fields (AlbumArtist, TrackLength, HasLyrics,
			// HasSubtitles, ISRC, SpotifyID, RecordingMBID). Widening this to copy
			// the incoming track is a behavior change, tracked separately, not a
			// silent tweak to make here.
			return models.Song{
				Track: models.Track{
					ArtistName:   track.ArtistName,
					TrackName:    track.TrackName,
					AlbumName:    track.AlbumName,
					Instrumental: 1,
				},
				DetectorVersion:    res.Version,
				DetectorMusicSum:   res.Confidence,
				DetectorVocalPeak:  res.VocalConfidence,
				DetectorSpeechMean: res.SpeechConfidence,
				DetectorVocalClass: res.WinningVocalClass,
			}, nil
		},
	}
}
