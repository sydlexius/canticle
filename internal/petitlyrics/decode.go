package petitlyrics

import (
	"bytes"
	"encoding/binary"
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
	tierLineSync = 2 // base64 obfuscated LSY binary; timings only, words come from tier 1
	tierWordSync = 3 // base64 <wsy> XML with per-word timings
)

// WordTiming is the shared models type, aliased here so this package's decoder
// signature stays readable. It lives in models because the orchestrator needs it
// to rank a word-synced result above a line-synced one, and because the
// Enhanced-LRC (A2) writer (#480) will consume it from models.Song.
type WordTiming = models.WordTiming

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
// This is deliberate, but NOT for the reason an earlier version of this comment
// gave. It claimed availableLyricsType was not a capability set, concluded from
// two probe tracks; that claim is RETRACTED. Measured over 107 hits the field
// predicts the returned tier with no exceptions.
//
// Classifying from the payload is still correct: it is the difference between
// trusting a field and validating what actually arrived, and it keeps the
// decoder honest if the API's own accounting ever drifts. The three shapes are
// cleanly separable:
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

// LSY container offsets. The tier-2 payload is a two-chunk binary: an
// MHDROBJT header followed by an MLRCOBJT payload. Verified against real
// captures -- every offset below held on all of them, and the size identity
// lsyPayloadStart + 2*lineCount + lineLength*lineCount == len(payload) was
// exact, which is what makes a truncated or wrongly parsed blob detectable rather
// than silently producing plausible garbage.
const (
	lsyKeySwitchFlagOff = 0x19 // u8: nonzero means the key is permuted once
	lsyProtectionIDOff  = 0x1a // u16 LE: the raw key before permutation
	lsyLineCountOff     = 0x38 // u32 LE: number of timed lines
	lsyLineLengthOff    = 0x42 // u16 LE: bytes per text slot (64 observed)
	lsyPayloadStart     = 0xcc // start of the u16 LE timestamp array
	lsyMinHeaderLen     = lsyPayloadStart

	// lsyMaxLines bounds a declared line count before it is used to size a
	// slice, so a corrupt or hostile u32 cannot drive a huge allocation. No
	// observed payload came close; a lyric with more lines than this is not a
	// lyric.
	lsyMaxLines = 10000

	// lsyWrapCS is the u16 rollover in centiseconds. A timestamp is stored in
	// 16 bits, so a track longer than ~10.9 minutes wraps and the decoder has
	// to unwrap it.
	lsyWrapCS = 65536
)

// deriveLSYKey reproduces the provider's key schedule: the protection id is
// used directly, or -- when the switch flag at 0x19 is set -- permuted once by
// a fixed mask/shift schedule that swaps adjacent bit PAIRS within each nibble
// while leaving the outermost pair of each byte in place.
//
// This is a ONE-TIME permutation of a single per-payload key, not a per-line
// rotation. Both the timestamps and the payload's text region use the result.
//
// Verified: on four real captures the flag was set and this reproduced, exactly,
// the key independently recovered from each payload's text by frequency
// analysis. That agreement between two unrelated derivations is the evidence
// this schedule is right -- the issue that specified it flagged the bit
// manipulation as suspect on read, and it is not.
func deriveLSYKey(protectionID uint16, switchFlag bool) uint16 {
	if !switchFlag {
		return protectionID
	}
	// Computed in uint16 rather than widening to uint32 and converting back.
	// Every left-shifted term is masked first, so none can carry past bit 15
	// (0x000c<<2 == 0x0030, 0x00c0<<2 == 0x0300, 0x0c00<<2 == 0x3000): the
	// arithmetic is exact at this width, and staying here means there is no
	// narrowing conversion to justify or suppress.
	k := protectionID
	return (k & 0x0003) |
		(k&0x000c)<<2 |
		(k&0x0030)>>2 |
		(k&0x00c0)<<2 |
		(k&0x0300)>>2 |
		(k&0x0c00)<<2 |
		(k&0x3000)>>2 |
		(k & 0xc000)
}

// decodeLineSyncTimings extracts the per-line cue times, in centiseconds, from
// an LSY (tier 2) payload.
//
// It returns ONLY the timings: the tier-2 blob does not carry usable lyric
// text. Its text region decodes to a partially-legible mix that never resolves
// cleanly under the payload key, so the words come from a separate
// lyricsType=1 request and are zipped against these timings by the caller.
// Attempting to recover text from this blob was measured and abandoned; do not
// retry it without new evidence.
//
// Timestamps are stored as u16 centiseconds and therefore ROLL OVER at ~10.9
// minutes. A decrease from one line to the next is read as a rollover and
// unwrapped. Note that the public reference implementation gets this wrong: its
// wrap counter is derived from the already-offset accumulator, so it can never
// increment and a long track decodes with every post-rollover cue ~10.9 minutes
// early. Unwrapping on the raw sequence is the fix.
func decodeLineSyncTimings(raw []byte) ([]int, error) {
	if len(raw) < lsyMinHeaderLen {
		return nil, fmt.Errorf("petitlyrics: line-sync payload is %d bytes, need at least %d: %w",
			len(raw), lsyMinHeaderLen, ErrNotFound)
	}

	lineCount := int(binary.LittleEndian.Uint32(raw[lsyLineCountOff:]))
	if lineCount <= 0 {
		return nil, fmt.Errorf("petitlyrics: line-sync payload declares %d lines: %w", lineCount, ErrNotFound)
	}
	if lineCount > lsyMaxLines {
		return nil, fmt.Errorf("petitlyrics: line-sync payload declares %d lines, over the %d cap: %w",
			lineCount, lsyMaxLines, ErrNotFound)
	}

	// The timestamp array must be fully present. Checked BEFORE indexing so a
	// truncated payload is a typed miss rather than a panic.
	end := lsyPayloadStart + 2*lineCount
	if len(raw) < end {
		return nil, fmt.Errorf("petitlyrics: line-sync payload is %d bytes, need %d for %d timestamps: %w",
			len(raw), end, lineCount, ErrNotFound)
	}

	// Cross-check the declared geometry against the actual length. The payload
	// is a timestamp array followed by lineCount fixed-width text slots, so
	// this identity holds exactly on a well-formed blob (verified on every real
	// capture). A mismatch means the shape is not what this decoder parses --
	// treat it as a miss rather than trusting timestamps read from it. Advisory
	// only when lineLength is absent: some field being zero is not proof of
	// corruption, but a nonzero one that disagrees is.
	if lineLength := int(binary.LittleEndian.Uint16(raw[lsyLineLengthOff:])); lineLength > 0 {
		want := lsyPayloadStart + 2*lineCount + lineLength*lineCount
		if len(raw) != want {
			return nil, fmt.Errorf(
				"petitlyrics: line-sync payload is %d bytes, geometry says %d (%d lines x %d-byte slots): %w",
				len(raw), want, lineCount, lineLength, ErrNotFound)
		}
	}

	key := deriveLSYKey(
		binary.LittleEndian.Uint16(raw[lsyProtectionIDOff:]),
		raw[lsyKeySwitchFlagOff] != 0,
	)

	out := make([]int, 0, lineCount)
	var wraps, prev int
	for i := range lineCount {
		cs := int(binary.LittleEndian.Uint16(raw[lsyPayloadStart+2*i:]) ^ key)
		if i > 0 && cs < prev {
			wraps++
		}
		prev = cs
		out = append(out, cs+wraps*lsyWrapCS)
	}
	return out, nil
}

// zipLineSync pairs decoded cue times with the lyric text fetched separately at
// tier 1.
//
// A count mismatch is a hard error, never a truncation to the shorter side.
// Zipping mismatched sequences yields an .lrc whose every later line is
// attributed to the wrong moment -- output that looks correct and is silently
// wrong, which is worse than no output at all.
//
// Trailing blank lines are dropped first: a text payload conventionally ends
// with a newline, and that alone must not fail an otherwise-aligned pair.
// Interior blanks are NOT dropped, because a blank between verses may or may
// not be a timed line, and guessing is exactly how misalignment gets in.
func zipLineSync(timings []int, text string) ([]models.Lines, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}

	if len(lines) != len(timings) {
		return nil, fmt.Errorf(
			"petitlyrics: line-sync timing/text mismatch: %d timestamps, %d text lines: %w",
			len(timings), len(lines), ErrNotFound)
	}

	// Ranging over timings and indexing lines (rather than the reverse) keeps the
	// bound visible: lines was just proven to be the same length, and reslicing it
	// to that length states the relationship for a reader and an analyzer alike.
	lines = lines[:len(timings)]
	cues := make([]models.Lines, 0, len(timings))
	for i, cs := range timings {
		cues = append(cues, models.Lines{
			Text: strings.TrimSpace(lines[i]),
			Time: models.MsToTime(cs * 10),
		})
	}
	return cues, nil
}

// decodeUnsynced returns plain lyric text from an unsynced payload.
func decodeUnsynced(raw []byte) string {
	return strings.TrimRight(string(raw), "\r\n")
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
			Time: models.MsToTime(line.Words[0].StartTime),
		})
		for _, w := range line.Words {
			// Clamp the same way models.MsToTime clamps the cue, so a negative timestamp
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
