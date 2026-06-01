package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config holds all application configuration.
type Config struct {
	API          APIConfig          `toml:"api"`
	Output       OutputConfig       `toml:"output"`
	DB           DBConfig           `toml:"db"`
	Server       ServerConfig       `toml:"server"`
	Providers    ProvidersConfig    `toml:"providers"`
	Verification VerificationConfig `toml:"verification"`
}

// APIConfig holds API-related configuration.
type APIConfig struct {
	Token    string `toml:"token"`
	Cooldown int    `toml:"cooldown"`
	// CircuitOpenDuration is the duration in seconds the worker pauses
	// dequeuing after the upstream API returns a rate-limit or unauthorized
	// signal. Default 1800 (30 min). Values below circuitOpenMinSeconds are
	// clamped at load time.
	CircuitOpenDuration int `toml:"circuit_open_duration"`
}

// circuitOpenDefaultSeconds is the default circuit-open window (30 min).
const circuitOpenDefaultSeconds = 30 * 60

// circuitOpenMinSeconds is the minimum permissible circuit-open window.
// Values below this are clamped to this floor with a warning.
const circuitOpenMinSeconds = 5 * 60

// OutputConfig holds output-related configuration.
type OutputConfig struct {
	Dir string `toml:"dir"`
}

// DBConfig holds database configuration.
type DBConfig struct {
	Path string `toml:"path"`
}

// ServerConfig holds HTTP server configuration.
type ServerConfig struct {
	Addr           string   `toml:"addr"`
	WebhookAPIKeys []string `toml:"webhook_api_keys"`
	// ScanIntervalSeconds is the scheduler scan interval in seconds for serve
	// mode. Default 900. A value of 0 disables repeat scanning (scan once).
	ScanIntervalSeconds int `toml:"scan_interval_seconds"`
	// WorkIntervalSeconds is the worker poll interval in seconds for serve mode.
	// Default 0, which means "fall back to api.cooldown". The effective interval
	// is clamped to a 15-second floor at runtime.
	WorkIntervalSeconds int `toml:"work_interval_seconds"`
}

// defaultScanIntervalSeconds is the built-in scheduler scan interval (15 min).
const defaultScanIntervalSeconds = 900

// ProvidersConfig holds lyrics provider selection settings.
type ProvidersConfig struct {
	Primary  string   `toml:"primary"`
	Disabled []string `toml:"disabled"`
}

// VerificationConfig holds optional STT verification settings.
type VerificationConfig struct {
	Enabled               bool    `toml:"enabled"`
	WhisperURL            string  `toml:"whisper_url"`
	FFmpegPath            string  `toml:"ffmpeg_path"`
	SampleDurationSeconds int     `toml:"sample_duration_seconds"`
	MinConfidence         float64 `toml:"min_confidence"`
	MinSimilarity         float64 `toml:"min_similarity"`
}

// defaults sets built-in fallback values.
func defaults() Config {
	return Config{
		API:          APIConfig{Cooldown: 15, CircuitOpenDuration: circuitOpenDefaultSeconds},
		Output:       OutputConfig{Dir: "lyrics"},
		DB:           DBConfig{Path: xdgDataPath("mxlrcgo-svc", "mxlrcgo.db")},
		Server:       ServerConfig{Addr: "127.0.0.1:3876", ScanIntervalSeconds: defaultScanIntervalSeconds},
		Providers:    ProvidersConfig{Primary: "musixmatch"},
		Verification: VerificationConfig{FFmpegPath: "ffmpeg", SampleDurationSeconds: 30, MinConfidence: 0.85, MinSimilarity: 0.35},
	}
}

// Load reads the TOML config file at path (or XDG default if empty),
// then overlays environment variables. A missing config file is not an error.
func Load(path string) (Config, error) {
	cfg := defaults()
	if path == "" {
		path = xdgConfigPath("mxlrcgo-svc", "config.toml")
	}
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			if _, err := toml.DecodeFile(path, &cfg); err != nil {
				return cfg, fmt.Errorf("config: decode %s: %w", path, err)
			}
			// Re-apply defaults for any fields the file set to blank.
			// This prevents a user copying config.example.toml verbatim from
			// clobbering computed defaults (e.g. XDG DB path, output dir).
			// Re-apply defaults for string fields that must not be blank.
			// Cooldown is intentionally excluded: 0 is a valid user-specified value.
			d := defaults()
			if cfg.DB.Path == "" {
				cfg.DB.Path = d.DB.Path
			}
			if cfg.Output.Dir == "" {
				cfg.Output.Dir = d.Output.Dir
			}
			if cfg.Server.Addr == "" {
				cfg.Server.Addr = d.Server.Addr
			}
			if cfg.Providers.Primary == "" {
				cfg.Providers.Primary = d.Providers.Primary
			}
			if cfg.Verification.SampleDurationSeconds <= 0 {
				cfg.Verification.SampleDurationSeconds = d.Verification.SampleDurationSeconds
			}
			if cfg.Verification.FFmpegPath == "" {
				cfg.Verification.FFmpegPath = d.Verification.FFmpegPath
			}
			if cfg.Verification.MinConfidence <= 0 || cfg.Verification.MinConfidence > 1 {
				cfg.Verification.MinConfidence = d.Verification.MinConfidence
			}
			if cfg.Verification.MinSimilarity <= 0 || cfg.Verification.MinSimilarity > 1 {
				cfg.Verification.MinSimilarity = d.Verification.MinSimilarity
			}
			// CircuitOpenDuration: 0 means "not set in file"; restore the
			// default so users copying config.example.toml don't disable
			// the breaker. Any non-zero value is honored and may be
			// clamped to the minimum below.
			if cfg.API.CircuitOpenDuration == 0 {
				cfg.API.CircuitOpenDuration = d.API.CircuitOpenDuration
			}
		} else if !os.IsNotExist(err) {
			return cfg, fmt.Errorf("config: stat %s: %w", path, err)
		}
	}
	applyEnvOverrides(&cfg)
	clampCircuitOpenDuration(&cfg)
	if cfg.DB.Path == "" {
		return cfg, fmt.Errorf("config: cannot determine DB path: set MXLRC_DB_PATH or XDG_DATA_HOME")
	}
	return cfg, nil
}

// applyEnvOverrides overlays environment variables onto cfg.
// Token precedence within env vars: MUSIXMATCH_TOKEN > MXLRC_API_TOKEN.
// Cooldown precedence: MXLRC_API_COOLDOWN > MXLRC_COOLDOWN.
// Supported: MUSIXMATCH_TOKEN, MXLRC_API_TOKEN, MXLRC_API_COOLDOWN, MXLRC_COOLDOWN, MXLRC_OUTPUT_DIR, MXLRC_DB_PATH, MXLRC_SERVER_ADDR, MXLRC_WEBHOOK_API_KEY, MXLRC_SCAN_INTERVAL, MXLRC_WORK_INTERVAL, MXLRC_PROVIDER_PRIMARY, MXLRC_PROVIDERS_DISABLED, MXLRC_VERIFICATION_ENABLED, MXLRC_VERIFICATION_WHISPER_URL, MXLRC_WHISPER_URL, MXLRC_VERIFICATION_FFMPEG_PATH, MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS, MXLRC_VERIFICATION_SAMPLE_DURATION, MXLRC_VERIFICATION_MIN_CONFIDENCE, MXLRC_VERIFICATION_MIN_SIMILARITY
func applyEnvOverrides(cfg *Config) {
	// Token: MUSIXMATCH_TOKEN takes precedence over MXLRC_API_TOKEN (backward compat).
	if v := os.Getenv("MUSIXMATCH_TOKEN"); v != "" {
		cfg.API.Token = v
	} else if v := os.Getenv("MXLRC_API_TOKEN"); v != "" {
		cfg.API.Token = v
	}

	// Cooldown: MXLRC_API_COOLDOWN (section-scoped) takes precedence over MXLRC_COOLDOWN (short alias).
	cooldownVar := "MXLRC_API_COOLDOWN"
	v := os.Getenv(cooldownVar)
	if v == "" {
		cooldownVar = "MXLRC_COOLDOWN"
		v = os.Getenv(cooldownVar)
	}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", cooldownVar, "value", v, "current", cfg.API.Cooldown) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.Cooldown = n
		}
	}

	if v := os.Getenv("MXLRC_API_CIRCUIT_OPEN_DURATION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_API_CIRCUIT_OPEN_DURATION", "value", v, "current", cfg.API.CircuitOpenDuration) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.CircuitOpenDuration = n
		}
	}

	if v := os.Getenv("MXLRC_OUTPUT_DIR"); v != "" {
		cfg.Output.Dir = v
	}
	if v := os.Getenv("MXLRC_DB_PATH"); v != "" {
		cfg.DB.Path = v
	}
	if v := os.Getenv("MXLRC_SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
	}
	if v := os.Getenv("MXLRC_WEBHOOK_API_KEY"); v != "" {
		cfg.Server.WebhookAPIKeys = splitCSV(v)
	}
	if v := os.Getenv("MXLRC_SCAN_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SCAN_INTERVAL", "value", v, "current", cfg.Server.ScanIntervalSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.ScanIntervalSeconds = n
		}
	}
	if v := os.Getenv("MXLRC_WORK_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_WORK_INTERVAL", "value", v, "current", cfg.Server.WorkIntervalSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.WorkIntervalSeconds = n
		}
	}
	if v := os.Getenv("MXLRC_PROVIDER_PRIMARY"); v != "" {
		cfg.Providers.Primary = v
	}
	if v := os.Getenv("MXLRC_PROVIDERS_DISABLED"); v != "" {
		cfg.Providers.Disabled = splitCSV(v)
	}
	if v := os.Getenv("MXLRC_VERIFICATION_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_ENABLED", "value", v, "current", cfg.Verification.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.Enabled = enabled
		}
	}
	whisperVar := "MXLRC_VERIFICATION_WHISPER_URL"
	v = os.Getenv(whisperVar)
	if v == "" {
		whisperVar = "MXLRC_WHISPER_URL"
		v = os.Getenv(whisperVar)
	}
	if v != "" {
		cfg.Verification.WhisperURL = v
	}
	if v := os.Getenv("MXLRC_VERIFICATION_FFMPEG_PATH"); v != "" {
		cfg.Verification.FFmpegPath = v
	}
	sampleDurationVar := "MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS"
	v = os.Getenv(sampleDurationVar)
	if v == "" {
		sampleDurationVar = "MXLRC_VERIFICATION_SAMPLE_DURATION"
		v = os.Getenv(sampleDurationVar)
	}
	if v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", sampleDurationVar, "value", v, "current", cfg.Verification.SampleDurationSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.SampleDurationSeconds = n
		}
	}
	if v := os.Getenv("MXLRC_VERIFICATION_MIN_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_MIN_CONFIDENCE", "value", v, "current", cfg.Verification.MinConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.MinConfidence = n
		}
	}
	if v := os.Getenv("MXLRC_VERIFICATION_MIN_SIMILARITY"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_MIN_SIMILARITY", "value", v, "current", cfg.Verification.MinSimilarity) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.MinSimilarity = n
		}
	}
}

// clampCircuitOpenDuration enforces the minimum window for the worker
// circuit breaker. Values below circuitOpenMinSeconds are raised to that
// floor and a warning is logged so misconfiguration is visible.
func clampCircuitOpenDuration(cfg *Config) {
	if cfg.API.CircuitOpenDuration <= 0 {
		cfg.API.CircuitOpenDuration = circuitOpenDefaultSeconds
		return
	}
	if cfg.API.CircuitOpenDuration < circuitOpenMinSeconds {
		slog.Warn("circuit_open_duration below minimum; clamping", "configured", cfg.API.CircuitOpenDuration, "minimum", circuitOpenMinSeconds)
		cfg.API.CircuitOpenDuration = circuitOpenMinSeconds
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// xdgConfigPath returns the XDG config path for the given app and file.
// Returns "" if the home directory cannot be determined.
func xdgConfigPath(app, file string) string {
	if dockerMode() {
		return filepath.Join("/config", file)
	}
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fall back to /config (Docker convention) only when running inside Docker.
			if _, statErr := os.Stat("/.dockerenv"); statErr == nil {
				return filepath.Join("/config", file)
			}
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, app, file)
}

// xdgDataPath returns the XDG data path for the given app and file.
// Returns "" if the home directory cannot be determined and not running in Docker.
func xdgDataPath(app, file string) string {
	if dockerMode() {
		return filepath.Join("/config", file)
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			// Fall back to /config (Docker convention) only when running inside Docker.
			if _, statErr := os.Stat("/.dockerenv"); statErr == nil {
				return filepath.Join("/config", file)
			}
			return ""
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, app, file)
}

func dockerMode() bool {
	v := strings.TrimSpace(os.Getenv("MXLRC_DOCKER"))
	return strings.EqualFold(v, "true") || v == "1"
}
