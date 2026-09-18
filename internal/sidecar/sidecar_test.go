package sidecar

import (
	"slices"
	"testing"
)

func TestExtensions(t *testing.T) {
	got := Extensions()
	want := []string{".lrc", ".txt"}
	if !slices.Equal(got, want) {
		t.Fatalf("Extensions() = %v, want %v", got, want)
	}
	// The returned slice must be a copy: mutating it cannot corrupt the table.
	got[0] = ".mutated"
	if again := Extensions(); !slices.Equal(again, want) {
		t.Fatalf("Extensions() returned an aliased slice: after mutation got %v, want %v", again, want)
	}
}

func TestIsSidecar(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{"lrc", "song.lrc", true},
		{"txt", "song.txt", true},
		{"uppercase lrc", "SONG.LRC", true},
		{"mixed case txt", "Song.TxT", true},
		{"full path", "/music/Artist/Album/01 - track.lrc", true},
		{"unrelated extension", "song.mp3", false},
		{"no extension", "song", false},
		{"bare name with no extension in a dotted dir", "/music/v1.2/song", false},
		// filepath.Ext reads from the FINAL dot, so a leading dot is a final
		// dot and ".lrc" reads as extension ".lrc". This is the pre-existing
		// realign.isSidecar behavior, preserved deliberately.
		{"dotfile named for the extension", ".lrc", true},
		{"dotfile txt", ".txt", true},
		{"dotfile unrelated", ".gitignore", false},
		{"empty", "", false},
		{"elrc is declared but NOT active", "song.elrc", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSidecar(tc.input); got != tc.want {
				t.Fatalf("IsSidecar(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestELRCDeclaredButInert pins the #986 slice-1 invariant: the word-synced
// Kind EXISTS (so later slices can name it) but is NOT yet treated as a real
// sidecar by any call site.
//
// THIS TEST IS EXPECTED TO FAIL when the slice that teaches the call sites to
// handle .elrc flips the table entry to active:true. That failure is the point
// -- it forces whoever flips it to have read this comment and confirmed that
// realign's walk, the writer's pairing, and the settled-sidecar probe all
// handle the new extension. Update this test IN THAT COMMIT, not before.
func TestELRCDeclaredButInert(t *testing.T) {
	if Ext(KindWordSynced) != ".elrc" {
		t.Fatalf("Ext(KindWordSynced) = %q, want %q -- the Kind must be DECLARED", Ext(KindWordSynced), ".elrc")
	}
	if KindOf("song.elrc") != KindWordSynced {
		t.Fatalf("KindOf(%q) = %v, want KindWordSynced -- the Kind must be DECLARED", "song.elrc", KindOf("song.elrc"))
	}
	if Active(KindWordSynced) {
		t.Fatal("Active(KindWordSynced) = true: .elrc was enabled early. See this test's doc comment.")
	}
	if IsSidecar("song.elrc") {
		t.Fatal("IsSidecar(\"song.elrc\") = true: .elrc was enabled early. See this test's doc comment.")
	}
	if slices.Contains(Extensions(), ".elrc") {
		t.Fatal("Extensions() contains .elrc: it was enabled early. See this test's doc comment.")
	}
}

func TestKindOf(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Kind
	}{
		{"lrc path", "/music/song.lrc", KindLineSynced},
		{"txt path", "/music/song.txt", KindUnsynced},
		{"elrc path", "/music/song.elrc", KindWordSynced},
		{"uppercase", "SONG.LRC", KindLineSynced},
		{"bare dotted extension", ".lrc", KindLineSynced},
		{"bare dotted elrc", ".elrc", KindWordSynced},
		{"audio", "song.mp3", KindUnknown},
		{"no extension", "song", KindUnknown},
		{"empty", "", KindUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := KindOf(tc.input); got != tc.want {
				t.Fatalf("KindOf(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

func TestExtAndActive(t *testing.T) {
	tests := []struct {
		kind       Kind
		wantExt    string
		wantActive bool
	}{
		{KindLineSynced, ".lrc", true},
		{KindUnsynced, ".txt", true},
		{KindWordSynced, ".elrc", false},
		{KindUnknown, "", false},
		{Kind(99), "", false},
	}
	for _, tc := range tests {
		if got := Ext(tc.kind); got != tc.wantExt {
			t.Errorf("Ext(%v) = %q, want %q", tc.kind, got, tc.wantExt)
		}
		if got := Active(tc.kind); got != tc.wantActive {
			t.Errorf("Active(%v) = %v, want %v", tc.kind, got, tc.wantActive)
		}
	}
}

// TestKindExtRoundTrip proves the two mappings agree for every declared Kind,
// so a future entry cannot be added to one direction only.
func TestKindExtRoundTrip(t *testing.T) {
	for _, k := range []Kind{KindLineSynced, KindUnsynced, KindWordSynced} {
		ext := Ext(k)
		if ext == "" {
			t.Fatalf("Ext(%v) is empty: Kind declared with no extension", k)
		}
		if got := KindOf("song" + ext); got != k {
			t.Fatalf("KindOf(%q) = %v, want %v -- Kind/ext mappings disagree", "song"+ext, got, k)
		}
	}
}

func TestStemOf(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"simple", "song.lrc", "song"},
		{"full path", "/music/Artist/song.lrc", "/music/Artist/song"},
		{"dots in directory", "/music/Artist v1.2/song.txt", "/music/Artist v1.2/song"},
		{"dots in directory, file with no extension", "/music/Artist v1.2/song", "/music/Artist v1.2/song"},
		{"no extension", "song", "song"},
		{"multiple dots in name", "song.live.2024.lrc", "song.live.2024"},
		// See TestIsSidecar: ".lrc" is all extension, so its stem is empty.
		{"dotfile named for the extension", ".lrc", ""},
		{"dotfile unrelated", ".gitignore", ""},
		{"empty", "", ""},
		{"trailing dot", "song.", "song"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := StemOf(tc.input); got != tc.want {
				t.Fatalf("StemOf(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestActivateForTest pins the seam every #986 flag-state test relies on: it
// moves IsSidecar, Extensions and Active together, as the table flip will, and
// restores the declared state on cleanup.
func TestActivateForTest(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		ActivateForTest(t, KindWordSynced)
		if !Active(KindWordSynced) || !IsSidecar("a.elrc") || len(Extensions()) != 3 {
			t.Fatalf("override did not activate the kind: active=%v isSidecar=%v exts=%v",
				Active(KindWordSynced), IsSidecar("a.elrc"), Extensions())
		}
	})
	if Active(KindWordSynced) || IsSidecar("a.elrc") {
		t.Fatalf("override leaked past its test")
	}
}
