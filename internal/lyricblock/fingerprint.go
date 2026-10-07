// Package lyricblock fingerprints a lyric result's words and stores the
// fingerprints an operator has marked wrong, per track identity (#1122).
package lyricblock

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode"

	"github.com/sydlexius/canticle/internal/lrcnormalize"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/timing"
)

// Fingerprint returns the SHA-256 (hex) of the word-token stream of body, a
// raw .lrc or plain-text lyric body as read off disk. Timestamps, word-timing
// markers, [key:value] ID tags and decorative lines are dropped, so a synced,
// unsynced, re-timed, word-timed or reflowed copy of the same words hashes
// equal. It returns "" when no words remain (an instrumental marker, an empty
// body): such a result has no fingerprint and must never be blocked.
func Fingerprint(body string) string {
	var lines []string
	for _, raw := range strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n") {
		doc := lrcnormalize.ParseBody(raw)
		switch {
		case len(doc.Cues) > 0:
			// A multi-stamp line expands to one cue per stamp; its words count once.
			lines = append(lines, doc.Cues[0].Text)
		case len(doc.Tags) > 0:
			// ID tag, not lyric text.
		default:
			lines = append(lines, raw)
		}
	}
	return fingerprintLines(lines)
}

// FingerprintSong fingerprints a fetched result the same way Fingerprint does
// its on-disk body: the synced cue text when present, else the plain body.
// The translation and romanization tracks are not part of the key.
func FingerprintSong(s models.Song) string {
	if len(s.Subtitles.Lines) > 0 {
		lines := make([]string, 0, len(s.Subtitles.Lines))
		for _, l := range s.Subtitles.Lines {
			lines = append(lines, l.Text)
		}
		return fingerprintLines(lines)
	}
	return Fingerprint(s.Lyrics.LyricsBody)
}

func fingerprintLines(lines []string) string {
	var tokens []string
	for _, l := range lines {
		l = strings.TrimSpace(timing.StripWordMarkers(l))
		if timing.IsDecorative(l) {
			continue
		}
		tokens = append(tokens, strings.FieldsFunc(normalize.NormalizeKey(l), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})...)
	}
	if len(tokens) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])
}
