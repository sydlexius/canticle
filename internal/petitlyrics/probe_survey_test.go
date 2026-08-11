package petitlyrics

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/models"
)

// maxSamples bounds the sweep independently of the track list length, so a
// longer list is truncated rather than silently extending the run.
const maxSamples = 100

// minTrackList is a setup-error floor. Below this, tier-2 captures are too few to
// be a corpus and the isOfficial cross-tabulation has no cells.
const minTrackList = 50

// maxConsecutiveTransportErrors aborts a run that has started measuring the
// network rather than the API. One timeout is noise; three in a row is not.
const maxConsecutiveTransportErrors = 3

// TestSurveyProbe performs ONE paced live sweep against the real petitlyrics API
// and reports aggregates. It answers three questions off the same responses:
// whether isOfficial discriminates usefully (#615/#600), what word-sync coverage
// looks like on this library (#480), and whether a tier-2 corpus can be
// collected (#602).
//
// It is gated on PLPROBE=1 and makes real outbound requests. Run it explicitly:
//
//	PLPROBE_DIR=/path/to/scratchpad/plprobe PLPROBE=1 \
//	  go test -run TestSurveyProbe ./internal/petitlyrics -v -timeout 90m
//
// The track list at $PLPROBE_DIR/tracks.tsv is artist<TAB>title<TAB>album per
// line. Neither it nor the captures may live under the repo.
func TestSurveyProbe(t *testing.T) {
	if os.Getenv("PLPROBE") != "1" {
		t.Skip("live probe: set PLPROBE=1 to run")
	}

	dir := os.Getenv("PLPROBE_DIR")
	if dir == "" {
		t.Fatal("PLPROBE_DIR must be set to a scratchpad directory outside the repo")
	}
	if strings.Contains(dir, "canticle/internal") || strings.Contains(dir, "canticle/docs") {
		t.Fatalf("PLPROBE_DIR must not be under the repo: %s", dir)
	}

	captureDir := filepath.Join(dir, "captures")
	if err := os.MkdirAll(captureDir, 0o700); err != nil {
		t.Fatalf("create capture dir: %v", err)
	}

	tracks, err := readTrackList(filepath.Join(dir, "tracks.tsv"))
	if err != nil {
		t.Fatalf("read track list: %v", err)
	}
	if len(tracks) < minTrackList {
		t.Fatalf("track list has %d entries, need at least %d", len(tracks), minTrackList)
	}
	if len(tracks) > maxSamples {
		t.Logf("track list has %d entries; truncating to %d", len(tracks), maxSamples)
		tracks = tracks[:maxSamples]
	}

	ct := newCaptureTransport(captureDir)
	c := NewClient()
	c.httpClient = &http.Client{
		Timeout:       30 * time.Second,
		Transport:     ct,
		CheckRedirect: c.checkRedirect,
	}
	c.WithMinInterval(DefaultMinInterval)

	report := newSurveyReport()
	ctx := context.Background()
	consecutiveTransportErrors := 0

	for i, track := range tracks {
		obs, fatal := surveySample(ctx, c, track, captureDir, i)
		report.add(obs)

		if fatal != nil {
			t.Logf("ABORT at sample %d of %d: %v", i, len(tracks), fatal)
			t.Logf("Results collected before this point are SUSPECT, not partial: " +
				"an auth or throttle failure makes later lookups resemble misses.")
			break
		}

		if obs.Err == "transport" {
			consecutiveTransportErrors++
			if consecutiveTransportErrors >= maxConsecutiveTransportErrors {
				t.Logf("ABORT at sample %d: %d consecutive transport errors; "+
					"the run is measuring the network", i, consecutiveTransportErrors)
				break
			}
		} else {
			consecutiveTransportErrors = 0
		}
	}

	out := report.render()
	t.Logf("\n===== SURVEY REPORT (safe to share) =====\n%s", out)

	reportPath := filepath.Join(dir, "report.txt")
	if err := os.WriteFile(reportPath, []byte(out), 0o600); err != nil {
		t.Errorf("write report: %v", err)
	}
	t.Logf("report written to %s", reportPath)
	t.Logf("captures written: %d (raw, keep out of the repo)", ct.count())
}

// surveySample performs one lookup and classifies the result. The second return
// is non-nil for a class that must abort the whole run.
func surveySample(ctx context.Context, c *Client, track models.Track, captureDir string, idx int) (sampleObservation, error) {
	songs, err := c.request(ctx, track, tierWordSync)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthorized):
			// The clientAppID is a hardcoded constant shared by every canticle
			// install (#607). If it is revoked mid-sweep every later lookup
			// resembles a miss, and the coverage number would read as poor
			// coverage rather than a dead credential.
			return sampleObservation{Err: "ErrUnauthorized"}, fmt.Errorf("credential rejected: %w", err)
		case errors.Is(err, ErrForbidden):
			// Per errors.go, a refused request shape rather than throttling. Means
			// the probe talks to the service differently than canticle does, which
			// invalidates the premise of the measurement.
			return sampleObservation{Err: "ErrForbidden"}, fmt.Errorf("request refused: %w", err)
		case errors.Is(err, ErrRateLimited):
			// Prod does not query petitlyrics, so a 429 at this pacing means the
			// floor is wrong. A real #535 finding worth stopping for.
			return sampleObservation{Err: "ErrRateLimited"}, fmt.Errorf("throttled at sample %d: %w", idx, err)
		case errors.Is(err, ErrNotFound):
			return sampleObservation{Err: "ErrNotFound"}, nil
		default:
			return sampleObservation{Err: "transport"}, nil
		}
	}

	candidate, err := selectCandidate(songs, track)
	if err != nil {
		return sampleObservation{Err: "no-candidate"}, nil
	}
	if candidate.LyricsData == "" {
		return sampleObservation{Err: "empty-payload"}, nil
	}

	raw, err := base64.StdEncoding.DecodeString(candidate.LyricsData)
	if err != nil {
		return sampleObservation{Err: "base64"}, nil
	}

	obs := sampleObservation{
		Tier:       classifyPayload(raw),
		IsOfficial: candidate.IsOfficial,
		Copyright:  candidate.Copyright,
	}

	switch obs.Tier {
	case tierWordSync:
		cues, timings, decErr := decodeWordSync(raw)
		if decErr != nil {
			obs.Err = "wsy-decode"
			return obs, nil
		}
		obs.CueCount = len(cues)
		obs.DistinctRatio = distinctStartRatio(cues, timings)
	case tierLineSync:
		// The #602 corpus. The capture transport already wrote the full response;
		// write the decoded binary payload separately so the follow-on has the
		// exact bytes without re-parsing the envelope.
		path := filepath.Join(captureDir, fmt.Sprintf("lsy-%04d.bin", idx))
		if writeErr := os.WriteFile(path, raw, 0o600); writeErr != nil {
			obs.Err = "lsy-write"
		}
	}

	return obs, nil
}

// distinctStartRatio reports the fraction of lines whose words do NOT all share
// one start time. A line where every word shares a timestamp is effectively
// line-level only, which the A2 writer (#480) has to handle per line rather than
// assuming uniform coverage.
func distinctStartRatio(cues []models.Lines, timings []models.WordTiming) float64 {
	if len(cues) == 0 {
		return 0
	}
	starts := make(map[int]map[int]struct{}, len(cues))
	for _, wt := range timings {
		if starts[wt.Line] == nil {
			starts[wt.Line] = map[int]struct{}{}
		}
		starts[wt.Line][wt.StartMS] = struct{}{}
	}
	distinct := 0
	for i := range cues {
		if len(starts[i]) > 1 {
			distinct++
		}
	}
	return float64(distinct) / float64(len(cues))
}

// readTrackList parses artist<TAB>title<TAB>album per line.
//
// The open below is a variable path (gosec G304), which is fine here and needs
// no //nolint: gosec is excluded on _test.go by .golangci.yml, so a directive
// would suppress nothing and nolintlint would flag it as dead. On the merits,
// the path is built from PLPROBE_DIR, an operator-supplied scratchpad path for a
// manually-invoked probe; there is no untrusted input and no server context.
func readTrackList(path string) ([]models.Track, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open track list: %w", err)
	}
	defer func() { _ = f.Close() }()

	var tracks []models.Track
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		tr := models.Track{ArtistName: parts[0], TrackName: parts[1]}
		if len(parts) > 2 {
			tr.AlbumName = parts[2]
		}
		tracks = append(tracks, tr)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan track list: %w", err)
	}
	return tracks, nil
}
