// Package vocalcalib re-thresholds stored per-track detector scores to
// calibrate the sung-vocal gate. All logic here is pure (no audio, no DB); the
// cmd/vocalcalib tool feeds it labeled scores gathered on a test environment.
package vocalcalib

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
)

// LabeledScore is one track's raw detector scores plus its ground-truth label.
// Label is "vocal" (provider returned lyrics) or "instrumental" (provider
// self-labeled instrumental, detector-independent).
type LabeledScore struct {
	MusicSum        float64 `json:"music_sum"`
	VocalPeak       float64 `json:"vocal_peak"`
	SpeechMean      float64 `json:"speech_mean"`
	VocalClass      string  `json:"vocal_class"`
	DetectorVersion string  `json:"detector_version"`
	Label           string  `json:"label"`
}

// WriteJSONL appends one score as a JSON line.
func WriteJSONL(w io.Writer, s LabeledScore) error {
	b, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("marshal score: %w", err)
	}
	if _, err := w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write score: %w", err)
	}
	return nil
}

// ReadJSONL reads all scores from a JSONL stream.
func ReadJSONL(r io.Reader) ([]LabeledScore, error) {
	var out []LabeledScore
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var s LabeledScore
		if err := json.Unmarshal(line, &s); err != nil {
			return nil, fmt.Errorf("unmarshal score: %w", err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}
