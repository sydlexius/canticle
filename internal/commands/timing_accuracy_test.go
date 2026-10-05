package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
)

const taSentinel = "ZQXSENT"

type taFetcher struct {
	songs map[string]models.Song // keyed by track title
	err   error
}

func (f taFetcher) FindLyrics(_ context.Context, tr models.Track) (models.Song, error) {
	if f.err != nil {
		return models.Song{}, f.err
	}
	s, ok := f.songs[tr.TrackName]
	if !ok {
		return models.Song{}, errors.New("no match")
	}
	return s, nil
}

func taSong(upstream string, shiftMS int, starts []int, texts []string) models.Song {
	var s models.Song
	s.Upstream = upstream
	for i, t := range texts {
		s.Subtitles.Lines = append(s.Subtitles.Lines, models.Lines{Text: t, Time: models.MsToTime(starts[i] + shiftMS)})
	}
	return s
}

var taTexts = []string{taSentinel + " line one", taSentinel + " line two", taSentinel + " line three"}

// taRefDir writes a two-track reference set under dir.
func taRefDir(t *testing.T, dir string, manifest string) string {
	t.Helper()
	ref := filepath.Join(dir, "ref")
	if err := os.MkdirAll(ref, 0o755); err != nil {
		t.Fatal(err)
	}
	lrc := "[ti:" + taSentinel + "]\n[00:01.00]" + taTexts[0] + "\n[00:05.00]" + taTexts[1] + "\n[00:09.00]" + taTexts[2] + "\n[00:12.00]\u266a\n"
	for _, n := range []string{"a.lrc", "b.lrc"} {
		if err := os.WriteFile(filepath.Join(ref, n), []byte(lrc), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ref, "manifest.toml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return ref
}

func taManifest(extra string) string {
	return fmt.Sprintf(`
[[track]]
id = "%[1]s-id-a"
file = "a.lrc"
artist = "%[1]s artist"
title = "%[1]s title a"
duration_seconds = 20
line_residual_ms = 40
%[2]s
[[track]]
id = "%[1]s-id-b"
file = "b.lrc"
artist = "%[1]s artist"
title = "%[1]s title b"
duration_seconds = 20
line_residual_ms = 60
`, taSentinel, extra)
}

func taSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snap := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		snap[p] = fmt.Sprintf("%d/%d", info.ModTime().UnixNano(), info.Size())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

type taLogSink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *taLogSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func TestRunTimingAccuracy_AggregateOnlyAndWritesNothing(t *testing.T) {
	root := t.TempDir()
	ref := taRefDir(t, root, taManifest(""))
	starts := []int{1000, 5000, 9000}
	both := map[string]models.Song{
		taSentinel + " title a": taSong("alphalicensor", 500, append(starts[:3:3], 12000), append(taTexts[:3:3], "\u266a")),
		taSentinel + " title b": taSong("", 500, starts, taTexts),
	}
	exact := map[string]models.Song{
		taSentinel + " title a": taSong("", 0, starts, taTexts),
		taSentinel + " title b": taSong("", 0, starts, taTexts),
	}
	lanes := []providers.LyricsProvider{
		providers.New("innertube", taFetcher{songs: both}),
		providers.New("petitlyrics", taFetcher{songs: exact}),
		providers.New("musixmatch", taFetcher{err: errors.New("GET https://x/?q=" + taSentinel + " failed")}),
	}

	sink := &taLogSink{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(sink, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	before := taSnapshot(t, root)
	var out bytes.Buffer
	code := runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, lanes, []string{"skippedlane"})
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	got := out.String()

	if strings.Contains(got, taSentinel) || strings.Contains(got, root) {
		t.Errorf("stdout leaked identity:\n%s", got)
	}
	sink.mu.Lock()
	logs := sink.buf.String()
	sink.mu.Unlock()
	if strings.Contains(logs, taSentinel) {
		t.Errorf("slog leaked identity:\n%s", logs)
	}
	for _, want := range []string{
		"2 reference tracks",
		"decorative cues dropped: reference=2 provider=1", // one note cue per reference file; one served
		"skipped lanes: skippedlane",
		"innertube: tracks=2 cues(ref=6 matched=6",
		"line-MAE=500ms within-300ms=0.0%",
		"innertube/alphalicensor: tracks=1",
		"innertube/(upstream not named): tracks=1",
		"petitlyrics: tracks=2",
		"line-MAE=0ms within-300ms=100.0%",
		"ref-residual(mean=50ms max=60ms)",
		"musixmatch: tracks=0",
		"line-MAE=n/a",
		"musixmatch: not_found=0 failed=2",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "petitlyrics/") {
		t.Errorf("lane with no named upstream got a per-upstream row:\n%s", got)
	}

	if after := taSnapshot(t, root); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("filesystem changed during the run:\nbefore=%v\nafter=%v", before, after)
	}
	_ = filepath.WalkDir(root, func(p string, _ fs.DirEntry, _ error) error {
		if strings.HasSuffix(p, ".db") || strings.Contains(p, "-wal") {
			t.Errorf("database file created: %s", filepath.Base(p))
		}
		return nil
	})
}

const (
	taMsgManifest = "manifest.toml could not be read or parsed"
	taMsgRefFile  = "a reference file could not be read"
)

func taRejectOne(t *testing.T, ref string) (int, string) {
	t.Helper()
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, nil, nil) }()
	select {
	case code := <-done:
		return code, out.String()
	case <-time.After(3 * time.Second):
		t.Fatal("run hung")
		return 0, ""
	}
}

func taMust(t *testing.T, err error) {
	if err != nil {
		t.Skip("setup unavailable: " + err.Error())
	}
}

func TestLoadRefSet_Rejections(t *testing.T) {
	one := func(file string) string {
		return "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"" + file + "\"\nline_residual_ms = 1\n"
	}
	cases := map[string]struct {
		manifest string
		mutate   func(t *testing.T, ref string)
		want     string
	}{
		"missing residual": {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\n", want: "line_residual_ms is required"},
		"escaping file with a real target": {manifest: one("../x.lrc"), want: "file must be a relative path", mutate: func(t *testing.T, ref string) {
			taMust(t, os.WriteFile(filepath.Join(filepath.Dir(ref), "x.lrc"), []byte("[00:01.00]a\n"), 0o600))
		}},
		"blank title":                        {manifest: "[[track]]\nid = \"x\"\ntitle = \" \"\nfile = \"a.lrc\"\nline_residual_ms = 1\n", want: "title is required"},
		"negative duration":                  {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 1\nduration_seconds = -1\n", want: "duration_seconds must not be negative"},
		"negative word residual":             {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 1\nword_residual_ms = -5\n", want: "word_residual_ms must not be negative"},
		"unknown key is named, value is not": {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 1\nartst = \"" + taSentinel + "\"\n", want: "unknown key: track.artst"},
		"unknown quoted key is not echoed":   {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 1\n\"" + taSentinel + " Song Title\" = 1\n", want: "manifest.toml has an unknown key\n"},
		"line residual above cap":            {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 60001\n", want: "line_residual_ms must not exceed 60000"},
		"word residual above cap":            {manifest: "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"a.lrc\"\nline_residual_ms = 1\nword_residual_ms = 60001\n", want: "word_residual_ms must not exceed 60000"},
		"duplicate id":                       {manifest: one("a.lrc") + one("b.lrc"), want: "duplicate id"},
		"git present": {manifest: taManifest(""), want: ".git entry", mutate: func(t *testing.T, ref string) {
			taMust(t, os.Mkdir(filepath.Join(ref, ".git"), 0o755))
		}},
		"malformed manifest with sentinel": {manifest: "[[track]\nid = \"" + taSentinel + "\"\n", want: taMsgManifest},
		"symlinked ref file": {manifest: one("l.lrc"), want: taMsgRefFile, mutate: func(t *testing.T, ref string) {
			taMust(t, os.Symlink("a.lrc", filepath.Join(ref, "l.lrc")))
		}},
		"symlinked manifest": {manifest: one("a.lrc"), want: taMsgManifest, mutate: func(t *testing.T, ref string) {
			outside := filepath.Join(filepath.Dir(ref), "m.toml")
			taMust(t, os.Rename(filepath.Join(ref, "manifest.toml"), outside))
			taMust(t, os.Symlink(outside, filepath.Join(ref, "manifest.toml")))
		}},
		"missing directory": {manifest: one("a.lrc"), want: "timing-accuracy: reference directory could not be opened\n", mutate: func(t *testing.T, ref string) {
			taMust(t, os.RemoveAll(ref))
		}},
		"directory as file": {manifest: one("sub"), want: taMsgRefFile, mutate: func(t *testing.T, ref string) {
			taMust(t, os.Mkdir(filepath.Join(ref, "sub"), 0o755))
		}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ref := taRefDir(t, t.TempDir(), tc.manifest)
			if tc.mutate != nil {
				tc.mutate(t, ref)
			}
			code, got := taRejectOne(t, ref)
			if code != 1 || !strings.Contains(got, tc.want) {
				t.Errorf("exit %d; want 1 with %q:\n%s", code, tc.want, got)
			}
			if strings.Contains(got, ref) || strings.Contains(got, taSentinel) {
				t.Errorf("error output leaked identity: %s", got)
			}
		})
	}
}

type taSeqFetcher struct {
	song  models.Song
	err   error
	calls int
	hook  func()
}

func (f *taSeqFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	if f.calls == 1 && f.hook == nil {
		return f.song, nil
	}
	return models.Song{}, f.err
}

// The petitlyrics outage sentinels wrap ErrNotFound: each must stop the lane as
// a failure, never count as a clean miss.
func TestRunTimingAccuracy_StopsOnThrottleOrOutage(t *testing.T) {
	m := taManifest("[[track]]\nid = \"c\"\ntitle = \"c\"\nfile = \"a.lrc\"\nline_residual_ms = 1\n")
	for _, tc := range []struct {
		err error
		why string
	}{
		{musixmatch.ErrRateLimited, "throttled"},
		{petitlyrics.ErrProviderUnavailable, "unavailable"},
		{petitlyrics.ErrOutageLatched, "unavailable"},
	} {
		ref := taRefDir(t, t.TempDir(), m)
		f := &taSeqFetcher{song: taSong("", 0, []int{1000, 5000, 9000}, taTexts), err: fmt.Errorf("lane: %w", tc.err)}
		var out bytes.Buffer
		code := runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, []providers.LyricsProvider{providers.New("musixmatch", f)}, nil)
		if code != 0 || f.calls != 2 {
			t.Fatalf("%v: exit %d calls %d (want 0, 2):\n%s", tc.err, code, f.calls, out.String())
		}
		for _, want := range []string{"musixmatch: tracks=1", "musixmatch: not_found=0 failed=1", "stopped early (" + tc.why + "); remaining tracks not asked: 1"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("%v: missing %q:\n%s", tc.err, want, out.String())
			}
		}
	}
}

func TestRunTimingAccuracy_InterruptedOnce(t *testing.T) {
	ref := taRefDir(t, t.TempDir(), taManifest(""))
	ctx, cancel := context.WithCancel(t.Context())
	f := &taSeqFetcher{err: context.Canceled, hook: cancel}
	lanes := []providers.LyricsProvider{providers.New("musixmatch", f), providers.New("petitlyrics", f), providers.New("innertube", f)}
	var out bytes.Buffer
	code := runTimingAccuracy(ctx, &out, TimingAccCmd{RefDir: ref}, lanes, nil)
	if n := strings.Count(out.String(), "interrupted"); code != 1 || f.calls != 1 || n != 1 || !strings.Contains(out.String(), "musixmatch: tracks=0") {
		t.Errorf("exit %d calls %d notices %d; want 1, 1, 1 plus a partial row:\n%s", code, f.calls, n, out.String())
	}
}

func TestAccuracyLanes(t *testing.T) {
	var cfg config.Config
	cfg.Providers.Disabled = []string{"innertube"}
	lanes, skipped := accuracyLanes(cfg, "", []string{"musixmatch", "petitlyrics", "innertube"}, func(string) musixmatch.Fetcher { return taFetcher{} })
	got := accuracyConfig(cfg).API.Cooldown
	if len(lanes) != 1 || lanes[0].Name() != "petitlyrics" || strings.Join(skipped, ",") != "musixmatch(no token),innertube(disabled)" || cfg.API.Cooldown != 0 || got != 1 {
		t.Errorf("lanes %v skipped %v cooldown caller=%d floored=%d", lanes, skipped, cfg.API.Cooldown, got)
	}
}

func TestRunTimingAccuracyCmd_LanesFlag(t *testing.T) {
	for _, raw := range []string{"bogus", "musixmatch,bogus"} {
		var out bytes.Buffer
		if code := runTimingAccuracyCmd(t.Context(), &out, TimingAccCmd{RefDir: "x", Lanes: raw}, nil); code != 2 || strings.Contains(out.String(), "bogus") {
			t.Errorf("--lanes %q: exit %d out %q", raw, code, out.String())
		}
	}
	for raw, want := range map[string]string{"innertube,INNERTUBE": "innertube", " innertube , ,musixmatch,": "innertube,musixmatch"} {
		got, ok := parseLaneNames(raw)
		if !ok || strings.Join(got, ",") != want {
			t.Errorf("parseLaneNames(%q) = %v %v; want %s", raw, got, ok, want)
		}
	}
}

func TestRun_TimingAccuracyWritesNothing(t *testing.T) {
	home := t.TempDir()
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		t.Setenv(k, home)
	}
	t.Setenv("MXLRC_PROVIDERS_DISABLED", "petitlyrics,innertube")
	t.Setenv("MUSIXMATCH_TOKEN", "")
	t.Setenv("MXLRC_API_TOKEN", "")
	ref := taRefDir(t, t.TempDir(), taManifest(""))
	var out bytes.Buffer
	deps := Deps{NewFetcher: func(string) musixmatch.Fetcher {
		t.Error("a fetcher was built; no lane should run")
		return taFetcher{}
	}}
	if code := Run(t.Context(), []string{"timing-accuracy", ref}, &out, deps); code != 1 {
		t.Fatalf("exit %d; want 1 when no lane measured:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "no lane produced a measurement") || !strings.Contains(out.String(), "skipped lanes: musixmatch(no token),petitlyrics(disabled),innertube(disabled)") || !strings.Contains(out.String(), "no lane was measured") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
	if ents, err := os.ReadDir(home); err != nil || len(ents) != 0 {
		t.Errorf("sandbox HOME not empty (err=%v): %v", err, ents)
	}
}

const taWordLRC = "[00:01.00]<00:01.00>alpha <00:01.50>beta <00:02.00>gamma\n[00:05.00]<00:05.00>delta <00:05.40>alpha\n"

// taWordSong serves the two lines of taWordLRC, each word shifted by shiftMS,
// with "beta" reworded to "bravo" and the second line's "alpha" repeated.
func taWordSong(shiftMS int) models.Song {
	s := taSong("", 0, []int{1000, 5000}, []string{"alpha beta gamma", "delta alpha"})
	s.WordTimings = append(s.WordTimings, []models.WordTiming{
		{Line: 0, Text: "alpha", StartMS: 1000 + shiftMS}, {Line: 0, Text: "bravo", StartMS: 1500 + shiftMS}, {Line: 0, Text: "gamma", StartMS: 2000 + shiftMS},
		{Line: 1, Text: "delta", StartMS: 5000 + shiftMS}, {Line: 1, Text: "alpha", StartMS: 5400 + shiftMS}, {Line: 1, Text: "alpha", StartMS: 5900 + shiftMS},
	}...)
	return s
}

func taWordRef(t *testing.T, wordResidual string) string {
	t.Helper()
	ref := filepath.Join(t.TempDir(), "ref")
	taMust(t, os.MkdirAll(ref, 0o755))
	taMust(t, os.WriteFile(filepath.Join(ref, "w.lrc"), []byte(taWordLRC), 0o600))
	m := "[[track]]\nid = \"w\"\nfile = \"w.lrc\"\ntitle = \"wt\"\nline_residual_ms = 20\n" + wordResidual
	taMust(t, os.WriteFile(filepath.Join(ref, "manifest.toml"), []byte(m), 0o600))
	return ref
}

func TestRunTimingAccuracy_WordStarts(t *testing.T) {
	ref := taWordRef(t, "word_residual_ms = 30\n")
	lane := func(name string, song models.Song) providers.LyricsProvider {
		return providers.New(name, taFetcher{songs: map[string]models.Song{"wt": song}})
	}
	// A line-only lane serves the same lines with no word timings at all.
	plain := taWordSong(0)
	plain.WordTimings = nil
	var out bytes.Buffer
	code := runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, []providers.LyricsProvider{lane("innertube", taWordSong(100)), lane("petitlyrics", plain)}, nil)
	got := out.String()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, got)
	}
	for _, want := range []string{
		// line 1: alpha and gamma pair, bravo is reworded; line 2: delta pairs and the
		// repeated alpha pairs once, to the nearer occurrence (5400), so 4 of 5 ref words.
		"innertube: word-tracks=1 words(ref=5 matched=4) word-MAE=100ms within-300ms=100.0% ref-residual(mean=30ms max=30ms) n=4",
		"petitlyrics: word-tracks=0 words(ref=5 matched=0) word-MAE=n/a",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("stdout missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "alpha") || strings.Contains(got, "delta") {
		t.Errorf("lyric text leaked:\n%s", got)
	}
}

func TestRunTimingAccuracy_WordResidualRequiredWhenRefHasWords(t *testing.T) {
	code, got := taRejectOne(t, taWordRef(t, ""))
	if code != 1 || !strings.Contains(got, "word_residual_ms is required") {
		t.Errorf("exit %d:\n%s", code, got)
	}
}

func taRunWords(t *testing.T, lrc string, song models.Song) (int, string) {
	t.Helper()
	ref := filepath.Join(t.TempDir(), "ref")
	taMust(t, os.MkdirAll(ref, 0o755))
	taMust(t, os.WriteFile(filepath.Join(ref, "w.lrc"), []byte(lrc), 0o600))
	m := "[[track]]\nid = \"w\"\nfile = \"w.lrc\"\ntitle = \"wt\"\nline_residual_ms = 20\nword_residual_ms = 30\n"
	taMust(t, os.WriteFile(filepath.Join(ref, "manifest.toml"), []byte(m), 0o600))
	lane := providers.New("innertube", taFetcher{songs: map[string]models.Song{"wt": song}})
	var out bytes.Buffer
	code := runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, []providers.LyricsProvider{lane}, nil)
	return code, out.String()
}

func TestRunTimingAccuracy_StackedReferenceWordsNotMeasured(t *testing.T) {
	lrc := "[00:01.00][00:20.00]<00:01.00>alpha <00:01.50>beta\n[00:30.00]<00:30.00>gamma\n"
	song := taSong("", 0, []int{1000, 20000, 30000}, []string{"alpha beta", "alpha beta", "gamma"})
	song.WordTimings = []models.WordTiming{
		{Line: 0, Text: "alpha", StartMS: 1000}, {Line: 0, Text: "beta", StartMS: 1500},
		{Line: 1, Text: "alpha", StartMS: 20000}, {Line: 1, Text: "beta", StartMS: 20500},
		{Line: 2, Text: "gamma", StartMS: 30000},
	}
	code, got := taRunWords(t, lrc, song)
	if code != 0 || !strings.Contains(got, "words not measured: 1\n") || !strings.Contains(got, "words(ref=1 matched=1) word-MAE=0ms") {
		t.Errorf("exit %d; want stacked words dropped and only the plain line measured:\n%s", code, got)
	}
}

func TestRunTimingAccuracy_StackedCountIsPerSourceLine(t *testing.T) {
	lrc := "[00:01.00][00:20.00][00:40.00]<00:01.00>alpha <00:01.50>beta\n[00:30.00][00:50.00]<00:30.00>gamma\n"
	song := taSong("", 0, []int{1000, 20000, 30000, 40000, 50000}, []string{"alpha beta", "alpha beta", "gamma", "alpha beta", "gamma"})
	code, got := taRunWords(t, lrc, song)
	if code != 0 || !strings.Contains(got, "words not measured: 2\n") {
		t.Errorf("exit %d; want 2 stacked source lines (one three-fold, one two-fold):\n%s", code, got)
	}
}

func TestRunTimingAccuracy_AllStackedWordReference(t *testing.T) {
	lrc := "[00:01.00][00:20.00]<00:01.00>alpha <00:01.50>beta\n"
	song := taSong("", 0, []int{1000, 20000}, []string{"alpha beta", "alpha beta"})
	ref := filepath.Join(t.TempDir(), "ref")
	taMust(t, os.MkdirAll(ref, 0o755))
	taMust(t, os.WriteFile(filepath.Join(ref, "w.lrc"), []byte(lrc), 0o600))
	m := "[[track]]\nid = \"w\"\nfile = \"w.lrc\"\ntitle = \"wt\"\nline_residual_ms = 20\n"
	taMust(t, os.WriteFile(filepath.Join(ref, "manifest.toml"), []byte(m), 0o600))
	if code, got := taRejectOne(t, ref); code != 1 || !strings.Contains(got, "word_residual_ms is required when the reference carries word timings") {
		t.Errorf("exit %d; want the residual required for an all-stacked word reference:\n%s", code, got)
	}
	code, got := taRunWords(t, lrc, song)
	if code != 0 || !strings.Contains(got, "line starts and word starts") || !strings.Contains(got, "words not measured: 1\n") || !strings.Contains(got, "word-MAE=n/a") {
		t.Errorf("exit %d; want the word section and the stacked count:\n%s", code, got)
	}
}

func TestRunTimingAccuracy_DecorativeFirstLineRemapsWordIndexes(t *testing.T) {
	song := taSong("", 0, []int{500, 1000, 5000}, []string{"\u266a", "alpha beta gamma", "delta alpha"})
	song.WordTimings = []models.WordTiming{
		{Line: 1, Text: "alpha", StartMS: 1100}, {Line: 1, Text: "beta", StartMS: 1600}, {Line: 1, Text: "gamma", StartMS: 2100},
		{Line: 2, Text: "delta", StartMS: 5100}, {Line: 2, Text: "alpha", StartMS: 5500},
	}
	code, got := taRunWords(t, taWordLRC, song)
	if code != 0 || !strings.Contains(got, "words(ref=5 matched=5) word-MAE=100ms") {
		t.Errorf("exit %d; want words kept through the decorative drop:\n%s", code, got)
	}
}

func TestRunTimingAccuracy_NoMatchExitsOne(t *testing.T) {
	song := taSong("", 0, []int{1000}, []string{"unrelated words here"})
	code, got := taRunWords(t, taWordLRC, song)
	if code != 1 || !strings.Contains(got, "no lane produced a measurement") {
		t.Errorf("exit %d; want 1:\n%s", code, got)
	}
}
