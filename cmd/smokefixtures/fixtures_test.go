package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/scanner"
)

func TestParseDuration(t *testing.T) {
	good := map[string]int{"225": 225, "3:45": 225, "0:59": 59, "10:00": 600, " 4:05 ": 245}
	for in, want := range good {
		got, err := parseDuration(in)
		if err != nil || got != want {
			t.Errorf("parseDuration(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0", "-5", "0:00", "3:60", "3:5", "a:bc", "3:4a", "-1:30", "abc"} {
		if got, err := parseDuration(in); err == nil {
			t.Errorf("parseDuration(%q) = %d, want error", in, got)
		}
	}
}

func writeFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tracks.toml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadTracks(t *testing.T) {
	p := writeFile(t, `
[[track]]
artist = "A"
album = "B"
title = "C"
duration = "3:45"

[[track]]
artist = "D"
album = "E"
title = "F"
duration = 200
`)
	got, err := LoadTracks(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Duration != 225 || got[1].Duration != 200 || got[1].Title != "F" {
		t.Fatalf("LoadTracks = %+v", got)
	}

	bad := map[string]string{
		"unknown key":   "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"C\"\nduration=1\nlength=2\n",
		"missing title": "[[track]]\nartist=\"A\"\nalbum=\"B\"\nduration=1\n",
		"no duration":   "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"C\"\n",
		"bad m:ss":      "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"C\"\nduration=\"3:99\"\n",
		"float":         "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"C\"\nduration=3.5\n",
		"empty":         "# nothing\n",
	}
	for name, body := range bad {
		if _, err := LoadTracks(writeFile(t, body)); err == nil {
			t.Errorf("%s: LoadTracks accepted it", name)
		}
	}
}

func TestWithControl(t *testing.T) {
	in := []Track{{Artist: "A", Album: "B", Title: "C", Duration: 10}}
	got := WithControl(in)
	if len(got) != 2 || got[1] != ControlTrack {
		t.Fatalf("control not appended: %+v", got)
	}
	if len(in) != 1 {
		t.Fatal("input mutated")
	}
	// The canonical control is ALWAYS appended: an entry sharing its artist/title
	// but not its album/duration must never stand in for it (LoadTracks rejects
	// such an entry; WithControl does not second-guess the list).
	lookalike := []Track{{Artist: ControlTrack.Artist, Album: "x", Title: ControlTrack.Title, Duration: 5}}
	if got := WithControl(lookalike); len(got) != 2 || got[1] != ControlTrack {
		t.Fatalf("canonical control suppressed by a look-alike: %+v", got)
	}
}

func TestLoadTracksRejectsControlIdentity(t *testing.T) {
	body := "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"C\"\nduration=10\n" +
		"[[track]]\nartist=\" " + strings.ToUpper(ControlTrack.Artist) + "\"\nalbum=\"Other\"\ntitle=\"" + strings.ToLower(ControlTrack.Title) + "\"\nduration=99\n"
	_, err := LoadTracks(writeFile(t, body))
	if err == nil || !strings.Contains(err.Error(), "entry 2") || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("got %v, want entry 2 rejected as the reserved control identity", err)
	}
	// Same artist, different title is an ordinary track.
	ok := "[[track]]\nartist=\"" + ControlTrack.Artist + "\"\nalbum=\"B\"\ntitle=\"Something Else\"\nduration=10\n"
	if _, err := LoadTracks(writeFile(t, ok)); err != nil {
		t.Fatalf("artist-only match rejected: %v", err)
	}
}

func TestRefuseInsideRepo(t *testing.T) {
	repo := t.TempDir()
	if err := os.Mkdir(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(repo, "sub")
	if err := os.Mkdir(cwd, 0o750); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	link := filepath.Join(outside, "link-into-repo")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatal(err)
	}

	inside := map[string]string{
		"relative":           "fixtures",
		"absolute nested":    filepath.Join(repo, "a", "b"),
		"repo root":          repo,
		"dotdot back in":     filepath.Join(outside, "..", filepath.Base(repo), "x"),
		"symlink into repo":  filepath.Join(link, "fixtures"),
		"relative up to top": "../x",
	}
	for name, out := range inside {
		err := RefuseInsideRepo(out, cwd)
		if err == nil || !strings.Contains(err.Error(), "inside the repository") {
			t.Errorf("%s (%s): got %v, want refusal", name, out, err)
		}
	}
	for _, out := range []string{filepath.Join(outside, "fixtures"), outside, "../../elsewhere-" + filepath.Base(repo)} {
		if err := RefuseInsideRepo(out, cwd); err != nil {
			t.Errorf("outside %s: unexpected refusal: %v", out, err)
		}
	}
}

func TestRunVerifiesDuration(t *testing.T) {
	tracks := []Track{{Artist: "A/B", Album: "x", Title: "T?", Duration: 100}}
	encode := func(_ context.Context, _ Track, p string) error { return os.WriteFile(p, nil, 0o600) }
	for measured, wantErr := range map[int]bool{100: false, 99: false, 101: false, 98: true, 102: true, 0: true} {
		g := Generator{Encode: encode, ReadDuration: func(string) (int, error) { return measured, nil }}
		paths, err := g.Run(context.Background(), tracks, t.TempDir())
		if wantErr {
			if err == nil || !strings.Contains(err.Error(), "measured duration") {
				t.Errorf("measured %d: got %v, want duration mismatch", measured, err)
			}
			continue
		}
		if err != nil || len(paths) != 1 || filepath.Base(paths[0]) != "01 - A_B - T_.mp3" {
			t.Errorf("measured %d: paths=%v err=%v", measured, paths, err)
		}
	}
}

func TestSafeNameTrimsDots(t *testing.T) {
	for in, want := range map[string]string{"..": "", " ..x.. ": "x", "a/..": "a_", ". T .": "T"} {
		if got := safeName(in); got != want {
			t.Errorf("safeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRefuseInsideCanticleModule(t *testing.T) {
	mod := t.TempDir()
	if err := os.WriteFile(filepath.Join(mod, "go.mod"), []byte("module "+canticleModule+"\n\ngo 1.24\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/other\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir() // no .git anywhere above it: only the go.mod walk can refuse
	if err := RefuseInsideRepo(filepath.Join(mod, "not", "yet", "created"), cwd); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("out under canticle go.mod: got %v, want refusal", err)
	}
	if err := RefuseInsideRepo(filepath.Join(other, "fixtures"), cwd); err != nil {
		t.Errorf("out under a foreign module: unexpected refusal: %v", err)
	}
}

// TestRefuseInsideRepoCaseInsensitive checks that a case-only spelling of the
// repo path cannot escape the guard. Meaningful only on a case-insensitive
// filesystem (default macOS/Windows), detected at runtime.
func TestRefuseInsideRepoCaseInsensitive(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o750); err != nil {
		t.Fatal(err)
	}
	upper := filepath.Join(filepath.Dir(repo), "REPO")
	if _, err := os.Stat(upper); err != nil {
		t.Skip("filesystem is case-sensitive; case-only bypass cannot occur")
	}
	if err := RefuseInsideRepo(filepath.Join(upper, "fixtures"), repo); err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Errorf("case-variant path: got %v, want refusal", err)
	}
}

func touch(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeGen writes the given body for every fixture and reports the listed length.
func fakeGen(body string) Generator {
	return Generator{
		Encode:       func(_ context.Context, _ Track, p string) error { return os.WriteFile(p, []byte(body), 0o600) },
		ReadDuration: func(string) (int, error) { return 10, nil },
	}
}

func exists(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func TestPrepareOut(t *testing.T) {
	if err := PrepareOut(filepath.Join(t.TempDir(), "missing"), false); err != nil {
		t.Errorf("missing dir: %v", err)
	}
	if err := PrepareOut(t.TempDir(), false); err != nil {
		t.Errorf("empty dir: %v", err)
	}

	dir := t.TempDir()
	tracks := []Track{{Artist: "A", Album: "B", Title: "T", Duration: 10}, {Artist: "Q", Album: "B", Title: "W", Duration: 10}}
	if _, err := fakeGen("old").Run(context.Background(), tracks, dir); err != nil {
		t.Fatal(err)
	}
	// Sidecars serve would have written for the generated fixtures.
	owned := []string{"01 - A - T.mp3", "01 - A - T.lrc", "01 - A - T.txt", "01 - A - T.lrc.orig", "02 - Q - W.mp3", ManifestName}
	for _, n := range []string{"01 - A - T.lrc", "01 - A - T.txt", "01 - A - T.lrc.orig"} {
		touch(t, filepath.Join(dir, n))
	}
	// Files named exactly like fixtures but NOT in the manifest are the user's.
	foreign := []string{"notes.txt", "song.mp3", "03 - unrelated - track.mp3", "03 - unrelated - track.lrc", "1 - A - T.mp3"}
	for _, n := range foreign {
		touch(t, filepath.Join(dir, n))
	}
	sub := filepath.Join(dir, "04 - nested.mp3")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(sub, "05 - X - Y.mp3"))

	if err := PrepareOut(dir, false); err == nil || !strings.Contains(err.Error(), "-clean") {
		t.Fatalf("non-empty without -clean: got %v, want refusal", err)
	}
	for _, n := range owned {
		if !exists(t, filepath.Join(dir, n)) {
			t.Fatalf("refusal touched %s", n)
		}
	}
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range owned {
		if exists(t, filepath.Join(dir, n)) {
			t.Errorf("owned %s not removed", n)
		}
	}
	for _, n := range append(foreign, filepath.Join("04 - nested.mp3", "05 - X - Y.mp3")) {
		if !exists(t, filepath.Join(dir, n)) {
			t.Errorf("foreign %s removed", n)
		}
	}
}

// TestPrepareOutRemovesOrphanSidecar: a listed fixture whose .mp3 is gone still
// has its sidecar removed, or a re-run could pass on that stale .lrc.
func TestPrepareOutRemovesOrphanSidecar(t *testing.T) {
	dir := t.TempDir()
	if _, err := fakeGen("x").Run(context.Background(), []Track{{Artist: "A", Album: "B", Title: "T", Duration: 10}}, dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "01 - A - T.mp3")); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "01 - A - T.lrc")
	touch(t, orphan)
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	if exists(t, orphan) {
		t.Errorf("orphaned owned sidecar %s survived -clean", filepath.Base(orphan))
	}
}

// TestPrepareOutRefusesWithoutManifest: -clean on a non-empty dir with no
// manifest must refuse and touch nothing, even fixture-shaped names.
func TestPrepareOutRefusesWithoutManifest(t *testing.T) {
	dir := t.TempDir()
	names := []string{"01 - A - T.mp3", "01 - A - T.lrc"}
	for _, n := range names {
		touch(t, filepath.Join(dir, n))
	}
	err := PrepareOut(dir, true)
	if err == nil || !strings.Contains(err.Error(), ManifestName) {
		t.Fatalf("got %v, want a no-manifest refusal", err)
	}
	for _, n := range names {
		if !exists(t, filepath.Join(dir, n)) {
			t.Errorf("refusal removed %s", n)
		}
	}
}

// TestPrepareOutRejectsEscapingStem: a hand-edited manifest cannot direct a
// removal outside the output directory.
func TestPrepareOutRejectsEscapingStem(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "out")
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(parent, "victim.mp3")
	touch(t, victim)
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(`{"stems":["../victim"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareOut(dir, true); err == nil || !strings.Contains(err.Error(), "invalid stem") {
		t.Fatalf("got %v, want invalid-stem refusal", err)
	}
	if !exists(t, victim) {
		t.Error("escaping stem removed a file outside the output dir")
	}
}

// TestCleanCoversThreeDigitIndex: Run's %02d is a MINIMUM width, so fixture 100+
// is "100 - ...". The manifest must record it and -clean must remove it.
func TestCleanCoversThreeDigitIndex(t *testing.T) {
	dir := t.TempDir()
	tracks := make([]Track, 101)
	for i := range tracks {
		tracks[i] = Track{Artist: "A", Album: "B", Title: "T" + strconv.Itoa(i+1), Duration: 10}
	}
	if _, err := fakeGen("x").Run(context.Background(), tracks, dir); err != nil {
		t.Fatal(err)
	}
	stems, err := readManifest(dir)
	if err != nil || len(stems) != 101 {
		t.Fatalf("manifest has %d stems (err=%v), want 101", len(stems), err)
	}
	big := filepath.Join(dir, "101 - A - T101.mp3")
	touch(t, filepath.Join(dir, "100 - A - T100.lrc"))
	if !exists(t, big) {
		t.Fatalf("expected %s to be generated", filepath.Base(big))
	}
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"101 - A - T101.mp3", "100 - A - T100.mp3", "100 - A - T100.lrc"} {
		if exists(t, filepath.Join(dir, n)) {
			t.Errorf("three-digit fixture %s survived -clean", n)
		}
	}
}

// TestCleanRerunReplacesFixtures: a normal re-run with -clean replaces the old
// fixtures and leaves a fresh manifest listing the new set.
func TestCleanRerunReplacesFixtures(t *testing.T) {
	dir := t.TempDir()
	first := []Track{{Artist: "A", Album: "B", Title: "T", Duration: 10}, {Artist: "Gone", Album: "B", Title: "Old", Duration: 10}}
	if _, err := fakeGen("old").Run(context.Background(), first, dir); err != nil {
		t.Fatal(err)
	}
	touch(t, filepath.Join(dir, "01 - A - T.lrc"))
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := fakeGen("new").Run(context.Background(), first[:1], dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "01 - A - T.mp3")); err != nil || string(b) != "new" {
		t.Errorf("fixture not replaced: %q %v", b, err)
	}
	for _, n := range []string{"01 - A - T.lrc", "02 - Gone - Old.mp3"} {
		if exists(t, filepath.Join(dir, n)) {
			t.Errorf("stale %s survived the re-run", n)
		}
	}
	if stems, err := readManifest(dir); err != nil || len(stems) != 1 || stems[0] != "01 - A - T" {
		t.Errorf("manifest after re-run = %v, %v; want [01 - A - T]", stems, err)
	}
}

// TestRunManifestSurvivesFailure: a run that fails mid-way still lists the file
// it was writing, so -clean can remove the partial output.
func TestRunManifestSurvivesFailure(t *testing.T) {
	dir := t.TempDir()
	g := Generator{
		Encode:       func(_ context.Context, _ Track, p string) error { return os.WriteFile(p, []byte("partial"), 0o600) },
		ReadDuration: func(string) (int, error) { return 1, nil },
	}
	if _, err := g.Run(context.Background(), []Track{{Artist: "A", Album: "B", Title: "T", Duration: 10}}, dir); err == nil {
		t.Fatal("want a duration-mismatch error")
	}
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	if exists(t, filepath.Join(dir, "01 - A - T.mp3")) {
		t.Error("partial fixture from a failed run survived -clean")
	}
}

func TestExpandHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for in, want := range map[string]string{
		"~/my-tracks.toml": filepath.Join(home, "my-tracks.toml"),
		"~":                "~",
		"~user/x":          "~user/x",
		"/abs/~/x":         "/abs/~/x",
		"rel.toml":         "rel.toml",
	} {
		if got, err := expandHome(in); err != nil || got != want {
			t.Errorf("expandHome(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func noResolve(t *testing.T) ffmpegResolver {
	return func(context.Context, string, ffmpeg.Options) (string, error) {
		t.Error("ffmpeg resolved; run should have stopped earlier")
		return "", errors.New("unexpected resolve")
	}
}

// TestRunRefusesRepoFirst: the test's cwd is this package, inside the repo, so a
// relative OUT is inside it; the refusal must come before the (missing) track
// list is read.
func TestRunRefusesRepoFirst(t *testing.T) {
	o := options{out: "smoke-out-should-be-refused", tracks: filepath.Join(t.TempDir(), "absent.toml")}
	err := run(context.Background(), o, noResolve(t))
	if err == nil || !strings.Contains(err.Error(), "inside the repository") {
		t.Fatalf("got %v, want repo refusal before the track list is read", err)
	}
	if _, statErr := os.Stat(o.out); !os.IsNotExist(statErr) {
		t.Errorf("run created %s", o.out)
	}
}

func TestRunRefusesNonEmptyOut(t *testing.T) {
	out := t.TempDir()
	touch(t, filepath.Join(out, "01 - A - T.lrc"))
	tracks := writeFile(t, "[[track]]\nartist=\"A\"\nalbum=\"B\"\ntitle=\"T\"\nduration=10\n")
	err := run(context.Background(), options{out: out, tracks: tracks}, noResolve(t))
	if err == nil || !strings.Contains(err.Error(), "-clean") {
		t.Fatalf("got %v, want non-empty refusal", err)
	}
}

// TestRunEndToEnd drives run with a real ffmpeg: len(list)+1 files, the last
// carrying the control tags, durations verified by the production reader (a
// 67s track, so a constant reader cannot pass both it and the 180s control).
func TestRunEndToEnd(t *testing.T) {
	bin, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH; skipping end-to-end run")
	}
	resolve := func(context.Context, string, ffmpeg.Options) (string, error) { return bin, nil }
	out := t.TempDir()
	tracks := writeFile(t, "[[track]]\nartist=\"Fixture Artist\"\nalbum=\"Fixture Album\"\ntitle=\"Fixture Title\"\nduration=67\n")
	if err := run(context.Background(), options{out: out, tracks: tracks}, resolve); err != nil {
		t.Fatalf("run: %v", err)
	}
	matches, err := filepath.Glob(filepath.Join(out, "*.mp3"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 2 {
		t.Fatalf("wrote %d files, want 2 (1 track + control): %v", len(matches), matches)
	}
	sort.Strings(matches)
	facts, err := scanner.ReadAudioFacts(matches[1])
	if err != nil {
		t.Fatal(err)
	}
	if facts.Artist != ControlTrack.Artist || facts.Title != ControlTrack.Title || facts.Album != ControlTrack.Album {
		t.Fatalf("last file tags = %q/%q/%q, want the control", facts.Artist, facts.Album, facts.Title)
	}
	for i, want := range []int{67, 180} {
		if got, err := audioSeconds(matches[i]); err != nil || got < want-durationTolerance || got > want+durationTolerance {
			t.Errorf("%s: audioSeconds = %d, %v; want ~%d", matches[i], got, err, want)
		}
	}
}

// TestRunExpandsHome: run must expand a leading "~/" in -tracks (make passes it
// through literally), so the not-found error names the home-anchored path.
func TestRunExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	name := "smokefixtures-absent-" + filepath.Base(t.TempDir()) + ".toml"
	err = run(context.Background(), options{out: t.TempDir(), tracks: "~/" + name}, noResolve(t))
	if err == nil || !strings.Contains(err.Error(), filepath.Join(home, name)) {
		t.Fatalf("got %v, want the not-found error for %s", err, filepath.Join(home, name))
	}
}
