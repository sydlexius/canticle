package lrcbackfill

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeFile_SkipsWhenBackupExists(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.lrc")
	stacked := "[00:30.00][01:05.00]C\n"
	if err := os.WriteFile(p, []byte(stacked), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .orig already exists but is NOT the original of the current .lrc. We must
	// not overwrite the .lrc without a fresh, verifiable backup -> skip untouched.
	if err := os.WriteFile(p+".orig", []byte("UNRELATED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NormalizeFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Errorf("status: want Skipped, got %v", res.Status)
	}
	if got, _ := os.ReadFile(p); string(got) != stacked {
		t.Errorf(".lrc must be left untouched, got %q", string(got))
	}
	if got, _ := os.ReadFile(p + ".orig"); string(got) != "UNRELATED\n" {
		t.Errorf("pre-existing .orig must be untouched, got %q", string(got))
	}
}

func TestRun_TalliesSkippedAndErrors(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "t.lrc")
	if err := os.WriteFile(target, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	bad := filepath.Join(dir, "bad.lrc")
	if err := os.WriteFile(bad, []byte("[00:30.00][01:05.00]C\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(bad, 0o644) }) // let TempDir cleanup remove it

	s, err := Run(Options{Roots: []string{dir}, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Normalized < 1 {
		t.Errorf("want >=1 normalized, got %d", s.Normalized)
	}
	if s.Skipped != 1 {
		t.Errorf("want 1 skipped (symlink), got %d", s.Skipped)
	}
	if s.Errors != 1 {
		t.Errorf("want 1 error (unreadable file), got %d", s.Errors)
	}
}

func TestRun_DryRunThenApply(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.lrc", "[00:30.00][01:05.00]C\n")   // stacked
	write("sub/b.lrc", "[00:10.00]x\n")         // clean
	write("c.txt", "not an lrc file\n")         // ignored (not .lrc)
	write("sub/d.lrc", "[00:05.00][00:09.00]Y") // stacked, no trailing newline

	// Dry run: reports, writes nothing.
	s, err := Run(Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if s.Scanned != 3 || s.Normalized != 2 || s.Clean != 1 {
		t.Errorf("dry-run summary: %+v (want scanned=3 normalized=2 clean=1)", s)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.lrc")); string(got) != "[00:30.00][01:05.00]C\n" {
		t.Error("dry run mutated a file")
	}

	// Apply: rewrites the stacked files and logs a JSONL record per rewrite.
	var buf bytes.Buffer
	s2, err := Run(Options{Roots: []string{dir}, Apply: true, Backup: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Normalized != 2 {
		t.Errorf("apply normalized=%d, want 2", s2.Normalized)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.lrc")); string(got) != "[00:30.00]C\n[01:05.00]C\n" {
		t.Errorf("a.lrc not expanded: %q", string(got))
	}
	if _, err := os.Stat(filepath.Join(dir, "a.lrc.orig")); err != nil {
		t.Errorf("a.lrc.orig backup missing: %v", err)
	}
	if lines := strings.Count(strings.TrimSpace(buf.String()), "\n") + 1; lines != 2 {
		t.Errorf("backup JSONL: want 2 records, got %d (%q)", lines, buf.String())
	}
}

func TestNormalizeFile_CleanFileUntouched(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "clean.lrc")
	body := "[ar:X]\n[00:10.00]a\n[00:20.00]b\n"
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NormalizeFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusClean {
		t.Errorf("status: want Clean, got %v", res.Status)
	}
	if _, err := os.Stat(p + ".orig"); !os.IsNotExist(err) {
		t.Error("clean file must not produce a .orig backup")
	}
	got, _ := os.ReadFile(p)
	if string(got) != body {
		t.Errorf("clean file mutated: %q", string(got))
	}
}

func TestNormalizeFile_IdempotentAndNeverOverwritesBackup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "song.lrc")
	orig := "[00:30.00][01:05.00]C\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	// First pass normalizes and backs up.
	if res, err := NormalizeFile(p); err != nil || res.Status != StatusNormalized {
		t.Fatalf("first pass: status=%v err=%v", res.Status, err)
	}
	afterFirst, _ := os.ReadFile(p)

	// Second pass is a no-op (already expanded), and must NOT overwrite .orig.
	res, err := NormalizeFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusClean {
		t.Errorf("second pass: want Clean, got %v", res.Status)
	}
	if got, _ := os.ReadFile(p); string(got) != string(afterFirst) {
		t.Error("second pass mutated an already-normalized file")
	}
	if backup, _ := os.ReadFile(p + ".orig"); string(backup) != orig {
		t.Errorf("backup no longer pristine: %q", string(backup))
	}
}

func TestNormalizeFile_SkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.lrc")
	if err := os.WriteFile(target, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	res, err := NormalizeFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusSkipped {
		t.Errorf("status: want Skipped, got %v", res.Status)
	}
	if _, err := os.Stat(link + ".orig"); !os.IsNotExist(err) {
		t.Error("symlink must not produce a backup")
	}
}

func TestNormalizeFile_ExpandsAndBacksUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "song.lrc")
	orig := "[ar:X]\n[00:30.00][01:05.00]Chorus\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := NormalizeFile(p)
	if err != nil {
		t.Fatalf("NormalizeFile: %v", err)
	}
	if res.Status != StatusNormalized {
		t.Fatalf("status: want Normalized, got %v", res.Status)
	}

	// The .lrc is now expanded, one cue per line.
	got, _ := os.ReadFile(p)
	want := "[ar:X]\n[00:30.00]Chorus\n[01:05.00]Chorus\n"
	if string(got) != want {
		t.Errorf("rewritten body:\n want %q\n got  %q", want, string(got))
	}

	// The pristine original is backed up verbatim.
	backup, err := os.ReadFile(p + ".orig")
	if err != nil {
		t.Fatalf("reading backup: %v", err)
	}
	if string(backup) != orig {
		t.Errorf("backup:\n want %q\n got  %q", orig, string(backup))
	}
	if res.Backup != p+".orig" {
		t.Errorf("Backup path: want %q, got %q", p+".orig", res.Backup)
	}
}
