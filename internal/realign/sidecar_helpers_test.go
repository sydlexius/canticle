package realign

import "testing"

// TestIsSidecar_Characterization pins the classification the realign walk
// applies, so the #986 refactor onto internal/sidecar cannot have changed which
// files the planner collects as orphans. With the word-synced Kind active,
// isSidecar reports ".elrc" too, so what keeps a companion from being planned
// as an orphan of its own is isCompanion, which the walk consults FIRST: every
// ".elrc" must be a companion, and nothing else may be.
func TestIsSidecar_Characterization(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		want      bool
		companion bool
	}{
		{"lrc", "song.lrc", true, false},
		{"txt", "song.txt", true, false},
		{"uppercase lrc", "SONG.LRC", true, false},
		{"mixed case txt", "Song.TxT", true, false},
		{"full path", "/music/Artist/Album/01 - track.lrc", true, false},
		{"elrc is a companion", "song.elrc", true, true},
		{"uppercase elrc is a companion", "SONG.ELRC", true, true},
		{"audio", "song.mp3", false, false},
		{"no extension", "song", false, false},
		{"empty", "", false, false},
		{"unrelated dotfile", ".gitignore", false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isSidecar(tc.in); got != tc.want {
				t.Fatalf("isSidecar(%q) = %v, want %v", tc.in, got, tc.want)
			}
			if got := isCompanion(tc.in); got != tc.companion {
				t.Fatalf("isCompanion(%q) = %v, want %v", tc.in, got, tc.companion)
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
