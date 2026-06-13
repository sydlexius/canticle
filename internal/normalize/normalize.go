package normalize

import (
	"strings"
	"unicode"

	"golang.org/x/text/runes"
	"golang.org/x/text/transform"
	"golang.org/x/text/unicode/norm"
)

// NormalizeKey converts s to a canonical lowercase cache key:
// NFKD decomposition, strip combining marks (Unicode category Mn), NFC, lowercase, trim spaces.
// Invalid UTF-8 sequences are replaced with empty string before transformation;
// if the transform still fails, the sanitized input is used as a best-effort fallback.
func NormalizeKey(s string) string {
	sanitized := strings.ToValidUTF8(s, "")
	t := transform.Chain(norm.NFKD, runes.Remove(runes.In(unicode.Mn)), norm.NFC)
	result, _, err := transform.String(t, sanitized)
	if err != nil {
		result = sanitized
	}
	return strings.ToLower(strings.TrimSpace(result))
}

// DurationBucket maps a track length in seconds to the 5-second floor bucket
// used as the duration component of the lyrics_cache key (migration 014).
// Returns 0 (the unknown-duration sentinel) when seconds <= 0.
func DurationBucket(seconds int) int {
	if seconds <= 0 {
		return 0
	}
	return seconds / 5
}

// MatchConfidence returns a Jaro-Winkler similarity score in [0.0, 1.0]
// between NormalizeKey(a) and NormalizeKey(b).
// 1.0 = identical, 0.0 = completely different.
func MatchConfidence(a, b string) float64 {
	a = NormalizeKey(a)
	b = NormalizeKey(b)
	return jaroWinkler(a, b)
}

// jaroWinkler computes the Jaro-Winkler similarity between two already-normalized strings.
// Uses p=0.1 (standard prefix scaling factor), max prefix length 4.
func jaroWinkler(s1, s2 string) float64 {
	if s1 == s2 {
		return 1.0
	}
	if len(s1) == 0 || len(s2) == 0 {
		return 0.0
	}

	r1 := []rune(s1)
	r2 := []rune(s2)
	l1, l2 := len(r1), len(r2)

	matchDist := max(l1, l2)/2 - 1
	if matchDist < 0 {
		matchDist = 0
	}

	m1 := make([]bool, l1)
	m2 := make([]bool, l2)
	matches := 0

	for i := 0; i < l1; i++ {
		lo := max(0, i-matchDist)
		hi := min(l2-1, i+matchDist)
		for j := lo; j <= hi; j++ {
			if !m2[j] && r1[i] == r2[j] {
				m1[i] = true
				m2[j] = true
				matches++
				break
			}
		}
	}

	if matches == 0 {
		return 0.0
	}

	transpositions := 0
	k := 0
	for i := 0; i < l1; i++ {
		if !m1[i] {
			continue
		}
		for !m2[k] {
			k++
		}
		if r1[i] != r2[k] {
			transpositions++
		}
		k++
	}

	fm := float64(matches)
	jaro := (fm/float64(l1) + fm/float64(l2) + (fm-float64(transpositions)/2)/fm) / 3.0

	// Jaro-Winkler prefix bonus (p=0.1, max prefix length 4).
	prefLen := 0
	maxPref := min(4, min(l1, l2))
	for i := 0; i < maxPref; i++ {
		if r1[i] == r2[i] {
			prefLen++
		} else {
			break
		}
	}
	return jaro + float64(prefLen)*0.1*(1.0-jaro)
}
