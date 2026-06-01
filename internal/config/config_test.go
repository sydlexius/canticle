package config

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateEnv clears all config-related env vars using t.Setenv (which enforces
// sequential-only execution -- calling t.Parallel() in a test that uses this
// helper will panic, making the constraint machine-enforceable).
// It also sets MXLRC_DB_PATH to a safe sentinel so Load never returns an
// "empty DB path" error in tests that don't care about the DB path.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"MUSIXMATCH_TOKEN", "MXLRC_API_TOKEN",
		"MXLRC_API_COOLDOWN", "MXLRC_COOLDOWN",
		"MXLRC_API_CIRCUIT_OPEN_DURATION",
		"MXLRC_OUTPUT_DIR", "MXLRC_SERVER_ADDR", "MXLRC_WEBHOOK_API_KEY",
		"MXLRC_SCAN_INTERVAL", "MXLRC_WORK_INTERVAL",
		"MXLRC_PROVIDER_PRIMARY", "MXLRC_PROVIDERS_DISABLED",
		"MXLRC_VERIFICATION_ENABLED", "MXLRC_VERIFICATION_WHISPER_URL", "MXLRC_WHISPER_URL",
		"MXLRC_VERIFICATION_FFMPEG_PATH",
		"MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "MXLRC_VERIFICATION_SAMPLE_DURATION",
		"MXLRC_VERIFICATION_MIN_CONFIDENCE", "MXLRC_VERIFICATION_MIN_SIMILARITY",
		"MXLRC_DOCKER",
		"XDG_CONFIG_HOME", "XDG_DATA_HOME",
	} {
		// t.Setenv("", "") clears the variable; applyEnvOverrides uses os.Getenv
		// which returns "" for both unset and empty, so behavior is identical.
		t.Setenv(k, "")
	}
	// Provide a non-empty DB path so Load doesn't error on path resolution.
	t.Setenv("MXLRC_DB_PATH", filepath.Join(t.TempDir(), "test.db"))
}

// TestLoad_MissingConfigFileIsNotFatal verifies that a non-existent config
// file path is silently ignored and defaults are returned.
func TestLoad_MissingConfigFileIsNotFatal(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load with missing file returned error: %v", err)
	}
	if cfg.API.Cooldown != 15 {
		t.Errorf("default cooldown = %d; want 15", cfg.API.Cooldown)
	}
	if cfg.Output.Dir != "lyrics" {
		t.Errorf("default output dir = %q; want %q", cfg.Output.Dir, "lyrics")
	}
	if cfg.Providers.Primary != "musixmatch" {
		t.Errorf("default provider = %q; want musixmatch", cfg.Providers.Primary)
	}
	if cfg.Verification.SampleDurationSeconds != 30 {
		t.Errorf("default verification sample duration = %d; want 30", cfg.Verification.SampleDurationSeconds)
	}
	if cfg.Verification.MinConfidence != 0.85 {
		t.Errorf("default verification min confidence = %v; want 0.85", cfg.Verification.MinConfidence)
	}
	if cfg.Verification.FFmpegPath != "ffmpeg" {
		t.Errorf("default verification ffmpeg path = %q; want ffmpeg", cfg.Verification.FFmpegPath)
	}
	if cfg.Verification.MinSimilarity != 0.35 {
		t.Errorf("default verification min similarity = %v; want 0.35", cfg.Verification.MinSimilarity)
	}
}

// TestLoad_BlankFieldsInTOMLDoNotClobberDefaults verifies that blank string
// fields in the TOML file trigger re-default logic: output.dir is restored to
// "lyrics". MXLRC_DB_PATH is set here, so the DB path assertion validates env
// override precedence (env > re-default), not the XDG path calculation itself
// (see TestLoad_BlankDBPathInTOMLReDefaultsViaXDG for the XDG case).
func TestLoad_BlankFieldsInTOMLDoNotClobberDefaults(t *testing.T) {
	isolateEnv(t)

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.toml")
	content := "[db]\npath = \"\"\n[output]\ndir = \"\"\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	// Override MXLRC_DB_PATH so the XDG default path is predictable.
	dbPath := filepath.Join(dir, "data.db")
	t.Setenv("MXLRC_DB_PATH", dbPath)

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}
	if cfg.Output.Dir != "lyrics" {
		t.Errorf("blank output.dir not re-defaulted; got %q, want %q", cfg.Output.Dir, "lyrics")
	}
	// DB path comes from MXLRC_DB_PATH env override (which wins over re-default).
	if cfg.DB.Path != dbPath {
		t.Errorf("DB.Path = %q; want %q", cfg.DB.Path, dbPath)
	}
}

// TestLoad_BlankDBPathInTOMLReDefaultsViaXDG verifies that when db.path is blank
// in the TOML file, the re-default logic computes a path from XDG_DATA_HOME
// (not left empty). MXLRC_DB_PATH is intentionally not set in this test so the
// env-override path does not mask the re-default behavior.
func TestLoad_BlankDBPathInTOMLReDefaultsViaXDG(t *testing.T) {
	isolateEnv(t)

	xdgData := t.TempDir()
	// Point XDG_DATA_HOME at our temp dir so the computed default is predictable.
	t.Setenv("XDG_DATA_HOME", xdgData)
	// Clear MXLRC_DB_PATH so the env override does not mask the re-default logic.
	t.Setenv("MXLRC_DB_PATH", "")

	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.toml")
	content := "[db]\npath = \"\"\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	// The re-default should compute: XDG_DATA_HOME/mxlrcgo-svc/mxlrcgo.db
	want := filepath.Join(xdgData, "mxlrcgo-svc", "mxlrcgo.db")
	if cfg.DB.Path != want {
		t.Errorf("DB.Path = %q; want re-defaulted XDG path %q", cfg.DB.Path, want)
	}
}

// TestLoad_EnvTokenPrecedence verifies MUSIXMATCH_TOKEN > MXLRC_API_TOKEN.
func TestLoad_EnvTokenPrecedence(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MUSIXMATCH_TOKEN", "token-musixmatch")
	t.Setenv("MXLRC_API_TOKEN", "token-mxlrc")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Token != "token-musixmatch" {
		t.Errorf("token = %q; want %q (MUSIXMATCH_TOKEN must win)", cfg.API.Token, "token-musixmatch")
	}
}

// TestLoad_EnvTokenFallback verifies MXLRC_API_TOKEN is used when
// MUSIXMATCH_TOKEN is absent.
func TestLoad_EnvTokenFallback(t *testing.T) {
	isolateEnv(t)
	// MUSIXMATCH_TOKEN is already cleared by isolateEnv.
	t.Setenv("MXLRC_API_TOKEN", "token-mxlrc")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Token != "token-mxlrc" {
		t.Errorf("token = %q; want %q", cfg.API.Token, "token-mxlrc")
	}
}

// TestLoad_EnvCooldownPrecedence verifies MXLRC_API_COOLDOWN > MXLRC_COOLDOWN.
func TestLoad_EnvCooldownPrecedence(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_COOLDOWN", "30")
	t.Setenv("MXLRC_COOLDOWN", "99")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Cooldown != 30 {
		t.Errorf("cooldown = %d; want 30 (MXLRC_API_COOLDOWN must win)", cfg.API.Cooldown)
	}
}

// TestLoad_EnvCooldownFallback verifies MXLRC_COOLDOWN is used when
// MXLRC_API_COOLDOWN is absent.
func TestLoad_EnvCooldownFallback(t *testing.T) {
	isolateEnv(t)
	// MXLRC_API_COOLDOWN is already cleared by isolateEnv.
	t.Setenv("MXLRC_COOLDOWN", "42")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Cooldown != 42 {
		t.Errorf("cooldown = %d; want 42", cfg.API.Cooldown)
	}
}

// TestLoad_EnvCooldownZeroIsValid verifies that cooldown=0 is accepted (not
// treated as "unset" or invalid).
func TestLoad_EnvCooldownZeroIsValid(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_COOLDOWN", "0")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Cooldown != 0 {
		t.Errorf("cooldown = %d; want 0", cfg.API.Cooldown)
	}
}

// TestLoad_EnvCooldownInvalidIsIgnored verifies that a non-numeric cooldown
// env var falls back to the current value rather than crashing.
func TestLoad_EnvCooldownInvalidIsIgnored(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_COOLDOWN", "notanumber")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.Cooldown != 15 {
		t.Errorf("cooldown = %d; want 15 (default) after invalid env var", cfg.API.Cooldown)
	}
}

// TestLoad_ServerIntervalDefaults verifies the service-loop intervals fall back
// to their built-in defaults: scan 900s and work 0 (meaning "use api.cooldown").
func TestLoad_ServerIntervalDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ScanIntervalSeconds != 900 {
		t.Errorf("scan_interval_seconds = %d; want 900", cfg.Server.ScanIntervalSeconds)
	}
	if cfg.Server.WorkIntervalSeconds != 0 {
		t.Errorf("work_interval_seconds = %d; want 0 (fall back to api.cooldown)", cfg.Server.WorkIntervalSeconds)
	}
}

// TestLoad_EnvServerIntervals verifies MXLRC_SCAN_INTERVAL and MXLRC_WORK_INTERVAL
// override the config values.
func TestLoad_EnvServerIntervals(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_SCAN_INTERVAL", "300")
	t.Setenv("MXLRC_WORK_INTERVAL", "20")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ScanIntervalSeconds != 300 {
		t.Errorf("scan_interval_seconds = %d; want 300", cfg.Server.ScanIntervalSeconds)
	}
	if cfg.Server.WorkIntervalSeconds != 20 {
		t.Errorf("work_interval_seconds = %d; want 20", cfg.Server.WorkIntervalSeconds)
	}
}

// TestLoad_EnvServerIntervalZeroIsValid verifies a zero scan interval is honored
// (it disables repeat scanning) rather than being re-defaulted.
func TestLoad_EnvServerIntervalZeroIsValid(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_SCAN_INTERVAL", "0")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ScanIntervalSeconds != 0 {
		t.Errorf("scan_interval_seconds = %d; want 0", cfg.Server.ScanIntervalSeconds)
	}
}

// TestLoad_EnvServerIntervalInvalidIsIgnored verifies a non-numeric interval env
// var falls back to the current value rather than crashing.
func TestLoad_EnvServerIntervalInvalidIsIgnored(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_SCAN_INTERVAL", "notanumber")
	t.Setenv("MXLRC_WORK_INTERVAL", "-5")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ScanIntervalSeconds != 900 {
		t.Errorf("scan_interval_seconds = %d; want 900 (default) after invalid env var", cfg.Server.ScanIntervalSeconds)
	}
	if cfg.Server.WorkIntervalSeconds != 0 {
		t.Errorf("work_interval_seconds = %d; want 0 (default) after invalid env var", cfg.Server.WorkIntervalSeconds)
	}
}

// TestLoad_DBPathFromEnv verifies MXLRC_DB_PATH overrides the computed default.
func TestLoad_DBPathFromEnv(t *testing.T) {
	isolateEnv(t)
	want := filepath.Join(t.TempDir(), "custom.db")
	t.Setenv("MXLRC_DB_PATH", want)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Path != want {
		t.Errorf("DB.Path = %q; want %q", cfg.DB.Path, want)
	}
}

func TestLoad_DockerModeUsesConfigForStorageDefaults(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_DOCKER", "true")
	t.Setenv("MXLRC_DB_PATH", "")
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DB.Path != filepath.Join("/config", "mxlrcgo.db") {
		t.Errorf("DB.Path = %q; want Docker /config DB path", cfg.DB.Path)
	}
}

func TestDockerModeAcceptedValues(t *testing.T) {
	isolateEnv(t)

	t.Setenv("MXLRC_DOCKER", "1")
	if !dockerMode() {
		t.Fatal("dockerMode false for MXLRC_DOCKER=1; want true")
	}

	t.Setenv("MXLRC_DOCKER", "TRUE")
	if !dockerMode() {
		t.Fatal("dockerMode false for MXLRC_DOCKER=TRUE; want true")
	}

	t.Setenv("MXLRC_DOCKER", "  true  ")
	if !dockerMode() {
		t.Fatal("dockerMode false for spaced MXLRC_DOCKER=true; want true")
	}

	t.Setenv("MXLRC_DOCKER", "false")
	if dockerMode() {
		t.Fatal("dockerMode true for MXLRC_DOCKER=false; want false")
	}
}

func TestLoad_ServerConfigFromFileAndEnv(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := "[server]\n" +
		"addr = \"127.0.0.1:9999\"\n" +
		"webhook_api_keys = [\"file-key\"]\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv("MXLRC_SERVER_ADDR", "127.0.0.1:8888")
	t.Setenv("MXLRC_WEBHOOK_API_KEY", "env-a, env-b")

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Addr != "127.0.0.1:8888" {
		t.Fatalf("server addr = %q; want env override", cfg.Server.Addr)
	}
	if len(cfg.Server.WebhookAPIKeys) != 2 || cfg.Server.WebhookAPIKeys[0] != "env-a" || cfg.Server.WebhookAPIKeys[1] != "env-b" {
		t.Fatalf("webhook keys = %+v; want env keys", cfg.Server.WebhookAPIKeys)
	}
}

func TestLoad_ProvidersAndVerificationFromFileAndEnv(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := "[providers]\n" +
		"primary = \"musixmatch\"\n" +
		"disabled = [\"future\"]\n" +
		"[verification]\n" +
		"enabled = true\n" +
		"whisper_url = \"http://whisper:9000\"\n" +
		"ffmpeg_path = \"/usr/bin/ffmpeg\"\n" +
		"sample_duration_seconds = 45\n" +
		"min_confidence = 0.8\n" +
		"min_similarity = 0.4\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}
	t.Setenv("MXLRC_PROVIDER_PRIMARY", "override")
	t.Setenv("MXLRC_PROVIDERS_DISABLED", "musixmatch, other")
	t.Setenv("MXLRC_VERIFICATION_ENABLED", "false")
	t.Setenv("MXLRC_VERIFICATION_WHISPER_URL", "http://env-whisper:9000")
	t.Setenv("MXLRC_WHISPER_URL", "http://legacy-whisper:9000")
	t.Setenv("MXLRC_VERIFICATION_FFMPEG_PATH", "/opt/ffmpeg")
	t.Setenv("MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "60")
	t.Setenv("MXLRC_VERIFICATION_SAMPLE_DURATION", "45")
	t.Setenv("MXLRC_VERIFICATION_MIN_CONFIDENCE", "0.7")
	t.Setenv("MXLRC_VERIFICATION_MIN_SIMILARITY", "0.5")

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Providers.Primary != "override" {
		t.Fatalf("providers.primary = %q; want override", cfg.Providers.Primary)
	}
	if len(cfg.Providers.Disabled) != 2 || cfg.Providers.Disabled[0] != "musixmatch" || cfg.Providers.Disabled[1] != "other" {
		t.Fatalf("providers.disabled = %#v; want musixmatch,other", cfg.Providers.Disabled)
	}
	if cfg.Verification.Enabled {
		t.Fatal("verification.enabled = true; want env override false")
	}
	if cfg.Verification.WhisperURL != "http://env-whisper:9000" {
		t.Fatalf("verification.whisper_url = %q; want env value", cfg.Verification.WhisperURL)
	}
	if cfg.Verification.SampleDurationSeconds != 60 {
		t.Fatalf("verification.sample_duration_seconds = %d; want 60", cfg.Verification.SampleDurationSeconds)
	}
	if cfg.Verification.FFmpegPath != "/opt/ffmpeg" {
		t.Fatalf("verification.ffmpeg_path = %q; want env value", cfg.Verification.FFmpegPath)
	}
	if cfg.Verification.MinConfidence != 0.7 {
		t.Fatalf("verification.min_confidence = %v; want 0.7", cfg.Verification.MinConfidence)
	}
	if cfg.Verification.MinSimilarity != 0.5 {
		t.Fatalf("verification.min_similarity = %v; want 0.5", cfg.Verification.MinSimilarity)
	}
}

func TestLoad_VerificationEnvLegacyFallbacks(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_WHISPER_URL", "http://legacy-whisper:9000")
	t.Setenv("MXLRC_VERIFICATION_SAMPLE_DURATION", "45")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Verification.WhisperURL != "http://legacy-whisper:9000" {
		t.Fatalf("verification.whisper_url = %q; want legacy env value", cfg.Verification.WhisperURL)
	}
	if cfg.Verification.SampleDurationSeconds != 45 {
		t.Fatalf("verification.sample_duration_seconds = %d; want legacy duration", cfg.Verification.SampleDurationSeconds)
	}
}

func TestLoad_BlankProviderAndInvalidVerificationSampleReDefault(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := "[providers]\nprimary = \"\"\n[verification]\nsample_duration_seconds = 0\nmin_confidence = 2\nmin_similarity = -1\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Providers.Primary != "musixmatch" {
		t.Fatalf("providers.primary = %q; want musixmatch", cfg.Providers.Primary)
	}
	if cfg.Verification.SampleDurationSeconds != 30 {
		t.Fatalf("verification.sample_duration_seconds = %d; want 30", cfg.Verification.SampleDurationSeconds)
	}
	if cfg.Verification.MinConfidence != 0.85 {
		t.Fatalf("verification.min_confidence = %v; want 0.85", cfg.Verification.MinConfidence)
	}
	if cfg.Verification.MinSimilarity != 0.35 {
		t.Fatalf("verification.min_similarity = %v; want 0.35", cfg.Verification.MinSimilarity)
	}
}

// TestLoad_CircuitOpenDurationDefault verifies the default 30 min window.
func TestLoad_CircuitOpenDurationDefault(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitOpenDuration != 30*60 {
		t.Fatalf("CircuitOpenDuration = %d; want 1800", cfg.API.CircuitOpenDuration)
	}
}

// TestLoad_CircuitOpenDurationEnvOverride verifies the env var overrides
// the default.
func TestLoad_CircuitOpenDurationEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_CIRCUIT_OPEN_DURATION", "1200")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitOpenDuration != 1200 {
		t.Fatalf("CircuitOpenDuration = %d; want 1200", cfg.API.CircuitOpenDuration)
	}
}

// TestLoad_CircuitOpenDurationClampsBelowMinimum verifies values below the
// 5 min minimum are clamped up.
func TestLoad_CircuitOpenDurationClampsBelowMinimum(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_CIRCUIT_OPEN_DURATION", "60")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitOpenDuration != 5*60 {
		t.Fatalf("CircuitOpenDuration = %d; want 300 (clamped)", cfg.API.CircuitOpenDuration)
	}
}

// TestLoad_InvalidTOMLReturnsError verifies that a malformed TOML file is
// reported as an error.
func TestLoad_InvalidTOMLReturnsError(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgFile, []byte("not valid toml ]["), 0600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Load(cfgFile)
	if err == nil {
		t.Fatal("Load with invalid TOML returned nil error; want an error")
	}
}
