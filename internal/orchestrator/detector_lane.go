package orchestrator

import (
	"context"
	"fmt"

	"github.com/doxazo-net/canticle/internal/circuit"
	"github.com/doxazo-net/canticle/internal/detector"
	"github.com/doxazo-net/canticle/internal/models"
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
				return models.Song{}, fmt.Errorf("%w: %v", ErrLaneOutage, err)
			}
			if !res.Instrumental {
				return models.Song{}, ErrLaneBenignMiss
			}
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
