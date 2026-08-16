package lyrics

import (
	"fmt"
	"os"
	"strings"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/timing"
)

// ReadSyncedLRC reads the .lrc file at path and returns its cues as the
// models.Synced that internal/timing judges.
//
// THIS IS THE DISK-TO-VERDICT SEAM. Everything downstream of a fetch already
// speaks models.Song: timing.Evaluate takes one, DecidePromotion takes one. What
// nothing did was go the other way -- take a .lrc that is ALREADY on disk and
// hand it back in that form -- so the backlog remediation pass (#442) and the
// serve sweep (#443) had no way to re-judge an existing file with the same
// predicate that judged it at accept time.
//
// The parse is DELEGATED to lrcnormalize.ParseBody rather than written here.
// That is the point: ParseBody already expands stacked [t1][t2]text lines into
// one cue per timestamp and classifies [key:value] header tags OUT of the cue
// list, so a canticle-written header can never be mistaken for a lyric line. A
// third LRC parser in this repo would be a third thing to keep in agreement with
// the writer, and the one it would disagree with first is the timing verdict.
//
// A file with no recognizable cue yields an empty Synced and a nil error: an
// unparsable or plain-text sidecar is a real state, not a failure, and it must
// reach timing.Evaluate as "no timing evidence" so the caller fails open on it.
func ReadSyncedLRC(path string) (models.Synced, error) {
	b, err := os.ReadFile(path) //nolint:gosec // reason: G304: path comes from the caller's own library-root walk, not untrusted input
	if err != nil {
		return models.Synced{}, fmt.Errorf("read lrc %q: %w", path, err)
	}
	doc := lrcnormalize.ParseBody(strings.TrimPrefix(string(b), utf8BOM))
	return models.Synced{Lines: doc.Cues}, nil
}

// EvaluateLRCFile reads the .lrc at path and classifies its timing against
// durationSeconds, returning the verdict, its magnitude, and how many cues were
// judged.
//
// It owns NO comparison logic: the verdict is timing.Evaluate's, computed over
// the corrected max (text-bearing lines only). That delegation is the whole
// reason this function exists rather than a bespoke max-timestamp check --
// roughly a third of naively-flagged files are perfectly-synced lyrics whose
// only past-duration timestamp is a trailing decorative marker, and only
// timing.Evaluate filters those out. A duration of 0 (unknown) yields
// timing.UnknownDuration, which every caller must treat as "do not remediate".
func EvaluateLRCFile(path string, durationSeconds int) (timing.TimingOutcome, timing.Magnitude, int, error) {
	synced, err := ReadSyncedLRC(path)
	if err != nil {
		return "", timing.Magnitude{}, 0, err
	}
	outcome, mag := timing.Evaluate(models.Song{Subtitles: synced}, durationSeconds)
	return outcome, mag, len(synced.Lines), nil
}

// PlainBody flattens synced cues to the plain words a demotion persists as
// .txt, dropping decorative cues via timing.IsDecorative.
//
// Exported so the backlog remediation pass demotes a file to exactly the body
// the accept-time guard would have written for the same lyric, rather than a
// near-copy that drifts. An empty result means there is nothing worth writing.
func PlainBody(synced models.Synced) string {
	var b strings.Builder
	for _, l := range synced.Lines {
		// Strip A2 word markers before anything else. These cues may have been
		// read back off disk from a file canticle itself wrote with word sync
		// enabled (#480), and a .txt is by definition the plain words -- persisting
		// `<00:01.50>alpha` there writes timestamp garbage into the user's
		// lyrics, unrecoverably. IsDecorative strips them too, so a marked
		// decorative cue is still dropped.
		text := strings.TrimSpace(timing.StripWordMarkers(l.Text))
		if timing.IsDecorative(text) {
			continue
		}
		b.WriteString(text)
		b.WriteString("\n")
	}
	return b.String()
}
