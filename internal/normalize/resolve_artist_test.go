package normalize

import "testing"

func TestResolveArtist(t *testing.T) {
	tests := []struct {
		name        string
		albumArtist string
		artist      string
		want        string
	}{
		{"album artist preferred", "Lady Gaga", "Lady Gaga feat. Bradley Cooper", "Lady Gaga"},
		{"empty album artist falls back", "", "Rihanna", "Rihanna"},
		{"various artists falls back", "Various Artists", "Adele", "Adele"},
		{"VA falls back", "VA", "Adele", "Adele"},
		{"various falls back", "Various", "Adele", "Adele"},
		{"various artists case-insensitive", "various artists", "Adele", "Adele"},
		{"VA lowercase", "va", "Adele", "Adele"},
		{"album artist with surrounding space trimmed and preferred", "  Muse  ", "Muse feat. X", "Muse"},
		{"both empty", "", "", ""},
		{"placeholder album artist but empty track artist yields empty", "Various Artists", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveArtist(tt.albumArtist, tt.artist); got != tt.want {
				t.Fatalf("ResolveArtist(%q, %q) = %q; want %q", tt.albumArtist, tt.artist, got, tt.want)
			}
		})
	}
}
