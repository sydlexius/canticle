package lrcbackfill

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeFile_BlockedWhenBackupExistsAndFileStillStacked(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.lrc")
	stacked := "[00:30.00][01:05.00]C\n"
	if err := os.WriteFile(p, []byte(stacked), 0o644); err != nil {
		t.Fatal(err)
	}
	// A .orig already exists but is NOT the original of the current .lrc. We must
	// not overwrite the .lrc without a fresh, verifiable backup -> decline. The
	// .lrc is still stacked, so this is the genuinely-blocked case (issue #487).
	if err := os.WriteFile(p+".orig", []byte("UNRELATED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := NormalizeFile(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusBlocked {
		t.Errorf("status: want Blocked, got %v", res.Status)
	}
	if got, _ := os.ReadFile(p); string(got) != stacked {
		t.Errorf(".lrc must be left untouched, got %q", string(got))
	}
	if got, _ := os.ReadFile(p + ".orig"); string(got) != "UNRELATED\n" {
		t.Errorf("pre-existing .orig must be untouched, got %q", string(got))
	}
}

// classifyBackupExists is the seam that separates the two states issue #487
// conflated. NormalizeFile's own `raw` predates the .orig check, so under a
// concurrent run it can be stale; classification must re-read the file.
func TestClassifyBackupExists(t *testing.T) {
	tests := []struct {
		name    string
		onDisk  string
		want    Status
		wantWhy string
	}{
		{
			// The benign case: a peer run expanded the .lrc and wrote the .orig
			// after we read stale stacked bytes. Nothing remains to do.
			name:    "already expanded by a peer run is clean, not blocked",
			onDisk:  "[00:30.00]C\n[01:05.00]C\n",
			want:    StatusClean,
			wantWhy: "the .orig is a legitimate backup of a finished rewrite",
		},
		{
			// The actionable case: the file really is still stacked and the
			// pre-existing .orig is what prevents its expansion.
			name:    "still stacked is blocked and needs an operator",
			onDisk:  "[00:30.00][01:05.00]C\n",
			want:    StatusBlocked,
			wantWhy: "the .orig blocks a file that still needs work",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			p := filepath.Join(dir, "s.lrc")
			if err := os.WriteFile(p, []byte(tc.onDisk), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p+".orig", []byte("PRIOR\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			res, err := classifyBackupExists(p, p+".orig")
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != tc.want {
				t.Errorf("status: want %v, got %v (%s)", tc.want, res.Status, tc.wantWhy)
			}
		})
	}
}

// The #470 AC2 blocker: a dry run must not promise a rewrite that --yes then
// declines to perform, because that count is the startup check's entire output.
func TestRun_DryRunCountMatchesApplyWhenBlocked(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "blocked.lrc")
	if err := os.WriteFile(blocked, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(blocked+".orig", []byte("PRIOR\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	free := filepath.Join(dir, "free.lrc")
	if err := os.WriteFile(free, []byte("[00:05.00][00:09.00]Y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	dry, err := Run(Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Normalized != 1 || dry.Blocked != 1 {
		t.Errorf("dry run: %+v (want normalized=1 blocked=1; the blocked file must not be promised)", dry)
	}

	apply, err := Run(Options{Roots: []string{dir}, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if apply.Normalized != dry.Normalized {
		t.Errorf("dry run promised normalized=%d but apply delivered %d", dry.Normalized, apply.Normalized)
	}
	if apply.Blocked != dry.Blocked {
		t.Errorf("dry run blocked=%d but apply blocked=%d", dry.Blocked, apply.Blocked)
	}
	// The blocked file and its pre-existing .orig must both survive untouched.
	if got, _ := os.ReadFile(blocked); string(got) != "[00:30.00][01:05.00]C\n" {
		t.Errorf("blocked .lrc mutated: %q", string(got))
	}
	if got, _ := os.ReadFile(blocked + ".orig"); string(got) != "PRIOR\n" {
		t.Errorf("pre-existing .orig mutated: %q", string(got))
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
	// 0o000 does not guarantee an unreadable file on every runner (e.g. root, or
	// some filesystems), so verify the premise deterministically before asserting
	// the error tally.
	if _, rerr := os.ReadFile(bad); rerr == nil { //nolint:gosec // test probe
		t.Skip("runner can read a 0o000 file; cannot exercise the unreadable-file error path")
	}

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

func TestNormalizeFile_ReportFailureRollsBackAndAborts(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "s.lrc")
	orig := "[00:30.00][01:05.00]C\n"
	if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("record failed")
	_, err := NormalizeFile(p, func(string) error { return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("want the report error, got %v", err)
	}
	// The .lrc must NOT have been rewritten (report ran before the rewrite).
	if got, _ := os.ReadFile(p); string(got) != orig {
		t.Errorf(".lrc mutated despite report failure: %q", string(got))
	}
	// The just-created backup must be rolled back so a re-run retries cleanly.
	if _, serr := os.Stat(p + ".orig"); !os.IsNotExist(serr) {
		t.Error(".orig backup not rolled back after report failure")
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
	res, err := NormalizeFile(p, nil)
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
	if res, err := NormalizeFile(p, nil); err != nil || res.Status != StatusNormalized {
		t.Fatalf("first pass: status=%v err=%v", res.Status, err)
	}
	afterFirst, _ := os.ReadFile(p)

	// Second pass is a no-op (already expanded), and must NOT overwrite .orig.
	res, err := NormalizeFile(p, nil)
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
	res, err := NormalizeFile(link, nil)
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

	res, err := NormalizeFile(p, nil)
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

// TestNormalizeFile_RoutesOrigGateThroughClassify pins the root-cause fix for
// #487: NormalizeFile must DELEGATE the .orig-exists decision to a fresh re-read,
// not decide from the bytes it loaded earlier.
//
// This needs a seam because the interesting case cannot be staged from outside.
// NormalizeFile only consults the .orig gate when the file was stacked at load
// time, so the benign state (a peer expanded it in between) requires the file to
// change between load() and the Lstat. Without this, a regression that classified
// from the stale bytes -- the exact pre-fix bug -- passed the entire suite:
// testing classifyBackupExists directly cannot tell a re-read from a stale read,
// because on disk they are the same bytes.
func TestNormalizeFile_RoutesOrigGateThroughClassify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(path, []byte("[00:39.26][00:47.06]Chorus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".orig", []byte("a pre-existing backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := classify
	t.Cleanup(func() { classify = prev })

	var gotPath, gotBackup string
	classify = func(p, b string) (Result, error) {
		gotPath, gotBackup = p, b
		return Result{Status: StatusClean}, nil
	}

	res, err := NormalizeFile(path, nil)
	if err != nil {
		t.Fatalf("NormalizeFile: %v", err)
	}
	if gotPath != path || gotBackup != path+".orig" {
		t.Fatalf("classify called with (%q, %q); want (%q, %q) -- NormalizeFile must delegate the .orig verdict, not decide from its stale bytes",
			gotPath, gotBackup, path, path+".orig")
	}
	if res.Status != StatusClean {
		t.Errorf("Status = %v; want the delegate's verdict to be returned verbatim", res.Status)
	}
}

// The same routing must hold for the dry-run path, or dry-run and apply would
// disagree on a .orig -- the #470 AC2 count agreement this fix exists to keep.
func TestInspect_RoutesOrigGateThroughClassify(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(path, []byte("[00:39.26][00:47.06]Chorus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".orig", []byte("a pre-existing backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := classify
	t.Cleanup(func() { classify = prev })

	called := false
	classify = func(string, string) (Result, error) {
		called = true
		return Result{Status: StatusBlocked}, nil
	}

	res, err := inspect(path)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if !called {
		t.Fatal("inspect did not route the .orig verdict through classify; a dry run would then promise a rewrite apply declines")
	}
	if res.Status != StatusBlocked {
		t.Errorf("Status = %v; want StatusBlocked from the delegate", res.Status)
	}
}

// TestClassifyBackupExists_ReReadFailureIsAnErrorNotAVerdict backs the "a zero
// blocked count alongside zero errors is a real all-clear" claim. If the .lrc
// becomes unreadable between the first read and the re-read, the classifier must
// surface an error rather than guess a verdict: reporting Clean would silently
// drop a file that may still need work, and reporting Blocked would invent an
// operator action for a file nobody can read. Either way the run must count it as
// an error, which is what keeps Blocked=0 meaningful.
func TestClassifyBackupExists_ReReadFailureIsAnErrorNotAVerdict(t *testing.T) {
	dir := t.TempDir()
	gone := filepath.Join(dir, "vanished.lrc")

	res, err := classifyBackupExists(gone, gone+".orig")
	if err == nil {
		t.Fatalf("classifyBackupExists on an unreadable file returned (%v, nil); want an error -- a verdict guessed from a file it could not read is exactly the conflation #487 removes", res.Status)
	}
}

// The dry-run path must PROPAGATE a classifier re-read failure, not swallow it,
// or dry-run and apply would disagree about what a run found. This must exercise
// the re-read path, not a missing-file initial load: inspect only reaches classify
// once the file is stacked AND a .orig exists, so a stacked file plus a .orig with
// an injected classify error is the only setup that proves inspect surfaces the
// re-read failure rather than counting the file clean.
func TestInspect_ReReadFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "song.lrc")
	if err := os.WriteFile(path, []byte("[00:39.26][00:47.06]Chorus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".orig", []byte("a pre-existing backup\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := classify
	t.Cleanup(func() { classify = prev })

	wantErr := errors.New("re-read failed")
	classify = func(string, string) (Result, error) {
		return Result{}, wantErr
	}

	if _, err := inspect(path); !errors.Is(err, wantErr) {
		t.Fatalf("inspect error = %v; want the classifier's re-read failure propagated -- a dry run must surface it, not swallow it and count the file clean", err)
	}
}
