package lrcbackfill

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// lockedBuffer is a concurrency-safe io.Writer for capturing slog output in
// tests; WalkDir's callback runs on one goroutine per Run call here, but a
// plain bytes.Buffer is still not safe against a stray concurrent write from
// an unrelated goroutine in the same test binary, so this mirrors the
// commands package's own lockedBuffer helper.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

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
//
// This also directly pins the PRODUCER side of ViaBackupCheck (issue #470
// round 4, Important 3): Run()'s status switch (TestRun_PeerExpandedLogSiteRespectsQuiet)
// substitutes the classify seam with a fake that hardcodes the flag, so it
// only pins Run's CONSUMPTION of ViaBackupCheck, never classifyBackupExists
// actually SETTING it. A regression that stopped setting the flag on the
// not-stacked branch would pass that seam test unchanged (the fake always
// returns true) while silently turning off the CLI's peer-expanded Debug log
// -- this table is what would catch that.
func TestClassifyBackupExists(t *testing.T) {
	tests := []struct {
		name          string
		onDisk        string
		want          Status
		wantViaBackup bool
		wantWhy       string
	}{
		{
			// The benign case: a peer run expanded the .lrc and wrote the .orig
			// after we read stale stacked bytes. Nothing remains to do.
			name:          "already expanded by a peer run is clean, not blocked",
			onDisk:        "[00:30.00]C\n[01:05.00]C\n",
			want:          StatusClean,
			wantViaBackup: true,
			wantWhy:       "the .orig is a legitimate backup of a finished rewrite",
		},
		{
			// The actionable case: the file really is still stacked and the
			// pre-existing .orig is what prevents its expansion.
			name:          "still stacked is blocked and needs an operator",
			onDisk:        "[00:30.00][01:05.00]C\n",
			want:          StatusBlocked,
			wantViaBackup: false,
			wantWhy:       "the .orig blocks a file that still needs work",
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
			if res.ViaBackupCheck != tc.wantViaBackup {
				t.Errorf("ViaBackupCheck: want %v, got %v (%s)", tc.wantViaBackup, res.ViaBackupCheck, tc.wantWhy)
			}
		})
	}

	// The re-read-failure branch is not part of the table above (it returns an
	// error, not a Result), but it shares the same producer and must not set
	// ViaBackupCheck on a zero Result either.
	t.Run("re-read failure returns a zero Result with ViaBackupCheck false", func(t *testing.T) {
		dir := t.TempDir()
		gone := filepath.Join(dir, "gone.lrc")
		res, err := classifyBackupExists(gone, gone+".orig")
		if err == nil {
			t.Fatal("want an error for an unreadable path")
		}
		if res.ViaBackupCheck {
			t.Errorf("ViaBackupCheck: want false on an error Result, got true")
		}
	})
}

// The #470 AC2 blocker: a dry run must not promise a rewrite that --yes then
// declines to perform, because that count is a caller's entire output when it
// has no path detail to fall back on.
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

	dry, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if dry.Normalized != 1 || dry.Blocked != 1 {
		t.Errorf("dry run: %+v (want normalized=1 blocked=1; the blocked file must not be promised)", dry)
	}

	apply, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: true})
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
	if _, rerr := os.ReadFile(bad); rerr == nil { //nolint:gosec // reason: test probe
		t.Skip("runner can read a 0o000 file; cannot exercise the unreadable-file error path")
	}

	s, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: true})
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
	s, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false})
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
	s2, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: true, Backup: &buf})
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

// A .lrc larger than the size guard is refused rather than read wholesale --
// this pass may run in-process inside a longer-lived server, so an
// implausibly large file (corrupt, wrongly named, or hostile) must not risk
// taking that process down via an unbounded os.ReadFile.
func TestNormalizeFile_OversizeFileIsAnError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.lrc")
	f, err := os.Create(p) //nolint:gosec // reason: test-controlled path
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxLRCFileSize + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := NormalizeFile(p, nil); err == nil {
		t.Fatal("want an error for a .lrc exceeding the size guard, got nil")
	}
}

// TestLoad_RejectsFileLargerThanGuard pins that load() rejects a .lrc whose
// on-disk content exceeds maxLRCFileSize, exercised through the bounded-read
// path (io.LimitReader over an open handle) that replaced the old
// os.Lstat-then-os.ReadFile shape (issue #923 review, Major finding): stat and
// read each resolve path independently, so a file that grows or is replaced
// between the two is a TOCTOU (time-of-check to time-of-use) race a
// prior-check guard cannot close -- reading through a bound on one
// already-open handle closes it by construction.
//
// This test is deliberately NOT a reproduction of the race itself: that needs
// a genuine concurrent mutation of the file between the size check and the
// read, which is inherently nondeterministic, and faking it with a sleep would
// be exactly the kind of flake the TestRun_ContextCanceledMidWalkStopsShort
// fix in this same round exists to remove. What this DOES pin, deterministically:
// a file whose real (non-sparse) on-disk content exceeds the guard is refused
// when read through load(), via the bounded-read mechanism specifically --
// confirmed by the mutation pass for this change, which deletes the
// io.LimitReader/length-check pair entirely (not a mere equivalent
// substitution) and observes this test fail.
func TestLoad_RejectsFileLargerThanGuard(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "huge.lrc")
	content := bytes.Repeat([]byte("x"), maxLRCFileSize+4096)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, skip, err := load(p)
	if err == nil {
		t.Fatal("want an error for a file exceeding the size guard, got nil")
	}
	if skip {
		t.Error("an oversize file must not be reported as a skip (symlink) case")
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

// Run(ctx, ...) with an already-canceled context aborts the walk before
// processing any file and returns context.Canceled, rather than completing the
// walk and reporting a count. This matters for any caller embedded inside a
// longer-lived process's shutdown path: it must not have to wait out a
// ~35k-file NAS walk. WalkDir's early-termination contract makes this
// deterministic: a non-nil, non-SkipDir/SkipAll callback error stops the walk
// immediately, so zero further entries are ever visited once ctx.Err() is
// non-nil -- this is not a race with the walk racing to finish first.
func TestRun_CanceledContextAbortsBeforeAnyFile(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 20; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%02d.lrc", i))
		if err := os.WriteFile(p, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s, err := Run(ctx, Options{Roots: []string{dir}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with a pre-canceled ctx returned err=%v; want context.Canceled", err)
	}
	if s.Scanned != 0 {
		t.Errorf("Scanned=%d after a pre-canceled ctx; want 0 -- the walk must abort before touching any file", s.Scanned)
	}
}

// countdownContext is a context.Context whose Err() reports context.Canceled
// starting on its (after+1)th call, with no timing involved. Run's WalkDir
// callback calls ctx.Err() exactly once per directory entry, from a single
// goroutine (see the lockedBuffer comment above), so a plain int counter needs
// no synchronization here. Embedding context.Background() supplies Deadline,
// Done, and Value; only Err() is consulted by the code under test.
type countdownContext struct {
	context.Context
	calls int
	after int
}

func (c *countdownContext) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

// A context canceled mid-walk stops the walk short of the full tree rather than
// running to completion. This pins the "stops mid-tree" half of the design's
// cancellation requirement, distinct from the pre-canceled case above.
//
// This must be deterministic, not timing-based: an earlier version canceled
// from a goroutine after a 1ms sleep, racing the walk itself -- if Run finished
// the whole 1000-file tree before the timer fired, err was nil and
// Scanned == total, so the test failed despite cancellation behaving correctly
// (issue #923 review). countdownContext flips to context.Canceled after a
// fixed number of Err() calls instead, so the walk always stops at the same
// point on every run, on every machine.
func TestRun_ContextCanceledMidWalkStopsShort(t *testing.T) {
	dir := t.TempDir()
	const total = 1000
	for i := 0; i < total; i++ {
		p := filepath.Join(dir, fmt.Sprintf("f%04d.lrc", i))
		if err := os.WriteFile(p, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// after=5 means the walk observes a handful of directory entries as
	// not-yet-canceled before Err() starts returning context.Canceled -- comfortably
	// mid-walk, and always the same handful, so Scanned < total on every run.
	ctx := &countdownContext{Context: context.Background(), after: 5}

	s, err := Run(ctx, Options{Roots: []string{dir}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run with a mid-walk-canceled ctx returned err=%v; want context.Canceled", err)
	}
	if s.Scanned >= total {
		t.Errorf("Scanned=%d; want fewer than the full %d-file tree -- cancellation must stop the walk short of completion", s.Scanned, total)
	}
}

// Visited must count every non-directory entry the walk reaches, not just
// .lrc files -- that is what lets a caller (runLRCStackedCheck) tell "this
// root is empty/unmounted" apart from "this root legitimately has no .lrc
// sidecars yet" (issue #470 round 2, Critical 1).
func TestRun_VisitedCountsEveryEntryNotJustLRC(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("song.mp3", "not an lrc file")
	write("cover.jpg", "not an lrc file either")
	write("stacked.lrc", "[00:30.00][01:05.00]C\n")

	s, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if s.Visited != 3 {
		t.Errorf("Visited=%d, want 3 (song.mp3, cover.jpg, stacked.lrc)", s.Visited)
	}
	if s.Scanned != 1 {
		t.Errorf("Scanned=%d, want 1 (only the .lrc)", s.Scanned)
	}
}

// An empty root (nothing under it at all -- the shape of an unmounted
// bind-mount at container start) must report Visited==0, distinct from a
// mounted root holding non-.lrc files, which must report Visited>0 even
// though Scanned stays 0.
func TestRun_VisitedZeroOnEmptyRootDistinctFromScannedZero(t *testing.T) {
	empty := t.TempDir()
	sEmpty, err := Run(context.Background(), Options{Roots: []string{empty}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if sEmpty.Visited != 0 {
		t.Errorf("empty root: Visited=%d, want 0", sEmpty.Visited)
	}
	if sEmpty.Scanned != 0 {
		t.Errorf("empty root: Scanned=%d, want 0", sEmpty.Scanned)
	}

	populated := t.TempDir()
	if err := os.WriteFile(filepath.Join(populated, "track.mp3"), []byte("audio, no lyrics yet"), 0o644); err != nil {
		t.Fatal(err)
	}
	sPopulated, err := Run(context.Background(), Options{Roots: []string{populated}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if sPopulated.Visited == 0 {
		t.Error("mounted root with audio but no .lrc: Visited=0, want >0 -- this must NOT look like an unmounted root")
	}
	if sPopulated.Scanned != 0 {
		t.Errorf("mounted root with audio but no .lrc: Scanned=%d, want 0", sPopulated.Scanned)
	}
}

// A root holding only stray dotfiles (a mount-checker's .mountcheck sentinel,
// Syncthing's .stfolder, macOS's .DS_Store) is exactly the shape that defeated
// the earlier Visited-only mount heuristic (issue #470 round 4, Critical 2):
// Visited is nonzero, but none of these entries is real library content, so
// MediaEntries must stay zero. A real audio or .lrc file in the same root must
// move MediaEntries.
func TestRun_MediaEntriesIgnoresSentinelDotfiles(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".mountcheck", "mount probe sentinel")
	write(".DS_Store", "macOS metadata")
	write(".stfolder", "syncthing marker")

	s, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if s.Visited != 3 {
		t.Errorf("Visited=%d, want 3 (the three sentinel dotfiles)", s.Visited)
	}
	if s.MediaEntries != 0 {
		t.Errorf("MediaEntries=%d, want 0 -- sentinel dotfiles must not read as library content", s.MediaEntries)
	}

	write("track.flac", "real audio content")
	s2, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false})
	if err != nil {
		t.Fatal(err)
	}
	if s2.MediaEntries != 1 {
		t.Errorf("MediaEntries=%d, want 1 once a real audio file is present", s2.MediaEntries)
	}
}

// The fourth path-bearing log site (issue #470 round 2, Important 3):
// classifyBackupExists' "already expanded by a peer run" case must stay
// silent under Quiet, and must log (at Debug) when Quiet is false, naming
// both the file and its backup -- exactly like the other three path-bearing
// sites in this package.
//
// This is the case the reviewer reproduced only via a genuine race (the file
// must change between inspect's load() and the .orig Lstat check, hit on
// attempt 8 of 400) -- not reproducible deterministically through a plain
// Run() call. Substituting the classify seam (as
// TestInspect_RoutesOrigGateThroughClassify already does for a different
// assertion) reaches the same Run()-side logging branch deterministically,
// without the race: it is Run()'s status switch, not classifyBackupExists
// itself, that decides whether to log, so driving that switch via the seam
// exercises the real code path this fix touches.
//
// This test pins Run's CONSUMPTION of ViaBackupCheck only -- the fake classify
// above hardcodes the flag rather than deriving it. The PRODUCER side
// (classifyBackupExists itself actually setting ViaBackupCheck on its
// not-stacked branch) is pinned separately by TestClassifyBackupExists (issue
// #470 round 4, Important 3): without that table, a regression that stopped
// setting the flag in classifyBackupExists would pass this test unchanged.
func TestRun_PeerExpandedLogSiteRespectsQuiet(t *testing.T) {
	prev := classify
	t.Cleanup(func() { classify = prev })
	classify = func(path, backupPath string) (Result, error) {
		return Result{Status: StatusClean, Backup: backupPath, ViaBackupCheck: true}, nil
	}

	run := func(quiet bool) string {
		dir := t.TempDir()
		p := filepath.Join(dir, "song.lrc")
		if err := os.WriteFile(p, []byte("[00:30.00][01:05.00]C\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p+".orig", []byte("PRIOR\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		var buf lockedBuffer
		prevLogger := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		t.Cleanup(func() { slog.SetDefault(prevLogger) })

		if _, err := Run(context.Background(), Options{Roots: []string{dir}, Apply: false, Quiet: quiet}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}

	if got := run(true); strings.Contains(got, "song.lrc") {
		t.Errorf("Quiet=true leaked the path via the peer-expanded log site: %s", got)
	}
	if got := run(false); !strings.Contains(got, "song.lrc") || !strings.Contains(got, "already expanded") {
		t.Errorf("Quiet=false: want the peer-expanded Debug log naming the path; got: %s", got)
	}
}
