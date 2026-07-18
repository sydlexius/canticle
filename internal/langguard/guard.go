package langguard

import (
	"fmt"
	"strings"

	"github.com/sydlexius/canticle/internal/models"
)

// Guard rejects lyric results whose body is dominated by scripts outside an
// allowlist. An empty allowlist disables it (Accept always true).
type Guard struct {
	accepted  map[string]bool
	threshold float64
}

// NewGuard builds a Guard from a script allowlist and a foreign-letter share
// threshold. A threshold <= 0 or > 1 is replaced with the 0.20 default.
func NewGuard(acceptedScripts []string, threshold float64) *Guard {
	if threshold <= 0 || threshold > 1 {
		threshold = 0.20
	}
	m := make(map[string]bool, len(acceptedScripts))
	for _, s := range acceptedScripts {
		if s = strings.TrimSpace(s); s != "" {
			m[s] = true
		}
	}
	return &Guard{accepted: m, threshold: threshold}
}

// Enabled reports whether any script filtering is active.
func (g *Guard) Enabled() bool { return len(g.accepted) > 0 }

// Accept reports whether the song's lyric body is within the allowlist. A
// disabled guard, or a body with no letters, accepts. Reason is "" on accept.
func (g *Guard) Accept(song models.Song) (bool, string) {
	if !g.Enabled() {
		return true, ""
	}
	var total, foreign int
	for _, r := range lyricBody(song) {
		s := ScriptOf(r)
		if s == "" {
			continue
		}
		total++
		if !g.accepted[s] {
			foreign++
		}
	}
	if total == 0 {
		return true, ""
	}
	share := float64(foreign) / float64(total)
	if share > g.threshold {
		return false, fmt.Sprintf("foreign-script share %.2f exceeds %.2f", share, g.threshold)
	}
	return true, ""
}

// lyricBody concatenates the song's lyric text (synced lines then unsynced
// body), excluding credit/attribution lines so they do not skew script scoring.
func lyricBody(song models.Song) string {
	var b strings.Builder
	for _, ln := range song.Subtitles.Lines {
		if t := strings.TrimSpace(ln.Text); t != "" && !IsCreditLine(t) {
			b.WriteString(t)
			b.WriteByte('\n')
		}
	}
	if song.Lyrics.LyricsBody != "" {
		for _, raw := range strings.Split(song.Lyrics.LyricsBody, "\n") {
			if t := strings.TrimSpace(raw); t != "" && !IsCreditLine(t) {
				b.WriteString(t)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}
