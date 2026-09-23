package sidecar

import (
	"os"
	"path/filepath"
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

// caseSensitiveFS reports whether dir's filesystem distinguishes "a" from "A"
// in a file name. macOS (APFS, the default local dev box) and Windows are
// case-insensitive; CI (ubuntu-latest, ext4/tmpfs) and the production
// deployment target are case-sensitive. Mirrors
// internal/revalidate/revalidate_test.go's helper of the same name and
// reasoning; duplicated rather than exported because it is test-only and this
// package has no test-support package to share it from.
func caseSensitiveFS(t *testing.T, dir string) bool {
	t.Helper()
	probe := filepath.Join(dir, "casecheck.tmp")
	if err := os.WriteFile(probe, []byte("x"), 0o600); err != nil {
		t.Fatalf("write case probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASECHECK.tmp"))
	return os.IsNotExist(err)
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestVariants_ExactAndAbsent covers the case-agnostic paths: an exact-case
// file is returned under its own name; nothing on disk, or a directory that
// cannot be listed, returns nothing.
func TestVariants_ExactAndAbsent(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "song.lrc")
	if got := List(dir).Variants(p); len(got) != 0 {
		t.Fatalf("Variants on empty dir = %q, want none", got)
	}
	touch(t, p)
	if got := List(dir).Variants(p); !slices.Equal(got, []string{p}) {
		t.Fatalf("Variants = %q, want [%q]", got, p)
	}
	gone := filepath.Join(dir, "gone", "song.lrc")
	if got := List(filepath.Dir(gone)).Variants(gone); len(got) != 0 {
		t.Fatalf("Variants in missing dir = %q, want none", got)
	}
}

// TestVariants_EveryExtensionCaseOfTheSameStem (#989, I1): on a
// case-sensitive filesystem every extension-case variant of the SAME stem is
// returned, exact name first, then name order -- so a caller removing stale
// sidecars removes all of them, not just the first.
func TestVariants_EveryExtensionCaseOfTheSameStem(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the variants would alias one file")
	}
	for _, n := range []string{"song.lrc", "song.Lrc", "song.LRC"} {
		touch(t, filepath.Join(dir, n))
	}
	cand := filepath.Join(dir, "song.lrc")
	want := []string{cand, filepath.Join(dir, "song.LRC"), filepath.Join(dir, "song.Lrc")}
	if got := List(dir).Variants(cand); !slices.Equal(got, want) {
		t.Fatalf("Variants = %q, want %q", got, want)
	}
}

// TestVariants_NeverAnotherTracksName is the C1/C3 guard: only the
// EXTENSION's ASCII case may differ. A stem differing in case ("Intro" vs
// "intro" are two tracks on a case-sensitive filesystem), a Unicode simple-fold
// alias of the stem (long s, micro sign vs mu, final vs medial sigma), a
// non-ASCII fold of the extension (Kelvin sign for k), a different stem, and
// a suffixed name must never match. strings.EqualFold on the whole name
// matches most of these; this is the assertion that reverting to it breaks.
func TestVariants_NeverAnotherTracksName(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the OS itself aliases these names (as it did before #989)")
	}
	cases := []struct{ onDisk, cand string }{
		{"Intro.lrc", "intro.lrc"},
		{"\u017fong.lrc", "song.lrc"},            // LATIN SMALL LETTER LONG S folds to s
		{"\u00b5u.lrc", "\u03bcu.lrc"},           // MICRO SIGN folds to GREEK SMALL MU
		{"\u03c3\u03c2.lrc", "\u03c3\u03c3.lrc"}, // final sigma folds to sigma
		{"song.elr\u212a", "song.elrk"},          // KELVIN SIGN folds to k (extension)
		{"songs.LRC", "song.lrc"},
		{"song.lrc.bak", "song.lrc"},
	}
	for _, c := range cases {
		touch(t, filepath.Join(dir, c.onDisk))
	}
	l := List(dir)
	for _, c := range cases {
		if got := l.Variants(filepath.Join(dir, c.cand)); len(got) != 0 {
			t.Errorf("Variants(%q) with %q on disk = %q, want none", c.cand, c.onDisk, got)
		}
	}
}

// TestVariants_OnlyRegularCaseVariants (M3): a directory or a symlink under a
// case-variant name is never returned (a caller would remove it); a directory
// at the exact name is not returned either.
func TestVariants_OnlyRegularCaseVariants(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the variants would alias one file")
	}
	if err := os.Mkdir(filepath.Join(dir, "song.LRC"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "elsewhere")
	touch(t, target)
	if err := os.Symlink(target, filepath.Join(dir, "song.Lrc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "song.lrc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if got := List(dir).Variants(filepath.Join(dir, "song.lrc")); len(got) != 0 {
		t.Fatalf("Variants = %q, want none (directories and case-variant symlinks are never candidates)", got)
	}
}

// TestVariants_HardLinkedExactAndVariant: a hard-linked variant is a distinct entry.
func TestVariants_HardLinkedExactAndVariant(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.LRC cannot be a second name")
	}
	p, v := filepath.Join(dir, "song.lrc"), filepath.Join(dir, "song.LRC")
	touch(t, p)
	if err := os.Link(p, v); err != nil {
		t.Fatal(err)
	}
	if got := List(dir).Variants(p); !slices.Equal(got, []string{p, v}) {
		t.Fatalf("Variants = %q, want [%q %q]", got, p, v)
	}
}

// TestAsciiEqualFold pins the extension comparison: ASCII letters fold, any
// non-ASCII byte must match exactly (the Kelvin sign is not "k").
func TestAsciiEqualFold(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{".lrc", ".LRC", true}, {".Elrc", ".eLRC", true}, {".lrc", ".lrc", true},
		{".lrc", ".txt", false}, {".lrc", ".lrcx", false}, {".elr\u212a", ".elrk", false},
		{"", "", true}, {".1", ".1", true}, {"[", "{", false},
	} {
		if got := asciiEqualFold(c.a, c.b); got != c.want {
			t.Errorf("asciiEqualFold(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestVariants_OneFilePerDirectoryEntry runs on EITHER filesystem: with only
// "song.LRC" on disk, a lookup of "song.lrc" yields exactly one path naming
// that file -- the exact name on a case-insensitive filesystem (the listing
// entry is the same file and is not reported twice), the real uppercase name
// on a case-sensitive one. Other sidecars and other stems are never returned,
// and a listing of a different directory contributes no variants.
func TestVariants_OneFilePerDirectoryEntry(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"song.LRC", "song.txt", "song.lrcx", "other.lrc"} {
		touch(t, filepath.Join(dir, n))
	}
	cand := filepath.Join(dir, "song.lrc")
	got := List(dir).Variants(cand)
	want := filepath.Join(dir, "song.LRC")
	if caseSensitiveFS(t, dir) {
		if !slices.Equal(got, []string{want}) {
			t.Fatalf("Variants = %q, want [%q]", got, want)
		}
	} else if !slices.Equal(got, []string{cand}) {
		t.Fatalf("Variants = %q, want [%q] (listing entry is the same file)", got, cand)
	}
	if got := List(t.TempDir()).Variants(filepath.Join(dir, "song.txt")); !slices.Equal(got, []string{filepath.Join(dir, "song.txt")}) {
		t.Fatalf("Variants with a foreign listing = %q, want only the exact name", got)
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
