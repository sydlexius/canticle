package orchestrator

import (
	"context"
	"errors"
	"fmt"

	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/models"
)

// detectorLaneName is the lane name a detector-backed lane reports.
const detectorLaneName = "detector"

// NewDetectorLane builds an orchestrator lane over the audio detector and its
// dedicated breaker. The resolve func runs the 3-gate over the work item's audio
// path: a gate-positive verdict returns a terminal-suitable instrumental Song
// carrying detector telemetry; a gate-negative verdict (or an empty path)
// returns ErrLaneBenignMiss so the providers run; a detector call failure
// returns ErrLaneOutage so the breaker trips.
func NewDetectorLane(d detector.Detector, breaker *circuit.Breaker) *Lane {
	return &Lane{
		name:        detectorLaneName,
		breaker:     breaker,
		classifyErr: detectorClassifier,
		resolve: func(ctx context.Context, track models.Track, sourcePath string) (models.Song, error) {
			// An empty sourcePath means instrumental detection is disabled for this
			// item (e.g. no audio path on the work item); the detector must never be
			// invoked in that case, so this is checked before calling Detect.
			if sourcePath == "" {
				return models.Song{}, ErrLaneBenignMiss
			}
			res, err := d.Detect(ctx, sourcePath)
			if err != nil {
				// Join rather than %v so BOTH the sentinel and the detector's own
				// cause stay matchable with errors.Is: the classifier keys on
				// ErrLaneOutage, while callers and logs need the underlying failure.
				return models.Song{}, fmt.Errorf("detector request failed: %w", errors.Join(ErrLaneOutage, err))
			}
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
