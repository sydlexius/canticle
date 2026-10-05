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
		"decorative cues dropped (both sides): 3", // one note cue per reference file plus one served
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
		"duplicate id": {manifest: one("a.lrc") + one("b.lrc"), want: "duplicate id"},
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

func TestRunTimingAccuracy_StopsOnThrottle(t *testing.T) {
	m := taManifest("[[track]]\nid = \"c\"\ntitle = \"c\"\nfile = \"a.lrc\"\nline_residual_ms = 1\n")
	ref := taRefDir(t, t.TempDir(), m)
	f := &taSeqFetcher{song: taSong("", 0, []int{1000, 5000, 9000}, taTexts), err: fmt.Errorf("lane: %w", musixmatch.ErrRateLimited)}
	var out bytes.Buffer
	code := runTimingAccuracy(t.Context(), &out, TimingAccCmd{RefDir: ref}, []providers.LyricsProvider{providers.New("musixmatch", f)}, nil)
	if code != 0 || f.calls != 2 {
		t.Fatalf("exit %d calls %d (want 0, 2):\n%s", code, f.calls, out.String())
	}
	for _, want := range []string{"musixmatch: tracks=1", "musixmatch: not_found=0 failed=1", "stopped early (throttled); remaining tracks not asked: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q:\n%s", want, out.String())
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
	if code := Run(t.Context(), []string{"timing-accuracy", ref}, &out, deps); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), "skipped lanes: musixmatch(no token),petitlyrics(disabled),innertube(disabled)") || !strings.Contains(out.String(), "no lane was measured") {
		t.Errorf("unexpected output:\n%s", out.String())
	}
	if ents, err := os.ReadDir(home); err != nil || len(ents) != 0 {
		t.Errorf("sandbox HOME not empty (err=%v): %v", err, ents)
	}
}
