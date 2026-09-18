package realign

import "testing"

// TestIsSidecar_Characterization pins the classification the realign walk has
// always applied, so the #986 refactor onto internal/sidecar cannot have
// changed which files the planner collects as orphans. ".elrc" must stay
// EXCLUDED: the planner has no pairing rules for it yet, and collecting it
// would let a move be planned for a file nothing knows how to handle.
func TestIsSidecar_Characterization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"lrc", "song.lrc", true},
		{"txt", "song.txt", true},
		{"uppercase lrc", "SONG.LRC", true},
		{"mixed case txt", "Song.TxT", true},
		{"full path", "/music/Artist/Album/01 - track.lrc", true},
		{"elrc is not yet a sidecar", "song.elrc", false},
		{"audio", "song.mp3", false},
		{"no extension", "song", false},
		{"empty", "", false},
		{"unrelated dotfile", ".gitignore", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSidecar(tc.in); got != tc.want {
				t.Fatalf("isSidecar(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestStemOf_Characterization pins that stemOf still returns the BASE name
// without its extension -- it drops the directory, unlike sidecar.StemOf,
// which preserves it. The refactor composes the two; this is the test that
// catches a drop of the filepath.Base wrapper.
func TestStemOf_Characterization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"strips directory and extension", "/music/Artist/song.lrc", "song"},
		{"relative", "song.txt", "song"},
		{"dots in directory", "/music/Artist v1.2/song.lrc", "song"},
		{"dots in file name", "/music/song.live.2024.lrc", "song.live.2024"},
		{"no extension", "/music/song", "song"},
		{"bare name", "song", "song"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := stemOf(tc.in); got != tc.want {
				t.Fatalf("stemOf(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
