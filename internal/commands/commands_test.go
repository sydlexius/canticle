package commands

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/innertube"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/server"
	"github.com/sydlexius/canticle/internal/worker"
)

type fakeFetcher struct{}

func (fakeFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{}, nil
}

type fakeWriter struct{}

func (fakeWriter) WriteLRC(models.Song, string, string) error {
	return nil
}

func TestSelectedProvider(t *testing.T) {
	cfg := config.Config{Providers: config.ProvidersConfig{Primary: "musixmatch"}}
	got, err := selectedProvider(cfg, "token", func(string) musixmatch.Fetcher { return fakeFetcher{} })
	if err != nil {
		t.Fatalf("selectedProvider: %v", err)
	}
	if got.Name() != "musixmatch" {
		t.Fatalf("provider name = %q; want musixmatch", got.Name())
	}

	cfg.Providers.Disabled = []string{"musixmatch"}
	if _, err := selectedProvider(cfg, "token", func(string) musixmatch.Fetcher { return fakeFetcher{} }); err == nil {
		t.Fatal("selectedProvider returned nil error for disabled provider")
	}
}

func TestSelectedProviderMusixmatchRequiresToken(t *testing.T) {
	cfg := config.Config{Providers: config.ProvidersConfig{Primary: "musixmatch"}}
	_, err := selectedProvider(cfg, "  ", func(string) musixmatch.Fetcher { return fakeFetcher{} })
	if err == nil {
		t.Fatal("musixmatch with an empty token must return an error")
	}
	// The error must be the typed sentinel so serve can detect the tokenless
	// Musixmatch-primary case and degrade instead of aborting (#385).
	if !errors.Is(err, errNoMusixmatchToken) {
		t.Fatalf("error = %v; want errors.Is(err, errNoMusixmatchToken)", err)
	}
}

// TestSelectedProviderPetitLyricsNotTokenSentinel proves a tokenless provider
// (petitlyrics) primary with an empty token does NOT trip the Musixmatch token
// sentinel, so the serve degrade path is Musixmatch-specific (#385).
func TestSelectedProviderPetitLyricsNotTokenSentinel(t *testing.T) {
	cfg := config.Config{Providers: config.ProvidersConfig{Primary: "petitlyrics"}}
	_, err := selectedProvider(cfg, "", func(string) musixmatch.Fetcher { return fakeFetcher{} })
	if err != nil {
		t.Fatalf("selectedProvider petitlyrics: %v", err)
	}
	if errors.Is(err, errNoMusixmatchToken) {
		t.Fatal("petitlyrics primary must not trip errNoMusixmatchToken")
	}
}

// TestResolveServeProvider exercises the serve startup degrade decision (#385):
// given the selectedProvider result and the available fallbacks, decide the
// effective fetcher and the two display/behavior flags.
func TestResolveServeProvider(t *testing.T) {
	primary := providers.New(providers.Musixmatch, fakeFetcher{})

	t.Run("token present: pass through, lyrics enabled", func(t *testing.T) {
		got, inactive, disabled, err := resolveServeProvider(primary, nil)
		if err != nil {
			t.Fatalf("resolveServeProvider: %v", err)
		}
		if got != primary || inactive || disabled {
			t.Fatalf("got=%v inactive=%v disabled=%v; want primary, false, false", got.Name(), inactive, disabled)
		}
	})

	t.Run("no token: noop provider, inactive AND disabled", func(t *testing.T) {
		got, inactive, disabled, err := resolveServeProvider(nil, errNoMusixmatchToken)
		if err != nil {
			t.Fatalf("resolveServeProvider: %v", err)
		}
		if got == nil {
			t.Fatal("a non-nil no-op fetcher must be returned so worker construction is nil-safe")
		}
		if got.Name() != providers.Musixmatch {
			t.Fatalf("noop provider name = %q; want musixmatch", got.Name())
		}
		// The no-op provider must report ErrNotFound, never panic.
		if _, ferr := got.FindLyrics(context.Background(), models.Track{}); !errors.Is(ferr, musixmatch.ErrNotFound) {
			t.Fatalf("noop FindLyrics err = %v; want ErrNotFound", ferr)
		}
		if !inactive || !disabled {
			t.Fatalf("inactive=%v disabled=%v; want both true", inactive, disabled)
		}
	})

	t.Run("other error: propagated", func(t *testing.T) {
		sentinel := errors.New("boom")
		_, _, _, err := resolveServeProvider(nil, sentinel)
		if !errors.Is(err, sentinel) {
			t.Fatalf("err = %v; want the original error propagated", err)
		}
	})
}

func TestSelectedProviderPetitLyrics(t *testing.T) {
	cfg := config.Config{Providers: config.ProvidersConfig{Primary: "petitlyrics"}}
	// petitlyrics is tokenless: an empty token must NOT block it.
	got, err := selectedProvider(cfg, "", func(string) musixmatch.Fetcher { return fakeFetcher{} })
	if err != nil {
		t.Fatalf("selectedProvider: %v", err)
	}
	if got.Name() != "petitlyrics" {
		t.Fatalf("provider name = %q; want petitlyrics", got.Name())
	}
}

func names(ps []providers.LyricsProvider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

func TestFallbackProviders(t *testing.T) {
	newFetcher := func(string) musixmatch.Fetcher { return fakeFetcher{} }

	t.Run("builds configured order, skipping the primary", func(t *testing.T) {
		cfg := config.Config{Providers: config.ProvidersConfig{
			Primary:       "musixmatch",
			FallbackOrder: []string{"musixmatch", "petitlyrics"},
		}}
		got := fallbackProviders(cfg, "token", "musixmatch", newFetcher)
		if len(got) != 1 || got[0].Name() != "petitlyrics" {
			t.Fatalf("fallbacks = %v; want [petitlyrics] (primary excluded)", names(got))
		}
	})

	t.Run("excludes disabled providers", func(t *testing.T) {
		cfg := config.Config{Providers: config.ProvidersConfig{
			Primary:       "musixmatch",
			FallbackOrder: []string{"petitlyrics"},
			Disabled:      []string{"petitlyrics"},
		}}
		if got := fallbackProviders(cfg, "token", "musixmatch", newFetcher); len(got) != 0 {
			t.Fatalf("fallbacks = %v; want empty (petitlyrics disabled)", names(got))
		}
	})

	t.Run("skips a musixmatch fallback without a token", func(t *testing.T) {
		cfg := config.Config{Providers: config.ProvidersConfig{
			Primary:       "petitlyrics",
			FallbackOrder: []string{"musixmatch"},
		}}
		if got := fallbackProviders(cfg, "  ", "petitlyrics", newFetcher); len(got) != 0 {
			t.Fatalf("fallbacks = %v; want empty (no token for musixmatch fallback)", names(got))
		}
	})
}

func TestProviderGeneration(t *testing.T) {
	newFetcher := func(string) musixmatch.Fetcher { return fakeFetcher{} }
	cfg := config.Config{Providers: config.ProvidersConfig{
		Primary:       "musixmatch",
		FallbackOrder: []string{"petitlyrics"},
	}}
	fallbacks := fallbackProviders(cfg, "token", "musixmatch", newFetcher)
	withFallback := providerGeneration("musixmatch", fallbacks)
	soloPrimary := providerGeneration("musixmatch", nil)
	if withFallback == soloPrimary {
		t.Fatal("generation must change when the provider set changes (adding petitlyrics)")
	}
	// The generation is a function of the set, so it is stable across calls.
	if again := providerGeneration("musixmatch", fallbacks); again != withFallback {
		t.Fatalf("generation not stable: %d vs %d", withFallback, again)
	}
}

func TestConfigureWriterBilingual(t *testing.T) {
	dir := t.TempDir()
	w := lyrics.NewLRCWriter(dir)
	cfg := config.Config{Output: config.OutputConfig{BilingualOutput: true}}
	configureWriterBilingual(w, cfg)

	song := models.Song{
		Track:                models.Track{ArtistName: "a", TrackName: "t"},
		Subtitles:            models.Synced{Lines: []models.Lines{{Text: "orig", Time: models.Time{Seconds: 1}}}},
		TranslationSubtitles: models.Synced{Lines: []models.Lines{{Text: "trans", Time: models.Time{Seconds: 1}}}},
	}
	if err := w.WriteLRC(song, "", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("want exactly one output file (err=%v): %v", err, entries)
	}
	b, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(b), "orig") || !strings.Contains(string(b), "trans") {
		t.Fatalf("expected interleaved original + translation, got:\n%s", b)
	}

	// A fake writer does not satisfy *lyrics.LRCWriter: the helper is a no-op and
	// must not panic.
	configureWriterBilingual(fakeWriter{}, cfg)
}

func TestNewVerifierRequiresURLWhenEnabled(t *testing.T) {
	_, err := newVerifier(config.Config{
		Verification: config.VerificationConfig{Enabled: true},
	}, "")
	if err == nil {
		t.Fatal("newVerifier returned nil error; want missing URL error")
	}
}

func TestNewVerifierDisabledDoesNotRequireFFmpeg(t *testing.T) {
	got, err := newVerifier(config.Config{
		Verification: config.VerificationConfig{Enabled: false},
	}, "/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	if got != nil {
		t.Fatalf("newVerifier = %#v; want nil", got)
	}
}

func TestConfigureWorkerVerificationAcceptsNilVerifier(t *testing.T) {
	w := worker.New(nil, nil, fakeFetcher{}, fakeWriter{})
	configureWorkerVerification(w, config.Config{}, nil)
}

func TestNewGuardDisabledReturnsUntypedNil(t *testing.T) {
	g := newGuard(config.Config{Guard: config.GuardConfig{Threshold: 0.20}})
	if g != nil {
		t.Fatalf("newGuard with empty allowlist = %#v; want untyped nil (not a typed-nil *Guard)", g)
	}
}

func TestNewGuardEnabledReturnsGuard(t *testing.T) {
	g := newGuard(config.Config{Guard: config.GuardConfig{AcceptedScripts: []string{"Latin"}, Threshold: 0.20}})
	if g == nil {
		t.Fatal("newGuard with a non-empty allowlist = nil; want a Guard")
	}
	if !g.Enabled() {
		t.Fatal("newGuard returned a disabled guard; want Enabled() == true")
	}
}

func TestConfigureWorkerGuardAcceptsNilGuard(t *testing.T) {
	w := worker.New(nil, nil, fakeFetcher{}, fakeWriter{})
	configureWorkerGuard(w, nil)
}

// TestConfigureWorkerProviderRecorderWiresRecorder guards the serve command
// wiring: configureWorkerProviderRecorder must install the recorder so that
// provider outcome events are recorded. This is the testable unit equivalent
// of the w.SetProviderRecorder(workQ) call in runServe.
func TestConfigureWorkerProviderRecorderWiresRecorder(t *testing.T) {
	w := worker.New(nil, nil, fakeFetcher{}, fakeWriter{})
	// nil recorder is a valid no-op (backward-compatible default).
	configureWorkerProviderRecorder(w, nil)
	// A non-nil recorder must also be accepted without panic.
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "wiring.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	workQ := queue.NewDBQueue(sqlDB)
	configureWorkerProviderRecorder(w, workQ)
}

// TestNewAudioDetectorDecoupledFromEnableFlag verifies the #218 decoupling: the
// detector is built whenever a classifier URL is configured, independent of the
// global Enabled flag, and is nil only when no classifier URL is set.
func TestNewAudioDetectorDecoupledFromEnableFlag(t *testing.T) {
	// Enabled=false but no classifier URL -> nil (no classifier configured).
	got, err := newAudioDetector(config.Config{
		InstrumentalDetector: config.InstrumentalDetectorConfig{
			Enabled: false,
		},
	}, "/path/that/does/not/exist")
	if err != nil {
		t.Fatalf("newAudioDetector (no URL): %v", err)
	}
	if got != nil {
		t.Fatalf("newAudioDetector (no URL) = %#v; want nil", got)
	}

	// Enabled=false but a classifier URL IS set -> detector still built (decoupled).
	// Point FFmpegPath at a stub executable so construction does not depend on a
	// real ffmpeg being on PATH (CI runners do not have one).
	stubFFmpeg := filepath.Join(t.TempDir(), "ffmpeg")
	if err := os.WriteFile(stubFFmpeg, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write stub ffmpeg: %v", err)
	}
	got, err = newAudioDetector(config.Config{
		InstrumentalDetector: config.InstrumentalDetectorConfig{
			Enabled:               false,
			ClassifierURL:         "http://yamnet:8080",
			SampleDurationSeconds: 30,
			MinConfidence:         0.90,
			InstrumentalClasses:   []string{"Music"},
		},
	}, stubFFmpeg)
	if err != nil {
		t.Fatalf("newAudioDetector (URL set, Enabled=false): %v", err)
	}
	if got == nil {
		t.Fatal("newAudioDetector (URL set, Enabled=false) = nil; want a detector (decoupled from Enabled)")
	}
}

// TestNewAudioDetectorEnabledWithoutFFmpegErrors verifies that enabling the
// detector with a non-existent ffmpeg path returns an error (not a nil detector).
func TestNewAudioDetectorEnabledWithoutFFmpegErrors(t *testing.T) {
	_, err := newAudioDetector(config.Config{
		InstrumentalDetector: config.InstrumentalDetectorConfig{
			Enabled:               true,
			ClassifierURL:         "http://yamnet:8080",
			SampleDurationSeconds: 30,
			MinConfidence:         0.90,
			InstrumentalClasses:   []string{"Music"},
			CooldownSeconds:       5,
		},
	}, filepath.Join(t.TempDir(), "nonexistent-ffmpeg"))
	if err == nil {
		t.Fatal("newAudioDetector with missing ffmpeg returned nil error; want error")
	}
}

// TestNewAudioDetectorBlankClassifierURLReturnsNil verifies that a blank classifier
// URL yields no detector (nil, nil) even when the global flag is enabled. Detector
// construction is decoupled from the enable flag (#218); a row that requests
// detection without a configured classifier is loud-skipped by the worker at
// runtime rather than failing detector construction.
func TestNewAudioDetectorBlankClassifierURLReturnsNil(t *testing.T) {
	d, err := newAudioDetector(config.Config{
		InstrumentalDetector: config.InstrumentalDetectorConfig{
			Enabled:               true,
			ClassifierURL:         "", // blank
			SampleDurationSeconds: 30,
			MinConfidence:         0.90,
			InstrumentalClasses:   []string{"Music"},
		},
	}, "")
	if err != nil {
		t.Fatalf("newAudioDetector with blank ClassifierURL returned error %v; want nil", err)
	}
	if d != nil {
		t.Fatal("newAudioDetector with blank ClassifierURL returned a detector; want nil (no classifier configured)")
	}
}

// TestConfigureWorkerAudioDetectorAcceptsNilDetector verifies that passing a
// nil detector to configureWorkerAudioDetector is a no-op and does not panic.
func TestConfigureWorkerAudioDetectorAcceptsNilDetector(t *testing.T) {
	w := worker.New(nil, nil, fakeFetcher{}, fakeWriter{})
	configureWorkerAudioDetector(w, nil)
}

func TestRunSubcommandHelpShowsSelectedCommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "serve",
			args: []string{"serve", "--help"},
			want: []string{"Usage: canticle serve", "--scan-interval", "--work-interval"},
		},
		{
			name: "scan",
			args: []string{"scan", "--help"},
			want: []string{"Usage: canticle scan", "--upgrade", "--bfs"},
		},
		{
			name: "library",
			args: []string{"library", "--help"},
			want: []string{"Usage: canticle library", "add", "list"},
		},
		{
			name: "library add",
			args: []string{"library", "add", "--help"},
			want: []string{"Usage: canticle library add", "--name", "--config"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			code := Run(context.Background(), tc.args, &out, Deps{})
			if code != 0 {
				t.Fatalf("Run exit code = %d; want 0", code)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("help output = %q; want %q", out.String(), want)
				}
			}
		})
	}
}

func TestRunSubcommandParseErrorShowsSelectedUsage(t *testing.T) {
	var out bytes.Buffer
	code := Run(context.Background(), []string{"serve", "--not-a-real-flag"}, &out, Deps{})
	if code != 2 {
		t.Fatalf("Run exit code = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "Usage: canticle serve") {
		t.Fatalf("usage output = %q; want serve usage", out.String())
	}
	if strings.Contains(out.String(), "Usage: canticle <command>") {
		t.Fatalf("usage output = %q; want selected subcommand usage, not top-level usage", out.String())
	}
}

func TestNormalizeWorkerInterval(t *testing.T) {
	tests := []struct {
		name     string
		interval time.Duration
		want     time.Duration
	}{
		{name: "zero", interval: 0, want: 15 * time.Second},
		{name: "below minimum", interval: 5 * time.Second, want: 15 * time.Second},
		{name: "minimum", interval: 15 * time.Second, want: 15 * time.Second},
		{name: "above minimum", interval: 30 * time.Second, want: 30 * time.Second},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeWorkerInterval(tc.interval); got != tc.want {
				t.Fatalf("normalizeWorkerInterval(%s) = %s; want %s", tc.interval, got, tc.want)
			}
		})
	}
}

// TestPetitLyricsIntervalPrecedence pins the resolution order for the
// petitlyrics pacing interval (#535): providers.petitlyrics_cooldown_seconds
// when set, otherwise api.cooldown as the historical fallback. Before this key
// existed the lane inherited api.cooldown unconditionally, so the fallback arm
// is what keeps an existing deployment's behavior unchanged.
func TestPetitLyricsIntervalPrecedence(t *testing.T) {
	// Unset (zero) falls back to api.cooldown, preserving pre-#535 behavior.
	cfg := config.Config{API: config.APIConfig{Cooldown: 45}}
	if got := petitLyricsInterval(cfg); got != 45*time.Second {
		t.Fatalf("petitLyricsInterval unset = %s; want 45s (api.cooldown fallback)", got)
	}

	// Set takes precedence over api.cooldown.
	cfg.Providers.PetitLyricsCooldownSeconds = 90
	if got := petitLyricsInterval(cfg); got != 90*time.Second {
		t.Fatalf("petitLyricsInterval set = %s; want 90s (own key wins)", got)
	}

	// A negative value is not a disable request: pacing this lane off is not
	// offered, so it falls back rather than yielding a negative interval that
	// WithMinInterval would silently read as "no pacing".
	cfg.Providers.PetitLyricsCooldownSeconds = -1
	if got := petitLyricsInterval(cfg); got != 45*time.Second {
		t.Fatalf("petitLyricsInterval negative = %s; want 45s (falls back)", got)
	}
}

// TestPetitLyricsIntervalNeverGoesUnpaced is the overflow guard (#535 review,
// finding C1). time.Duration is int64 NANOSECONDS, so `seconds * time.Second`
// wraps NEGATIVE above 9223372036 -- and WithMinInterval only clamps values it
// sees as > 0, so a wrapped negative sails past the 10s policy floor into
// pace(), which returns immediately on minInterval <= 0. The lane would run
// COMPLETELY UNPACED, which is the opposite of what a large cooldown asks for.
//
// No validator catches this: every input surface (TOML, env, CLI, web) bounds
// the value below at 0 and not at all above. So the resolver is the last line,
// and it must never hand a non-positive duration to a positive config.
func TestPetitLyricsIntervalNeverGoesUnpaced(t *testing.T) {
	// The exact measured wrap threshold, plus the absurd value a fat-finger or a
	// units mix-up (nanoseconds pasted into a seconds field) actually produces.
	for _, seconds := range []int{9223372036, 9223372037, 1 << 40, 999999999999999999} {
		cfg := config.Config{
			API:       config.APIConfig{Cooldown: 15},
			Providers: config.ProvidersConfig{PetitLyricsCooldownSeconds: seconds},
		}
		got := petitLyricsInterval(cfg)
		if got <= 0 {
			t.Errorf("petitLyricsInterval(%d) = %s; a positive cooldown must NEVER resolve to a non-positive duration (that disables pacing entirely)", seconds, got)
		}
		if got > maxPacingInterval {
			t.Errorf("petitLyricsInterval(%d) = %s; want it clamped to at most %s", seconds, got, maxPacingInterval)
		}
	}

	// The same overflow is reachable through the api.cooldown FALLBACK arm, so
	// the clamp has to cover both inputs, not just the new key. This arm is
	// pre-existing (the bug is inherited, not introduced), but it flows through
	// this one resolver, so guarding here fixes it for this lane.
	cfg := config.Config{API: config.APIConfig{Cooldown: 999999999999999999}}
	if got := petitLyricsInterval(cfg); got <= 0 {
		t.Errorf("petitLyricsInterval via api.cooldown overflow = %s; want a positive clamped duration", got)
	}

	// A sane value must pass through untouched -- a clamp that mangles ordinary
	// input would be worse than the bug.
	sane := config.Config{Providers: config.ProvidersConfig{PetitLyricsCooldownSeconds: 90}}
	if got := petitLyricsInterval(sane); got != 90*time.Second {
		t.Errorf("petitLyricsInterval(90) = %s; want 90s (the clamp must not touch sane values)", got)
	}
}

// TestBuildProviderAppliesPetitLyricsInterval pins the ONE line issue #535 is
// actually about: that the resolved interval reaches the client rather than
// being accepted and ignored (the issue's acceptance criterion #2).
//
// Without this, reverting buildProvider to the pre-change
// `WithMinInterval(cfg.API.Cooldown)` leaves the entire suite green -- verified
// by mutation. Every other test here covers the resolver and the config
// plumbing AROUND the call site, not the call site itself.
func TestBuildProviderAppliesPetitLyricsInterval(t *testing.T) {
	cfg := config.Config{
		// Deliberately DIFFERENT values: if the wiring regresses to api.cooldown
		// the observed interval is 15s, not 90s, so the assertion discriminates.
		API:       config.APIConfig{Cooldown: 15},
		Providers: config.ProvidersConfig{PetitLyricsCooldownSeconds: 90},
	}

	p := buildProvider(providers.PetitLyrics, cfg, "", nil)
	if p == nil {
		t.Fatal("buildProvider returned nil for the petitlyrics provider")
	}

	inner, ok := p.(interface{ Unwrap() providers.Fetcher })
	if !ok {
		t.Fatal("provider wrapper does not expose Unwrap; cannot verify the interval reached the client")
	}
	client, ok := inner.Unwrap().(*petitlyrics.Client)
	if !ok {
		t.Fatalf("unwrapped fetcher is %T; want *petitlyrics.Client", inner.Unwrap())
	}

	if got := client.MinInterval(); got != 90*time.Second {
		t.Errorf("client MinInterval = %s; want 90s from providers.petitlyrics_cooldown_seconds.\n"+
			"15s means buildProvider is still using api.cooldown and the key is accepted but ignored (#535 AC2).", got)
	}
}

// TestBuildProviderConstructsInnerTube pins that #857's registration actually
// CONSTRUCTS a client and applies the pacing floor, rather than merely making
// the name selectable.
//
// The distinction is the point: providers.Known() is what makes a provider
// configurable, but buildProvider is what makes it FETCH. A name registered
// without a construction arm resolves, validates, renders in the settings
// dropdown, and then returns nil from every lane -- configurable and inert.
func TestBuildProviderConstructsInnerTube(t *testing.T) {
	// Below innertube's own 2s floor, so the clamp is observable: a client that
	// took this value verbatim would report 1s.
	cfg := config.Config{API: config.APIConfig{Cooldown: 1}}

	p := buildProvider(providers.InnerTube, cfg, "", nil)
	if p == nil {
		t.Fatal("buildProvider returned nil for the innertube provider; " +
			"the name is registered but nothing constructs it")
	}
	if p.Name() != providers.InnerTube {
		t.Errorf("provider Name = %q, want %q -- the lane name is the attribution "+
			"shown to users and the key every per-lane metric groups by",
			p.Name(), providers.InnerTube)
	}

	inner, ok := p.(interface{ Unwrap() providers.Fetcher })
	if !ok {
		t.Fatal("provider wrapper does not expose Unwrap; cannot verify the interval reached the client")
	}
	client, ok := inner.Unwrap().(*innertube.Client)
	if !ok {
		t.Fatalf("unwrapped fetcher is %T; want *innertube.Client", inner.Unwrap())
	}

	// Clamped UP to the provider's own floor, not taken verbatim. That floor is
	// per REQUEST and matters more here than for the other two lanes: one
	// successful lookup costs three requests (search, next, browse).
	if got := client.MinInterval(); got != innertube.MinAllowedInterval {
		t.Errorf("client MinInterval = %s; want %s (innertube.MinAllowedInterval).\n"+
			"1s means buildProvider passed api.cooldown through without the client "+
			"clamping it, so a misconfigured cooldown could make this lane impolite.",
			got, innertube.MinAllowedInterval)
	}
}

// TestBuildProviderClampsInnerTubeOverflowCooldown pins that the InnerTube arm
// routes api.cooldown through clampPacingSeconds rather than converting it
// directly, and it is the ONLY case that can tell the two apart.
//
// Measured: replacing clampPacingSeconds(cfg.API.Cooldown) with
// time.Duration(cfg.API.Cooldown) * time.Second left the whole suite green,
// because at any ORDINARY cooldown both forms produce the same duration and
// innertube's own floor clamps them identically. The two diverge only past
// maxPacingSeconds, where the multiplication OVERFLOWS int64 and wraps
// NEGATIVE.
//
// A negative is the dangerous direction. WithMinInterval clamps up only when
// d > 0, so a wrapped value disables pacing entirely -- an absurd config value
// silently turning the politeness floor OFF, which is the opposite of what a
// clamp is for. clampPacingSeconds caps it and warns instead.
func TestBuildProviderClampsInnerTubeOverflowCooldown(t *testing.T) {
	cfg := config.Config{API: config.APIConfig{Cooldown: maxPacingSeconds + 1}}

	p := buildProvider(providers.InnerTube, cfg, "", nil)
	if p == nil {
		t.Fatal("buildProvider returned nil for the innertube provider")
	}
	inner, ok := p.(interface{ Unwrap() providers.Fetcher })
	if !ok {
		t.Fatal("provider wrapper does not expose Unwrap")
	}
	client, ok := inner.Unwrap().(*innertube.Client)
	if !ok {
		t.Fatalf("unwrapped fetcher is %T; want *innertube.Client", inner.Unwrap())
	}

	got := client.MinInterval()
	if got <= 0 {
		t.Fatalf("client MinInterval = %s; a non-positive interval DISABLES pacing. "+
			"An overflowing api.cooldown wrapped negative, so the lane is now "+
			"completely unpaced against a third-party gateway.", got)
	}
	if got != maxPacingInterval {
		t.Errorf("client MinInterval = %s; want %s (clampPacingSeconds' cap)",
			got, maxPacingInterval)
	}
}

// TestSelectedProviderAcceptsInnerTube drives the real resolver, which is the
// path serve mode takes. buildProvider alone proves the arm exists; this proves
// the arm is REACHABLE from a configured primary.
func TestSelectedProviderAcceptsInnerTube(t *testing.T) {
	cfg := config.Config{}
	cfg.Providers.Primary = providers.InnerTube

	// No token deliberately: innertube is tokenless (its API key is a public,
	// non-authenticating constant), so a missing token must not block it the way
	// it blocks a musixmatch primary.
	//
	// A real fetcher factory is still required even though innertube never uses
	// it: selectedProvider builds EVERY candidate before Select picks one, so
	// the musixmatch arm calls newFetcher unconditionally and a nil factory
	// panics there. That eager construction is pre-existing and shared by every
	// caller in this file.
	p, err := selectedProvider(cfg, "", func(string) musixmatch.Fetcher { return fakeFetcher{} })
	if err != nil {
		t.Fatalf("selectedProvider with innertube primary and no token: %v", err)
	}
	if p.Name() != providers.InnerTube {
		t.Errorf("selected provider = %q, want %q", p.Name(), providers.InnerTube)
	}
}

// TestFallbackProvidersIncludesInnerTube covers the construction path
// registration makes reachable but nothing else tests: an innertube FALLBACK
// lane.
//
// This is the likeliest real deployment of the provider -- an operator adds the
// new lane behind a working primary rather than replacing it -- and it went in
// with no coverage. Measured: inserting a `continue` for innertube into
// fallbackProviders' skip chain left the ENTIRE suite green. Any later edit to
// that chain (a new "requires a credential" condition, a lane allowlist, a
// copy-paste of the musixmatch guard) would silently drop the lane, and the
// operator sees no error, no warning, and no rows in lane_attempts -- a provider
// they configured that simply never runs.
//
// NO TOKEN is passed deliberately. fallbackProviders skips a tokenless
// MUSIXMATCH lane; the assertion is that no analogous condition touches
// innertube, which is tokenless by design.
func TestFallbackProvidersIncludesInnerTube(t *testing.T) {
	cfg := config.Config{}
	cfg.Providers.Primary = providers.Musixmatch
	cfg.Providers.FallbackOrder = []string{providers.InnerTube}

	got := fallbackProviders(cfg, "", providers.Musixmatch,
		func(string) musixmatch.Fetcher { return fakeFetcher{} })

	var names []string
	for _, p := range got {
		names = append(names, p.Name())
	}
	if !slices.Contains(names, providers.InnerTube) {
		t.Errorf("fallback lanes = %v, want one named %q. A configured lane that "+
			"never appears is invisible in production: no error, no warning, and "+
			"no lane_attempts rows.", names, providers.InnerTube)
	}
}

// TestFallbackProvidersRespectsInnerTubeDisabled is the other branch of the same
// skip chain: providerDisabledIn had no innertube coverage either, and a
// disabled lane that still runs is the inverse defect -- traffic to a
// third-party gateway the operator explicitly turned off.
func TestFallbackProvidersRespectsInnerTubeDisabled(t *testing.T) {
	cfg := config.Config{}
	cfg.Providers.Primary = providers.Musixmatch
	cfg.Providers.FallbackOrder = []string{providers.InnerTube}
	cfg.Providers.Disabled = []string{providers.InnerTube}

	got := fallbackProviders(cfg, "", providers.Musixmatch,
		func(string) musixmatch.Fetcher { return fakeFetcher{} })

	for _, p := range got {
		if p.Name() == providers.InnerTube {
			t.Error("a disabled innertube lane was still constructed; the operator " +
				"turned it off and it would still send traffic")
		}
	}
}

func TestServeWorkerIntervalUsesConfigUnlessFlagProvided(t *testing.T) {
	cfg := config.Config{
		API: config.APIConfig{Cooldown: 45},
	}

	if got := serveWorkerInterval(cfg, ServeCmd{}); got != 45*time.Second {
		t.Fatalf("serveWorkerInterval without flag = %s; want 45s", got)
	}

	flag := 30
	if got := serveWorkerInterval(cfg, ServeCmd{WorkInterval: &flag}); got != 30*time.Second {
		t.Fatalf("serveWorkerInterval with flag = %s; want 30s", got)
	}
}

// TestServeWorkerIntervalPrecedence verifies CLI flag > server.work_interval_seconds
// > api.cooldown. A zero server.work_interval_seconds means "fall back to cooldown".
func TestServeWorkerIntervalPrecedence(t *testing.T) {
	cfg := config.Config{
		API:    config.APIConfig{Cooldown: 45},
		Server: config.ServerConfig{WorkIntervalSeconds: 25},
	}

	if got := serveWorkerInterval(cfg, ServeCmd{}); got != 25*time.Second {
		t.Fatalf("serveWorkerInterval config = %s; want 25s (server config over cooldown)", got)
	}

	cfgNoServer := config.Config{API: config.APIConfig{Cooldown: 45}}
	if got := serveWorkerInterval(cfgNoServer, ServeCmd{}); got != 45*time.Second {
		t.Fatalf("serveWorkerInterval cooldown fallback = %s; want 45s", got)
	}

	flag := 30
	if got := serveWorkerInterval(cfg, ServeCmd{WorkInterval: &flag}); got != 30*time.Second {
		t.Fatalf("serveWorkerInterval flag = %s; want 30s (flag wins)", got)
	}
}

// TestServeScanIntervalPrecedence verifies CLI flag > server.scan_interval_seconds.
func TestServeScanIntervalPrecedence(t *testing.T) {
	cfg := config.Config{
		Server: config.ServerConfig{ScanIntervalSeconds: 600},
	}

	if got := serveScanInterval(cfg, ServeCmd{}); got != 600*time.Second {
		t.Fatalf("serveScanInterval config = %s; want 600s", got)
	}

	flag := 120
	if got := serveScanInterval(cfg, ServeCmd{ScanInterval: &flag}); got != 120*time.Second {
		t.Fatalf("serveScanInterval flag = %s; want 120s (flag wins)", got)
	}

	zero := 0
	if got := serveScanInterval(cfg, ServeCmd{ScanInterval: &zero}); got != 0 {
		t.Fatalf("serveScanInterval zero flag = %s; want 0 (disables repeat)", got)
	}
}

func TestSchedulerBuildsScanEnqueuer(t *testing.T) {
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(context.Background(), "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(context.Background(), lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
		Status:   scan.StatusPending,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert scan result: %v", err)
	}

	got := scheduler(sqlDB, scanner.ScanOptions{MaxDepth: 7}, nil, false, nil, nil, "", 0)
	if got.OnScanComplete == nil {
		t.Fatal("scheduler OnScanComplete = nil; want enqueue callback")
	}
	if err := got.OnScanComplete(context.Background(), models.Library{ID: lib.ID}, nil, lib.Path, scan.TriggerScheduler); err != nil {
		t.Fatalf("OnScanComplete: %v", err)
	}
	item, err := queue.NewDBQueue(sqlDB).Dequeue(context.Background())
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if item.Priority != queue.PriorityScan {
		t.Fatalf("Dequeue priority = %d; want scan priority %d", item.Priority, queue.PriorityScan)
	}
}

func TestRunLibraryUpdate(t *testing.T) {
	isolateCommandsEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := writeCommandsConfig(t, filepath.Join(dir, "state", "test.db"))
	libPath := filepath.Join(dir, "music")
	if err := os.Mkdir(libPath, 0o750); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}

	var out bytes.Buffer
	code := runLibrary(ctx, &out, LibraryCmd{Add: &LibraryAddCmd{
		Path:       libPath,
		Name:       "Music",
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("library add exit code = %d; want 0", code)
	}

	renamedPath := filepath.Join(dir, "renamed")
	if err := os.Mkdir(renamedPath, 0o750); err != nil {
		t.Fatalf("mkdir renamed library: %v", err)
	}
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Path:       renamedPath,
		Name:       "Renamed",
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("library update exit code = %d; want 0", code)
	}
	gotOut := strings.TrimSpace(out.String())
	wantOut := "1\tRenamed\t" + renamedPath
	if gotOut != wantOut {
		t.Fatalf("library update output = %q; want %q", gotOut, wantOut)
	}

	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Name:       "Display",
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("library update name exit code = %d; want 0", code)
	}
	gotOut = strings.TrimSpace(out.String())
	wantOut = "1\tDisplay\t" + renamedPath
	if gotOut != wantOut {
		t.Fatalf("library update name output = %q; want %q", gotOut, wantOut)
	}
}

func TestRunLibraryUpdateFailures(t *testing.T) {
	isolateCommandsEnv(t)
	ctx := context.Background()
	dir := t.TempDir()
	cfg := writeCommandsConfig(t, filepath.Join(dir, "state", "test.db"))

	var out bytes.Buffer
	code := runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		ConfigPath: cfg,
	}})
	if code != 2 {
		t.Fatalf("library update without changes exit code = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "requires --path") {
		t.Fatalf("library update without changes output = %q; want validation message", out.String())
	}

	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         99,
		Name:       "Missing",
		ConfigPath: cfg,
	}})
	if code != 1 {
		t.Fatalf("library update missing exit code = %d; want 1", code)
	}
	if !strings.Contains(out.String(), "library 99 not found") {
		t.Fatalf("library update missing output = %q; want not-found message", out.String())
	}

	libPath := filepath.Join(dir, "music")
	if err := os.Mkdir(libPath, 0o750); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}
	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Add: &LibraryAddCmd{
		Path:       libPath,
		Name:       "Music",
		ConfigPath: cfg,
	}})
	if code != 0 {
		t.Fatalf("library add exit code = %d; want 0", code)
	}

	out.Reset()
	code = runLibrary(ctx, &out, LibraryCmd{Update: &LibraryUpdateCmd{
		ID:         1,
		Name:       " ",
		ConfigPath: cfg,
	}})
	if code != 1 {
		t.Fatalf("library update invalid exit code = %d; want 1", code)
	}
}

func TestVerificationConfigKeys(t *testing.T) {
	cfg := config.Config{
		Verification: config.VerificationConfig{
			Enabled:               true,
			WhisperURL:            "http://whisper:9000",
			FFmpegPath:            "/usr/bin/ffmpeg",
			SampleDurationSeconds: 45,
			MinConfidence:         0.7,
			MinSimilarity:         0.5,
		},
	}
	tests := map[string]string{
		"verification.enabled":                 "true",
		"verification.whisper_url":             "http://whisper:9000",
		"verification.ffmpeg_path":             "/usr/bin/ffmpeg",
		"verification.sample_duration_seconds": "45",
		"verification.min_confidence":          "0.7",
		"verification.min_similarity":          "0.5",
	}
	for key, want := range tests {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Fatalf("configValue(%q) ok = false; want true", key)
		}
		if got != want {
			t.Fatalf("configValue(%q) = %q; want %q", key, got, want)
		}
	}

	if err := setConfigValue(&cfg, "verification.min_similarity", "0"); err == nil {
		t.Fatal("setConfigValue accepted invalid verification.min_similarity")
	}
	if err := setConfigValue(&cfg, "verification.ffmpeg_path", " "); err == nil {
		t.Fatal("setConfigValue accepted blank verification.ffmpeg_path")
	}
}

func TestConfigGuardGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		Guard: config.GuardConfig{
			AcceptedScripts: []string{"Latin", "Han"},
			Threshold:       0.3,
		},
	}
	gets := map[string]string{
		"guard.accepted_scripts":       "Latin,Han",
		"guard.script_guard_threshold": "0.3",
	}
	for key, want := range gets {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Fatalf("configValue(%q) ok = false; want true", key)
		}
		if got != want {
			t.Fatalf("configValue(%q) = %q; want %q", key, got, want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Fatalf("configKeys missing %q", key)
		}
	}

	if err := setConfigValue(&cfg, "guard.accepted_scripts", "Latin, Hangul"); err != nil {
		t.Fatalf("setConfigValue accepted_scripts: %v", err)
	}
	if len(cfg.Guard.AcceptedScripts) != 2 || cfg.Guard.AcceptedScripts[0] != "Latin" || cfg.Guard.AcceptedScripts[1] != "Hangul" {
		t.Fatalf("accepted_scripts = %v; want [Latin Hangul]", cfg.Guard.AcceptedScripts)
	}
	// An empty accepted_scripts is valid: it disables the guard.
	if err := setConfigValue(&cfg, "guard.accepted_scripts", ""); err != nil {
		t.Fatalf("setConfigValue empty accepted_scripts: %v (empty is valid, disables the guard)", err)
	}
	if len(cfg.Guard.AcceptedScripts) != 0 {
		t.Fatalf("accepted_scripts = %v; want empty", cfg.Guard.AcceptedScripts)
	}

	if err := setConfigValue(&cfg, "guard.script_guard_threshold", "0.5"); err != nil {
		t.Fatalf("setConfigValue threshold: %v", err)
	}
	if cfg.Guard.Threshold != 0.5 {
		t.Fatalf("threshold = %v; want 0.5", cfg.Guard.Threshold)
	}
	for _, bad := range []string{"abc", "0", "2", "-1"} {
		if err := setConfigValue(&cfg, "guard.script_guard_threshold", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid guard.script_guard_threshold %q", bad)
		}
	}
}

func TestConfigProvidersModeAndRaceWaitGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		Providers: config.ProvidersConfig{Mode: "parallel", RaceWaitSeconds: 3},
	}
	gets := map[string]string{
		"providers.mode":              "parallel",
		"providers.race_wait_seconds": "3",
	}
	for key, want := range gets {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Fatalf("configValue(%q) ok = false; want true", key)
		}
		if got != want {
			t.Fatalf("configValue(%q) = %q; want %q", key, got, want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Fatalf("configKeys missing %q", key)
		}
	}

	// Both modes are settable; an unknown mode is rejected.
	for _, mode := range []string{"ordered", "parallel"} {
		if err := setConfigValue(&cfg, "providers.mode", mode); err != nil {
			t.Fatalf("setConfigValue providers.mode=%q: %v", mode, err)
		}
		if cfg.Providers.Mode != mode {
			t.Fatalf("providers.mode = %q; want %q", cfg.Providers.Mode, mode)
		}
	}
	if err := setConfigValue(&cfg, "providers.mode", "sequential"); err == nil {
		t.Fatal("setConfigValue accepted an unknown providers.mode")
	}

	if err := setConfigValue(&cfg, "providers.race_wait_seconds", "5"); err != nil {
		t.Fatalf("setConfigValue race_wait_seconds: %v", err)
	}
	if cfg.Providers.RaceWaitSeconds != 5 {
		t.Fatalf("race_wait_seconds = %d; want 5", cfg.Providers.RaceWaitSeconds)
	}
	// Non-positive is the "use the default" sentinel in the config stack (config
	// load clamps it), so the CLI must accept it rather than reject it.
	if err := setConfigValue(&cfg, "providers.race_wait_seconds", "0"); err != nil {
		t.Fatalf("setConfigValue race_wait_seconds=0: %v (non-positive must be accepted; config load clamps it)", err)
	}
	if cfg.Providers.RaceWaitSeconds != 0 {
		t.Fatalf("race_wait_seconds = %d; want 0 stored raw (clamped to the default only at load)", cfg.Providers.RaceWaitSeconds)
	}
	// Only a non-integer is rejected.
	if err := setConfigValue(&cfg, "providers.race_wait_seconds", "abc"); err == nil {
		t.Fatal("setConfigValue accepted a non-integer providers.race_wait_seconds")
	}
}

// TestConfigPetitLyricsCooldownGetSetRoundTrip covers the config get/set arms
// for providers.petitlyrics_cooldown_seconds (#535). The key is Editable in the
// registry, so a missing CLI arm would be exactly the registry/CLI drift #670
// tracks.
func TestConfigPetitLyricsCooldownGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		Providers: config.ProvidersConfig{PetitLyricsCooldownSeconds: 60},
	}

	got, ok := configValue(cfg, "providers.petitlyrics_cooldown_seconds")
	if !ok {
		t.Fatal("configValue(providers.petitlyrics_cooldown_seconds) ok = false; want true")
	}
	if got != "60" {
		t.Fatalf("configValue = %q; want %q", got, "60")
	}
	if !slices.Contains(configKeys(), "providers.petitlyrics_cooldown_seconds") {
		t.Fatal("configKeys missing providers.petitlyrics_cooldown_seconds")
	}

	if err := setConfigValue(&cfg, "providers.petitlyrics_cooldown_seconds", "90"); err != nil {
		t.Fatalf("setConfigValue: %v", err)
	}
	if cfg.Providers.PetitLyricsCooldownSeconds != 90 {
		t.Fatalf("petitlyrics_cooldown_seconds = %d; want 90", cfg.Providers.PetitLyricsCooldownSeconds)
	}

	// 0 is the documented "use api.cooldown" sentinel, so it must be accepted.
	if err := setConfigValue(&cfg, "providers.petitlyrics_cooldown_seconds", "0"); err != nil {
		t.Fatalf("setConfigValue 0: %v (0 is the api.cooldown-fallback sentinel)", err)
	}
	if cfg.Providers.PetitLyricsCooldownSeconds != 0 {
		t.Fatalf("petitlyrics_cooldown_seconds = %d; want 0", cfg.Providers.PetitLyricsCooldownSeconds)
	}

	// A negative carries no meaning here (0 already means fall back), so unlike
	// race_wait_seconds it is rejected rather than stored.
	if err := setConfigValue(&cfg, "providers.petitlyrics_cooldown_seconds", "-1"); err == nil {
		t.Fatal("setConfigValue accepted a negative providers.petitlyrics_cooldown_seconds")
	}
	if err := setConfigValue(&cfg, "providers.petitlyrics_cooldown_seconds", "abc"); err == nil {
		t.Fatal("setConfigValue accepted a non-integer providers.petitlyrics_cooldown_seconds")
	}
}

// TestPetitLyricsCooldownValidatorMatchesCLI pins the CLI arm and the web save
// path to the SAME non-negative rule (#535). The web path validates through
// config.ValidateAndSet (registry-driven, TypeInt -> ValidateNonNegativeInt)
// while the CLI arm hand-rolls its bound, so the two could silently diverge and
// let a value in through one surface that the other rejects.
func TestPetitLyricsCooldownValidatorMatchesCLI(t *testing.T) {
	const path = "providers.petitlyrics_cooldown_seconds"
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"90", true},
		{"0", true},
		{"-1", false},
		{"abc", false},
	} {
		webErr := config.ValidateAndSet(path, tc.value)
		cliErr := setConfigValue(&config.Config{}, path, tc.value)
		if (webErr == nil) != tc.valid {
			t.Errorf("ValidateAndSet(%q) err = %v; want valid=%v", tc.value, webErr, tc.valid)
		}
		if (webErr == nil) != (cliErr == nil) {
			t.Errorf("value %q: web accepts=%v but CLI accepts=%v; the two surfaces disagree",
				tc.value, webErr == nil, cliErr == nil)
		}
	}
}

func TestConfigInstrumentalDetectorOrderingGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		InstrumentalDetector: config.InstrumentalDetectorConfig{Ordering: "demoted"},
	}
	got, ok := configValue(cfg, "instrumental_detector.ordering")
	if !ok {
		t.Fatal("configValue(instrumental_detector.ordering) ok = false; want true")
	}
	if got != "demoted" {
		t.Fatalf("configValue(instrumental_detector.ordering) = %q; want %q", got, "demoted")
	}
	if !slices.Contains(configKeys(), "instrumental_detector.ordering") {
		t.Fatal("configKeys missing instrumental_detector.ordering")
	}

	for _, ordering := range []string{"front", "demoted"} {
		if err := setConfigValue(&cfg, "instrumental_detector.ordering", ordering); err != nil {
			t.Fatalf("setConfigValue instrumental_detector.ordering=%q: %v", ordering, err)
		}
		if cfg.InstrumentalDetector.Ordering != ordering {
			t.Fatalf("instrumental_detector.ordering = %q; want %q", cfg.InstrumentalDetector.Ordering, ordering)
		}
	}
	if err := setConfigValue(&cfg, "instrumental_detector.ordering", "sideways"); err == nil {
		t.Fatal("setConfigValue accepted an unknown instrumental_detector.ordering")
	}
}

func TestConfigBilingualOutputGetSetRoundTrip(t *testing.T) {
	cfg := config.Config{
		Output: config.OutputConfig{BilingualOutput: true},
	}
	got, ok := configValue(cfg, "output.bilingual_output")
	if !ok {
		t.Fatal("configValue(output.bilingual_output) ok = false; want true")
	}
	if got != "true" {
		t.Fatalf("configValue(output.bilingual_output) = %q; want \"true\"", got)
	}
	if !slices.Contains(configKeys(), "output.bilingual_output") {
		t.Fatal("configKeys missing output.bilingual_output")
	}

	if err := setConfigValue(&cfg, "output.bilingual_output", "false"); err != nil {
		t.Fatalf("setConfigValue bilingual_output false: %v", err)
	}
	if cfg.Output.BilingualOutput {
		t.Fatal("BilingualOutput = true; want false after set")
	}
	if err := setConfigValue(&cfg, "output.bilingual_output", "true"); err != nil {
		t.Fatalf("setConfigValue bilingual_output true: %v", err)
	}
	if !cfg.Output.BilingualOutput {
		t.Fatal("BilingualOutput = false; want true after set")
	}
	if err := setConfigValue(&cfg, "output.bilingual_output", "notabool"); err == nil {
		t.Fatal("setConfigValue accepted invalid output.bilingual_output")
	}
}

func TestConfigKeysIncludesVerificationFFmpegPath(t *testing.T) {
	for _, key := range configKeys() {
		if key == "verification.ffmpeg_path" {
			return
		}
	}
	t.Fatal("configKeys missing verification.ffmpeg_path")
}

func TestCircuitOpenDurationConfigKey(t *testing.T) {
	cfg := config.Config{API: config.APIConfig{CircuitOpenDuration: 1800}}

	got, ok := configValue(cfg, "api.circuit_open_duration")
	if !ok {
		t.Fatal("configValue(api.circuit_open_duration) ok = false; want true")
	}
	if got != "1800" {
		t.Fatalf("configValue(api.circuit_open_duration) = %q; want %q", got, "1800")
	}

	if err := setConfigValue(&cfg, "api.circuit_open_duration", "600"); err != nil {
		t.Fatalf("setConfigValue valid: %v", err)
	}
	if cfg.API.CircuitOpenDuration != 600 {
		t.Fatalf("CircuitOpenDuration = %d; want 600", cfg.API.CircuitOpenDuration)
	}
	for _, bad := range []string{"", "abc", "0", "-30"} {
		if err := setConfigValue(&cfg, "api.circuit_open_duration", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid api.circuit_open_duration %q", bad)
		}
	}

	if !slices.Contains(configKeys(), "api.circuit_open_duration") {
		t.Fatal("configKeys missing api.circuit_open_duration")
	}
}

func TestServerIntervalConfigKeys(t *testing.T) {
	cfg := config.Config{Server: config.ServerConfig{ScanIntervalSeconds: 900, WorkIntervalSeconds: 20}}

	for key, want := range map[string]string{
		"server.scan_interval_seconds": "900",
		"server.work_interval_seconds": "20",
	} {
		got, ok := configValue(cfg, key)
		if !ok {
			t.Fatalf("configValue(%s) ok = false; want true", key)
		}
		if got != want {
			t.Fatalf("configValue(%s) = %q; want %q", key, got, want)
		}
		if !slices.Contains(configKeys(), key) {
			t.Fatalf("configKeys missing %s", key)
		}
	}

	if err := setConfigValue(&cfg, "server.scan_interval_seconds", "0"); err != nil {
		t.Fatalf("setConfigValue scan 0: %v (zero must be allowed to disable repeat)", err)
	}
	if cfg.Server.ScanIntervalSeconds != 0 {
		t.Fatalf("ScanIntervalSeconds = %d; want 0", cfg.Server.ScanIntervalSeconds)
	}
	if err := setConfigValue(&cfg, "server.work_interval_seconds", "30"); err != nil {
		t.Fatalf("setConfigValue work 30: %v", err)
	}
	if cfg.Server.WorkIntervalSeconds != 30 {
		t.Fatalf("WorkIntervalSeconds = %d; want 30", cfg.Server.WorkIntervalSeconds)
	}
	for _, bad := range []string{"", "abc", "-1"} {
		if err := setConfigValue(&cfg, "server.scan_interval_seconds", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid server.scan_interval_seconds %q", bad)
		}
		if err := setConfigValue(&cfg, "server.work_interval_seconds", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid server.work_interval_seconds %q", bad)
		}
	}
}

func isolateCommandsEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{
		"MUSIXMATCH_TOKEN", "MXLRC_API_TOKEN",
		"MXLRC_API_COOLDOWN", "MXLRC_COOLDOWN",
		"MXLRC_OUTPUT_DIR", "MXLRC_DB_PATH", "MXLRC_SERVER_ADDR", "MXLRC_WEBHOOK_API_KEY",
		"MXLRC_PROVIDER_PRIMARY", "MXLRC_PROVIDERS_DISABLED",
		"MXLRC_VERIFICATION_ENABLED", "MXLRC_VERIFICATION_WHISPER_URL", "MXLRC_WHISPER_URL",
		"MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "MXLRC_VERIFICATION_SAMPLE_DURATION",
		"MXLRC_VERIFICATION_MIN_CONFIDENCE", "MXLRC_VERIFICATION_MIN_SIMILARITY",
	} {
		t.Setenv(v, "")
	}
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
}

func writeCommandsConfig(t *testing.T, dbPath string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	content := "[db]\npath = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg
}

var _ lyrics.Writer = fakeWriter{}

// commandsTestEnv prepares an isolated config + DB and pre-seeds a library and
// optional queue/scan rows. Returns the config path used by run* helpers.
func commandsTestEnv(t *testing.T) (cfgPath string, dbPath string) {
	t.Helper()
	isolateCommandsEnv(t)
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "state", "test.db")
	cfgPath = writeCommandsConfig(t, dbPath)
	return cfgPath, dbPath
}

func TestRunQueueList_FiltersByStatus(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false) // this test asserts FIFO dequeue order
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Pending", TrackName: "Track"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Failing", TrackName: "Track"}}, 1); err != nil {
		t.Fatalf("Enqueue 2: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, claimed.ID, errorsNew("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "list", "--status", "failed", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("Run exit code = %d; want 0; out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "ID") || !strings.Contains(got, "Status") || !strings.Contains(got, "LastError") {
		t.Fatalf("missing header in output: %q", got)
	}
	// The first dequeue (FIFO) claims the row inserted first ("Pending"),
	// which is then failed. So a single failed row shows up; the "Failing"
	// row stays pending and must NOT appear under --status=failed.
	if !strings.Contains(got, "Pending") {
		t.Fatalf("expected failed row (artist=Pending) in output: %q", got)
	}
	if strings.Contains(got, "Failing") {
		t.Fatalf("status filter leaked pending row: %q", got)
	}
}

func TestRunQueueRetry_RejectsNonFailedRow(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Pending"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "retry", strconvFormatInt(item.ID), "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("Run exit code = 0; want non-zero (retry on pending must fail). out=%s", out.String())
	}
	if !strings.Contains(out.String(), "not in failed status") {
		t.Fatalf("expected not-retryable message; got %q", out.String())
	}
}

func TestRunQueueRetry_ResetsFailedRow(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, claimed.ID, errorsNew("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "retry", strconvFormatInt(claimed.ID), "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("Run exit code = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "retried") {
		t.Fatalf("expected retried message; got %q", out.String())
	}

	// Verify state.
	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB.Close()
	items, err := queue.NewDBQueue(sqlDB).List(ctx, queue.ListFilter{Status: queue.StatusPending})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 || items[0].Attempts != 0 {
		t.Fatalf("post-retry pending items = %+v; want one with attempts=0", items)
	}
}

func TestRunQueueClear_DryRunDoesNotDelete(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Title"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, claimed.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "clear", "--done", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("Run dry-run exit code = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "would delete 1") {
		t.Fatalf("expected dry-run message; got %q", out.String())
	}

	// Re-open and confirm row still there.
	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	c, err := queue.NewDBQueue(sqlDB).CountDone(ctx)
	if err != nil {
		t.Fatalf("CountDone: %v", err)
	}
	_ = sqlDB.Close()
	if c != 1 {
		t.Fatalf("dry-run deleted rows: CountDone=%d, want 1", c)
	}

	// Now actually delete.
	out.Reset()
	code = Run(ctx, []string{"queue", "clear", "--done", "--yes", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("Run --yes exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "deleted 1") {
		t.Fatalf("expected deletion message; got %q", out.String())
	}
}

func TestRunScanResults_ResolvesLibraryByNameAndID(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "MusicLib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_ = sqlDB.Close()

	for _, ref := range []string{"MusicLib", strconvFormatInt(lib.ID)} {
		var out bytes.Buffer
		code := Run(ctx, []string{"scan", "results", "--library", ref, "--config", cfg}, &out, Deps{})
		if code != 0 {
			t.Fatalf("Run --library=%q exit code = %d; want 0; out=%s", ref, code, out.String())
		}
		if !strings.Contains(out.String(), "/music/a.mp3") {
			t.Fatalf("expected scan_result row for ref=%q; got %q", ref, out.String())
		}
		if !strings.Contains(out.String(), "MusicLib") {
			t.Fatalf("expected library name in output for ref=%q; got %q", ref, out.String())
		}
	}
}

func TestRunScanClear_DryRunAndConfirm(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "MusicLib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{
		{FilePath: "/music/a.mp3", Track: models.Track{ArtistName: "Artist", TrackName: "A"}},
		{FilePath: "/music/b.mp3", Track: models.Track{ArtistName: "Artist", TrackName: "B"}},
	}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_ = sqlDB.Close()

	// Dry run.
	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "clear", "--library", "MusicLib", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("dry-run exit code = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "would delete 2") {
		t.Fatalf("expected dry-run message; got %q", out.String())
	}

	// Confirm dry run did nothing.
	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	c, err := scan.New(sqlDB).CountByLibrary(ctx, lib.ID)
	if err != nil {
		t.Fatalf("CountByLibrary: %v", err)
	}
	_ = sqlDB.Close()
	if c != 2 {
		t.Fatalf("CountByLibrary post dry-run = %d; want 2", c)
	}

	// Confirm.
	out.Reset()
	code = Run(ctx, []string{"scan", "clear", "--library", "MusicLib", "--yes", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("--yes exit code = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "deleted 2") {
		t.Fatalf("expected deletion message; got %q", out.String())
	}

	// Confirm library still exists.
	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB.Close()
	if _, err := library.New(sqlDB).Get(ctx, lib.ID); err != nil {
		t.Fatalf("library removed by scan clear: %v", err)
	}
}

func TestRunScanClear_CancelsLinkedWorkQueueRow(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "MusicLib", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	pending, err := scanRepo.ListPendingByLibrary(ctx, lib.ID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingByLibrary: %v / %d rows", err, len(pending))
	}
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Title"},
		OutputPaths:  []models.OutputPath{{Outdir: "/music", Filename: "a.lrc"}},
		ScanResultID: pending[0].ID,
	}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	_ = sqlDB.Close()

	// Dry run: must report the queue cancellation.
	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "clear", "--library", "MusicLib", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("dry-run exit = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "cancel 1") {
		t.Fatalf("dry-run output missing queue cancel count: %q", out.String())
	}

	// Confirm: queue row must be gone.
	out.Reset()
	code = Run(ctx, []string{"scan", "clear", "--library", "MusicLib", "--yes", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("--yes exit = %d; want 0; out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "canceled 1") {
		t.Fatalf("--yes output missing queue cancel count: %q", out.String())
	}

	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB.Close()
	var qCount int
	if err := sqlDB.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue`).Scan(&qCount); err != nil {
		t.Fatalf("count work_queue: %v", err)
	}
	if qCount != 0 {
		t.Fatalf("work_queue rows after scan clear = %d; want 0", qCount)
	}
}

func TestRunScan_LibrarySelectorScansOnlyTheNamedLibrary(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	// Two real, empty directories so scanner.ScanLibrary does not error.
	dirA := t.TempDir()
	dirB := t.TempDir()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	libA, err := libRepo.Add(ctx, dirA, "Alpha", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add Alpha: %v", err)
	}
	libB, err := libRepo.Add(ctx, dirB, "Beta", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add Beta: %v", err)
	}
	// Seed a stale scan_results row in each library; the scan should leave
	// the unselected library's row alone (the scanner walks the empty dir
	// and finds nothing, but Upsert is not invoked for unselected libs).
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, libA.ID, []models.ScanResult{{
		FilePath: filepath.Join(dirA, "stale.mp3"),
		Track:    models.Track{ArtistName: "Stale", TrackName: "A"},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert A: %v", err)
	}
	if err := scanRepo.Upsert(ctx, libB.ID, []models.ScanResult{{
		FilePath: filepath.Join(dirB, "stale.mp3"),
		Track:    models.Track{ArtistName: "Stale", TrackName: "B"},
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert B: %v", err)
	}
	// Make Beta's directory unscannable. With --only Alpha, Beta is never
	// walked, so this is harmless; but if the selector were broken and Beta
	// got scanned, scanner.ScanLibrary would hit the missing directory and
	// error (scheduler propagates it), forcing a non-zero exit and failing
	// the test. Without this, both seed rows survive and the test passes even
	// if nothing was scanned at all.
	if err := os.RemoveAll(dirB); err != nil {
		t.Fatalf("remove Beta dir: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "--only", "Alpha", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("scan --library Alpha exit = %d; want 0; out=%s", code, out.String())
	}

	// Reopen and confirm: Alpha was rescanned (Upsert ran against an empty
	// directory, so its only seeded row is preserved by default UpsertOptions
	// status-preserving semantics). Beta must not have been touched at all.
	// We assert behaviorally by checking both libraries still hold their seed
	// row; the more meaningful assertion is that the run exited 0 with a
	// non-existent Beta directory would have errored had it been scanned.
	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB.Close()
	countA, err := scan.New(sqlDB).CountByLibrary(ctx, libA.ID)
	if err != nil {
		t.Fatalf("CountByLibrary A: %v", err)
	}
	countB, err := scan.New(sqlDB).CountByLibrary(ctx, libB.ID)
	if err != nil {
		t.Fatalf("CountByLibrary B: %v", err)
	}
	if countA != 1 || countB != 1 {
		t.Fatalf("CountByLibrary A=%d B=%d; want 1 each", countA, countB)
	}
}

func TestRunScan_LibrarySelectorReportsScanFailure(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	dir := t.TempDir()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if _, err := library.New(sqlDB).Add(ctx, dir, "Alpha", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add Alpha: %v", err)
	}
	_ = sqlDB.Close()

	// Remove the selected library's directory so scanner.ScanLibrary errors
	// when the scheduler walks it. The selector path must surface that as a
	// non-zero exit rather than swallowing it.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}

	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "--only", "Alpha", "--config", cfg}, &out, Deps{})
	if code != 1 {
		t.Fatalf("scan --only Alpha with missing dir: exit = %d; want 1; out=%s", code, out.String())
	}
}

func TestRunScan_LibrarySelectorRejectsUnknownName(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"scan", "--only", "no-such-library", "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("unknown library: exit code 0; want non-zero. out=%s", out.String())
	}
	if !strings.Contains(out.String(), "no-such-library") {
		t.Fatalf("output missing offending library reference: %q", out.String())
	}
}

func TestRunScan_LibrarySelectorRejectsAmbiguousName(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	// Two libraries where the second one is literally named after the first
	// one's numeric id. Looking up "1" resolves to both: ID match (lib1) and
	// name match (lib2). resolveLibrary must reject that as ambiguous.
	lib1, err := libRepo.Add(ctx, t.TempDir(), "music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add lib1: %v", err)
	}
	if _, err := libRepo.Add(ctx, t.TempDir(), strconvFormatInt(lib1.ID), models.LibrarySettings{}); err != nil {
		t.Fatalf("Add lib2: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"scan", "--only", strconvFormatInt(lib1.ID), "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("ambiguous library: exit code 0; want non-zero. out=%s", out.String())
	}
	if !strings.Contains(out.String(), "ambiguous") {
		t.Fatalf("output missing 'ambiguous': %q", out.String())
	}
}

func errorsNew(s string) error { return errors.New(s) }

func strconvFormatInt(i int64) string { return strconv.FormatInt(i, 10) }

func TestValidateQueueStatus(t *testing.T) {
	for _, ok := range []string{"", "pending", "processing", "failed", "done"} {
		if err := validateQueueStatus(ok); err != nil {
			t.Errorf("validateQueueStatus(%q) = %v; want nil", ok, err)
		}
	}
	for _, bad := range []string{"running", "PENDING", "x"} {
		if err := validateQueueStatus(bad); err == nil {
			t.Errorf("validateQueueStatus(%q) = nil; want error", bad)
		}
	}
}

func TestValidateScanStatus(t *testing.T) {
	for _, ok := range []string{"", "pending", "processing", "done"} {
		if err := validateScanStatus(ok); err != nil {
			t.Errorf("validateScanStatus(%q) = %v; want nil", ok, err)
		}
	}
	// scan_results never transitions to "failed" anywhere in the codebase, so
	// validateScanStatus deliberately rejects it to avoid surfacing an
	// always-empty filter.
	for _, bad := range []string{"failed", "queued", "DONE", "?"} {
		if err := validateScanStatus(bad); err == nil {
			t.Errorf("validateScanStatus(%q) = nil; want error", bad)
		}
	}
}

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 5, "ab..."},
		{"abcdef", 3, "abc"},
		{"abcdef", 1, "a"},
		{"abcdef", 0, "abcdef"},
		{"abcdef", -1, "abcdef"},
	}
	for _, tc := range cases {
		got := truncate(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("truncate(%q, %d) = %q; want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

func TestRunQueueList_InvalidStatusReturnsError(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"queue", "list", "--status", "bogus", "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("queue list with bogus status: exit code 0; want non-zero. out=%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid status") {
		t.Fatalf("output missing 'invalid status': %q", out.String())
	}
}

func TestRunQueueRetry_MissingIDSurfacesNotFound(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"queue", "retry", "--config", cfg, "9999"}, &out, Deps{})
	if code == 0 {
		t.Fatalf("queue retry of missing id: exit code 0; want non-zero. out=%s", out.String())
	}
}

func TestRunQueueClear_ConfirmDeletes(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "A", TrackName: "Done"}}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if err := q.Complete(ctx, item.ID); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "clear", "--done", "--yes", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("queue clear --yes exit code = %d; want 0. out=%s", code, out.String())
	}
}

func TestRunScanResults_InvalidStatusReturnsError(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"scan", "results", "--status", "bogus", "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("scan results bogus status: exit code 0; want non-zero. out=%s", out.String())
	}
	if !strings.Contains(out.String(), "invalid status") {
		t.Fatalf("output missing 'invalid status': %q", out.String())
	}
}

func TestRunScanClear_RequiresLibrary(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"scan", "clear", "--library", "Nonexistent", "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("scan clear unknown library: exit code 0; want non-zero. out=%s", out.String())
	}
}

func TestResolveLibraryRejectsBlankRef(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	repo := library.New(sqlDB)
	if _, err := resolveLibrary(ctx, repo, ""); err == nil {
		t.Fatal("resolveLibrary empty ref returned nil error")
	}
	if _, err := resolveLibrary(ctx, repo, "  "); err == nil {
		t.Fatal("resolveLibrary whitespace ref returned nil error")
	}
	_ = cfg
}

func TestRunQueueCmd_MissingSubcommand(t *testing.T) {
	var out bytes.Buffer
	code := runQueueCmd(context.Background(), &out, QueueCmd{})
	if code != 2 {
		t.Fatalf("runQueueCmd empty = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "missing queue subcommand") {
		t.Fatalf("output = %q; want missing-subcommand message", out.String())
	}
}

func TestRunQueueCmd_FailedRoutesToList(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueCmd(context.Background(), &out, QueueCmd{Failed: &QueueFailedCmd{ConfigPath: cfg, Limit: 5}})
	if code != 0 {
		t.Fatalf("queue failed = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ID") {
		t.Fatalf("queue failed missing header: %q", out.String())
	}
}

func TestRunQueueClear_RequiresDoneFlag(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueClear(context.Background(), &out, QueueClearCmd{ConfigPath: cfg, Done: false})
	if code != 2 {
		t.Fatalf("queue clear without --done = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "requires --done") {
		t.Fatalf("output = %q; want --done required message", out.String())
	}
}

func TestRunScanResults_EmptyDBPrintsHeaderOnly(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runScanResults(context.Background(), &out, ScanResultsCmd{ConfigPath: cfg})
	if code != 0 {
		t.Fatalf("scan results empty = %d; want 0. out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "ID") || !strings.Contains(got, "Library") {
		t.Fatalf("output missing header: %q", got)
	}
}

func TestRunQueueList_EmptyDBPrintsHeaderOnly(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueList(context.Background(), &out, QueueListCmd{ConfigPath: cfg})
	if code != 0 {
		t.Fatalf("queue list empty = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ID") {
		t.Fatalf("output missing header: %q", out.String())
	}
}

func TestRunQueueClear_DryRunCountsZero(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueClear(context.Background(), &out, QueueClearCmd{ConfigPath: cfg, Done: true, Yes: false})
	if code != 0 {
		t.Fatalf("dry-run on empty db = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "would delete 0") {
		t.Fatalf("dry-run output = %q; want 'would delete 0'", out.String())
	}
}

func TestRunScanClear_DryRunOnEmpty(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	if _, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add library: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runScanClear(ctx, &out, ScanClearCmd{ConfigPath: cfg, Library: "Music", Yes: false})
	if code != 0 {
		t.Fatalf("scan clear dry-run = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "would delete 0") {
		t.Fatalf("scan clear dry-run output = %q; want 'would delete 0'", out.String())
	}
}

func TestRunScanClear_RequiresLibraryFlag(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runScanClear(context.Background(), &out, ScanClearCmd{ConfigPath: cfg})
	if code == 0 {
		t.Fatalf("scan clear without --library = 0; want non-zero")
	}
}

func TestRunScanResults_FilterByLibraryByID(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "A", TrackName: "Track"},
		Outdir:   "/music",
		Filename: "a.lrc",
		Status:   scan.StatusPending,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runScanResults(ctx, &out, ScanResultsCmd{ConfigPath: cfg, Library: strconv.FormatInt(lib.ID, 10)})
	if code != 0 {
		t.Fatalf("scan results --library <id> = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "/music/a.mp3") {
		t.Fatalf("scan results --library <id> missing row: %q", out.String())
	}
}

func TestResolveLibrary_NumericNameLooksUpByName(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	repo := library.New(sqlDB)
	added, err := repo.Add(ctx, "/music/numeric", "9999", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, err := resolveLibrary(ctx, repo, "9999")
	if err != nil {
		t.Fatalf("resolveLibrary numeric-name: %v", err)
	}
	if got.ID != added.ID {
		t.Fatalf("resolveLibrary id = %d; want %d", got.ID, added.ID)
	}
	_ = cfg
}

func TestResolveLibrary_NumericRefAmbiguousIDvsName(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	repo := library.New(sqlDB)
	// Library #1 (Music)
	if _, err := repo.Add(ctx, "/music/a", "Music", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add a: %v", err)
	}
	// Library #2 named "1" — ref "1" now matches BOTH id=1 and name="1".
	if _, err := repo.Add(ctx, "/music/b", "1", models.LibrarySettings{}); err != nil {
		t.Fatalf("Add b: %v", err)
	}

	_, err = resolveLibrary(ctx, repo, "1")
	if err == nil {
		t.Fatal("resolveLibrary returned nil error for ambiguous ID-vs-name match")
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v; want substring 'ambiguous'", err)
	}
	_ = cfg
}

func TestRunScanResults_FilterByStatus(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	rows := []models.ScanResult{
		{FilePath: "/music/a.mp3", Track: models.Track{ArtistName: "A", TrackName: "Pending"}, Outdir: "/music", Filename: "a.lrc", Status: scan.StatusPending},
		{FilePath: "/music/b.mp3", Track: models.Track{ArtistName: "B", TrackName: "Done"}, Outdir: "/music", Filename: "b.lrc", Status: scan.StatusPending},
	}
	if err := scanRepo.Upsert(ctx, lib.ID, rows, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE scan_results SET status = 'done' WHERE artist = 'B'`); err != nil {
		t.Fatalf("set status done: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runScanResults(ctx, &out, ScanResultsCmd{ConfigPath: cfg, Status: "done"})
	if code != 0 {
		t.Fatalf("scan results --status done: %d. out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "/music/b.mp3") {
		t.Fatalf("want done row in output: %q", got)
	}
	if strings.Contains(got, "/music/a.mp3") {
		t.Fatalf("filter leaked pending row: %q", got)
	}
}

func TestRunScanResults_LimitTrimsResults(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	var rows []models.ScanResult
	for i := 0; i < 5; i++ {
		rows = append(rows, models.ScanResult{
			FilePath: "/music/" + strconv.Itoa(i) + ".mp3",
			Track:    models.Track{ArtistName: "A", TrackName: strconv.Itoa(i)},
			Outdir:   "/music",
			Filename: strconv.Itoa(i) + ".lrc",
			Status:   scan.StatusPending,
		})
	}
	if err := scanRepo.Upsert(ctx, lib.ID, rows, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runScanResults(ctx, &out, ScanResultsCmd{ConfigPath: cfg, Limit: 2})
	if code != 0 {
		t.Fatalf("scan results --limit 2: %d. out=%s", code, out.String())
	}
	count := strings.Count(out.String(), "/music/")
	if count != 2 {
		t.Fatalf("limit 2 returned %d data rows; want 2. out=%s", count, out.String())
	}
}

func TestRunQueueRetry_ResetsLinkedScanResults(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	libRepo := library.New(sqlDB)
	lib, err := libRepo.Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	scanRepo := scan.New(sqlDB)
	if err := scanRepo.Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/song.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Song"},
		Outdir:   "/music",
		Filename: "song.lrc",
		Status:   scan.StatusPending,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	var scanID int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM scan_results LIMIT 1`).Scan(&scanID); err != nil {
		t.Fatalf("scan id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Artist", TrackName: "Song"},
		ScanResultID: scanID,
	}, 1)
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := q.Dequeue(ctx); err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Fail(ctx, item.ID, errors.New("rate limited")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE scan_results SET status = 'processing' WHERE id = ?`, scanID); err != nil {
		t.Fatalf("seed processing: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "retry", "--config", cfg, strconv.FormatInt(item.ID, 10)}, &out, Deps{})
	if code != 0 {
		t.Fatalf("queue retry: %d. out=%s", code, out.String())
	}

	sqlDB, err = db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer func() { _ = sqlDB.Close() }()
	var scanStatus string
	if err := sqlDB.QueryRowContext(ctx, `SELECT status FROM scan_results WHERE id = ?`, scanID).Scan(&scanStatus); err != nil {
		t.Fatalf("read scan_results status: %v", err)
	}
	if scanStatus != "pending" {
		t.Fatalf("scan_results status = %q after retry; want pending", scanStatus)
	}
}

// TestValidateQueueStatus_AcceptsDeferred verifies the deferred status is
// accepted by the validator (and that the error message names it).
func TestValidateQueueStatus_AcceptsDeferred(t *testing.T) {
	if err := validateQueueStatus(queue.StatusDeferred); err != nil {
		t.Fatalf("validateQueueStatus(%q) = %v; want nil", queue.StatusDeferred, err)
	}
	err := validateQueueStatus("bogus")
	if err == nil {
		t.Fatal("validateQueueStatus(bogus) = nil; want error")
	}
	if !strings.Contains(err.Error(), "deferred") {
		t.Fatalf("error message missing 'deferred': %q", err.Error())
	}
}

// TestRunQueueRetry_RejectsDeferredRow verifies that retrying a deferred row
// returns ErrNotRetryable with an informative message pointing at queue deferred.
func TestRunQueueRetry_RejectsDeferredRow(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Artist", TrackName: "Deferred"}}, 1); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	claimed, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, claimed.ID, 7*24*time.Hour, errorsNew("no results")); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := Run(ctx, []string{"queue", "retry", strconvFormatInt(claimed.ID), "--config", cfg}, &out, Deps{})
	if code == 0 {
		t.Fatalf("Run exit code = 0; want non-zero (retry on deferred must fail). out=%s", out.String())
	}
	if !strings.Contains(out.String(), "not in failed status") {
		t.Fatalf("expected not-retryable message; got %q", out.String())
	}
	if !strings.Contains(out.String(), "deferred") {
		t.Fatalf("expected deferred hint in message; got %q", out.String())
	}
}

// TestRunQueueCmd_DeferredRoutesToList verifies the `queue deferred` convenience
// subcommand returns the table header (and only lists deferred rows, not failed).
func TestRunQueueCmd_DeferredRoutesToList(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)

	// Enqueue one item, defer it (becomes StatusDeferred).
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Missed", TrackName: "Song"}}, 1); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}
	ditem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, ditem.ID, 7*24*time.Hour, errorsNew("no results")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	// Enqueue another item, fail it (becomes StatusFailed).
	if _, err := q.Enqueue(ctx, models.Inputs{Track: models.Track{ArtistName: "Failed", TrackName: "Song"}}, 1); err != nil {
		t.Fatalf("Enqueue failed: %v", err)
	}
	fitem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue failed: %v", err)
	}
	if _, err := q.Fail(ctx, fitem.ID, errorsNew("boom")); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runQueueCmd(ctx, &out, QueueCmd{Deferred: &QueueDeferredCmd{ConfigPath: cfg, Limit: 50}})
	if code != 0 {
		t.Fatalf("queue deferred = %d; want 0. out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "ID") {
		t.Fatalf("queue deferred missing header: %q", got)
	}
	// Deferred artist must appear; failed artist must not.
	if !strings.Contains(got, "Missed") {
		t.Fatalf("deferred row missing from output: %q", got)
	}
	if strings.Contains(got, "Failed") {
		t.Fatalf("failed row must not appear in queue deferred output: %q", got)
	}
}

// TestRunQueueList_StatusDeferred verifies `queue list --status deferred` works.
func TestRunQueueList_StatusDeferred(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := Run(context.Background(), []string{"queue", "list", "--status", "deferred", "--config", cfg}, &out, Deps{})
	if code != 0 {
		t.Fatalf("queue list --status deferred = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "ID") {
		t.Fatalf("output missing header: %q", out.String())
	}
}

// TestMissCadenceConfigKeys verifies that the three miss-cadence knobs are
// accessible via configValue, setConfigValue, and configKeys.
func TestMissCadenceConfigKeys(t *testing.T) {
	cfg := config.Config{API: config.APIConfig{
		MissBackoffBaseHours: 168,
		MissBackoffCapHours:  672,
		MaxMissAttempts:      15,
	}}

	tests := []struct {
		key  string
		want string
	}{
		{"api.miss_backoff_base_hours", "168"},
		{"api.miss_backoff_cap_hours", "672"},
		{"api.max_miss_attempts", "15"},
	}
	for _, tc := range tests {
		got, ok := configValue(cfg, tc.key)
		if !ok {
			t.Fatalf("configValue(%q) ok = false; want true", tc.key)
		}
		if got != tc.want {
			t.Fatalf("configValue(%q) = %q; want %q", tc.key, got, tc.want)
		}
	}

	// setConfigValue round-trip.
	if err := setConfigValue(&cfg, "api.miss_backoff_base_hours", "48"); err != nil {
		t.Fatalf("setConfigValue miss_backoff_base_hours: %v", err)
	}
	if cfg.API.MissBackoffBaseHours != 48 {
		t.Fatalf("MissBackoffBaseHours = %d; want 48", cfg.API.MissBackoffBaseHours)
	}

	if err := setConfigValue(&cfg, "api.miss_backoff_cap_hours", "336"); err != nil {
		t.Fatalf("setConfigValue miss_backoff_cap_hours: %v", err)
	}
	if cfg.API.MissBackoffCapHours != 336 {
		t.Fatalf("MissBackoffCapHours = %d; want 336", cfg.API.MissBackoffCapHours)
	}

	if err := setConfigValue(&cfg, "api.max_miss_attempts", "10"); err != nil {
		t.Fatalf("setConfigValue max_miss_attempts: %v", err)
	}
	if cfg.API.MaxMissAttempts != 10 {
		t.Fatalf("MaxMissAttempts = %d; want 10", cfg.API.MaxMissAttempts)
	}

	// Reject invalid values.
	for _, bad := range []string{"", "abc", "-1", "0"} {
		if err := setConfigValue(&cfg, "api.miss_backoff_base_hours", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid miss_backoff_base_hours %q", bad)
		}
		if err := setConfigValue(&cfg, "api.miss_backoff_cap_hours", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid miss_backoff_cap_hours %q", bad)
		}
	}
	for _, bad := range []string{"", "abc", "-1"} {
		if err := setConfigValue(&cfg, "api.max_miss_attempts", bad); err == nil {
			t.Fatalf("setConfigValue accepted invalid max_miss_attempts %q", bad)
		}
	}
	// 0 is valid for max_miss_attempts (means no cap).
	if err := setConfigValue(&cfg, "api.max_miss_attempts", "0"); err != nil {
		t.Fatalf("setConfigValue(max_miss_attempts, 0) = %v; want nil (0 is valid)", err)
	}

	// All three must appear in configKeys.
	for _, key := range []string{"api.miss_backoff_base_hours", "api.miss_backoff_cap_hours", "api.max_miss_attempts"} {
		if !slices.Contains(configKeys(), key) {
			t.Fatalf("configKeys missing %q", key)
		}
	}
}

func TestSelectedProviderWiresMinIntervalOnRealClient(t *testing.T) {
	cfg := config.Config{
		API:       config.APIConfig{Cooldown: 30},
		Providers: config.ProvidersConfig{Primary: "musixmatch"},
	}
	// Use the real NewClient factory so we get a *musixmatch.Client back.
	// Capture the client before selectedProvider wraps it so we can inspect it.
	var captured *musixmatch.Client
	_, err := selectedProvider(cfg, "token", func(token string) musixmatch.Fetcher {
		captured = musixmatch.NewClient(token)
		return captured
	})
	if err != nil {
		t.Fatalf("selectedProvider: %v", err)
	}
	if captured == nil {
		t.Fatal("factory was never called")
	}
	// selectedProvider must have called WithMinInterval(30s) on the captured client.
	if captured.MinInterval() != 30*time.Second {
		t.Fatalf("MinInterval = %v; want 30s (from cfg.API.Cooldown=30)", captured.MinInterval())
	}
}

func TestSelectedProviderFakeInjectionUnaffected(t *testing.T) {
	cfg := config.Config{
		API:       config.APIConfig{Cooldown: 30},
		Providers: config.ProvidersConfig{Primary: "musixmatch"},
	}
	// Fake fetcher: should be used unchanged; the type assertion inside
	// selectedProvider will not match, so the fake is not paced.
	var gotFetcher musixmatch.Fetcher
	_, err := selectedProvider(cfg, "token", func(token string) musixmatch.Fetcher {
		gotFetcher = fakeFetcher{}
		return gotFetcher
	})
	if err != nil {
		t.Fatalf("selectedProvider: %v", err)
	}
	if _, ok := gotFetcher.(fakeFetcher); !ok {
		t.Fatal("fake fetcher was replaced; injection seam is broken")
	}
}

// TestRunQueueRecheck_NoFlags verifies that calling queue recheck without
// --deferred or --retired returns exit code 2 and a helpful error message.
func TestRunQueueRecheck_NoFlags(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueRecheck(context.Background(), &out, QueueRecheckCmd{ConfigPath: cfg})
	if code != 2 {
		t.Fatalf("no-flags exit code = %d; want 2", code)
	}
	if !strings.Contains(out.String(), "--deferred") || !strings.Contains(out.String(), "--retired") {
		t.Fatalf("error message = %q; want --deferred and --retired mentioned", out.String())
	}
}

// TestRunQueueRecheck_DryRun verifies the dry-run path (no --yes): counts are
// printed without any modification and the "pass --yes" footer is shown.
func TestRunQueueRecheck_DryRun(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	// Seed one deferred row and one retired row.
	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Deferred", TrackName: "Song"},
	}, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue deferred: %v", err)
	}
	ditem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, ditem.ID, time.Hour, errorsNew("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}

	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Retired", TrackName: "Song"},
	}, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue retired: %v", err)
	}
	ritem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue retired: %v", err)
	}
	if _, err := q.RetireMiss(ctx, ritem.ID); err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runQueueRecheck(ctx, &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Deferred:   true,
		Retired:    true,
	})
	if code != 0 {
		t.Fatalf("dry-run exit code = %d; want 0. out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "would revive 1 deferred") {
		t.Fatalf("output missing deferred count: %q", got)
	}
	if !strings.Contains(got, "would revive 1 retired") {
		t.Fatalf("output missing retired count: %q", got)
	}
	if !strings.Contains(got, "pass --yes") {
		t.Fatalf("output missing --yes footer: %q", got)
	}

	// Rows must NOT have been changed.
	sqlDB2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB2.Close() //nolint:errcheck // test cleanup
	q2 := queue.NewDBQueue(sqlDB2)
	items, err := q2.List(ctx, queue.ListFilter{Status: queue.StatusDeferred})
	if err != nil {
		t.Fatalf("List deferred: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("deferred rows after dry-run = %d; want 1 (no change)", len(items))
	}
}

// TestRunQueueRecheck_Apply verifies the --yes path: rows are actually revived.
func TestRunQueueRecheck_Apply(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)

	// Seed one deferred row.
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Def", TrackName: "Track"},
	}, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	ditem, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, ditem.ID, 7*24*time.Hour, errorsNew("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runQueueRecheck(ctx, &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Deferred:   true,
		Yes:        true,
	})
	if code != 0 {
		t.Fatalf("apply exit code = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "revived 1 deferred") {
		t.Fatalf("output missing revive confirmation: %q", out.String())
	}
}

// TestRunQueueRecheck_ApplyRetired verifies the --retired apply path revives a
// row that was permanently retired after hitting the miss-attempt cap.
func TestRunQueueRecheck_ApplyRetired(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track: models.Track{ArtistName: "Ret", TrackName: "Track"},
	}, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.RetireMiss(ctx, item.ID); err != nil {
		t.Fatalf("RetireMiss: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runQueueRecheck(ctx, &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Retired:    true,
		Yes:        true,
	})
	if code != 0 {
		t.Fatalf("apply exit code = %d; want 0. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "revived 1 retired") {
		t.Fatalf("output missing retired revive confirmation: %q", out.String())
	}

	// The retired row must now be deferred again.
	sqlDB2, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer sqlDB2.Close() //nolint:errcheck // test cleanup
	items, err := queue.NewDBQueue(sqlDB2).List(ctx, queue.ListFilter{Status: queue.StatusDeferred})
	if err != nil {
		t.Fatalf("List deferred: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("deferred rows after retired revival = %d; want 1", len(items))
	}
}

// TestRunQueueRecheck_LibraryNotFound verifies that an unresolvable --library
// argument exits 1 with a not-found message and touches nothing.
func TestRunQueueRecheck_LibraryNotFound(t *testing.T) {
	cfg, _ := commandsTestEnv(t)
	var out bytes.Buffer
	code := runQueueRecheck(context.Background(), &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Deferred:   true,
		Library:    "no-such-library",
	})
	if code != 1 {
		t.Fatalf("library-not-found exit code = %d; want 1. out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "not found") {
		t.Fatalf("output missing not-found message: %q", out.String())
	}
}

// TestRunQueueRecheck_LibraryScoped verifies that --library resolves a library
// by name and scopes the revival to it, printing the library label.
func TestRunQueueRecheck_LibraryScoped(t *testing.T) {
	cfg, dbPath := commandsTestEnv(t)
	ctx := context.Background()

	sqlDB, err := db.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)

	// Seed a deferred row linked to library "Target" via a scan_result.
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES ('/target', 'Target') RETURNING id`,
	).Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	var srID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, artist, title, file_path, outdir, filename, status)
         VALUES (?, 'Scoped', 'T', '/target/s.flac', 'out', 's.lrc', 'processing')
         RETURNING id`,
		libID,
	).Scan(&srID); err != nil {
		t.Fatalf("insert scan_result: %v", err)
	}
	if _, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "Scoped", TrackName: "T"},
		ScanResultID: srID,
	}, queue.PriorityScan); err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	item, err := q.Dequeue(ctx)
	if err != nil {
		t.Fatalf("Dequeue: %v", err)
	}
	if _, err := q.Defer(ctx, item.ID, time.Hour, errorsNew("miss")); err != nil {
		t.Fatalf("Defer: %v", err)
	}
	_ = sqlDB.Close()

	var out bytes.Buffer
	code := runQueueRecheck(ctx, &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Deferred:   true,
		Library:    "Target",
		Yes:        true,
	})
	if code != 0 {
		t.Fatalf("scoped apply exit code = %d; want 0. out=%s", code, out.String())
	}
	got := out.String()
	if !strings.Contains(got, "revived 1 deferred") {
		t.Fatalf("output missing revive count: %q", got)
	}
	if !strings.Contains(got, `for library "Target"`) {
		t.Fatalf("output missing library label: %q", got)
	}
}

// TestRunQueueRecheck_ConfigLoadError verifies a malformed config file exits 1.
func TestRunQueueRecheck_ConfigLoadError(t *testing.T) {
	isolateCommandsEnv(t)
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("this is = not valid = toml ]["), 0o600); err != nil {
		t.Fatalf("write bad config: %v", err)
	}
	var out bytes.Buffer
	code := runQueueRecheck(context.Background(), &out, QueueRecheckCmd{
		ConfigPath: bad,
		Deferred:   true,
	})
	if code != 1 {
		t.Fatalf("config-load-error exit code = %d; want 1. out=%s", code, out.String())
	}
}

// TestRunQueueRecheck_DBOpenError verifies an unopenable DB path exits 1. A
// directory cannot be opened as a SQLite database file.
func TestRunQueueRecheck_DBOpenError(t *testing.T) {
	isolateCommandsEnv(t)
	dir := t.TempDir()
	cfg := writeCommandsConfig(t, dir)
	var out bytes.Buffer
	code := runQueueRecheck(context.Background(), &out, QueueRecheckCmd{
		ConfigPath: cfg,
		Deferred:   true,
	})
	if code != 1 {
		t.Fatalf("db-open-error exit code = %d; want 1. out=%s", code, out.String())
	}
}

// TestLogStartupBanner_EmitsVersion verifies the startup banner writes the
// version string to the console writer.
func TestLogStartupBanner_EmitsVersion(t *testing.T) {
	cfg := config.Config{
		API:     config.APIConfig{Token: "topsecret", Cooldown: 15},
		Output:  config.OutputConfig{Dir: "lyrics"},
		Logging: config.LoggingConfig{Level: "info", Format: "text"},
	}
	var buf bytes.Buffer
	logStartupBanner(context.Background(), cfg, "mxlrcgo-svc test-ver", &buf, nil, nil)
	got := buf.String()
	if !strings.Contains(got, "mxlrcgo-svc test-ver") {
		t.Errorf("version not in banner output: %q", got)
	}
}

// TestLogStartupBanner_RedactsToken verifies the token never appears in
// plaintext in the banner console output.
func TestLogStartupBanner_RedactsToken(t *testing.T) {
	cfg := config.Config{
		API:     config.APIConfig{Token: "topsecret", Cooldown: 15},
		Output:  config.OutputConfig{Dir: "lyrics"},
		Logging: config.LoggingConfig{Level: "info", Format: "text"},
	}
	var buf bytes.Buffer
	logStartupBanner(context.Background(), cfg, "mxlrcgo-svc test-ver", &buf, nil, nil)
	got := buf.String()
	if strings.Contains(got, "topsecret") {
		t.Errorf("token in plaintext in banner output: %q", got)
	}
}

// TestLogStartupBanner_CLIAnnotation verifies the (cli) source annotation
// appears in the console output when a field is in the cliSrc map.
func TestLogStartupBanner_CLIAnnotation(t *testing.T) {
	cfg := config.Config{
		API:     config.APIConfig{Cooldown: 30},
		Output:  config.OutputConfig{Dir: "custom-dir"},
		Logging: config.LoggingConfig{Level: "info", Format: "text"},
	}
	cliSrc := map[string]bool{"output.dir": true}
	var buf bytes.Buffer
	logStartupBanner(context.Background(), cfg, "mxlrcgo-svc test-ver", &buf, nil, cliSrc)
	got := buf.String()
	if !strings.Contains(got, "(cli)") {
		t.Errorf("missing (cli) annotation in banner output: %q", got)
	}
}

// serveTestAuth is a minimal Authenticator for use in serve-handler regression
// tests. It accepts every key as if it were admin-scoped.
type serveTestAuth struct{}

func (serveTestAuth) ValidateKey(_ context.Context, _ string, _ auth.Scope) (auth.Key, error) {
	return auth.Key{ID: "test-admin"}, nil
}

// TestServeHandlerWiresMetricsReporter guards the serve command wiring: if
// WithMetricsReporter is ever removed from the serve handler option list,
// GET /metrics will return 500 instead of 200. This test constructs the
// handler with the same option set the serve command uses and asserts that
// /metrics returns 200 with the expected Prometheus metric families.
func TestServeHandlerWiresMetricsReporter(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	workQ := queue.NewDBQueue(sqlDB)

	// Build the handler with the same option set as the serve command.
	// WithMetricsReporter is required here; omitting it causes /metrics to 500.
	h := server.NewHandler(&serveTestAuth{}, workQ, t.TempDir(),
		server.WithReadiness(sqlDB),
		server.WithStatusReporter(workQ),
		server.WithMetricsReporter(workQ),
		server.WithInventory(scan.New(sqlDB)),
		server.WithAllowedRoots(nil),
	)

	rec := httptest.NewRecorder()
	// /metrics is gated by the trusted-network allowlist (#204, S3); loopback is
	// implicitly trusted, so scrape from 127.0.0.1 (no API key required).
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.RemoteAddr = "127.0.0.1:43210"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d; want 200 (body: %q)", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if !strings.Contains(body, "mxlrcgo_queue_items") {
		t.Errorf("response body missing mxlrcgo_queue_items\nbody:\n%s", body)
	}
	if !strings.Contains(body, "mxlrcgo_queue_failures") {
		t.Errorf("response body missing mxlrcgo_queue_failures\nbody:\n%s", body)
	}
}

// detectorScanVersion returns the SIDECAR MODEL version, not the app version
// (#684). It previously returned the app version, which meant every canticle
// release reopened every on-disk [dv:] marker and invalidated every stored
// verdict even though the classifier had not changed.
func TestDetectorScanVersion(t *testing.T) {
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			t.Errorf("path = %q; want /health (the version probe must not run ffmpeg or classify)", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"ok","model_version":"model-sha-under-test"}`))
	}))
	defer srv.Close()

	enabled := config.Config{InstrumentalDetector: config.InstrumentalDetectorConfig{
		Enabled: true, ClassifierURL: srv.URL,
	}}
	if got := detectorScanVersion(ctx, enabled); got != "model-sha-under-test" {
		t.Errorf("enabled: detectorScanVersion = %q, want the sidecar model version", got)
	}
	if got := detectorScanVersion(ctx, enabled); got == version {
		t.Errorf("detectorScanVersion returned the APP version %q; that is the #684 defect", version)
	}

	disabled := config.Config{InstrumentalDetector: config.InstrumentalDetectorConfig{
		Enabled: false, ClassifierURL: srv.URL,
	}}
	if got := detectorScanVersion(ctx, disabled); got != "" {
		t.Errorf("disabled: detectorScanVersion = %q, want empty (no version-invalidation churn)", got)
	}

	// An unreachable sidecar must degrade to UNKNOWN, not to a wrong-but-confident
	// value: reopen.go treats "" as "do not reopen", so a transient probe failure
	// leaves existing markers alone instead of invalidating the whole library.
	unreachable := config.Config{InstrumentalDetector: config.InstrumentalDetectorConfig{
		Enabled: true, ClassifierURL: "http://127.0.0.1:1",
	}}
	if got := detectorScanVersion(ctx, unreachable); got != "" {
		t.Errorf("unreachable sidecar: detectorScanVersion = %q, want empty (unknown)", got)
	}
}

// The scheduler's work queue must be stamped with the provider generation.
//
// Enqueue writes q.providersVersion onto every new row, and the #679
// suppression compares that STORED value against the live generation. If the
// queue scheduler() builds is left at the default 0 while the enqueuer is handed
// a real generation, the two can never match: every row is written as
// generation 0, read back as 0, compared against N, and the suppression is a
// silent no-op in serve mode -- the only mode that has a generation at all.
//
// CodeRabbit caught this on #707; nothing in the suite did, because every
// suppression test constructed the Enqueuer directly and never exercised
// scheduler()'s wiring. This drives OnScanComplete -- the real callback, over
// the real queue -- rather than a queue the test built itself, which would
// verify the fixture instead of the code.
func TestSchedulerStampsProvidersVersionOnItsQueue(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	lib, err := library.New(sqlDB).Add(ctx, "/music", "Music", models.LibrarySettings{})
	if err != nil {
		t.Fatalf("Add library: %v", err)
	}
	if err := scan.New(sqlDB).Upsert(ctx, lib.ID, []models.ScanResult{{
		FilePath: "/music/a.mp3",
		Track:    models.Track{ArtistName: "Artist", TrackName: "Title"},
		Outdir:   "/music",
		Filename: "a.lrc",
		Status:   scan.StatusPending,
	}}, scan.UpsertOptions{}); err != nil {
		t.Fatalf("Upsert scan result: %v", err)
	}

	const gen = 42
	s := scheduler(sqlDB, scanner.ScanOptions{}, nil, false, nil, nil, "", gen)
	if err := s.OnScanComplete(ctx, models.Library{ID: lib.ID}, nil, lib.Path, scan.TriggerScheduler); err != nil {
		t.Fatalf("OnScanComplete: %v", err)
	}

	var stored int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT providers_version FROM work_queue`).Scan(&stored); err != nil {
		t.Fatalf("read providers_version: %v", err)
	}
	if stored != gen {
		t.Fatalf("providers_version = %d; want %d -- a row stamped 0 can never match the live generation, "+
			"so the #679 suppression would silently never fire", stored, gen)
	}
}

// TestWordSyncValidatorMatchesCLI pins the CLI arm and the registry-driven web
// save path to the SAME rule for output.word_sync (#480). The web path validates
// through config.ValidateAndSet (TypeBool -> ValidateBool) while the CLI arm
// hand-rolls its parse, so the two could silently diverge and accept a value
// through one surface that the other rejects.
func TestWordSyncValidatorMatchesCLI(t *testing.T) {
	const path = "output.word_sync"
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{"true", true},
		{"false", true},
		{"yes", false},
		{"", false},
	} {
		webErr := config.ValidateAndSet(path, tc.value)
		cliErr := setConfigValue(&config.Config{}, path, tc.value)
		if (webErr == nil) != tc.valid {
			t.Errorf("ValidateAndSet(%q) err = %v; want valid=%v", tc.value, webErr, tc.valid)
		}
		if (webErr == nil) != (cliErr == nil) {
			t.Errorf("value %q: web accepts=%v but CLI accepts=%v; the two surfaces disagree",
				tc.value, webErr == nil, cliErr == nil)
		}
	}
}

// TestConfigureWriterWordSync covers the config-to-writer seam: the type
// assertion body that actually calls SetWordSync. Without this, the helper is
// called by both serve and fetch wiring but its effect is never observed, so a
// helper that silently did nothing would still look exercised.
func TestConfigureWriterWordSync(t *testing.T) {
	w := lyrics.NewLRCWriter()
	configureWriterWordSync(w, config.Config{Output: config.OutputConfig{WordSync: true}})

	dir := t.TempDir()
	song := models.Song{
		Track: models.Track{ArtistName: "A", TrackName: "T"},
		Subtitles: models.Synced{Lines: []models.Lines{
			{Text: "alpha beta", Time: models.Time{Total: 1.5, Seconds: 1, Hundredths: 50}},
		}},
		AudioDurationSeconds: 240,
		WordTimings: []models.WordTiming{
			{Line: 0, Text: "alpha ", StartMS: 1500, EndMS: 2000},
			{Line: 0, Text: "beta", StartMS: 2000, EndMS: 2500},
		},
	}
	if err := w.WriteLRC(song, "s.lrc", dir); err != nil {
		t.Fatalf("WriteLRC: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "s.lrc")) //nolint:gosec // reason: test path from t.TempDir
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Observed through the OUTPUT, not by reading the flag back: the point is
	// that config reaches the file, not that a setter stored a bool.
	if !strings.Contains(string(body), "<00:01.50>alpha") {
		t.Errorf("word sync did not reach the writer; got %q", body)
	}
}

// TestConfigValueWordSync covers the CLI `config get` arm. configKeys lists the
// key, so an operator can ask for it; a missing configValue arm would answer
// with a bare empty string rather than the setting.
func TestConfigValueWordSync(t *testing.T) {
	got, ok := configValue(config.Config{Output: config.OutputConfig{WordSync: true}}, "output.word_sync")
	if !ok {
		t.Fatal("configValue(output.word_sync) ok = false; the key is listed in configKeys")
	}
	if got != "true" {
		t.Errorf("configValue = %q; want %q", got, "true")
	}
	if !slices.Contains(configKeys(), "output.word_sync") {
		t.Error("configKeys is missing output.word_sync")
	}
}

// TestSetConfigValueBoolErrorsWrapTheCause pins that a bad boolean preserves
// strconv's own error (#760 review). Without %w the caller sees only "must be a
// boolean" and loses which value failed and why -- CLAUDE.md requires wrapping
// for exactly that reason.
//
// Covers all three bool arms in this switch, not just the new one: they share
// the shape, and fixing one while leaving its neighbors would make the file
// inconsistent in the other direction.
func TestSetConfigValueBoolErrorsWrapTheCause(t *testing.T) {
	for _, key := range []string{"output.word_sync", "output.bilingual_output", "verification.enabled"} {
		cfg := config.Config{}
		err := setConfigValue(&cfg, key, "not-a-bool")
		if err == nil {
			t.Errorf("%s: setConfigValue accepted a non-boolean", key)
			continue
		}
		var numErr *strconv.NumError
		if !errors.As(err, &numErr) {
			t.Errorf("%s: error does not wrap the strconv cause: %v", key, err)
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error does not name the key: %v", key, err)
		}
	}
}

// TestMusixmatchIsServing covers the predicate gating the Musixmatch attribution
// credit (#600, API Terms clause 2.1.5).
//
// This is a GENUINE regression test, not a guard: the PetitLyrics case below
// fails against the original implementation, which gated the credit on
// !musixmatchInactive and therefore credited Musixmatch for another provider's
// results. Caught by CodeRabbit on PR #769.
//
// The predicate was extracted from runServe specifically so this table could
// exist. Inline, it needed a full serve to exercise, which is why nothing caught
// the defect before review.
func TestMusixmatchIsServing(t *testing.T) {
	musixmatch := providers.New(providers.Musixmatch, fakeFetcher{})
	petitlyrics := providers.New(providers.PetitLyrics, fakeFetcher{})

	tests := []struct {
		name           string
		fetcher        providers.LyricsProvider
		lyricsDisabled bool
		want           bool
	}{
		{"musixmatch serving: credit is required", musixmatch, false, true},
		{
			// THE REGRESSION CASE. A healthy PetitLyrics-primary deployment: the
			// provider selected cleanly, so the old !musixmatchInactive gate read
			// true and credited Musixmatch for PetitLyrics results.
			"petitlyrics primary: no Musixmatch Data is used, so no credit",
			petitlyrics, false, false,
		},
		{
			// The degraded no-token path returns a NO-OP fetcher wrapped in a
			// providers.Musixmatch identity, so the name alone says "musixmatch"
			// while nothing is fetched. lyricsDisabled is what excludes it.
			"musixmatch identity but lyrics disabled: nothing is served, so no credit",
			musixmatch, true, false,
		},
		{"petitlyrics and disabled: no credit", petitlyrics, true, false},
		{"nil fetcher: no credit rather than a panic", nil, false, false},
		{"nil fetcher and disabled: no credit", nil, true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := musixmatchIsServing(tt.fetcher, tt.lyricsDisabled); got != tt.want {
				t.Errorf("musixmatchIsServing() = %v; want %v", got, tt.want)
			}
		})
	}
}

// TestMusixmatchIsServingMatchesResolveServeProvider ties the predicate to the
// function that actually produces its inputs, so the pair cannot drift.
//
// The unit table above uses hand-built providers; this asserts the real
// resolveServeProvider outputs flow through to the right answer. Without it, a
// change to the degrade path could keep every case above passing while the live
// wiring credited the wrong provider.
func TestMusixmatchIsServingMatchesResolveServeProvider(t *testing.T) {
	t.Run("healthy musixmatch: serving", func(t *testing.T) {
		primary := providers.New(providers.Musixmatch, fakeFetcher{})
		fetcher, _, disabled, err := resolveServeProvider(primary, nil)
		if err != nil {
			t.Fatalf("resolveServeProvider: %v", err)
		}
		if !musixmatchIsServing(fetcher, disabled) {
			t.Error("a healthy Musixmatch primary must be credited")
		}
	})

	t.Run("healthy petitlyrics: not serving", func(t *testing.T) {
		primary := providers.New(providers.PetitLyrics, fakeFetcher{})
		fetcher, inactive, disabled, err := resolveServeProvider(primary, nil)
		if err != nil {
			t.Fatalf("resolveServeProvider: %v", err)
		}
		// The flags are NOT complements, which is the whole defect: inactive is
		// false here (selection succeeded) while Musixmatch serves nothing.
		if inactive {
			t.Fatal("precondition: a clean PetitLyrics selection must not be musixmatchInactive")
		}
		if musixmatchIsServing(fetcher, disabled) {
			t.Error("PetitLyrics results must not be credited to Musixmatch")
		}
	})

	t.Run("no token: not serving despite the musixmatch identity", func(t *testing.T) {
		fetcher, inactive, disabled, err := resolveServeProvider(nil, errNoMusixmatchToken)
		if err != nil {
			t.Fatalf("resolveServeProvider: %v", err)
		}
		if !inactive || !disabled {
			t.Fatal("precondition: the no-token path must be both inactive and disabled")
		}
		if fetcher.Name() != providers.Musixmatch {
			t.Fatalf("precondition: the no-op fetcher carries the musixmatch identity, got %q", fetcher.Name())
		}
		if musixmatchIsServing(fetcher, disabled) {
			t.Error("a no-op fetcher wearing the musixmatch name must not be credited")
		}
	})
}
