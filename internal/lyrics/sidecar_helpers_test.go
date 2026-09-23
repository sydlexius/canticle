package lyrics

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/sydlexius/canticle/internal/sidecar"
)

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
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil { //nolint:gosec // reason: test fixture, not a security-relevant mode
		t.Fatalf("write case probe: %v", err)
	}
	_, err := os.Stat(filepath.Join(dir, "CASECHECK.tmp"))
	return os.IsNotExist(err)
}

// TestOppositeSidecar_Characterization pins the pairing oppositeSidecar
// decides on a NAME ALONE (no disk involved): which extension is the opposite
// of which, and that a third extension or a non-sidecar name maps to "". This
// is the classification half, unaffected by #989 -- KindOf already read case
// insensitively before and after.
func TestOppositeSidecar_Characterization(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"lrc pairs to txt", "/music/song.lrc", "/music/song.txt"},
		{"txt pairs to lrc", "/music/song.txt", "/music/song.lrc"},
		{"relative lrc", "song.lrc", "song.txt"},
		{"dots in directory", "/music/Artist v1.2/song.lrc", "/music/Artist v1.2/song.txt"},
		{"dots in file name", "/music/song.live.2024.txt", "/music/song.live.2024.lrc"},
		// NOT a pairing: the write path has no rules for a third extension yet.
		{"elrc has no opposite", "/music/song.elrc", ""},
		{"audio has no opposite", "/music/song.mp3", ""},
		{"no extension has no opposite", "/music/song", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := oppositeSidecar(tc.in); got != tc.want {
				t.Fatalf("oppositeSidecar(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestOppositeSidecar_UppercaseInput_NothingOnDisk: an uppercase fp is
// paired case-insensitively (KindOf) with the lowercase-extension name of its
// opposite; oppositeSidecar is name-only and never consults the disk.
func TestOppositeSidecar_UppercaseInput_NothingOnDisk(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "song.LRC")
	want := filepath.Join(dir, "song.txt")
	if got := oppositeSidecar(fp); got != want {
		t.Fatalf("oppositeSidecar(%q) = %q, want %q", fp, got, want)
	}
}

// settledAt runs settledSidecar against a fresh listing of p's directory, as
// WriteLRC does.
func settledAt(p string) (string, bool) {
	return settledSidecar(p, sidecar.List(filepath.Dir(p)))
}

// TestStaleSidecars_CaseVariants is the #989 pairing on a case-sensitive
// filesystem (skipped elsewhere: there the variants alias one file). Every
// extension-case variant of the SAME stem is stale and returned under its real
// name (I1: not only the first); a different-case stem is another track's
// sidecar and is never returned (C1).
func TestStaleSidecars_CaseVariants(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; song.lrc and song.LRC would alias the same file")
	}
	for _, n := range []string{"song.LRC", "song.Lrc", "Song.lrc", "SONG.LRC"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	fp := filepath.Join(dir, "song.txt")
	got := staleSidecars(fp, sidecar.List(dir))
	want := []string{filepath.Join(dir, "song.LRC"), filepath.Join(dir, "song.Lrc")}
	if !slices.Equal(got, want) {
		t.Fatalf("staleSidecars(%q) = %q, want %q", fp, got, want)
	}
}

// TestSettledSidecar_CaseVariants (M4): an extension-case variant of the same
// stem is settled; a different-case stem is another track's and is not.
func TestSettledSidecar_CaseVariants(t *testing.T) {
	dir := t.TempDir()
	if !caseSensitiveFS(t, dir) {
		t.Skip("filesystem is case-insensitive; the variants would alias the same file")
	}
	other := filepath.Join(dir, "Intro.lrc")
	if err := os.WriteFile(other, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := settledAt(filepath.Join(dir, "intro.lrc")); ok {
		t.Fatalf("settledSidecar(intro) = %q: another track's Intro.lrc read as settled", got)
	}
	upper := filepath.Join(dir, "song.LRC")
	if err := os.WriteFile(upper, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, ok := settledAt(filepath.Join(dir, "song.lrc")); !ok || got != upper {
		t.Fatalf("settledSidecar(song) = (%q, %v), want (%q, true)", got, ok, upper)
	}
}

// TestSettledSidecar_Characterization pins the probe ORDER (.txt before .lrc)
// and the fail-closed stat behavior, both of which the #986 refactor had to
// preserve exactly.
func TestSettledSidecar_Characterization(t *testing.T) {
	write := func(t *testing.T, p string) {
		t.Helper()
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { //nolint:gosec // reason: test fixture, not a security-relevant mode
			t.Fatalf("writing fixture %s: %v", p, err)
		}
	}

	t.Run("neither present", func(t *testing.T) {
		dir := t.TempDir()
		got, ok := settledAt(filepath.Join(dir, "song.lrc"))
		if ok || got != "" {
			t.Fatalf("settledSidecar = (%q, %v), want (\"\", false)", got, ok)
		}
	})

	t.Run("only lrc present", func(t *testing.T) {
		dir := t.TempDir()
		lrc := filepath.Join(dir, "song.lrc")
		write(t, lrc)
		got, ok := settledAt(filepath.Join(dir, "song.txt"))
		if !ok || got != lrc {
			t.Fatalf("settledSidecar = (%q, %v), want (%q, true)", got, ok, lrc)
		}
	})

	t.Run("only txt present", func(t *testing.T) {
		dir := t.TempDir()
		txt := filepath.Join(dir, "song.txt")
		write(t, txt)
		got, ok := settledAt(filepath.Join(dir, "song.lrc"))
		if !ok || got != txt {
			t.Fatalf("settledSidecar = (%q, %v), want (%q, true)", got, ok, txt)
		}
	})

	// ORDER: with both on disk the .txt is reported, because the probe checks
	// unsynced first. This is the assertion a reordered loop breaks.
	t.Run("both present reports txt first", func(t *testing.T) {
		dir := t.TempDir()
		txt := filepath.Join(dir, "song.txt")
		write(t, txt)
		write(t, filepath.Join(dir, "song.lrc"))
		got, ok := settledAt(filepath.Join(dir, "song.lrc"))
		if !ok || got != txt {
			t.Fatalf("settledSidecar = (%q, %v), want (%q, true) -- probe order changed", got, ok, txt)
		}
	})

	// FAIL-CLOSED: a stat error that is not not-exist reads as PRESENT. An
	// unsearchable parent directory produces EACCES on both candidates, so the
	// first probed extension (.txt) is reported as occupied.
	t.Run("stat error other than not-exist reads as present", func(t *testing.T) {
		// Both skips are load-bearing, and for the same underlying reason: this
		// case needs a stat to fail with something OTHER than not-exist, and it
		// manufactures that with directory permissions. Where permissions do not
		// deny a stat, the probe succeeds-as-absent and the assertion below would
		// fail on a guard that is actually correct.
		//
		// Windows is not hypothetical here: os.Geteuid() returns -1 there, so the
		// root check alone does not skip, and os.Chmod(dir, 0o000) does not make a
		// directory unsearchable -- both candidates read as absent. CI is
		// ubuntu-only so it would never catch it, but GoReleaser ships
		// windows/amd64, so `go test ./...` on a Windows checkout would fail.
		// Mirrors internal/secrets/key_test.go, which pairs these two skips on the
		// same reasoning.
		if runtime.GOOS == "windows" {
			t.Skip("permission-based stat errors unreliable on Windows")
		}
		if os.Geteuid() == 0 {
			t.Skip("running as root: directory permissions do not deny stat")
		}
		dir := t.TempDir()
		sub := filepath.Join(dir, "locked")
		if err := os.Mkdir(sub, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.Chmod(sub, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(sub, 0o755) })

		target := filepath.Join(sub, "song.lrc")
		got, ok := settledAt(target)
		if !ok {
			t.Fatal("settledSidecar reported no settled sidecar on an unreadable path; the guard must fail CLOSED")
		}
		if want := filepath.Join(sub, "song.txt"); got != want {
			t.Fatalf("settledSidecar = %q, want %q (first probed extension)", got, want)
		}
	})

	t.Run("no extension probes the whole name as the stem", func(t *testing.T) {
		dir := t.TempDir()
		txt := filepath.Join(dir, "song.txt")
		write(t, txt)
		got, ok := settledAt(filepath.Join(dir, "song"))
		if !ok || got != txt {
			t.Fatalf("settledSidecar = (%q, %v), want (%q, true)", got, ok, txt)
		}
	})
}
