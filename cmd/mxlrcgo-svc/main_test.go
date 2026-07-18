package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/queue"
)

type runRecord struct {
	token       string
	findCalls   int
	appCreated  bool
	runCalls    int
	cooldown    int
	mode        string
	inputs      []models.Inputs
	runErr      error
	createAppFn func(*runRecord)
}

type fakeFetcher struct {
	rec *runRecord
}

func (f *fakeFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	f.rec.findCalls++
	return models.Song{}, errors.New("unexpected lyrics fetch in startup test")
}

type fakeWriter struct{}

func (fakeWriter) WriteLRC(models.Song, string, string) error {
	return errors.New("unexpected LRC write in startup test")
}

type fakeRunner struct {
	rec *runRecord
}

func (r fakeRunner) Run(context.Context) error {
	r.rec.runCalls++
	return r.rec.runErr
}

func isolateCLIEnv(t *testing.T) {
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

func writeConfig(t *testing.T, token string, cooldown int, outdir string, dbPath string) string {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "config.toml")
	content := "[api]\n" +
		"token = \"" + token + "\"\n" +
		"cooldown = " + strconv.Itoa(cooldown) + "\n" +
		"[output]\n" +
		"dir = \"" + filepath.ToSlash(outdir) + "\"\n" +
		"[db]\n" +
		"path = \"" + filepath.ToSlash(dbPath) + "\"\n"
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfg
}

func runStartup(t *testing.T, args []string, rec *runRecord) int {
	t.Helper()
	return runStartupWithDotenv(t, args, rec, func() error { return nil })
}

func runStartupWithDotenv(t *testing.T, args []string, rec *runRecord, loadDotenv func() error) int {
	t.Helper()
	return runWithOptions(runOptions{
		args:       args,
		loadDotenv: loadDotenv,
		newFetcher: func(token string) musixmatch.Fetcher {
			rec.token = token
			return &fakeFetcher{rec: rec}
		},
		newWriter: func(roots ...string) lyrics.Writer {
			return fakeWriter{}
		},
		newApp: func(_ musixmatch.Fetcher, _ lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) appRunner {
			rec.appCreated = true
			rec.cooldown = cooldown
			rec.mode = mode
			for !inputs.Empty() {
				v, err := inputs.Pop()
				if err != nil {
					t.Fatalf("pop input: %v", err)
				}
				rec.inputs = append(rec.inputs, v)
			}
			if rec.createAppFn != nil {
				rec.createAppFn(rec)
			}
			return fakeRunner{rec: rec}
		},
	})
}

func TestRunWithOptions_HelpDoesNotStartApplication(t *testing.T) {
	isolateCLIEnv(t)
	var out bytes.Buffer
	rec := &runRecord{}

	code := runWithOptions(runOptions{
		args:       []string{"--help"},
		out:        &out,
		loadDotenv: func() error { return nil },
		newFetcher: func(token string) musixmatch.Fetcher {
			rec.token = token
			return &fakeFetcher{rec: rec}
		},
		newWriter: func(roots ...string) lyrics.Writer { return fakeWriter{} },
		newApp: func(musixmatch.Fetcher, lyrics.Writer, *queue.InputsQueue, int, string) appRunner {
			rec.appCreated = true
			return fakeRunner{rec: rec}
		},
	})
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.appCreated {
		t.Fatal("app was created; want help to stop before startup")
	}
	if !strings.Contains(out.String(), "Usage: canticle") {
		t.Fatalf("help output = %q; want usage", out.String())
	}
}

func TestRunWithOptions_SubcommandHelpShowsSelectedCommand(t *testing.T) {
	isolateCLIEnv(t)

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
			rec := &runRecord{}
			code := runWithOptions(runOptions{
				args:       tc.args,
				out:        &out,
				loadDotenv: func() error { return nil },
				newFetcher: func(token string) musixmatch.Fetcher {
					rec.token = token
					return &fakeFetcher{rec: rec}
				},
				newWriter: func(roots ...string) lyrics.Writer { return fakeWriter{} },
				newApp: func(musixmatch.Fetcher, lyrics.Writer, *queue.InputsQueue, int, string) appRunner {
					rec.appCreated = true
					return fakeRunner{rec: rec}
				},
			})
			if code != 0 {
				t.Fatalf("run exit code = %d; want 0", code)
			}
			if rec.appCreated {
				t.Fatal("app was created; want help to stop before startup")
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("help output = %q; want %q", out.String(), want)
				}
			}
		})
	}
}

func TestRunWithOptions_CLIPrecedenceAndPairInput(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfgOut := filepath.Join(dir, "config-out")
	envOut := filepath.Join(dir, "env-out")
	cliOut := filepath.Join(dir, "cli-out")
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeConfig(t, "config-token", 9, cfgOut, dbPath)
	t.Setenv("MUSIXMATCH_TOKEN", "env-token")
	t.Setenv("MXLRC_API_COOLDOWN", "7")
	t.Setenv("MXLRC_OUTPUT_DIR", envOut)

	rec := &runRecord{}
	code := runStartup(t, []string{
		"--config", cfg,
		"--token", "cli-token",
		"--cooldown", "0",
		"--outdir", cliOut,
		"Artist,Title",
	}, rec)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "cli-token" {
		t.Fatalf("token = %q; want CLI token", rec.token)
	}
	if rec.cooldown != 0 {
		t.Fatalf("cooldown = %d; want CLI cooldown 0", rec.cooldown)
	}
	if rec.mode != "cli" {
		t.Fatalf("mode = %q; want cli", rec.mode)
	}
	if len(rec.inputs) != 1 {
		t.Fatalf("inputs = %d; want 1", len(rec.inputs))
	}
	got := rec.inputs[0]
	if got.Track.ArtistName != "Artist" || got.Track.TrackName != "Title" {
		t.Fatalf("track = %+v; want Artist/Title", got.Track)
	}
	if got.Outdir != cliOut {
		t.Fatalf("outdir = %q; want %q", got.Outdir, cliOut)
	}
	if rec.findCalls != 0 {
		t.Fatalf("FindLyrics calls = %d; want 0", rec.findCalls)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("stat db path error = %v; want not exist for standalone fetch", err)
	}
}

func TestRunWithOptions_DotenvTokenPrecedence(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))
	loadDotenv := func() error {
		if os.Getenv("MUSIXMATCH_TOKEN") == "" {
			t.Setenv("MUSIXMATCH_TOKEN", "dotenv-token")
		}
		return nil
	}

	rec := &runRecord{}
	code := runStartupWithDotenv(t, []string{"--config", cfg, "Artist,Title"}, rec, loadDotenv)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "dotenv-token" {
		t.Fatalf("token = %q; want dotenv token", rec.token)
	}

	t.Setenv("MUSIXMATCH_TOKEN", "env-token")
	rec = &runRecord{}
	code = runStartupWithDotenv(t, []string{"--config", cfg, "Artist,Title"}, rec, loadDotenv)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "env-token" {
		t.Fatalf("token = %q; want env token", rec.token)
	}

	rec = &runRecord{}
	code = runStartupWithDotenv(t, []string{"--config", cfg, "--token", "cli-token", "Artist,Title"}, rec, loadDotenv)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "cli-token" {
		t.Fatalf("token = %q; want CLI token", rec.token)
	}
}

func TestRunWithOptions_EnvPrecedenceOverConfig(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfgOut := filepath.Join(dir, "config-out")
	envOut := filepath.Join(dir, "env-out")
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeConfig(t, "config-token", 9, cfgOut, dbPath)
	t.Setenv("MUSIXMATCH_TOKEN", "env-token")
	t.Setenv("MXLRC_API_COOLDOWN", "7")
	t.Setenv("MXLRC_OUTPUT_DIR", envOut)

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, "Artist,Title"}, rec)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "env-token" {
		t.Fatalf("token = %q; want env token", rec.token)
	}
	if rec.cooldown != 7 {
		t.Fatalf("cooldown = %d; want env cooldown 7", rec.cooldown)
	}
	if len(rec.inputs) != 1 || rec.inputs[0].Outdir != envOut {
		t.Fatalf("inputs = %+v; want env output dir %q", rec.inputs, envOut)
	}
}

func TestRunWithOptions_MissingTokenFailsBeforeAppRun(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeConfig(t, "", 1, filepath.Join(dir, "out"), dbPath)

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, "Artist,Title"}, rec)
	if code == 0 {
		t.Fatal("run exit code = 0; want failure")
	}
	if rec.appCreated {
		t.Fatal("app was created; want missing token to stop startup before app creation")
	}
	if rec.findCalls != 0 {
		t.Fatalf("FindLyrics calls = %d; want 0", rec.findCalls)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("stat db path error = %v; want not exist", err)
	}
}

func TestRunWithOptions_DisabledConfiguredProviderFailsBeforeAppRun(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))
	t.Setenv("MXLRC_PROVIDERS_DISABLED", "musixmatch")

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, "Artist,Title"}, rec)
	if code == 0 {
		t.Fatal("run exit code = 0; want provider failure")
	}
	if rec.appCreated {
		t.Fatal("app was created; want disabled provider to stop startup before app creation")
	}
}

func TestRunWithOptions_UnsupportedConfiguredProviderFailsBeforeAppRun(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))
	t.Setenv("MXLRC_PROVIDER_PRIMARY", "future")

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, "Artist,Title"}, rec)
	if code == 0 {
		t.Fatal("run exit code = 0; want provider failure")
	}
	if rec.appCreated {
		t.Fatal("app was created; want unsupported provider to stop startup before app creation")
	}
}

func TestRunWithOptions_TextFileInputSetup(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	outdir := filepath.Join(dir, "lyrics")
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeConfig(t, "config-token", 1, outdir, dbPath)
	textFile := filepath.Join(dir, "songs.txt")
	if err := os.WriteFile(textFile, []byte("Artist A,Title A\nArtist B,Title B\n"), 0o600); err != nil {
		t.Fatalf("write text input: %v", err)
	}

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, textFile}, rec)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.mode != "text" {
		t.Fatalf("mode = %q; want text", rec.mode)
	}
	if len(rec.inputs) != 2 {
		t.Fatalf("inputs = %d; want 2", len(rec.inputs))
	}
	if rec.inputs[0].Track.ArtistName != "Artist A" || rec.inputs[1].Track.TrackName != "Title B" {
		t.Fatalf("inputs = %+v; want text-file tracks", rec.inputs)
	}
	for _, v := range rec.inputs {
		if v.Outdir != outdir {
			t.Fatalf("input outdir = %q; want %q", v.Outdir, outdir)
		}
	}
}

func TestRunWithOptions_FetchSubcommandUsesOneShotPath(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "state", "test.db")
	cfg := writeConfig(t, "config-token", 4, filepath.Join(dir, "out"), dbPath)

	rec := &runRecord{}
	code := runStartup(t, []string{"fetch", "--config", cfg, "Artist,Title"}, rec)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "config-token" {
		t.Fatalf("token = %q; want config token", rec.token)
	}
	if rec.cooldown != 4 {
		t.Fatalf("cooldown = %d; want 4", rec.cooldown)
	}
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("stat db path error = %v; want not exist for fetch subcommand", err)
	}
}

func TestRunWithOptions_LegacyFlagValueNamedFetchIsNotSubcommand(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))

	rec := &runRecord{}
	code := runStartup(t, []string{"--config", cfg, "--token", "fetch", "Artist,Title"}, rec)
	if code != 0 {
		t.Fatalf("run exit code = %d; want 0", code)
	}
	if rec.token != "fetch" {
		t.Fatalf("token = %q; want legacy flag value", rec.token)
	}
}

func TestRunWithOptions_ServeSubcommandRequiresToken(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))

	code := runWithOptions(runOptions{
		args:       []string{"serve", "--config", cfg},
		loadDotenv: func() error { return nil },
	})
	if code == 0 {
		t.Fatal("run exit code = 0; want missing-token failure")
	}
}

func TestRunWithOptions_LibrarySubcommands(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))
	libPath := filepath.Join(dir, "music")
	if err := os.Mkdir(libPath, 0o750); err != nil {
		t.Fatalf("mkdir library: %v", err)
	}

	var out bytes.Buffer
	code := runWithOptions(runOptions{args: []string{"library", "add", libPath, "--name", "Music", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("library add exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "Music") || !strings.Contains(out.String(), libPath) {
		t.Fatalf("library add output = %q; want name and path", out.String())
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"library", "list", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("library list exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "Music") {
		t.Fatalf("library list output = %q; want Music", out.String())
	}

	renamedPath := filepath.Join(dir, "renamed")
	if err := os.Mkdir(renamedPath, 0o750); err != nil {
		t.Fatalf("mkdir renamed library: %v", err)
	}
	out.Reset()
	code = runWithOptions(runOptions{
		args:       []string{"library", "update", "1", "--path", renamedPath, "--name", "Renamed", "--config", cfg},
		out:        &out,
		loadDotenv: func() error { return nil },
	})
	if code != 0 {
		t.Fatalf("library update exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "Renamed") || !strings.Contains(out.String(), renamedPath) {
		t.Fatalf("library update output = %q; want updated name and path", out.String())
	}

	out.Reset()
	code = runWithOptions(runOptions{
		args:       []string{"library", "update", "1", "--name", "Display", "--config", cfg},
		out:        &out,
		loadDotenv: func() error { return nil },
	})
	if code != 0 {
		t.Fatalf("library update name exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "Display") || !strings.Contains(out.String(), renamedPath) {
		t.Fatalf("library update name output = %q; want new name and existing path", out.String())
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"library", "remove", "1", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("library remove exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "removed library 1") {
		t.Fatalf("library remove output = %q; want removed message", out.String())
	}
}

func TestRunWithOptions_LibraryUpdateMissingLibraryFails(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))

	var out bytes.Buffer
	code := runWithOptions(runOptions{
		args:       []string{"library", "update", "99", "--name", "Missing", "--config", cfg},
		out:        &out,
		loadDotenv: func() error { return nil },
	})
	if code == 0 {
		t.Fatal("library update missing exit code = 0; want failure")
	}
	if !strings.Contains(out.String(), "library 99 not found") {
		t.Fatalf("library update missing output = %q; want not-found message", out.String())
	}
}

func TestRunWithOptions_KeySubcommands(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))

	var out bytes.Buffer
	code := runWithOptions(runOptions{args: []string{"keys", "create", "--name", "webhook", "--scope", "webhook", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("keys create exit code = %d; want 0", code)
	}
	raw := strings.TrimSpace(out.String())
	if !strings.HasPrefix(raw, "mxlrc_") {
		t.Fatalf("created key = %q; want mxlrc_ prefix", raw)
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"keys", "list", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("keys list exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "webhook") {
		t.Fatalf("keys list output = %q; want webhook", out.String())
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"keys", "revoke", raw, "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("keys revoke exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Fatalf("keys revoke output = %q; want revoked", out.String())
	}
}

func TestRunWithOptions_ConfigSubcommands(t *testing.T) {
	isolateCLIEnv(t)
	cfg := filepath.Join(t.TempDir(), "config.toml")

	var out bytes.Buffer
	code := runWithOptions(runOptions{args: []string{"config", "set", "api.cooldown", "3", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("config set exit code = %d; want 0", code)
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"config", "get", "api.cooldown", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("config get exit code = %d; want 0", code)
	}
	if strings.TrimSpace(out.String()) != "3" {
		t.Fatalf("config get output = %q; want 3", out.String())
	}

	out.Reset()
	code = runWithOptions(runOptions{args: []string{"config", "list", "--config", cfg}, out: &out, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("config list exit code = %d; want 0", code)
	}
	if !strings.Contains(out.String(), "api.cooldown=3") {
		t.Fatalf("config list output = %q; want api.cooldown", out.String())
	}
}

func TestRunWithOptions_ScanSubcommand(t *testing.T) {
	isolateCLIEnv(t)
	dir := t.TempDir()
	cfg := writeConfig(t, "config-token", 1, filepath.Join(dir, "out"), filepath.Join(dir, "state", "test.db"))

	code := runWithOptions(runOptions{args: []string{"scan", "--config", cfg}, loadDotenv: func() error { return nil }})
	if code != 0 {
		t.Fatalf("scan exit code = %d; want 0", code)
	}
}
