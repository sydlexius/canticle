package petitlyrics

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/sydlexius/canticle/internal/models"
)

// Lyrics tiers advertised by the API's lyricsType parameter and field.
const (
	tierUnsynced = 1 // base64 plain UTF-8 text
	tierLineSync = 2 // base64 encrypted LSY binary; not yet decodable
	tierWordSync = 3 // base64 <wsy> XML with per-word timings
)

// WordTiming is one word's timing within a synced line. Emitted alongside the
// line-level cues so the Enhanced-LRC (A2) writer (#480) has word granularity
// without re-parsing the payload.
type WordTiming struct {
	// Line is the zero-based index of the owning line in the returned cues.
	Line int
	// Text is the word as it appears in the payload.
	Text string
	// StartMS and EndMS are milliseconds from the start of the track.
	StartMS int
	EndMS   int
}

// wsyDoc mirrors the <wsy> word-sync payload. Element names are taken from
// observed responses: <wsy> holds <line> elements, each with a <linestring>
// and one <word> per word carrying <starttime>/<endtime>/<wordstring>.
type wsyDoc struct {
	XMLName xml.Name  `xml:"wsy"`
	Lines   []wsyLine `xml:"line"`
}

type wsyLine struct {
	LineString string    `xml:"linestring"`
	Words      []wsyWord `xml:"word"`
}

type wsyWord struct {
	StartTime int    `xml:"starttime"`
	EndTime   int    `xml:"endtime"`
	WordStr   string `xml:"wordstring"`
}

// xmlRootPrefix returns raw with leading whitespace and any XML prologue
// (declarations, processing instructions, comments) stripped, so the caller sees
// the first real element. Observed payloads open with <wsy> directly, but a
// declaration is valid XML and must not change the classification.
func xmlRootPrefix(raw []byte) []byte {
	b := bytes.TrimLeft(raw, " \t\r\n")
	for {
		switch {
		case bytes.HasPrefix(b, []byte("<?")):
			end := bytes.Index(b, []byte("?>"))
			if end < 0 {
				return b
			}
			b = b[end+2:]
		case bytes.HasPrefix(b, []byte("<!--")):
			end := bytes.Index(b, []byte("-->"))
			if end < 0 {
				return b
			}
			b = b[end+3:]
		default:
			return b
		}
		b = bytes.TrimLeft(b, " \t\r\n")
	}
}

// classifyPayload reports which tier a decoded payload actually is, derived from
// the bytes rather than from the response's lyricsType field.
//
// This is deliberate. The API's availableLyricsType is not a capability set (it
// reported the same value regardless of the tier requested), and lyricsType
// echoes the request. The payload itself is the only trustworthy discriminator,
// and the three shapes are cleanly separable:
//
//	tierWordSync -- XML with a <wsy> root
//	tierLineSync -- binary: not valid UTF-8, and carries NUL bytes
//	tierUnsynced -- valid UTF-8 text with no NUL bytes
func classifyPayload(raw []byte) int {
	if bytes.HasPrefix(xmlRootPrefix(raw), []byte("<wsy")) {
		return tierWordSync
	}
	if !utf8.Valid(raw) || bytes.IndexByte(raw, 0) >= 0 {
		return tierLineSync
	}
	return tierUnsynced
}

// decodeUnsynced returns plain lyric text from an unsynced payload.
func decodeUnsynced(raw []byte) string {
	return strings.TrimRight(string(raw), "\r\n")
}

// msToTime converts milliseconds to the models.Time shape used by the writers.
func msToTime(ms int) models.Time {
	if ms < 0 {
		ms = 0
	}
	return models.Time{
		Total:      float64(ms) / 1000.0,
		Minutes:    ms / 60000,
		Seconds:    (ms / 1000) % 60,
		Hundredths: (ms / 10) % 100,
	}
}

// decodeWordSync parses a <wsy> payload into line-level cues plus per-word
// timings.
//
// A line's cue timestamp is its FIRST word's starttime, which is what makes a
// normal .lrc fall out of a word-synced payload for free.
//
// Word-level coverage is not uniform in practice: on a verified word-synced
// track, 10 of 86 lines had every word sharing a single timestamp, so those
// lines are effectively line-level only. Such lines are still emitted as cues;
// callers that need genuine word granularity should check WordTiming spans
// rather than assume every line carries distinct word times.
//
// Lines carrying no words are skipped: without a word there is no timestamp to
// anchor the cue, and an untimed cue is worse than an absent one.
//
// Lines are sorted by their first word's start time BEFORE indices are assigned.
// That is what keeps WordTiming.Line valid downstream: the caller runs the cues
// through lrcnormalize.Expand, which sorts them, so indices computed against an
// unsorted slice would silently point at the wrong cue after normalization.
// Sorting here makes Expand's sort a no-op on this path and the indices stable.
func decodeWordSync(raw []byte) ([]models.Lines, []WordTiming, error) {
	var doc wsyDoc
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, nil, fmt.Errorf("petitlyrics: decode word-sync payload: %w", err)
	}

	timed := make([]wsyLine, 0, len(doc.Lines))
	for _, line := range doc.Lines {
		if len(line.Words) > 0 {
			timed = append(timed, line)
		}
	}
	sort.SliceStable(timed, func(i, j int) bool {
		return timed[i].Words[0].StartTime < timed[j].Words[0].StartTime
	})

	cues := make([]models.Lines, 0, len(timed))
	var timings []WordTiming
	for _, line := range timed {
		idx := len(cues)
		text := strings.TrimSpace(line.LineString)
		if text == "" {
			// Fall back to joining the words when <linestring> is absent, so a
			// cue is never emitted with empty text while timings exist.
			parts := make([]string, 0, len(line.Words))
			for _, w := range line.Words {
				parts = append(parts, w.WordStr)
			}
			text = strings.TrimSpace(strings.Join(parts, ""))
		}
		cues = append(cues, models.Lines{
			Text: text,
			Time: msToTime(line.Words[0].StartTime),
		})
		for _, w := range line.Words {
			// Clamp the same way msToTime clamps the cue, so a negative timestamp
			// cannot leave the cue and its word timings disagreeing about the same
			// word.
			timings = append(timings, WordTiming{
				Line:    idx,
				Text:    w.WordStr,
				StartMS: max(w.StartTime, 0),
				EndMS:   max(w.EndTime, 0),
			})
		}
	}

	if len(cues) == 0 {
		return nil, nil, fmt.Errorf("petitlyrics: word-sync payload carried no timed lines: %w", ErrNotFound)
	}
	return cues, timings, nil
}
