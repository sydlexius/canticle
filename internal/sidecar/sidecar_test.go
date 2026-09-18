package sidecar

import (
	"slices"
	"testing"
)

func TestExtensions(t *testing.T) {
	got := Extensions()
	want := []string{".lrc", ".txt", ".elrc"}
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
		{"elrc is active", "song.elrc", true},
		{"uppercase elrc", "SONG.ELRC", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsSidecar(tc.input); got != tc.want {
				t.Fatalf("IsSidecar(%q) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestELRCActive pins the #986 slice-4c state: the word-synced Kind is
// declared AND active, so every call site that reads the active set (realign's
// walk, the scanner's skip, the writer's companion gate, OwnedCompanionOf)
// handles ".elrc". Deactivating it again would strand every companion already
// written into a library, so a change here needs the same audit the flip had.
func TestELRCActive(t *testing.T) {
	if Ext(KindWordSynced) != ".elrc" {
		t.Fatalf("Ext(KindWordSynced) = %q, want %q", Ext(KindWordSynced), ".elrc")
	}
	if KindOf("song.elrc") != KindWordSynced {
		t.Fatalf("KindOf(%q) = %v, want KindWordSynced", "song.elrc", KindOf("song.elrc"))
	}
	if !Active(KindWordSynced) {
		t.Fatal("Active(KindWordSynced) = false: the word-synced companion is switched off")
	}
	if !IsSidecar("song.elrc") {
		t.Fatal(`IsSidecar("song.elrc") = false: the word-synced companion is switched off`)
	}
	if !slices.Contains(Extensions(), ".elrc") {
		t.Fatal("Extensions() lacks .elrc: the word-synced companion is switched off")
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
		{KindWordSynced, ".elrc", true},
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
// moves IsSidecar, Extensions and Active together, as a table flip does, and
// restores the previous override state on cleanup. Every declared Kind is
// active now, so its observable effect is nil; what is left to pin is that the
// call is harmless on an active Kind and that its override does not leak.
func TestActivateForTest(t *testing.T) {
	t.Run("active", func(t *testing.T) {
		ActivateForTest(t, KindWordSynced)
		if !Active(KindWordSynced) || !IsSidecar("a.elrc") || len(Extensions()) != 3 {
			t.Fatalf("override did not activate the kind: active=%v isSidecar=%v exts=%v",
				Active(KindWordSynced), IsSidecar("a.elrc"), Extensions())
		}
		if override.Load() == nil {
			t.Fatal("ActivateForTest installed no override")
		}
	})
	if override.Load() != nil {
		t.Fatalf("override leaked past its test: %v", *override.Load())
	}
}
