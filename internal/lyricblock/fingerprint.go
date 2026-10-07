// Package lyricblock fingerprints a lyric result's words and stores the
// fingerprints an operator has marked wrong, per track identity (#1122).
//
// A fingerprint is an EXACT-words hash: the letters and digits of the body
// after normalize.NormalizeKey, concatenated with no separator. It ignores
// timestamps, word-timing markers, [key:value] ID tags, decorative lines, case,
// spacing, punctuation and line breaks (so an unspaced script hashes the same
// however a provider segments it). It does NOT ignore any word: a section
// header such as "[Chorus]" that one copy has and another lacks changes the
// hash, as does any added or removed line. NormalizeKey strips combining marks,
// so texts differing only in such marks hash equal; acceptable, since the track
// identity is part of the block key.
//
// Compute a fingerprint once per fetched result and reuse it: it parses the
// whole body and hashes every line.
package lyricblock

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/timing"
)

// Fingerprint returns the SHA-256 (hex) of the words of body, a raw .lrc or
// plain-text lyric body as read off disk. It returns "" when no words remain (an
// instrumental marker, an empty body): that result must never be blocked.
func Fingerprint(body string) string {
	body = strings.TrimPrefix(body, "\uFEFF")
	doc := lrcnormalize.ParseBody(body)
	if len(doc.Cues) > 0 {
		// Expanded and time-sorted: [t1][t2]X contributes X once per stamp.
		return hashLines(doc.Cues)
	}
	tags := make(map[string]bool, len(doc.Tags))
	for _, tg := range doc.Tags {
		tags[tg.Raw] = true
	}
	var lines []models.Lines
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		if !tags[raw] {
			lines = append(lines, models.Lines{Text: raw})
		}
	}
	return hashLines(lines)
}

// SongFingerprints returns every fingerprint the writer could put on disk for a
// fetched result, deduplicated, in a stable order: the cue text stream, the cue
// stream with time-matched translations interleaved (bilingual output), then
// the Lyrics body (the unsynced and timing-demoted .txt). An instrumental has
// none: the writer emits a marker whatever else it carries. A result is blocked
// if ANY fingerprint is blocked (Store.AnyBlocked).
func SongFingerprints(s models.Song) []string {
	if s.Track.Instrumental == 1 {
		return nil
	}
	orig, tr := s.Subtitles.Lines, s.TranslationSubtitles.Lines
	cands := []string{hashLines(timeSorted(orig)), Fingerprint(s.Lyrics.LyricsBody)}
	if len(tr) > 0 && len(orig) > 0 {
		cands = slices.Insert(cands, 1, hashLines(interleave(orig, tr)))
	}
	var out []string
	for _, fp := range cands {
		if fp != "" && !slices.Contains(out, fp) {
			out = append(out, fp)
		}
	}
	return out
}

// timeSorted returns a copy of lines stably sorted by timestamp, the order
// ParseBody reads the written file back in (the writer emits slice order, a
// provider may hand it lines out of order). The caller's slice is not touched.
func timeSorted(lines []models.Lines) []models.Lines {
	out := slices.Clone(lines)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Total < out[j].Time.Total })
	return out
}

// interleave mirrors lyrics.writeSyncedLRC with bilingual output on: each cue is
// followed by its translation, matched FIFO by rendered stamp as
// lyrics.pairBilingualTranslations does, then stably time-sorted as ParseBody
// does on read.
func interleave(orig, tr []models.Lines) []models.Lines {
	byStamp := make(map[string][]int, len(tr))
	for j, t := range tr {
		st := t.Time.Stamp()
		byStamp[st] = append(byStamp[st], j)
	}
	out := make([]models.Lines, 0, 2*len(orig))
	for _, o := range orig {
		out = append(out, o)
		st := o.Time.Stamp()
		if q := byStamp[st]; len(q) > 0 {
			out = append(out, models.Lines{Text: tr[q[0]].Text, Time: o.Time})
			byStamp[st] = q[1:]
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Total < out[j].Time.Total })
	return out
}

// hashLines hashes the letters and digits of every non-decorative line's text,
// concatenated with no separator.
func hashLines(lines []models.Lines) string {
	var b strings.Builder
	for _, l := range lines {
		t := strings.TrimSpace(timing.StripWordMarkers(l.Text))
		if timing.IsDecorative(t) {
			continue
		}
		for _, r := range normalize.NormalizeKey(t) {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				b.WriteRune(r)
			}
		}
	}
	if b.Len() == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}
