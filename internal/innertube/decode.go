package innertube

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// browsePayload mirrors the nested shape of a timed-lyrics browse response
// (ANDROID_MUSIC / IOS_MUSIC clients only -- see doc.go). lyricLine is the
// cue's text; cueRange is its SIBLING, not nested inside lyricLine -- an
// earlier probe searched inside lyricLine and found zero timings.
type browsePayload struct {
	Contents struct {
		ElementRenderer struct {
			NewElement struct {
				Type struct {
					ComponentType struct {
						Model struct {
							TimedLyricsModel struct {
								LyricsData struct {
									TimedLyricsData []browseCue `json:"timedLyricsData"`
									// SourceMessage names the upstream licensor this
									// result was routed to, and is a SIBLING of the cue
									// list rather than a field on it -- the attribution
									// is per RESPONSE, not per line. CAPTURED, not
									// inferred: measured live at this exact path on
									// 2026-09-07 across four public reference tracks.
									//
									// It is a DISPLAY STRING, not a token: the observed
									// values carry a "Source: " prefix and mixed case
									// ("Source: LyricFind"). upstreamToken owns the
									// mapping; nothing else may format this value.
									SourceMessage string `json:"sourceMessage"`
								} `json:"lyricsData"`
							} `json:"timedLyricsModel"`
						} `json:"model"`
					} `json:"componentType"`
				} `json:"type"`
			} `json:"newElement"`
		} `json:"elementRenderer"`
	} `json:"contents"`
}

// browseCue is one raw timedLyricsData entry. start/end times arrive as
// quoted decimal strings, not JSON numbers.
type browseCue struct {
	LyricLine string `json:"lyricLine"`
	CueRange  struct {
		StartTimeMilliseconds string `json:"startTimeMilliseconds"`
		EndTimeMilliseconds   string `json:"endTimeMilliseconds"`
	} `json:"cueRange"`
}

// ExtractCues parses a raw browse response into the Cue list it carries,
// preserving EndMs (see the Cue doc comment in types.go -- no current
// models type has anywhere to put a line-level end time, so this extraction
// step is where that value is carried rather than silently dropped).
//
// The sentinel split follows the petitlyrics convention (decode.go and
// neighbors): JSON that fails to unmarshal is a transport-level problem and
// does not wrap ErrNotFound -- it is not a benign miss. JSON that unmarshals
// cleanly but yields zero cues -- whether because the lyrics section is
// entirely absent from this response or because it is present and empty --
// is a clean miss, wrapping ErrNotFound. Go's json.Unmarshal does not
// distinguish those two cases: an absent nested object simply leaves the
// corresponding struct fields at their zero value, the same result as an
// explicit empty array, so both necessarily land in the same bucket here.
func ExtractCues(raw []byte) ([]Cue, error) {
	var payload browsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("innertube: decode browse response: %w", err)
	}

	rawCues := payload.Contents.ElementRenderer.NewElement.Type.ComponentType.Model.
		TimedLyricsModel.LyricsData.TimedLyricsData
	if len(rawCues) == 0 {
		return nil, fmt.Errorf("innertube: browse response carried no timed lyric cues: %w", ErrNotFound)
	}

	cues := make([]Cue, 0, len(rawCues))
	allTextEmpty := true
	for i, rc := range rawCues {
		startMs, err := strconv.Atoi(rc.CueRange.StartTimeMilliseconds)
		if err != nil {
			return nil, fmt.Errorf("innertube: cue %d: parse startTimeMilliseconds %q: %w", i, rc.CueRange.StartTimeMilliseconds, err)
		}
		// A negative offset is a malformed payload, not a benign miss, so it
		// is rejected here (unwrapped, transport-class) rather than left to
		// be silently clamped to zero downstream by models.MsToTime -- the
		// two exported entry points must agree about the same payload.
		if startMs < 0 {
			return nil, fmt.Errorf("innertube: cue %d: startTimeMilliseconds %d is negative", i, startMs)
		}
		endMs, err := strconv.Atoi(rc.CueRange.EndTimeMilliseconds)
		if err != nil {
			return nil, fmt.Errorf("innertube: cue %d: parse endTimeMilliseconds %q: %w", i, rc.CueRange.EndTimeMilliseconds, err)
		}
		if endMs < 0 {
			return nil, fmt.Errorf("innertube: cue %d: endTimeMilliseconds %d is negative", i, endMs)
		}
		// endMs == startMs (a zero-length cue) is accepted: some providers
		// emit a single-instant cue for a very short vocalization, and that
		// is a legitimate degenerate case, not a malformed payload. Only
		// endMs < startMs -- the range running backwards -- is rejected here,
		// transport-class like the negative checks above, since a backwards
		// range cannot describe any real timing.
		if endMs < startMs {
			return nil, fmt.Errorf("innertube: cue %d: endTimeMilliseconds %d is before startTimeMilliseconds %d", i, endMs, startMs)
		}
		if strings.TrimSpace(rc.LyricLine) != "" {
			allTextEmpty = false
		}
		cues = append(cues, Cue{
			Text:    strings.TrimSpace(rc.LyricLine),
			StartMs: startMs,
			EndMs:   endMs,
		})
	}
	if allTextEmpty {
		// Every cue's text is empty (after trimming): the response was
		// reached and parsed cleanly but carries nothing usable. Writing an
		// all-empty-cue .lrc would retire the queue row and block another
		// provider lane from answering, which is worse than reporting a
		// miss, so this bucket wraps ErrNotFound like the zero-cue case
		// above.
		//
		// A PARTIALLY empty set (some cues carry text, some do not) is
		// deliberately left alone here: dropping individual empty cues would
		// shift every later cue's apparent position without any signal that
		// it happened, which is a worse failure mode than passing an
		// occasional blank line through to the writer.
		return nil, fmt.Errorf("innertube: browse response carried %d cues but every cue's text was empty: %w", len(cues), ErrNotFound)
	}
	return cues, nil
}

// Decode parses a raw browse response into a models.Song carrying timed
// cues in Subtitles. It is pure: no I/O, no network.
//
// Each Cue's StartMs is converted via models.MsToTime (#863). Total keeps
// full millisecond precision while Minutes/Seconds/Hundredths are derived by
// integer division/modulo and truncate to the nearest 10ms -- the four
// fields agree only when the input is already a multiple of 10 (tracked as
// #868; out of scope here because the arithmetic lives in models, not this
// package -- this provider's own fixture happens to be 22/22 multiples of
// 10, so nothing in this package's own tests exercises the mismatch, but the
// mismatch itself is reachable elsewhere: petitlyrics' word-sync lane passes
// a raw, unmultiplied millisecond value into the same conversion).
//
// Lines are sorted directly by Time.Total rather than run through
// lrcnormalize.Expand: Expand exists to split timestamp-shaped tokens back
// out of cue TEXT, repairing a petitlyrics parse bug where a stacked
// multi-timestamp line arrives with stamps embedded in the text.
// InnerTube's payload cannot have that defect -- lyricLine (text) and
// cueRange (timing) are separate sibling fields, never combined -- so
// running Expand here bought only the sort, at the cost of a silent
// corruption path: any lyric line that legitimately begins with a
// bracketed, timestamp-shaped substring would be split into a fabricated
// extra cue.
func Decode(raw []byte) (models.Song, error) {
	cues, err := ExtractCues(raw)
	if err != nil {
		return models.Song{}, err
	}

	// EndMs is dropped here: models.Lines has no field for a line-level end
	// time (see the Cue doc comment in types.go for why that value has
	// nowhere to go downstream), so only Text and a start Time survive the
	// conversion.
	lines := make([]models.Lines, 0, len(cues))
	for _, c := range cues {
		lines = append(lines, models.Lines{
			Text: c.Text,
			Time: models.MsToTime(c.StartMs),
		})
	}

	sort.SliceStable(lines, func(i, j int) bool {
		return lines[i].Time.Total < lines[j].Time.Total
	})

	return models.Song{
		Subtitles: models.Synced{Lines: lines},
		Upstream:  ExtractUpstream(raw),
	}, nil
}

// Upstream tokens for the licensors this lane multiplexes. Lowercase and
// unpunctuated, matching the existing provider-token convention
// (`petitlyrics`, `canticle-detector`).
//
// UpstreamMusixmatch is DELIBERATELY byte-identical to the token the direct
// first-party Musixmatch lane writes into [source:]. That collision is exactly
// why the upstream is carried in its own [upstream:] tag and never folded into
// [source:] -- see docs/provider-attribution.md. Writing it into [source:]
// would make an InnerTube-routed result indistinguishable from a first-party
// one, so `--source musixmatch` would sweep in files the operator never
// targeted, which is the #827 class of defect.
const (
	UpstreamMusixmatch = "musixmatch"
	UpstreamLyricFind  = "lyricfind"
)

// sourceMessagePrefix is the display prefix the API puts in front of the
// licensor name. MEASURED, not assumed: every observed value took the form
// "Source: Musixmatch" / "Source: LyricFind".
const sourceMessagePrefix = "Source:"

// ExtractUpstream reports which licensor served this response, as one of the
// Upstream* constants, or "" when the response names none.
//
// A CLOSED SET, NEVER A PASSTHROUGH. An unrecognized, renamed, absent or
// malformed value yields "", and the caller then writes no [upstream:] tag at
// all -- an omitted tag asserts nothing, which is honest, whereas a passthrough
// would format an unsanitized third-party string straight into an LRC header.
// That matters concretely: the fetch-time writer formats the token with
// fmt.Sprintf and does NOT run sanitizeTagValue, and a NEWLINE in a header
// value truncates the tag and ends the header block early (parser.go). A closed
// set means the writer never holds an unsanitized string in the first place.
// It also keeps the token stable if the upstream renames itself upstream.
//
// Parse failure is not distinguished from absence. Both mean "no attribution
// established", the caller treats them identically, and this function is never
// the place a transport problem is reported -- ExtractCues already owns that.
func ExtractUpstream(raw []byte) string {
	var payload browsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	msg := payload.Contents.ElementRenderer.NewElement.Type.ComponentType.
		Model.TimedLyricsModel.LyricsData.SourceMessage
	return upstreamToken(msg)
}

// upstreamToken maps one raw sourceMessage to a constant, or "" for anything
// it does not recognize.
//
// The prefix is trimmed with TrimPrefix on a case-folded copy rather than
// matched exactly, because the casing of the NAME is what varies in the
// observed data ("LyricFind" carries an interior capital), and a future
// response that drops or re-cases the prefix should still map rather than
// silently losing attribution. A value with no prefix at all still maps: the
// trim is a no-op and the switch sees the bare name.
func upstreamToken(sourceMessage string) string {
	s := strings.ToLower(strings.TrimSpace(sourceMessage))
	s = strings.TrimSpace(strings.TrimPrefix(s, strings.ToLower(sourceMessagePrefix)))
	switch s {
	case UpstreamMusixmatch:
		return UpstreamMusixmatch
	case "lyricfind":
		return UpstreamLyricFind
	default:
		return ""
	}
}
