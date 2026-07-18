package langguard

import (
	"testing"

	"github.com/sydlexius/canticle/internal/models"
)

func synced(lines ...string) models.Song {
	var s models.Song
	for _, l := range lines {
		s.Subtitles.Lines = append(s.Subtitles.Lines, models.Lines{Text: l})
	}
	return s
}

func TestGuardDisabledAcceptsEverything(t *testing.T) {
	g := NewGuard(nil, 0.20) // empty allowlist => disabled
	if g.Enabled() {
		t.Fatal("Enabled() = true for empty allowlist; want false")
	}
	if ok, _ := g.Accept(synced("七里香", "作词 : 方文山")); !ok {
		t.Fatal("disabled guard rejected a song; want accept")
	}
}

func TestGuardLatinAllowlist(t *testing.T) {
	g := NewGuard([]string{Latin}, 0.20)

	en := synced("作词 : Max Martin", "Nice to meet you, where you been?",
		"I could show you incredible things")
	if ok, reason := g.Accept(en); !ok {
		t.Fatalf("English-with-CJK-credits rejected: %s", reason)
	}

	if ok, _ := g.Accept(synced("七里香", "窗外的麻雀", "在电线杆上多嘴")); ok {
		t.Fatal("CJK body accepted; want reject")
	}

	if ok, _ := g.Accept(synced("♪ ♪ ♪")); !ok {
		t.Fatal("no-letter body rejected; want accept")
	}
}

func TestGuardThresholdBoundary(t *testing.T) {
	g := NewGuard([]string{Latin}, 0.20)
	merged := synced(
		"Nice to meet you where you been",
		"I could show you incredible things",
		"很高兴认识你你去哪了我可以给你看不可思议的东西很高兴",
	)
	if ok, _ := g.Accept(merged); ok {
		t.Fatal("merged ~30% Han accepted at threshold 0.20; want reject")
	}
}

func TestNewGuardDefaultsThreshold(t *testing.T) {
	if g := NewGuard([]string{Latin}, 0); g.threshold != 0.20 {
		t.Fatalf("threshold = %v; want 0.20 default", g.threshold)
	}
	if g := NewGuard([]string{Latin}, 1.5); g.threshold != 0.20 {
		t.Fatalf("out-of-range threshold not defaulted: %v", g.threshold)
	}
}
