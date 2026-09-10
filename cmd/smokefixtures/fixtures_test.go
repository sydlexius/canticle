package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
	// Already present (case-insensitively): not duplicated.
	listed := []Track{{Artist: strings.ToUpper(ControlTrack.Artist), Album: "x", Title: ControlTrack.Title, Duration: 5}}
	if got := WithControl(listed); len(got) != 1 {
		t.Fatalf("control duplicated: %+v", got)
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

func TestPrepareOut(t *testing.T) {
	if err := PrepareOut(filepath.Join(t.TempDir(), "missing"), false); err != nil {
		t.Errorf("missing dir: %v", err)
	}
	if err := PrepareOut(t.TempDir(), false); err != nil {
		t.Errorf("empty dir: %v", err)
	}

	dir := t.TempDir()
	owned := []string{"01 - A - T.mp3", "01 - A - T.lrc", "01 - A - T.txt", "01 - A - T.lrc.orig", "02 - Q - W.mp3"}
	foreign := []string{"notes.txt", "song.mp3", "03 - orphan.lrc", "1 - A - T.mp3"}
	for _, n := range append(append([]string{}, owned...), foreign...) {
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
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Fatalf("refusal touched %s: %v", n, err)
		}
	}
	if err := PrepareOut(dir, true); err != nil {
		t.Fatal(err)
	}
	for _, n := range owned {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("owned %s not removed (err=%v)", n, err)
		}
	}
	for _, n := range append(foreign, filepath.Join("04 - nested.mp3", "05 - X - Y.mp3")) {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("foreign %s removed: %v", n, err)
		}
	}

	// With -clean, generation then replaces the owned fixtures.
	encode := func(_ context.Context, _ Track, p string) error { return os.WriteFile(p, []byte("new"), 0o600) }
	g := Generator{Encode: encode, ReadDuration: func(string) (int, error) { return 10, nil }}
	if _, err := g.Run(context.Background(), []Track{{Artist: "A", Album: "B", Title: "T", Duration: 10}}, dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "01 - A - T.mp3")); err != nil || string(b) != "new" {
		t.Errorf("fixture not replaced: %q %v", b, err)
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
