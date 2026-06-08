// Package langguard classifies and filters lyric text by Unicode script so a
// configured allowlist can reject unwanted-language results (e.g. CJK lyrics for
// an English library). It depends only on models and the standard library so it
// can be reused by a future opt-in translation feature.
package langguard

import "unicode"

// Script bucket names returned by ScriptOf.
const (
	Latin  = "Latin"
	Han    = "Han"
	Kana   = "Kana"
	Hangul = "Hangul"
	Other  = "Other"
)

// ScriptOf classifies a rune into a coarse Unicode script bucket. It returns ""
// for non-letters (digits, punctuation, whitespace, symbols) so callers can
// ignore them when scoring a lyric body.
func ScriptOf(r rune) string {
	if !unicode.IsLetter(r) {
		return ""
	}
	switch {
	case (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3400 && r <= 0x4DBF):
		return Han
	case r >= 0x3040 && r <= 0x30FF:
		return Kana
	case r >= 0xAC00 && r <= 0xD7AF:
		return Hangul
	case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'):
		return Latin
	default:
		return Other
	}
}
