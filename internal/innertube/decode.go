package innertube

import (
	"encoding/json"
	"errors"
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
									// The attribution field (sourceMessage) is a
									// SIBLING of this cue list in the wire payload,
									// but is deliberately NOT declared here -- see
									// upstreamPayload for why the two are decoded
									// separately.
									TimedLyricsData []browseCue `json:"timedLyricsData"`
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
//
// CueRange is a POINTER so an ABSENT cueRange stays distinguishable from a
// present-but-broken one. That distinction is the whole defect: a value struct
// decodes an absent object to its zero value, which made an untimed entry look
// like a timed entry carrying an empty timestamp, and strconv.Atoi("") then
// reported a malformed payload for a response that was merely plain text.
type browseCue struct {
	LyricLine string    `json:"lyricLine"`
	CueRange  *cueRange `json:"cueRange"`
}

type cueRange struct {
	StartTimeMilliseconds string `json:"startTimeMilliseconds"`
	EndTimeMilliseconds   string `json:"endTimeMilliseconds"`
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

	// UNTIMED PAYLOAD: every entry carries text and no cueRange at all. This
	// is a real, common shape from this API (see ErrUntimedLyrics), not a
	// malformed one, so it is separated out BEFORE the timestamp parse rather
	// than being discovered as an Atoi failure on an empty string.
	//
	// Counting first (rather than branching inside the loop) is what makes the
	// three cases separable: all-timed is the normal path, all-untimed is the
	// plain-text path, and a MIX is a shape neither branch can serve honestly
	// -- timing the untimed lines at 00:00 would fabricate positions, and
	// dropping them would lose words -- so a mix falls through to the timed
	// path and is reported transport-class by the parse below, which is the
	// conservative reading for a shape never observed live.
	untimed := 0
	for _, rc := range rawCues {
		if rc.CueRange == nil {
			untimed++
		}
	}
	if untimed == len(rawCues) {
		return nil, fmt.Errorf("innertube: browse response carried %d lyric lines with no timings: %w", len(rawCues), ErrUntimedLyrics)
	}

	cues := make([]Cue, 0, len(rawCues))
	allTextEmpty := true
	for i, rc := range rawCues {
		if rc.CueRange == nil {
			// Reachable only for a PARTIALLY timed payload -- the all-untimed
			// case returned above. Transport-class (unwrapped) so the row is
			// retried rather than retired on a shape we have never measured.
			return nil, fmt.Errorf("innertube: cue %d: cueRange absent in a partially timed payload", i)
		}
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

// ExtractPlainLyrics parses the lyric TEXT out of an untimed browse response
// -- the shape ExtractCues reports as ErrUntimedLyrics -- and returns it as a
// single newline-joined body suitable for models.Lyrics.LyricsBody.
//
// Blank lines are PRESERVED rather than dropped. They are stanza breaks in the
// provider's own rendering, and this path has no timings whose positions a
// retained blank could shift, so there is no reason to edit the text. (The
// timed path's identical restraint about a partially empty cue set is argued
// in ExtractCues; the conclusion is the same for a different reason.)
//
// An all-blank body is an ErrNotFound miss, matching the timed path: writing
// an empty .txt would retire the queue row and block another lane from
// answering, which is worse than reporting nothing found.
//
// Re-unmarshaling rather than threading the lines out of ExtractCues is
// deliberate. Cue has no representation for "text with no timing", and
// widening it (or returning a second slice from ExtractCues) would put an
// always-empty field in front of every timed caller to serve a branch none of
// them take. The cost is one extra unmarshal on the untimed path only, which
// is the same trade ExtractUpstream already makes.
func ExtractPlainLyrics(raw []byte) (string, error) {
	var payload browsePayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", fmt.Errorf("innertube: decode browse response: %w", err)
	}
	rawCues := payload.Contents.ElementRenderer.NewElement.Type.ComponentType.Model.
		TimedLyricsModel.LyricsData.TimedLyricsData

	lines := make([]string, 0, len(rawCues))
	allTextEmpty := true
	for _, rc := range rawCues {
		text := strings.TrimSpace(rc.LyricLine)
		if text != "" {
			allTextEmpty = false
		}
		lines = append(lines, text)
	}
	if allTextEmpty {
		return "", fmt.Errorf("innertube: browse response carried %d untimed lines but every line was empty: %w", len(lines), ErrNotFound)
	}
	return strings.Join(lines, "\n"), nil
}

// Decode parses a raw browse response into a models.Song carrying timed
// cues in Subtitles. It is pure: no I/O, no network.
//
// A response carrying text but NO timings (ErrUntimedLyrics -- see errors.go
// for why this API serves two shapes) degrades to an UNSYNCED result:
// Lyrics.LyricsBody is populated and Subtitles is left empty, which is what
// makes the writer emit .txt instead of .lrc. Returning the error instead
// would discard lyrics the API had already handed us and, because that error
// was previously unwrapped, retire the queue row as a hard failure -- the
// measured production defect this branch exists to fix.
//
// THAT DEGRADATION HAS A SECOND CONSEQUENCE, in the orchestrator rather than
// here, and it is stated rather than left to be discovered. An unsynced result
// is a SUCCESS: orchestrator.QualityOf scores it QualityUnsynced, IsSuitable
// accepts at that level, and findOrdered returns on the first suitable lane. So
// in ordered mode a later lane that would have served SYNCED lyrics is no
// longer consulted, where previously this lane's error fell through to it. The
// better result is not deferred, it is not sought.
//
// Accepted deliberately, and it is still strictly better than the behavior it
// replaces: the alternative is a hard queue failure that discards the words and
// burns a retry, and the .txt remains promotable by --upgrade. It is also
// exactly what any other provider's unsynced result already does here -- this
// lane is not being given a special power, it is being made to behave like the
// others. Parallel mode is unaffected: its raceWait upgrade window already
// prefers a synced result that arrives within the window.
//
// Tracked as #915. It matters most where innertube is ordered AHEAD of another
// lyric lane, which is the live production configuration, so the tracking issue
// owns measuring the real cost rather than this comment asserting it is small.
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
	if errors.Is(err, ErrUntimedLyrics) {
		body, plainErr := ExtractPlainLyrics(raw)
		if plainErr != nil {
			return models.Song{}, plainErr
		}
		return models.Song{
			Lyrics:   models.Lyrics{LyricsBody: body},
			Upstream: ExtractUpstream(raw),
		}, nil
	}
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
	var payload upstreamPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	raw2 := payload.Contents.ElementRenderer.NewElement.Type.ComponentType.
		Model.TimedLyricsModel.LyricsData.SourceMessage
	return upstreamToken(sourceMessageText(raw2))
}

// upstreamPayload decodes ONLY the attribution field, and its separateness from
// browsePayload is a correctness requirement rather than tidiness.
//
// The two fields are siblings on the wire, so declaring both on one struct is
// the obvious shape -- and it couples them fatally. encoding/json aborts the
// WHOLE unmarshal on a single type mismatch, so a sourceMessage that is not a
// bare string would take the CUES down with it: measured, a payload carrying a
// valid timedLyricsData array plus a `{"runs":[...]}` sourceMessage returned
// zero cues and a transport-class error, which does NOT degrade to a benign
// miss. That shape is not hypothetical -- a runs-object is this API's dominant
// form for a display string, and the bare string measured here is the exception.
//
// Decoding the attribution separately means the worst a surprising shape can do
// is cost the attribution (SourceMessage stays "", no [upstream:] tag is
// written, which asserts nothing) while the lyrics still arrive. The cue path
// cannot be broken by a field it does not read.
//
// SourceMessage is json.RawMessage rather than string for the same reason: a
// mismatch must never fail the decode. upstreamToken owns the interpretation.
type upstreamPayload struct {
	Contents struct {
		ElementRenderer struct {
			NewElement struct {
				Type struct {
					ComponentType struct {
						Model struct {
							TimedLyricsModel struct {
								LyricsData struct {
									// CAPTURED, not inferred: measured live at this
									// path on 2026-09-07 across four public
									// reference tracks. It is a DISPLAY STRING, not
									// a token -- the observed values carry a
									// "Source: " prefix and mixed case
									// ("Source: LyricFind").
									SourceMessage json.RawMessage `json:"sourceMessage"`
								} `json:"lyricsData"`
							} `json:"timedLyricsModel"`
						} `json:"model"`
					} `json:"componentType"`
				} `json:"type"`
			} `json:"newElement"`
		} `json:"elementRenderer"`
	} `json:"contents"`
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
// sourceMessageText coerces the raw attribution value to the display string it
// carries, or "" for any shape it does not recognize.
//
// TWO SHAPES ARE ACCEPTED, and the second is the reason this function exists
// rather than a plain string field. A bare JSON string is what was measured
// live. A `{"runs":[{"text":"..."}]}` object is this API's dominant form for a
// display string elsewhere, so it is the likeliest way the value changes shape
// without notice; reading it costs a few lines and turns a silent loss of
// attribution into a correct one.
//
// Anything else -- a number, an array, null, absent, malformed -- yields "" and
// therefore no [upstream:] tag. That is the honest outcome: an absent tag
// asserts nothing, and this function must never fail the caller, which is why
// it returns a string rather than an error.
func sourceMessageText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var runs struct {
		Runs []struct {
			Text string `json:"text"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(raw, &runs); err != nil {
		return ""
	}
	// Concatenated, not just the first run: a display string is split across
	// runs at styling boundaries, so taking runs[0] alone would truncate
	// "Source: X" to "Source: " whenever the licensor name is styled separately.
	var b strings.Builder
	for _, r := range runs.Runs {
		b.WriteString(r.Text)
	}
	return b.String()
}

func upstreamToken(sourceMessage string) string {
	s := strings.ToLower(strings.TrimSpace(sourceMessage))
	s = strings.TrimSpace(strings.TrimPrefix(s, strings.ToLower(sourceMessagePrefix)))
	// THE CASE ARMS ARE WIRE NAMES; THE RETURNS ARE OUR TOKENS. The two sides
	// of each arm mean different things, and today they coincide because the
	// tokens were deliberately chosen to match the lowercased upstream names.
	//
	// So the case arms are string LITERALS on purpose, and both of them, rather
	// than the constants they happen to equal. A review flagged the previous
	// mixture (one arm a constant, one a literal) as an inconsistency, which it
	// was -- but resolving it toward the CONSTANT is the wrong direction: that
	// makes a rename of OUR token silently stop matching the upstream's
	// unchanged wire name, which is the one thing this function must keep doing.
	// Written as literals, renaming a token changes only what we emit, and the
	// mapping keeps working. The coincidence is pinned by TestUpstreamToken,
	// whose inputs are the captured wire strings.
	switch s {
	case "musixmatch":
		return UpstreamMusixmatch
	case "lyricfind":
		return UpstreamLyricFind
	default:
		return ""
	}
}
