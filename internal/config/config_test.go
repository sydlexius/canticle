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
		"MXLRC_API_CIRCUIT_OPEN_DURATION", "MXLRC_API_CIRCUIT_BACKOFF_BASE",
		"MXLRC_MISS_BACKOFF_BASE_HOURS", "MXLRC_MISS_BACKOFF_CAP_HOURS", "MXLRC_MAX_MISS_ATTEMPTS",
		"MXLRC_OUTPUT_DIR", "MXLRC_BILINGUAL_OUTPUT", "MXLRC_SERVER_ADDR", "MXLRC_WEBHOOK_API_KEY",
		"MXLRC_SCAN_INTERVAL", "MXLRC_WORK_INTERVAL",
		"MXLRC_PROVIDER_PRIMARY", "MXLRC_PROVIDERS_DISABLED", "MXLRC_PROVIDERS_MODE", "MXLRC_PROVIDERS_FALLBACK_ORDER",
		"MXLRC_PROVIDERS_RACE_WAIT_SECONDS",
		"MXLRC_VERIFICATION_ENABLED", "MXLRC_VERIFICATION_WHISPER_URL", "MXLRC_WHISPER_URL",
		"MXLRC_VERIFICATION_FFMPEG_PATH",
		"MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "MXLRC_VERIFICATION_SAMPLE_DURATION",
		"MXLRC_VERIFICATION_MIN_CONFIDENCE", "MXLRC_VERIFICATION_MIN_SIMILARITY",
		"MXLRC_GUARD_ACCEPTED_SCRIPTS", "MXLRC_GUARD_THRESHOLD",
		"MXLRC_QUEUE_RANDOMIZE",
		"MXLRC_LOG_LEVEL", "MXLRC_LOG_FORMAT", "MXLRC_LOG_FILE",
		"MXLRC_LOG_MAX_SIZE_MB", "MXLRC_LOG_MAX_FILES", "MXLRC_LOG_MAX_AGE_DAYS", "MXLRC_LOG_COMPRESS",
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
	if cfg.Providers.Mode != "ordered" {
		t.Errorf("default providers mode = %q; want ordered", cfg.Providers.Mode)
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
	if len(cfg.Guard.AcceptedScripts) != 0 {
		t.Errorf("default guard accepted scripts = %v; want empty (disabled)", cfg.Guard.AcceptedScripts)
	}
	if cfg.Guard.Threshold != 0.20 {
		t.Errorf("default guard threshold = %v; want 0.20", cfg.Guard.Threshold)
	}
}

func TestLoad_GuardThresholdReDefaultsWhenUndefinedOrInvalid(t *testing.T) {
	for name, tomlBody := range map[string]string{
		"omitted":   "[guard]\naccepted_scripts = [\"Latin\"]\n",
		"zero":      "[guard]\nscript_guard_threshold = 0.0\n",
		"too large": "[guard]\nscript_guard_threshold = 1.5\n",
	} {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tomlBody), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Guard.Threshold != 0.20 {
				t.Errorf("guard threshold = %v; want 0.20 (re-defaulted)", cfg.Guard.Threshold)
			}
		})
	}
}

func TestLoad_GuardThresholdExplicitValueHonored(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	body := "[guard]\naccepted_scripts = [\"Latin\", \"Han\"]\nscript_guard_threshold = 0.5\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Guard.Threshold != 0.5 {
		t.Errorf("guard threshold = %v; want 0.5", cfg.Guard.Threshold)
	}
	if len(cfg.Guard.AcceptedScripts) != 2 || cfg.Guard.AcceptedScripts[0] != "Latin" || cfg.Guard.AcceptedScripts[1] != "Han" {
		t.Errorf("guard accepted scripts = %v; want [Latin Han]", cfg.Guard.AcceptedScripts)
	}
}

func TestLoad_EnvGuardOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_GUARD_ACCEPTED_SCRIPTS", "Latin, Hangul")
	t.Setenv("MXLRC_GUARD_THRESHOLD", "0.4")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Guard.AcceptedScripts) != 2 || cfg.Guard.AcceptedScripts[0] != "Latin" || cfg.Guard.AcceptedScripts[1] != "Hangul" {
		t.Errorf("guard accepted scripts = %v; want [Latin Hangul]", cfg.Guard.AcceptedScripts)
	}
	if cfg.Guard.Threshold != 0.4 {
		t.Errorf("guard threshold = %v; want 0.4", cfg.Guard.Threshold)
	}
}

func TestLoad_EnvGuardThresholdInvalidIsIgnored(t *testing.T) {
	for name, val := range map[string]string{
		"not a number": "abc",
		"zero":         "0",
		"too large":    "2",
	} {
		t.Run(name, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv("MXLRC_GUARD_THRESHOLD", val)
			cfg, err := Load("")
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Guard.Threshold != 0.20 {
				t.Errorf("guard threshold = %v; want 0.20 (invalid env kept default)", cfg.Guard.Threshold)
			}
		})
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

// TestLoadWithSources_EnvHintReflectsAppliedOverrides verifies that the
// applied-env map returned by LoadWithSources marks a field "(env)" only when
// its override actually took effect: a valid numeric env applies and is
// recorded; an invalid one is rejected and is NOT recorded (the false-label bug
// fixed in F1).
func TestLoadWithSources_EnvHintReflectsAppliedOverrides(t *testing.T) {
	t.Run("valid applies and is recorded", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_API_COOLDOWN", "30")

		cfg, envSrc, err := LoadWithSources("")
		if err != nil {
			t.Fatalf("LoadWithSources: %v", err)
		}
		if cfg.API.Cooldown != 30 {
			t.Errorf("cooldown = %d; want 30", cfg.API.Cooldown)
		}
		if !envSrc["api.cooldown"] {
			t.Error("envSrc[api.cooldown] = false; want true when a valid env override applied")
		}
	})

	t.Run("invalid is rejected and not recorded", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_API_COOLDOWN", "notanumber")

		cfg, envSrc, err := LoadWithSources("")
		if err != nil {
			t.Fatalf("LoadWithSources: %v", err)
		}
		if cfg.API.Cooldown != 15 {
			t.Errorf("cooldown = %d; want 15 (default) after invalid env var", cfg.API.Cooldown)
		}
		if envSrc["api.cooldown"] {
			t.Error("envSrc[api.cooldown] = true; want false when the env value was invalid and rejected")
		}
	})
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

// TestLoad_CircuitBackoffBaseDefault verifies the default 60s trip-1 window.
func TestLoad_CircuitBackoffBaseDefault(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 60 {
		t.Fatalf("CircuitBackoffBase = %d; want 60", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseEnvOverride verifies the env var overrides the default.
func TestLoad_CircuitBackoffBaseEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_CIRCUIT_BACKOFF_BASE", "120")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 120 {
		t.Fatalf("CircuitBackoffBase = %d; want 120", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseClampsBelowMinimum verifies values below the 15s
// floor are clamped up.
func TestLoad_CircuitBackoffBaseClampsBelowMinimum(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_CIRCUIT_BACKOFF_BASE", "5")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 15 {
		t.Fatalf("CircuitBackoffBase = %d; want 15 (clamped to floor)", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseClampedToCapWhenExceeds verifies the base cannot
// exceed the circuit_open_duration cap.
func TestLoad_CircuitBackoffBaseClampedToCapWhenExceeds(t *testing.T) {
	isolateEnv(t)
	// Cap clamps to its 300s floor; a 600s base must clamp down to that cap.
	t.Setenv("MXLRC_API_CIRCUIT_OPEN_DURATION", "300")
	t.Setenv("MXLRC_API_CIRCUIT_BACKOFF_BASE", "600")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 300 {
		t.Fatalf("CircuitBackoffBase = %d; want 300 (clamped down to the cap)", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseExplicitZeroRedefaults verifies that an explicit
// circuit_backoff_base_seconds = 0 in a config file restores the default rather
// than leaving the breaker with a zero base (the Load() re-default block).
func TestLoad_CircuitBackoffBaseExplicitZeroRedefaults(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[api]\ncircuit_backoff_base_seconds = 0\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 60 {
		t.Fatalf("CircuitBackoffBase = %d; want 60 (explicit 0 re-defaulted)", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseInvalidEnvKeepsCurrent verifies a non-numeric env
// value is ignored with a warning, leaving the current value intact.
func TestLoad_CircuitBackoffBaseInvalidEnvKeepsCurrent(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_API_CIRCUIT_BACKOFF_BASE", "not-a-number")
	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 60 {
		t.Fatalf("CircuitBackoffBase = %d; want 60 (invalid env ignored)", cfg.API.CircuitBackoffBase)
	}
}

// TestLoad_CircuitBackoffBaseNegativeClampsToDefault verifies an explicit
// negative value in the file (which survives the zero re-default) is clamped
// back to the default by clampCircuitBackoffBase.
func TestLoad_CircuitBackoffBaseNegativeClampsToDefault(t *testing.T) {
	isolateEnv(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("[api]\ncircuit_backoff_base_seconds = -5\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.CircuitBackoffBase != 60 {
		t.Fatalf("CircuitBackoffBase = %d; want 60 (negative clamped to default)", cfg.API.CircuitBackoffBase)
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

// TestLoad_MissBackoffDefaults verifies that the built-in defaults for the
// miss-cadence knobs match the documented values.
func TestLoad_MissBackoffDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MissBackoffBaseHours != 168 {
		t.Fatalf("MissBackoffBaseHours = %d; want 168", cfg.API.MissBackoffBaseHours)
	}
	if cfg.API.MissBackoffCapHours != 672 {
		t.Fatalf("MissBackoffCapHours = %d; want 672", cfg.API.MissBackoffCapHours)
	}
	if cfg.API.MaxMissAttempts != 15 {
		t.Fatalf("MaxMissAttempts = %d; want 15", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MaxMissAttemptsOmittedInTOMLGetsDefault verifies that when
// max_miss_attempts is absent from the TOML file, Load restores the default
// (15) rather than leaving the TOML zero-value (0 = no cap). This matters
// because plain-int TOML cannot distinguish "omitted" from "explicit 0";
// MetaData.IsDefined is used to detect the omitted case.
func TestLoad_MaxMissAttemptsOmittedInTOMLGetsDefault(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	// TOML file with [api] table but no max_miss_attempts key.
	if err := os.WriteFile(cfgFile, []byte("[api]\ncooldown = 10\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MaxMissAttempts != 15 {
		t.Fatalf("MaxMissAttempts = %d; want 15 (omitted key must restore default)", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MaxMissAttemptsExplicitZeroInTOMLIsPreserved verifies that an
// explicit max_miss_attempts = 0 in the TOML file is honored as "no cap"
// and not overwritten by the default (15).
func TestLoad_MaxMissAttemptsExplicitZeroInTOMLIsPreserved(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgFile, []byte("[api]\nmax_miss_attempts = 0\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MaxMissAttempts != 0 {
		t.Fatalf("MaxMissAttempts = %d; want 0 (explicit 0 must be preserved as no-cap)", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MaxMissAttemptsExplicitNonZeroInTOML verifies a positive
// max_miss_attempts value in the TOML file is picked up correctly.
func TestLoad_MaxMissAttemptsExplicitNonZeroInTOML(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgFile, []byte("[api]\nmax_miss_attempts = 5\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MaxMissAttempts != 5 {
		t.Fatalf("MaxMissAttempts = %d; want 5", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MaxMissAttemptsEnvZeroIsNoCapEvenWithDefault verifies that
// MXLRC_MAX_MISS_ATTEMPTS=0 results in 0 (no cap), overriding the default of 15.
func TestLoad_MaxMissAttemptsEnvZeroIsNoCap(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_MAX_MISS_ATTEMPTS", "0")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MaxMissAttempts != 0 {
		t.Fatalf("MaxMissAttempts = %d; want 0 (MXLRC_MAX_MISS_ATTEMPTS=0 must mean no cap)", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MissBackoffFromEnv verifies that the three miss-cadence env vars
// override the defaults.
func TestLoad_MissBackoffFromEnv(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_MISS_BACKOFF_BASE_HOURS", "48")
	t.Setenv("MXLRC_MISS_BACKOFF_CAP_HOURS", "336")
	t.Setenv("MXLRC_MAX_MISS_ATTEMPTS", "5")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MissBackoffBaseHours != 48 {
		t.Fatalf("MissBackoffBaseHours = %d; want 48", cfg.API.MissBackoffBaseHours)
	}
	if cfg.API.MissBackoffCapHours != 336 {
		t.Fatalf("MissBackoffCapHours = %d; want 336", cfg.API.MissBackoffCapHours)
	}
	if cfg.API.MaxMissAttempts != 5 {
		t.Fatalf("MaxMissAttempts = %d; want 5", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MissBackoffClampsBase verifies that base < 1h is clamped to 1h.
func TestLoad_MissBackoffClampsBase(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_MISS_BACKOFF_BASE_HOURS", "0")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// 0 is treated as "not set" by env override (env override only applies >0);
	// clampMissBackoff then raises 0 to the default, which after the env parse
	// becomes the built-in default (168h / 7d).
	if cfg.API.MissBackoffBaseHours < 1 {
		t.Fatalf("MissBackoffBaseHours = %d; want >= 1 (clamped from 0)", cfg.API.MissBackoffBaseHours)
	}
}

// TestLoad_MissBackoffClampsCapBelowBase verifies that cap < base is clamped
// up to base.
func TestLoad_MissBackoffClampsCapBelowBase(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_MISS_BACKOFF_BASE_HOURS", "48")
	t.Setenv("MXLRC_MISS_BACKOFF_CAP_HOURS", "24")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MissBackoffCapHours < cfg.API.MissBackoffBaseHours {
		t.Fatalf("MissBackoffCapHours (%d) < MissBackoffBaseHours (%d); should be clamped to base", cfg.API.MissBackoffCapHours, cfg.API.MissBackoffBaseHours)
	}
	if cfg.API.MissBackoffCapHours != 48 {
		t.Fatalf("MissBackoffCapHours = %d; want 48 (clamped to base)", cfg.API.MissBackoffCapHours)
	}
}

// TestLoad_MaxMissAttemptsClampsNegative verifies that negative MaxMissAttempts
// is clamped to 0.
func TestLoad_MaxMissAttemptsClampsNegative(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_MAX_MISS_ATTEMPTS", "-1")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// -1 is not parsed (env override only sets positive; negative falls through
	// and clampMissBackoff clamps to 0).
	if cfg.API.MaxMissAttempts != 0 {
		t.Fatalf("MaxMissAttempts = %d; want 0 (clamped from negative)", cfg.API.MaxMissAttempts)
	}
}

// TestLoad_MissBackoffFromTOML verifies that TOML values are picked up.
func TestLoad_MissBackoffFromTOML(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgFile, []byte(`
[api]
miss_backoff_base_hours = 12
miss_backoff_cap_hours = 96
max_miss_attempts = 10
`), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.API.MissBackoffBaseHours != 12 {
		t.Fatalf("MissBackoffBaseHours = %d; want 12", cfg.API.MissBackoffBaseHours)
	}
	if cfg.API.MissBackoffCapHours != 96 {
		t.Fatalf("MissBackoffCapHours = %d; want 96", cfg.API.MissBackoffCapHours)
	}
	if cfg.API.MaxMissAttempts != 10 {
		t.Fatalf("MaxMissAttempts = %d; want 10", cfg.API.MaxMissAttempts)
	}
}

func TestLoad_QueueRandomizeDefaultsTrue(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Queue.Randomize {
		t.Fatal("queue.randomize = false; want default true")
	}
}

func TestLoad_QueueRandomizeEnvOverride(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_QUEUE_RANDOMIZE", "false")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Queue.Randomize {
		t.Fatal("queue.randomize = true; want env override false")
	}
}

func TestLoad_QueueRandomizeTOMLFalse(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgFile, []byte("[queue]\nrandomize = false\n"), 0600); err != nil {
		t.Fatalf("write config file: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Queue.Randomize {
		t.Fatal("queue.randomize = true; want TOML override false")
	}
}

func TestLoad_QueueRandomizeInvalidEnvKeepsCurrent(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_QUEUE_RANDOMIZE", "notabool")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Invalid env warns and keeps the current (default true) value.
	if !cfg.Queue.Randomize {
		t.Fatal("queue.randomize = false; want unchanged default true on invalid env")
	}
}

// TestLoad_LoggingDefaults verifies built-in logging defaults when no TOML
// section is present.
func TestLoad_LoggingDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q; want info", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Logging.Format = %q; want text", cfg.Logging.Format)
	}
	if cfg.Logging.File != "" {
		t.Errorf("Logging.File = %q; want empty", cfg.Logging.File)
	}
	if cfg.Logging.MaxSizeMB != 10 {
		t.Errorf("Logging.MaxSizeMB = %d; want 10", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 5 {
		t.Errorf("Logging.MaxFiles = %d; want 5", cfg.Logging.MaxFiles)
	}
	if cfg.Logging.MaxAgeDays != 30 {
		t.Errorf("Logging.MaxAgeDays = %d; want 30", cfg.Logging.MaxAgeDays)
	}
	if !cfg.Logging.Compress {
		t.Error("Logging.Compress = false; want default true")
	}
}

// TestLoad_LoggingFromTOML verifies that a [logging] section in the TOML file
// is decoded correctly.
func TestLoad_LoggingFromTOML(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := "[logging]\nlevel = \"debug\"\nformat = \"json\"\nfile = \"/var/log/mxlrc.log\"\nmax_size_mb = 20\nmax_files = 3\nmax_age_days = 7\ncompress = false\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q; want debug", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("Logging.Format = %q; want json", cfg.Logging.Format)
	}
	if cfg.Logging.File != "/var/log/mxlrc.log" {
		t.Errorf("Logging.File = %q; want /var/log/mxlrc.log", cfg.Logging.File)
	}
	if cfg.Logging.MaxSizeMB != 20 {
		t.Errorf("Logging.MaxSizeMB = %d; want 20", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 3 {
		t.Errorf("Logging.MaxFiles = %d; want 3", cfg.Logging.MaxFiles)
	}
	if cfg.Logging.MaxAgeDays != 7 {
		t.Errorf("Logging.MaxAgeDays = %d; want 7", cfg.Logging.MaxAgeDays)
	}
	if cfg.Logging.Compress {
		t.Error("Logging.Compress = true; want explicit false from TOML")
	}
}

// TestLoad_LoggingEnvOverrides verifies that MXLRC_LOG_* env vars override
// TOML and default values, following the same precedence pattern as other
// config sections.
func TestLoad_LoggingEnvOverrides(t *testing.T) {
	isolateEnv(t)

	// Write a TOML file with non-default values to prove env wins.
	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := "[logging]\nlevel = \"warn\"\nformat = \"json\"\nmax_size_mb = 50\n"
	if err := os.WriteFile(cfgFile, []byte(content), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("MXLRC_LOG_LEVEL", "debug")
	t.Setenv("MXLRC_LOG_FORMAT", "text")
	t.Setenv("MXLRC_LOG_FILE", "/tmp/override.log")
	t.Setenv("MXLRC_LOG_MAX_SIZE_MB", "25")
	t.Setenv("MXLRC_LOG_MAX_FILES", "2")
	t.Setenv("MXLRC_LOG_MAX_AGE_DAYS", "14")
	t.Setenv("MXLRC_LOG_COMPRESS", "false")

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q; want debug (env override)", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "text" {
		t.Errorf("Logging.Format = %q; want text (env override)", cfg.Logging.Format)
	}
	if cfg.Logging.File != "/tmp/override.log" {
		t.Errorf("Logging.File = %q; want /tmp/override.log (env override)", cfg.Logging.File)
	}
	if cfg.Logging.MaxSizeMB != 25 {
		t.Errorf("Logging.MaxSizeMB = %d; want 25 (env override)", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 2 {
		t.Errorf("Logging.MaxFiles = %d; want 2 (env override)", cfg.Logging.MaxFiles)
	}
	if cfg.Logging.MaxAgeDays != 14 {
		t.Errorf("Logging.MaxAgeDays = %d; want 14 (env override)", cfg.Logging.MaxAgeDays)
	}
	if cfg.Logging.Compress {
		t.Error("Logging.Compress = true; want false (env override)")
	}
}

// TestLoad_LoggingEnvInvalidIntsIgnored verifies that non-numeric values for
// integer logging env vars are silently ignored, leaving the current value intact.
func TestLoad_LoggingEnvInvalidIntsIgnored(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_LOG_MAX_SIZE_MB", "notanumber")
	t.Setenv("MXLRC_LOG_MAX_FILES", "bad")
	t.Setenv("MXLRC_LOG_MAX_AGE_DAYS", "-5")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// All three should remain at their defaults.
	if cfg.Logging.MaxSizeMB != 10 {
		t.Errorf("MaxSizeMB = %d; want 10 (default after invalid env)", cfg.Logging.MaxSizeMB)
	}
	if cfg.Logging.MaxFiles != 5 {
		t.Errorf("MaxFiles = %d; want 5 (default after invalid env)", cfg.Logging.MaxFiles)
	}
	if cfg.Logging.MaxAgeDays != 30 {
		t.Errorf("MaxAgeDays = %d; want 30 (default after invalid env)", cfg.Logging.MaxAgeDays)
	}
}

// TestLoad_LoggingCompressDefaultTrueWhenKeyOmitted verifies that an omitted
// compress key in TOML restores the default (true), not the zero-value (false).
func TestLoad_LoggingCompressDefaultTrueWhenKeyOmitted(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	// [logging] table exists but compress is not set.
	if err := os.WriteFile(cfgFile, []byte("[logging]\nlevel = \"debug\"\n"), 0600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Logging.Compress {
		t.Error("Logging.Compress = false; want default true when key is omitted from TOML")
	}
}

func TestProvidersModeEnvOverrideAndValidation(t *testing.T) {
	t.Run("env override accepts ordered", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_MODE", "ORDERED") // case-insensitive
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.Mode != "ordered" {
			t.Fatalf("providers.mode = %q; want ordered", cfg.Providers.Mode)
		}
	})

	t.Run("env override accepts parallel", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_MODE", "PARALLEL") // case-insensitive
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load with providers.mode=parallel: %v", err)
		}
		if cfg.Providers.Mode != "parallel" {
			t.Fatalf("providers.mode = %q; want parallel", cfg.Providers.Mode)
		}
	})

	t.Run("unsupported mode is rejected", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_MODE", "sequential")
		_, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err == nil {
			t.Fatal("Load with providers.mode=sequential returned nil error; want a rejection (unknown mode)")
		}
	})

	t.Run("blank mode restores default", func(t *testing.T) {
		isolateEnv(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[providers]\nmode = \"\"\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.Mode != "ordered" {
			t.Fatalf("blank mode = %q; want ordered default", cfg.Providers.Mode)
		}
	})
}

func TestProvidersRaceWaitSeconds(t *testing.T) {
	t.Run("default is 2", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.RaceWaitSeconds != 2 {
			t.Fatalf("default race_wait_seconds = %d; want 2", cfg.Providers.RaceWaitSeconds)
		}
	})

	t.Run("parsed from file", func(t *testing.T) {
		isolateEnv(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[providers]\nrace_wait_seconds = 5\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.RaceWaitSeconds != 5 {
			t.Fatalf("race_wait_seconds = %d; want 5", cfg.Providers.RaceWaitSeconds)
		}
	})

	t.Run("env override", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_RACE_WAIT_SECONDS", "7")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.RaceWaitSeconds != 7 {
			t.Fatalf("race_wait_seconds = %d; want 7", cfg.Providers.RaceWaitSeconds)
		}
	})

	t.Run("non-positive is clamped to default", func(t *testing.T) {
		isolateEnv(t)
		path := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(path, []byte("[providers]\nrace_wait_seconds = 0\n"), 0o600); err != nil {
			t.Fatalf("write config: %v", err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.RaceWaitSeconds != 2 {
			t.Fatalf("clamped race_wait_seconds = %d; want 2 (default)", cfg.Providers.RaceWaitSeconds)
		}
	})

	t.Run("unparsable env is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_RACE_WAIT_SECONDS", "abc")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.Providers.RaceWaitSeconds != 2 {
			t.Fatalf("race_wait_seconds = %d; want 2 (unparsable env ignored)", cfg.Providers.RaceWaitSeconds)
		}
	})
}

func TestProvidersFallbackOrder(t *testing.T) {
	t.Run("default is empty", func(t *testing.T) {
		isolateEnv(t)
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(cfg.Providers.FallbackOrder) != 0 {
			t.Fatalf("default fallback_order = %v; want empty", cfg.Providers.FallbackOrder)
		}
	})

	t.Run("env override normalizes and dedups", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_FALLBACK_ORDER", "PetitLyrics, petitlyrics , musixmatch")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		got := cfg.Providers.FallbackOrder
		want := []string{"petitlyrics", "musixmatch"}
		if len(got) != len(want) {
			t.Fatalf("fallback_order = %v; want %v", got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("fallback_order = %v; want %v", got, want)
			}
		}
	})

	t.Run("unknown provider is rejected", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_PROVIDERS_FALLBACK_ORDER", "petitlyrics, bogus")
		if _, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml")); err == nil {
			t.Fatal("Load with an unknown fallback provider returned nil error; want a rejection")
		}
	})
}

// TestLoad_InstrumentalDetectorDefaults verifies built-in defaults for the
// InstrumentalDetectorConfig section when no TOML or env overrides are present.
func TestLoad_InstrumentalDetectorDefaults(t *testing.T) {
	isolateEnv(t)

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = true; want false (disabled by default)")
	}
	if cfg.InstrumentalDetector.FFmpegPath != "ffmpeg" {
		t.Errorf("InstrumentalDetector.FFmpegPath = %q; want ffmpeg", cfg.InstrumentalDetector.FFmpegPath)
	}
	if cfg.InstrumentalDetector.SampleDurationSeconds != 30 {
		t.Errorf("InstrumentalDetector.SampleDurationSeconds = %d; want 30", cfg.InstrumentalDetector.SampleDurationSeconds)
	}
	if cfg.InstrumentalDetector.MinConfidence != 0.90 {
		t.Errorf("InstrumentalDetector.MinConfidence = %v; want 0.90", cfg.InstrumentalDetector.MinConfidence)
	}
	if len(cfg.InstrumentalDetector.InstrumentalClasses) != 2 ||
		cfg.InstrumentalDetector.InstrumentalClasses[0] != "Music" ||
		cfg.InstrumentalDetector.InstrumentalClasses[1] != "Musical instrument" {
		t.Errorf("InstrumentalDetector.InstrumentalClasses = %v; want [Music, Musical instrument]", cfg.InstrumentalDetector.InstrumentalClasses)
	}
	if cfg.InstrumentalDetector.CooldownSeconds != 5 {
		t.Errorf("InstrumentalDetector.CooldownSeconds = %d; want 5", cfg.InstrumentalDetector.CooldownSeconds)
	}
	if cfg.InstrumentalDetector.ClassifierURL != "" {
		t.Errorf("InstrumentalDetector.ClassifierURL = %q; want empty (not set)", cfg.InstrumentalDetector.ClassifierURL)
	}
}

// TestLoad_InstrumentalDetectorEnvEnabled verifies MXLRC_INSTRUMENTAL_DETECTOR_ENABLED
// overrides the default disabled state.
func TestLoad_InstrumentalDetectorEnvEnabled(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_ENABLED", "true")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = false; want true (env override)")
	}
}

// TestLoad_InstrumentalDetectorEnvEnabledInvalidIgnored verifies that an invalid
// MXLRC_INSTRUMENTAL_DETECTOR_ENABLED value falls back to the current (default false).
func TestLoad_InstrumentalDetectorEnvEnabledInvalidIgnored(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_ENABLED", "notabool")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = true; want false (invalid env ignored)")
	}
}

// TestLoad_InstrumentalDetectorEnvClassifierURL verifies MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL.
func TestLoad_InstrumentalDetectorEnvClassifierURL(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL", "http://yamnet:8080")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstrumentalDetector.ClassifierURL != "http://yamnet:8080" {
		t.Errorf("ClassifierURL = %q; want http://yamnet:8080", cfg.InstrumentalDetector.ClassifierURL)
	}
}

// TestLoad_InstrumentalDetectorEnvFFmpegPath verifies MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH.
func TestLoad_InstrumentalDetectorEnvFFmpegPath(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH", "/opt/ffmpeg")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstrumentalDetector.FFmpegPath != "/opt/ffmpeg" {
		t.Errorf("FFmpegPath = %q; want /opt/ffmpeg", cfg.InstrumentalDetector.FFmpegPath)
	}
}

// TestLoad_InstrumentalDetectorEnvSampleDuration verifies
// MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS. Valid positive values
// override the default; invalid/non-positive values are ignored.
func TestLoad_InstrumentalDetectorEnvSampleDuration(t *testing.T) {
	t.Run("valid override", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS", "45")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.SampleDurationSeconds != 45 {
			t.Errorf("SampleDurationSeconds = %d; want 45", cfg.InstrumentalDetector.SampleDurationSeconds)
		}
	})

	t.Run("invalid value is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS", "notanumber")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.SampleDurationSeconds != 30 {
			t.Errorf("SampleDurationSeconds = %d; want 30 (default after invalid env)", cfg.InstrumentalDetector.SampleDurationSeconds)
		}
	})

	t.Run("zero is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS", "0")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.SampleDurationSeconds != 30 {
			t.Errorf("SampleDurationSeconds = %d; want 30 (zero is invalid)", cfg.InstrumentalDetector.SampleDurationSeconds)
		}
	})
}

// TestLoad_InstrumentalDetectorEnvMinConfidence verifies
// MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE. Valid values in (0,1] override the
// default; out-of-range values are ignored.
func TestLoad_InstrumentalDetectorEnvMinConfidence(t *testing.T) {
	t.Run("valid override", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "0.75")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.MinConfidence != 0.75 {
			t.Errorf("MinConfidence = %v; want 0.75", cfg.InstrumentalDetector.MinConfidence)
		}
	})

	t.Run("zero is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "0")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.MinConfidence != 0.90 {
			t.Errorf("MinConfidence = %v; want 0.90 (zero ignored)", cfg.InstrumentalDetector.MinConfidence)
		}
	})

	t.Run("above 1 is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "1.5")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.MinConfidence != 0.90 {
			t.Errorf("MinConfidence = %v; want 0.90 (>1 ignored)", cfg.InstrumentalDetector.MinConfidence)
		}
	})

	t.Run("not a number is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "bad")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.MinConfidence != 0.90 {
			t.Errorf("MinConfidence = %v; want 0.90 (bad value ignored)", cfg.InstrumentalDetector.MinConfidence)
		}
	})
}

// TestLoad_InstrumentalDetectorEnvClasses verifies MXLRC_INSTRUMENTAL_DETECTOR_CLASSES
// accepts a CSV of class names.
func TestLoad_InstrumentalDetectorEnvClasses(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_CLASSES", "Music, Silence, Wind instrument")

	cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"Music", "Silence", "Wind instrument"}
	if len(cfg.InstrumentalDetector.InstrumentalClasses) != len(want) {
		t.Fatalf("InstrumentalClasses = %v; want %v", cfg.InstrumentalDetector.InstrumentalClasses, want)
	}
	for i, v := range want {
		if cfg.InstrumentalDetector.InstrumentalClasses[i] != v {
			t.Errorf("InstrumentalClasses[%d] = %q; want %q", i, cfg.InstrumentalDetector.InstrumentalClasses[i], v)
		}
	}
}

// TestLoad_InstrumentalDetectorEnvCooldownSeconds verifies
// MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS. Zero is a valid value (disables
// cooldown); negative values are ignored.
func TestLoad_InstrumentalDetectorEnvCooldownSeconds(t *testing.T) {
	t.Run("positive override", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS", "10")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.CooldownSeconds != 10 {
			t.Errorf("CooldownSeconds = %d; want 10", cfg.InstrumentalDetector.CooldownSeconds)
		}
	})

	t.Run("zero is valid (disables cooldown)", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS", "0")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.CooldownSeconds != 0 {
			t.Errorf("CooldownSeconds = %d; want 0 (zero disables cooldown)", cfg.InstrumentalDetector.CooldownSeconds)
		}
	})

	t.Run("negative is ignored", func(t *testing.T) {
		isolateEnv(t)
		t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS", "-3")
		cfg, err := Load(filepath.Join(t.TempDir(), "nonexistent.toml"))
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.InstrumentalDetector.CooldownSeconds != 5 {
			t.Errorf("CooldownSeconds = %d; want 5 (default after negative env)", cfg.InstrumentalDetector.CooldownSeconds)
		}
	})
}

// TestLoad_InstrumentalDetectorTOMLMerge verifies that an [instrumental_detector]
// section in the TOML file is decoded correctly and re-default logic kicks in for
// blank/zero fields.
func TestLoad_InstrumentalDetectorTOMLMerge(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := `[instrumental_detector]
enabled = true
classifier_url = "http://yamnet:8080"
ffmpeg_path = "/usr/bin/ffmpeg"
sample_duration_seconds = 45
min_confidence = 0.85
instrumental_classes = ["Music", "Silence"]
cooldown_seconds = 10
`
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = false; want true")
	}
	if cfg.InstrumentalDetector.ClassifierURL != "http://yamnet:8080" {
		t.Errorf("ClassifierURL = %q; want http://yamnet:8080", cfg.InstrumentalDetector.ClassifierURL)
	}
	if cfg.InstrumentalDetector.FFmpegPath != "/usr/bin/ffmpeg" {
		t.Errorf("FFmpegPath = %q; want /usr/bin/ffmpeg", cfg.InstrumentalDetector.FFmpegPath)
	}
	if cfg.InstrumentalDetector.SampleDurationSeconds != 45 {
		t.Errorf("SampleDurationSeconds = %d; want 45", cfg.InstrumentalDetector.SampleDurationSeconds)
	}
	if cfg.InstrumentalDetector.MinConfidence != 0.85 {
		t.Errorf("MinConfidence = %v; want 0.85", cfg.InstrumentalDetector.MinConfidence)
	}
	if len(cfg.InstrumentalDetector.InstrumentalClasses) != 2 ||
		cfg.InstrumentalDetector.InstrumentalClasses[0] != "Music" ||
		cfg.InstrumentalDetector.InstrumentalClasses[1] != "Silence" {
		t.Errorf("InstrumentalClasses = %v; want [Music Silence]", cfg.InstrumentalDetector.InstrumentalClasses)
	}
	if cfg.InstrumentalDetector.CooldownSeconds != 10 {
		t.Errorf("CooldownSeconds = %d; want 10", cfg.InstrumentalDetector.CooldownSeconds)
	}
}

// TestLoad_InstrumentalDetectorTOMLZeroFieldsReDefault verifies that zero/blank
// fields in the TOML file trigger re-default logic for the
// InstrumentalDetectorConfig. Enabled is intentionally NOT re-defaulted (false is
// the correct off state); sample_duration=0 gets the default (30); ffmpeg_path=""
// gets "ffmpeg"; min_confidence out of range gets 0.90; empty classes get the
// built-in defaults; negative cooldown is clamped to 0.
func TestLoad_InstrumentalDetectorTOMLZeroFieldsReDefault(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := `[instrumental_detector]
sample_duration_seconds = 0
ffmpeg_path = ""
min_confidence = 0
instrumental_classes = []
cooldown_seconds = -5
`
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Enabled stays false (zero value is the correct default).
	if cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = true; want false (zero value honored)")
	}
	// sample_duration_seconds=0 is re-defaulted to 30.
	if cfg.InstrumentalDetector.SampleDurationSeconds != 30 {
		t.Errorf("SampleDurationSeconds = %d; want 30 (zero re-defaulted)", cfg.InstrumentalDetector.SampleDurationSeconds)
	}
	// ffmpeg_path="" is re-defaulted to "ffmpeg".
	if cfg.InstrumentalDetector.FFmpegPath != "ffmpeg" {
		t.Errorf("FFmpegPath = %q; want ffmpeg (blank re-defaulted)", cfg.InstrumentalDetector.FFmpegPath)
	}
	// min_confidence=0 is out of (0,1]; re-defaulted to 0.90.
	if cfg.InstrumentalDetector.MinConfidence != 0.90 {
		t.Errorf("MinConfidence = %v; want 0.90 (zero out-of-range re-defaulted)", cfg.InstrumentalDetector.MinConfidence)
	}
	// Empty classes are re-defaulted to the built-in list.
	if len(cfg.InstrumentalDetector.InstrumentalClasses) != 2 {
		t.Errorf("InstrumentalClasses = %v; want default [Music, Musical instrument]", cfg.InstrumentalDetector.InstrumentalClasses)
	}
	// Negative cooldown is clamped to 0.
	if cfg.InstrumentalDetector.CooldownSeconds != 0 {
		t.Errorf("CooldownSeconds = %d; want 0 (negative clamped)", cfg.InstrumentalDetector.CooldownSeconds)
	}
}

// TestLoad_InstrumentalDetectorEnvWinsOverTOML verifies that env vars override
// values set in the TOML file for the InstrumentalDetectorConfig.
func TestLoad_InstrumentalDetectorEnvWinsOverTOML(t *testing.T) {
	isolateEnv(t)

	cfgFile := filepath.Join(t.TempDir(), "config.toml")
	content := `[instrumental_detector]
enabled = true
classifier_url = "http://file-classifier:8080"
min_confidence = 0.85
`
	if err := os.WriteFile(cfgFile, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_ENABLED", "false")
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL", "http://env-classifier:9090")
	t.Setenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "0.95")

	cfg, err := Load(cfgFile)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.InstrumentalDetector.Enabled {
		t.Error("InstrumentalDetector.Enabled = true; want false (env override)")
	}
	if cfg.InstrumentalDetector.ClassifierURL != "http://env-classifier:9090" {
		t.Errorf("ClassifierURL = %q; want env value", cfg.InstrumentalDetector.ClassifierURL)
	}
	if cfg.InstrumentalDetector.MinConfidence != 0.95 {
		t.Errorf("MinConfidence = %v; want 0.95 (env override)", cfg.InstrumentalDetector.MinConfidence)
	}
}
