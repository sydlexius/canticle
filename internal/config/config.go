package config

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
	"github.com/sydlexius/mxlrcgo-svc/internal/secrets"
	"github.com/sydlexius/mxlrcgo-svc/internal/trustnet"
)

// Config holds all application configuration.
type Config struct {
	API                  APIConfig                  `toml:"api"`
	Output               OutputConfig               `toml:"output"`
	DB                   DBConfig                   `toml:"db"`
	Server               ServerConfig               `toml:"server"`
	Providers            ProvidersConfig            `toml:"providers"`
	Verification         VerificationConfig         `toml:"verification"`
	InstrumentalDetector InstrumentalDetectorConfig `toml:"instrumental_detector"`
	Enrichment           EnrichmentConfig           `toml:"enrichment"`
	Guard                GuardConfig                `toml:"guard"`
	Queue                QueueConfig                `toml:"queue"`
	Logging              LoggingConfig              `toml:"logging"`
	Secrets              SecretsConfig              `toml:"secrets"`
}

// SecretsConfig holds the encrypted-at-rest secret store settings.
type SecretsConfig struct {
	// KeyFile is the path to the 32-byte AES-256 master key file used to
	// encrypt secrets at rest. Empty (the default) resolves to the hidden file
	// beside the database (xdgDataPath ".mxlrcgo.key"). Auto-generated 0600 on
	// first use on all platforms including Docker. Set MXLRC_MASTER_KEY to skip
	// the key file entirely (key/data separation).
	// Override: MXLRC_SECRETS_KEY_FILE.
	KeyFile string `toml:"key_file"`
}

// SecretsKeyOptions builds the secrets.KeyOptions used to resolve the master
// key at startup, keeping the data-dir logic in config (the single source of
// those paths). MasterKeyB64 is sourced from MXLRC_MASTER_KEY by the caller
// (it is never persisted in Config and must stay out of logs), so it is left
// empty here; the caller fills it in. KeyFilePath is the resolved
// secrets.key_file (env > TOML > default). The auto-generated 0600 key file
// is the universal zero-setup default on all platforms including Docker;
// MXLRC_MASTER_KEY is the opt-in override for key/data separation.
func (c *Config) SecretsKeyOptions() secrets.KeyOptions {
	keyFile := strings.TrimSpace(c.Secrets.KeyFile)
	if keyFile == "" {
		keyFile = xdgDataPath("mxlrcgo-svc", secrets.DefaultKeyFileName)
	}
	return secrets.KeyOptions{
		KeyFilePath: keyFile,
	}
}

// LoggingConfig holds log-output settings.
type LoggingConfig struct {
	// Level is the minimum log level: debug, info, warn, error. Default info.
	// Override: MXLRC_LOG_LEVEL.
	Level string `toml:"level"`
	// Format is the log format: text or json. Default text.
	// Override: MXLRC_LOG_FORMAT.
	Format string `toml:"format"`
	// File is the log file path. Empty means console-only (stderr). Default "".
	// Override: MXLRC_LOG_FILE.
	File string `toml:"file"`
	// MaxSizeMB is the maximum size in megabytes before rotation. Default 10.
	// Override: MXLRC_LOG_MAX_SIZE_MB.
	MaxSizeMB int `toml:"max_size_mb"`
	// MaxFiles is the number of rotated log files to retain. Default 5.
	// Override: MXLRC_LOG_MAX_FILES.
	MaxFiles int `toml:"max_files"`
	// MaxAgeDays is the maximum age in days of retained log files. Default 30.
	// Override: MXLRC_LOG_MAX_AGE_DAYS.
	MaxAgeDays int `toml:"max_age_days"`
	// Compress enables gzip compression of rotated log files. Default true.
	// Override: MXLRC_LOG_COMPRESS.
	Compress bool `toml:"compress"`
}

// APIConfig holds API-related configuration.
type APIConfig struct {
	Token string `toml:"token"`
	// Cooldown is the minimum gap (in seconds) between Musixmatch API requests.
	// It serves two roles: (1) the worker's inter-item pause in serve mode, and
	// (2) the HTTP client's hard per-request floor -- the client will not issue a
	// new request until at least Cooldown seconds have elapsed since the last
	// one, regardless of how the worker schedules work. Default 15.
	Cooldown int `toml:"cooldown"`
	// CircuitOpenDuration is the duration in seconds the worker pauses
	// dequeuing after the upstream API returns a rate-limit or unauthorized
	// signal. Default 1800 (30 min). Values below circuitOpenMinSeconds are
	// clamped at load time.
	CircuitOpenDuration int `toml:"circuit_open_duration"`
	// CircuitBackoffBase is the initial circuit-open window in seconds applied
	// to the first throttle trip. Successive trips double from this base up to
	// CircuitOpenDuration (the cap). Default 60. Clamped to circuitBackoffBaseMin
	// (15s) from below and to CircuitOpenDuration from above at load time.
	CircuitBackoffBase int `toml:"circuit_backoff_base_seconds"`
	// MissBackoffBaseHours is the initial re-check delay (in hours) for a
	// benign miss (no matching track or no usable lyrics). The cadence doubles
	// each miss: base, 2*base, 4*base, ... up to MissBackoffCapHours. Default 168.
	// Values below 1 are clamped to 1 with a warning.
	MissBackoffBaseHours int `toml:"miss_backoff_base_hours"`
	// MissBackoffCapHours is the maximum re-check delay (in hours) for a
	// benign miss. Default 672 (28 days). Must be >= MissBackoffBaseHours;
	// smaller values are clamped to MissBackoffBaseHours with a warning.
	MissBackoffCapHours int `toml:"miss_backoff_cap_hours"`
	// MaxMissAttempts caps the total number of re-check attempts for a benign
	// miss. When miss_count reaches this value the queue row is retired
	// (status='done', last_error='miss limit reached') without writing any
	// scan_results success. Default 15 (~1 year with the default cadence).
	// Set to 0 for no cap (retry indefinitely). Negative values are clamped
	// to 0 with a warning.
	MaxMissAttempts int `toml:"max_miss_attempts"`
}

// circuitOpenDefaultSeconds is the default circuit-open window (30 min).
const circuitOpenDefaultSeconds = 30 * 60

// circuitOpenMinSeconds is the minimum permissible circuit-open window.
// Values below this are clamped to this floor with a warning.
const circuitOpenMinSeconds = 5 * 60

// circuitBackoffBaseDefault is the default trip-1 circuit window (60s).
const circuitBackoffBaseDefault = 60

// circuitBackoffBaseMin is the minimum permissible trip-1 circuit window. It
// matches the worker tick floor; values below it are clamped up with a warning.
const circuitBackoffBaseMin = 15

// missBackoffBaseDefault is the default initial miss re-check delay (168 hours = 7 days).
const missBackoffBaseDefault = 168

// missBackoffCapDefault is the default maximum miss re-check delay (672 hours = 28 days).
const missBackoffCapDefault = 672

// missBackoffBaseMin is the minimum permissible miss backoff base (1 hour).
const missBackoffBaseMin = 1

// OutputConfig holds output-related configuration.
type OutputConfig struct {
	Dir string `toml:"dir"`
	// EmbeddedLyrics controls handling of unsynced lyrics embedded in audio tags:
	// "off" (default), "respect" (skip files that already carry embedded lyrics),
	// or "extract" (write them to a .txt sidecar, then skip). env:
	// MXLRC_EMBEDDED_LYRICS; CLI: --embedded-lyrics.
	EmbeddedLyrics string `toml:"embedded_lyrics"`
	// BilingualOutput opts into interleaved original+translation .lrc output.
	// Default false (original-only). When true AND a provider returns a non-empty
	// translation track, the original and translation lines are interleaved under
	// shared timestamps per docs/multilingual-output-policy.md.
	// Override: MXLRC_BILINGUAL_OUTPUT.
	BilingualOutput bool `toml:"bilingual_output"`
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
	// WebUIEnabled gates the read-only browser UI (serve mode). Defaults to false.
	// Do NOT enable until the auth/onboarding layer (#204) ships -- enabling this
	// before auth lands exposes an unauthenticated UI on the serve listener.
	WebUIEnabled bool `toml:"web_ui_enabled"`
	// TrustedNetworks configures the CIDR allowlist that gates trusted-network
	// access (issue #204, Area 2): loopback-implicit access to GET /metrics and,
	// once the login layer lands, the interactive-login bypass for the web UI.
	TrustedNetworks TrustedNetworksConfig `toml:"trusted_networks"`
}

// TrustedNetworksConfig holds the trusted-network allowlist for serve mode.
// Both lists are CIDR strings, validated at load (an invalid CIDR is a fatal
// startup error). Default closed: an empty Cidrs trusts only loopback.
type TrustedNetworksConfig struct {
	// Cidrs is the allowlist of trusted client networks (e.g.
	// "192.168.1.0/24"). Loopback (127.0.0.0/8, ::1) is always implicitly
	// trusted on top of this list. Empty (the default) trusts only loopback.
	// Override: MXLRC_TRUSTED_CIDRS (comma-separated).
	Cidrs []string `toml:"cidrs"`
	// TrustedProxies lists the CIDRs of reverse proxies permitted to set
	// X-Forwarded-For. Only when a request's immediate peer is within one of
	// these networks is XFF consulted (then walked right-to-left, skipping
	// proxies) to find the real client IP. Empty (the default) means XFF is
	// never trusted. Override: MXLRC_TRUSTED_PROXIES (comma-separated).
	TrustedProxies []string `toml:"trusted_proxies"`
}

// defaultScanIntervalSeconds is the built-in scheduler scan interval (15 min).
const defaultScanIntervalSeconds = 900

// ProvidersConfig holds lyrics provider selection settings.
type ProvidersConfig struct {
	Primary  string   `toml:"primary"`
	Disabled []string `toml:"disabled"`
	// Mode is the multi-provider dispatch strategy. "ordered" (the default) queries
	// lanes in priority order and returns the first suitable result. "parallel"
	// dispatches every lane concurrently and races them (the first synced result
	// wins; a faster unsynced result is held briefly so a slower synced result can
	// preempt it). Any other value is rejected at load time. Parallel makes more
	// upstream calls, so it is not advised against rate-limited providers unless
	// latency matters more than call volume. Override: MXLRC_PROVIDERS_MODE.
	Mode string `toml:"mode"`
	// RaceWaitSeconds bounds the parallel-mode upgrade window: after a suitable
	// unsynced result arrives, the orchestrator waits up to this many seconds for a
	// synced result (a strict quality upgrade) to preempt it before committing the
	// unsynced one. Default 2. Non-positive values are clamped to the default. Only
	// consulted in "parallel" mode. Override: MXLRC_PROVIDERS_RACE_WAIT_SECONDS.
	RaceWaitSeconds int `toml:"race_wait_seconds"`
	// FallbackOrder lists provider names consulted, in order, AFTER the primary
	// when the primary yields no suitable result (ordered-fallback dispatch). Each
	// name must be a known provider; unknown names are rejected at load. Empty (the
	// default) means no fallback - only the primary lane runs, preserving
	// single-provider behavior. Override: MXLRC_PROVIDERS_FALLBACK_ORDER (CSV).
	FallbackOrder []string `toml:"fallback_order"`
}

// providersModeDefault and providersModeParallel are the supported dispatch
// modes. raceWaitSecondsDefault is the default parallel-mode upgrade window.
const (
	providersModeDefault   = "ordered"
	providersModeParallel  = "parallel"
	raceWaitSecondsDefault = 2
)

// VerificationConfig holds optional STT verification settings.
type VerificationConfig struct {
	Enabled               bool    `toml:"enabled"`
	WhisperURL            string  `toml:"whisper_url"`
	FFmpegPath            string  `toml:"ffmpeg_path"`
	SampleDurationSeconds int     `toml:"sample_duration_seconds"`
	MinConfidence         float64 `toml:"min_confidence"`
	MinSimilarity         float64 `toml:"min_similarity"`
}

// InstrumentalDetectorConfig holds optional audio-based instrumental detection
// settings. When Enabled is false (the default) no HTTP calls are made and the
// feature is completely dormant. The detector runs ONLY on provider misses and
// never overrides provider-supplied data.
type InstrumentalDetectorConfig struct {
	// Enabled activates the instrumental detector sidecar. Default false.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_ENABLED.
	Enabled bool `toml:"enabled"`
	// ClassifierURL is the base URL of the AudioSet classifier sidecar
	// (e.g. "http://yamnet:8080"). Required when Enabled is true.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL.
	ClassifierURL string `toml:"classifier_url"`
	// FFmpegPath is the path to the ffmpeg binary used for audio sampling.
	// Default "ffmpeg". Override: MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH.
	FFmpegPath string `toml:"ffmpeg_path"`
	// SampleDurationSeconds is the length of the audio sample extracted for
	// classification, clamped to [30, 60]. Default 30.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS.
	SampleDurationSeconds int `toml:"sample_duration_seconds"`
	// MinConfidence is the minimum summed class probability required to mark a
	// track instrumental. Values outside (0, 1] are reset to 0.90. Default 0.90.
	// The threshold is intentionally conservative: false-instrumental errors
	// (marking a vocal track as instrumental) are worse than false-misses.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE.
	MinConfidence float64 `toml:"min_confidence"`
	// InstrumentalClasses is the list of AudioSet class names whose probabilities
	// are summed and compared against MinConfidence. Default ["Music", "Musical
	// instrument"]. Override: MXLRC_INSTRUMENTAL_DETECTOR_CLASSES (CSV).
	InstrumentalClasses []string `toml:"instrumental_classes"`
	// CooldownSeconds is the minimum gap between consecutive inference calls.
	// Default 5. A value of 0 disables the cooldown.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS.
	CooldownSeconds int `toml:"cooldown_seconds"`
}

// EnrichmentConfig holds the global default for recording enrichment (reading
// ISRC / MusicBrainz recording ID / duration from audio tags and feeding them to
// the matcher). Per-library settings and the scan CLI override resolve against
// this default via ResolveBool.
type EnrichmentConfig struct {
	// Enabled is the global default for recording enrichment. Default true,
	// preserving the pre-#217 always-on behavior. A per-library setting or the
	// scan --enrich/--no-enrich flag overrides it.
	// Override: MXLRC_ENRICHMENT_ENABLED.
	Enabled bool `toml:"enabled"`
}

// GuardConfig holds optional language/script guard settings. An empty
// AcceptedScripts disables the guard.
type GuardConfig struct {
	// AcceptedScripts is the allowlist of Unicode script buckets a lyric body may
	// be written in: Latin, Han, Kana, Hangul, Other. Empty disables the guard.
	// Override: MXLRC_GUARD_ACCEPTED_SCRIPTS (comma-separated).
	AcceptedScripts []string `toml:"accepted_scripts"`
	// Threshold is the maximum tolerated share of foreign-script letters (outside
	// AcceptedScripts) before a result is rejected. Default 0.20. Values outside
	// (0,1] are reset to the default. Override: MXLRC_GUARD_THRESHOLD.
	Threshold float64 `toml:"script_guard_threshold"`
}

// guardThresholdDefault is the default foreign-script share threshold. It
// mirrors langguard's built-in default so an empty config and an empty
// allowlist agree.
const guardThresholdDefault = 0.20

// QueueConfig holds work-queue behavior settings.
type QueueConfig struct {
	// Randomize shuffles the dequeue order within each priority tier so the
	// worker stops querying the upstream API in strict alphabetical (insertion)
	// order. A strictly alphabetical request stream is a plausible scraping
	// fingerprint; randomizing removes that tell at effectively zero cost.
	// Defaults to true. Set queue.randomize = false (or MXLRC_QUEUE_RANDOMIZE=false)
	// to restore the deterministic created_at/id ordering.
	Randomize bool `toml:"randomize"`
}

// defaults sets built-in fallback values.
func defaults() Config {
	return Config{
		API: APIConfig{
			Cooldown:             15,
			CircuitOpenDuration:  circuitOpenDefaultSeconds,
			CircuitBackoffBase:   circuitBackoffBaseDefault,
			MissBackoffBaseHours: missBackoffBaseDefault,
			MissBackoffCapHours:  missBackoffCapDefault,
			MaxMissAttempts:      15,
		},
		Output:       OutputConfig{Dir: "lyrics", EmbeddedLyrics: "off"},
		DB:           DBConfig{Path: xdgDataPath("mxlrcgo-svc", "mxlrcgo.db")},
		Server:       ServerConfig{Addr: "127.0.0.1:3876", ScanIntervalSeconds: defaultScanIntervalSeconds},
		Providers:    ProvidersConfig{Primary: "musixmatch", Mode: providersModeDefault, RaceWaitSeconds: raceWaitSecondsDefault},
		Verification: VerificationConfig{FFmpegPath: "ffmpeg", SampleDurationSeconds: 30, MinConfidence: 0.85, MinSimilarity: 0.35},
		InstrumentalDetector: InstrumentalDetectorConfig{
			FFmpegPath:            "ffmpeg",
			SampleDurationSeconds: 30,
			MinConfidence:         0.90,
			InstrumentalClasses:   []string{"Music", "Musical instrument"},
			CooldownSeconds:       5,
		},
		Enrichment: EnrichmentConfig{Enabled: true},
		Guard:      GuardConfig{Threshold: guardThresholdDefault},
		Queue:      QueueConfig{Randomize: true},
		Logging: LoggingConfig{
			Level:      "info",
			Format:     "text",
			MaxSizeMB:  10,
			MaxFiles:   5,
			MaxAgeDays: 30,
			Compress:   true,
		},
	}
}

// Load reads the TOML config file at path (or XDG default if empty),
// then overlays environment variables. A missing config file is not an error.
func Load(path string) (Config, error) {
	cfg, _, err := LoadWithSources(path)
	return cfg, err
}

// LoadWithSources is Load plus the set of config field paths whose environment
// override was ACTUALLY APPLIED (invalid env values that were rejected are not
// included). The returned map is suitable as the envSrc hint for
// FormatConfigText / ConfigToSlogAttrs so a field is annotated "(env)" only
// when its override truly took effect. The map is non-nil even on error.
func LoadWithSources(path string) (Config, map[string]bool, error) {
	cfg := defaults()
	appliedEnv := map[string]bool{}
	if path == "" {
		path = xdgConfigPath("mxlrcgo-svc", "config.toml")
	}
	if path != "" {
		if _, err := os.Stat(path); err == nil {
			md, err := toml.DecodeFile(path, &cfg)
			if err != nil {
				return cfg, appliedEnv, fmt.Errorf("config: decode %s: %w", path, err)
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
			if cfg.Output.EmbeddedLyrics == "" {
				cfg.Output.EmbeddedLyrics = d.Output.EmbeddedLyrics
			}
			// BilingualOutput defaults to false (the bool zero-value), so an
			// explicit "bilingual_output = false" in the file is indistinguishable
			// from "not set" by equality. Mirror the logging.compress pattern: use
			// MetaData.IsDefined so an omitted key restores the default and an
			// explicit value (true or false) is preserved as decoded.
			if !md.IsDefined("output", "bilingual_output") {
				cfg.Output.BilingualOutput = d.Output.BilingualOutput
			}
			if cfg.Server.Addr == "" {
				cfg.Server.Addr = d.Server.Addr
			}
			if cfg.Providers.Primary == "" {
				cfg.Providers.Primary = d.Providers.Primary
			}
			if cfg.Providers.Mode == "" {
				cfg.Providers.Mode = d.Providers.Mode
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
			// InstrumentalDetector: restore defaults for zero/blank fields.
			// Enabled defaults to false, so it is intentionally not re-defaulted.
			if cfg.InstrumentalDetector.SampleDurationSeconds <= 0 {
				cfg.InstrumentalDetector.SampleDurationSeconds = d.InstrumentalDetector.SampleDurationSeconds
			}
			if cfg.InstrumentalDetector.FFmpegPath == "" {
				cfg.InstrumentalDetector.FFmpegPath = d.InstrumentalDetector.FFmpegPath
			}
			if cfg.InstrumentalDetector.MinConfidence <= 0 || cfg.InstrumentalDetector.MinConfidence > 1 {
				cfg.InstrumentalDetector.MinConfidence = d.InstrumentalDetector.MinConfidence
			}
			if len(cfg.InstrumentalDetector.InstrumentalClasses) == 0 {
				cfg.InstrumentalDetector.InstrumentalClasses = d.InstrumentalDetector.InstrumentalClasses
			}
			// CooldownSeconds=0 is a valid user value (disable cooldown), so it is
			// not re-defaulted. Negative values are clamped to 0.
			if cfg.InstrumentalDetector.CooldownSeconds < 0 {
				cfg.InstrumentalDetector.CooldownSeconds = 0
			}
			// CircuitOpenDuration: 0 means "not set in file"; restore the
			// default so users copying config.example.toml don't disable
			// the breaker. Any non-zero value is honored and may be
			// clamped to the minimum below.
			if cfg.API.CircuitOpenDuration == 0 {
				cfg.API.CircuitOpenDuration = d.API.CircuitOpenDuration
			}
			// CircuitBackoffBase: 0 means "not set in file"; restore the default
			// so a blank config.example.toml copy keeps the documented ramp.
			if cfg.API.CircuitBackoffBase == 0 {
				cfg.API.CircuitBackoffBase = d.API.CircuitBackoffBase
			}
			// MissBackoffBaseHours/MissBackoffCapHours: 0 means "not set in
			// file"; restore defaults so a blank config.example.toml copy
			// gets the documented cadence.
			if cfg.API.MissBackoffBaseHours == 0 {
				cfg.API.MissBackoffBaseHours = d.API.MissBackoffBaseHours
			}
			if cfg.API.MissBackoffCapHours == 0 {
				cfg.API.MissBackoffCapHours = d.API.MissBackoffCapHours
			}
			// MaxMissAttempts: 0 is a valid user value (no cap), so a plain
			// int TOML field cannot distinguish "omitted" from "explicit 0".
			// Use MetaData.IsDefined to restore the default (15) only when the
			// key is absent from the file; an explicit max_miss_attempts = 0
			// is preserved as-is (user opts out of the cap).
			if !md.IsDefined("api", "max_miss_attempts") {
				cfg.API.MaxMissAttempts = d.API.MaxMissAttempts
			}
			// Guard: an empty accepted_scripts is valid (the guard is disabled),
			// so it is never re-defaulted. The threshold default is restored when
			// the key is absent (a plain float field cannot tell "omitted" from
			// "explicit 0") or set out of the valid (0,1] range.
			if !md.IsDefined("guard", "script_guard_threshold") || cfg.Guard.Threshold <= 0 || cfg.Guard.Threshold > 1 {
				cfg.Guard.Threshold = d.Guard.Threshold
			}
			// Logging: restore defaults for blank string fields and zero ints.
			if cfg.Logging.Level == "" {
				cfg.Logging.Level = d.Logging.Level
			}
			if cfg.Logging.Format == "" {
				cfg.Logging.Format = d.Logging.Format
			}
			if cfg.Logging.MaxSizeMB == 0 {
				cfg.Logging.MaxSizeMB = d.Logging.MaxSizeMB
			}
			if cfg.Logging.MaxFiles == 0 {
				cfg.Logging.MaxFiles = d.Logging.MaxFiles
			}
			if cfg.Logging.MaxAgeDays == 0 {
				cfg.Logging.MaxAgeDays = d.Logging.MaxAgeDays
			}
			// Compress defaults to true but the bool zero-value is false, so
			// "compress = false" in the file is indistinguishable from "not set"
			// via simple equality. Use MetaData.IsDefined so an explicit
			// compress = false is preserved and an omitted key restores the default.
			if !md.IsDefined("logging", "compress") {
				cfg.Logging.Compress = d.Logging.Compress
			}
		} else if !os.IsNotExist(err) {
			return cfg, appliedEnv, fmt.Errorf("config: stat %s: %w", path, err)
		}
	}
	applyEnvOverrides(&cfg, appliedEnv)
	normalizeEmbeddedLyrics(&cfg)
	if err := normalizeProvidersMode(&cfg); err != nil {
		return cfg, appliedEnv, err
	}
	normalizeProvidersRaceWait(&cfg)
	if err := normalizeProvidersFallback(&cfg); err != nil {
		return cfg, appliedEnv, err
	}
	clampCircuitOpenDuration(&cfg)
	// Must run AFTER clampCircuitOpenDuration: the base is clamped against the
	// final (clamped) cap value.
	clampCircuitBackoffBase(&cfg)
	clampMissBackoff(&cfg)
	if err := validateTrustedNetworks(cfg); err != nil {
		return cfg, appliedEnv, err
	}
	if cfg.DB.Path == "" {
		return cfg, appliedEnv, fmt.Errorf("config: cannot determine DB path: set MXLRC_DB_PATH or XDG_DATA_HOME")
	}
	return cfg, appliedEnv, nil
}

// applyEnvOverrides overlays environment variables onto cfg.
// Token precedence within env vars: MUSIXMATCH_TOKEN > MXLRC_API_TOKEN.
// Cooldown precedence: MXLRC_API_COOLDOWN > MXLRC_COOLDOWN.
// Supported: MUSIXMATCH_TOKEN, MXLRC_API_TOKEN, MXLRC_API_COOLDOWN, MXLRC_COOLDOWN, MXLRC_API_CIRCUIT_OPEN_DURATION, MXLRC_API_CIRCUIT_BACKOFF_BASE, MXLRC_MISS_BACKOFF_BASE_HOURS, MXLRC_MISS_BACKOFF_CAP_HOURS, MXLRC_MAX_MISS_ATTEMPTS, MXLRC_OUTPUT_DIR, MXLRC_BILINGUAL_OUTPUT, MXLRC_DB_PATH, MXLRC_SECRETS_KEY_FILE, MXLRC_SERVER_ADDR, MXLRC_WEBHOOK_API_KEY, MXLRC_SCAN_INTERVAL, MXLRC_WORK_INTERVAL, MXLRC_TRUSTED_CIDRS, MXLRC_TRUSTED_PROXIES, MXLRC_PROVIDER_PRIMARY, MXLRC_PROVIDERS_DISABLED, MXLRC_PROVIDERS_MODE, MXLRC_PROVIDERS_RACE_WAIT_SECONDS, MXLRC_PROVIDERS_FALLBACK_ORDER, MXLRC_VERIFICATION_ENABLED, MXLRC_VERIFICATION_WHISPER_URL, MXLRC_WHISPER_URL, MXLRC_VERIFICATION_FFMPEG_PATH, MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS, MXLRC_VERIFICATION_SAMPLE_DURATION, MXLRC_VERIFICATION_MIN_CONFIDENCE, MXLRC_VERIFICATION_MIN_SIMILARITY, MXLRC_INSTRUMENTAL_DETECTOR_ENABLED, MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL, MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH, MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS, MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE, MXLRC_INSTRUMENTAL_DETECTOR_CLASSES, MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS, MXLRC_ENRICHMENT_ENABLED, MXLRC_GUARD_ACCEPTED_SCRIPTS, MXLRC_GUARD_THRESHOLD, MXLRC_QUEUE_RANDOMIZE, MXLRC_LOG_LEVEL, MXLRC_LOG_FORMAT, MXLRC_LOG_FILE, MXLRC_LOG_MAX_SIZE_MB, MXLRC_LOG_MAX_FILES, MXLRC_LOG_MAX_AGE_DAYS, MXLRC_LOG_COMPRESS
//
// applied (must be non-nil) records the dotted config field path for every
// override that ACTUALLY took effect. Env values that are rejected (invalid
// numeric/bool, out-of-range) leave the prior value in place and are NOT
// recorded, so callers can annotate "(env)" only for overrides that applied.
func applyEnvOverrides(cfg *Config, applied map[string]bool) {
	// Token: MUSIXMATCH_TOKEN takes precedence over MXLRC_API_TOKEN (backward compat).
	if v := os.Getenv("MUSIXMATCH_TOKEN"); v != "" {
		cfg.API.Token = v
		applied["api.token"] = true
	} else if v := os.Getenv("MXLRC_API_TOKEN"); v != "" {
		cfg.API.Token = v
		applied["api.token"] = true
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
			applied["api.cooldown"] = true
		}
	}

	if v := os.Getenv("MXLRC_API_CIRCUIT_OPEN_DURATION"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_API_CIRCUIT_OPEN_DURATION", "value", v, "current", cfg.API.CircuitOpenDuration) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.CircuitOpenDuration = n
			applied["api.circuit_open_duration"] = true
		}
	}

	if v := os.Getenv("MXLRC_API_CIRCUIT_BACKOFF_BASE"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_API_CIRCUIT_BACKOFF_BASE", "value", v, "current", cfg.API.CircuitBackoffBase) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.CircuitBackoffBase = n
			applied["api.circuit_backoff_base_seconds"] = true
		}
	}

	if v := os.Getenv("MXLRC_MISS_BACKOFF_BASE_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_MISS_BACKOFF_BASE_HOURS", "value", v, "current", cfg.API.MissBackoffBaseHours) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.MissBackoffBaseHours = n
			applied["api.miss_backoff_base_hours"] = true
		}
	}
	if v := os.Getenv("MXLRC_MISS_BACKOFF_CAP_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_MISS_BACKOFF_CAP_HOURS", "value", v, "current", cfg.API.MissBackoffCapHours) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.MissBackoffCapHours = n
			applied["api.miss_backoff_cap_hours"] = true
		}
	}
	if v := os.Getenv("MXLRC_MAX_MISS_ATTEMPTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_MAX_MISS_ATTEMPTS", "value", v, "current", cfg.API.MaxMissAttempts) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.API.MaxMissAttempts = n
			applied["api.max_miss_attempts"] = true
		}
	}

	if v := os.Getenv("MXLRC_OUTPUT_DIR"); v != "" {
		cfg.Output.Dir = v
		applied["output.dir"] = true
	}
	if v := os.Getenv("MXLRC_EMBEDDED_LYRICS"); v != "" {
		cfg.Output.EmbeddedLyrics = v
		applied["output.embedded_lyrics"] = true
	}
	if v := os.Getenv("MXLRC_BILINGUAL_OUTPUT"); v != "" {
		bilingual, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_BILINGUAL_OUTPUT", "value", v, "current", cfg.Output.BilingualOutput) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Output.BilingualOutput = bilingual
			applied["output.bilingual_output"] = true
		}
	}
	if v := os.Getenv("MXLRC_DB_PATH"); v != "" {
		cfg.DB.Path = v
		applied["db.path"] = true
	}
	if v := os.Getenv("MXLRC_SECRETS_KEY_FILE"); v != "" {
		cfg.Secrets.KeyFile = v
		applied["secrets.key_file"] = true
	}
	if v := os.Getenv("MXLRC_SERVER_ADDR"); v != "" {
		cfg.Server.Addr = v
		applied["server.addr"] = true
	}
	if v := os.Getenv("MXLRC_WEBHOOK_API_KEY"); v != "" {
		cfg.Server.WebhookAPIKeys = splitCSV(v)
		applied["server.webhook_api_keys"] = true
	}
	if v := os.Getenv("MXLRC_SCAN_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SCAN_INTERVAL", "value", v, "current", cfg.Server.ScanIntervalSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.ScanIntervalSeconds = n
			applied["server.scan_interval_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_WORK_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_WORK_INTERVAL", "value", v, "current", cfg.Server.WorkIntervalSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.WorkIntervalSeconds = n
			applied["server.work_interval_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_TRUSTED_CIDRS"); v != "" {
		cfg.Server.TrustedNetworks.Cidrs = splitCSV(v)
		applied["server.trusted_networks.cidrs"] = true
	}
	if v := os.Getenv("MXLRC_TRUSTED_PROXIES"); v != "" {
		cfg.Server.TrustedNetworks.TrustedProxies = splitCSV(v)
		applied["server.trusted_networks.trusted_proxies"] = true
	}
	if v := os.Getenv("MXLRC_PROVIDER_PRIMARY"); v != "" {
		cfg.Providers.Primary = v
		applied["providers.primary"] = true
	}
	if v := os.Getenv("MXLRC_PROVIDERS_DISABLED"); v != "" {
		cfg.Providers.Disabled = splitCSV(v)
		applied["providers.disabled"] = true
	}
	if v := os.Getenv("MXLRC_PROVIDERS_MODE"); v != "" {
		cfg.Providers.Mode = v
		applied["providers.mode"] = true
	}
	if v := os.Getenv("MXLRC_PROVIDERS_RACE_WAIT_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_PROVIDERS_RACE_WAIT_SECONDS", "value", v, "current", cfg.Providers.RaceWaitSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Providers.RaceWaitSeconds = n
			applied["providers.race_wait_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_PROVIDERS_FALLBACK_ORDER"); v != "" {
		cfg.Providers.FallbackOrder = splitCSV(v)
		applied["providers.fallback_order"] = true
	}
	if v := os.Getenv("MXLRC_VERIFICATION_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_ENABLED", "value", v, "current", cfg.Verification.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.Enabled = enabled
			applied["verification.enabled"] = true
		}
	}
	if v := os.Getenv("MXLRC_QUEUE_RANDOMIZE"); v != "" {
		randomize, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_QUEUE_RANDOMIZE", "value", v, "current", cfg.Queue.Randomize) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Queue.Randomize = randomize
			applied["queue.randomize"] = true
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
		applied["verification.whisper_url"] = true
	}
	if v := os.Getenv("MXLRC_VERIFICATION_FFMPEG_PATH"); v != "" {
		cfg.Verification.FFmpegPath = v
		applied["verification.ffmpeg_path"] = true
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
			applied["verification.sample_duration_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_VERIFICATION_MIN_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_MIN_CONFIDENCE", "value", v, "current", cfg.Verification.MinConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.MinConfidence = n
			applied["verification.min_confidence"] = true
		}
	}
	if v := os.Getenv("MXLRC_VERIFICATION_MIN_SIMILARITY"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_VERIFICATION_MIN_SIMILARITY", "value", v, "current", cfg.Verification.MinSimilarity) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Verification.MinSimilarity = n
			applied["verification.min_similarity"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_ENABLED", "value", v, "current", cfg.InstrumentalDetector.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.Enabled = enabled
			applied["instrumental_detector.enabled"] = true
		}
	}
	if v := os.Getenv("MXLRC_ENRICHMENT_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_ENRICHMENT_ENABLED", "value", v, "current", cfg.Enrichment.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Enrichment.Enabled = enabled
			applied["enrichment.enabled"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL"); v != "" {
		cfg.InstrumentalDetector.ClassifierURL = v
		applied["instrumental_detector.classifier_url"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH"); v != "" {
		cfg.InstrumentalDetector.FFmpegPath = v
		applied["instrumental_detector.ffmpeg_path"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS", "value", v, "current", cfg.InstrumentalDetector.SampleDurationSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.SampleDurationSeconds = n
			applied["instrumental_detector.sample_duration_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE", "value", v, "current", cfg.InstrumentalDetector.MinConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.MinConfidence = n
			applied["instrumental_detector.min_confidence"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_CLASSES"); v != "" {
		cfg.InstrumentalDetector.InstrumentalClasses = splitCSV(v)
		applied["instrumental_detector.instrumental_classes"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS", "value", v, "current", cfg.InstrumentalDetector.CooldownSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.CooldownSeconds = n
			applied["instrumental_detector.cooldown_seconds"] = true
		}
	}
	if v := os.Getenv("MXLRC_GUARD_ACCEPTED_SCRIPTS"); v != "" {
		cfg.Guard.AcceptedScripts = splitCSV(v)
		applied["guard.accepted_scripts"] = true
	}
	if v := os.Getenv("MXLRC_GUARD_THRESHOLD"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_GUARD_THRESHOLD", "value", v, "current", cfg.Guard.Threshold) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Guard.Threshold = n
			applied["guard.script_guard_threshold"] = true
		}
	}
	// Logging: string env overrides.
	if v := os.Getenv("MXLRC_LOG_LEVEL"); v != "" {
		cfg.Logging.Level = v //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		applied["logging.level"] = true
	}
	if v := os.Getenv("MXLRC_LOG_FORMAT"); v != "" {
		cfg.Logging.Format = v //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		applied["logging.format"] = true
	}
	if v := os.Getenv("MXLRC_LOG_FILE"); v != "" {
		cfg.Logging.File = v //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		applied["logging.file"] = true
	}
	// Logging: integer env overrides.
	if v := os.Getenv("MXLRC_LOG_MAX_SIZE_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_LOG_MAX_SIZE_MB", "value", v, "current", cfg.Logging.MaxSizeMB) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Logging.MaxSizeMB = n
			applied["logging.max_size_mb"] = true
		}
	}
	if v := os.Getenv("MXLRC_LOG_MAX_FILES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_LOG_MAX_FILES", "value", v, "current", cfg.Logging.MaxFiles) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Logging.MaxFiles = n
			applied["logging.max_files"] = true
		}
	}
	if v := os.Getenv("MXLRC_LOG_MAX_AGE_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_LOG_MAX_AGE_DAYS", "value", v, "current", cfg.Logging.MaxAgeDays) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Logging.MaxAgeDays = n
			applied["logging.max_age_days"] = true
		}
	}
	// Logging: bool env override.
	if v := os.Getenv("MXLRC_LOG_COMPRESS"); v != "" {
		compress, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_LOG_COMPRESS", "value", v, "current", cfg.Logging.Compress) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Logging.Compress = compress
			applied["logging.compress"] = true
		}
	}
}

// normalizeEmbeddedLyrics lowercases the embedded-lyrics mode and clamps any
// unrecognized value to "off" with a warning, so a typo can never silently
// enable extraction or skip fetching.
func normalizeEmbeddedLyrics(cfg *Config) {
	v := strings.ToLower(strings.TrimSpace(cfg.Output.EmbeddedLyrics))
	switch v {
	case "off", "respect", "extract":
		cfg.Output.EmbeddedLyrics = v
	case "":
		cfg.Output.EmbeddedLyrics = "off"
	default:
		slog.Warn("invalid embedded_lyrics value; using off", "value", cfg.Output.EmbeddedLyrics) //nolint:gosec // G706: tainted config value passed as a structured slog field, not a format string
		cfg.Output.EmbeddedLyrics = "off"
	}
}

// normalizeProvidersMode lowercases and validates the provider dispatch mode.
// An empty value restores the default ("ordered"). "ordered" and "parallel" are
// supported; any other value is rejected with an error so a typo fails loudly at
// load rather than silently degrading. See docs/multi-provider-orchestration.md.
func normalizeProvidersMode(cfg *Config) error {
	v := strings.ToLower(strings.TrimSpace(cfg.Providers.Mode))
	if v == "" {
		v = providersModeDefault
	}
	if v != providersModeDefault && v != providersModeParallel {
		return fmt.Errorf("config: unsupported providers.mode %q (supported: %q, %q)", cfg.Providers.Mode, providersModeDefault, providersModeParallel)
	}
	cfg.Providers.Mode = v
	return nil
}

// normalizeProvidersRaceWait clamps the parallel-mode upgrade window to the
// default when it is non-positive, so a misconfigured value cannot produce a
// zero-wait busy dispatch path. It is only consulted in "parallel" mode.
func normalizeProvidersRaceWait(cfg *Config) {
	if cfg.Providers.RaceWaitSeconds <= 0 {
		cfg.Providers.RaceWaitSeconds = raceWaitSecondsDefault
	}
}

// normalizeProvidersFallback lowercases, trims, drops blanks, and de-duplicates
// the fallback provider list, rejecting any unknown provider name so a typo
// fails loudly at load rather than silently dropping a lane. Order is preserved
// (it is the fallback priority). An empty list is valid (no fallback).
func normalizeProvidersFallback(cfg *Config) error {
	if len(cfg.Providers.FallbackOrder) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(cfg.Providers.FallbackOrder))
	normalized := make([]string, 0, len(cfg.Providers.FallbackOrder))
	for _, name := range cfg.Providers.FallbackOrder {
		n := providers.NormalizeName(name)
		if n == "" {
			continue
		}
		if !providers.IsKnown(n) {
			return fmt.Errorf("config: unknown providers.fallback_order entry %q (known providers: %s)", name, strings.Join(providers.Known(), ", "))
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		normalized = append(normalized, n)
	}
	cfg.Providers.FallbackOrder = normalized
	return nil
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

// clampCircuitBackoffBase keeps the trip-1 circuit window within valid bounds.
// It MUST run after clampCircuitOpenDuration, since the upper bound is the
// final (clamped) cap. Values <= 0 restore the default; values below
// circuitBackoffBaseMin are raised to the floor; values above the cap are
// lowered to the cap (the base can never exceed the ceiling it ramps toward).
func clampCircuitBackoffBase(cfg *Config) {
	if cfg.API.CircuitBackoffBase <= 0 {
		cfg.API.CircuitBackoffBase = circuitBackoffBaseDefault
	}
	if cfg.API.CircuitBackoffBase < circuitBackoffBaseMin {
		slog.Warn("circuit_backoff_base_seconds below minimum; clamping", "configured", cfg.API.CircuitBackoffBase, "minimum", circuitBackoffBaseMin)
		cfg.API.CircuitBackoffBase = circuitBackoffBaseMin
	}
	if cfg.API.CircuitBackoffBase > cfg.API.CircuitOpenDuration {
		slog.Warn("circuit_backoff_base_seconds above cap; clamping to circuit_open_duration", "configured", cfg.API.CircuitBackoffBase, "cap", cfg.API.CircuitOpenDuration)
		cfg.API.CircuitBackoffBase = cfg.API.CircuitOpenDuration
	}
}

// clampMissBackoff enforces valid ranges for the miss-cadence knobs.
//   - MissBackoffBaseHours: clamped to missBackoffBaseMin (1h) from below.
//   - MissBackoffCapHours: clamped to MissBackoffBaseHours from below (cap must >= base).
//   - MaxMissAttempts: clamped to 0 from below (negative means no cap).
func clampMissBackoff(cfg *Config) {
	if cfg.API.MissBackoffBaseHours < missBackoffBaseMin {
		slog.Warn("miss_backoff_base_hours below minimum; clamping", "configured", cfg.API.MissBackoffBaseHours, "minimum", missBackoffBaseMin)
		cfg.API.MissBackoffBaseHours = missBackoffBaseMin
	}
	if cfg.API.MissBackoffCapHours < cfg.API.MissBackoffBaseHours {
		slog.Warn("miss_backoff_cap_hours below base; clamping to base", "configured", cfg.API.MissBackoffCapHours, "base", cfg.API.MissBackoffBaseHours)
		cfg.API.MissBackoffCapHours = cfg.API.MissBackoffBaseHours
	}
	if cfg.API.MaxMissAttempts < 0 {
		slog.Warn("max_miss_attempts is negative; clamping to 0 (no cap)", "configured", cfg.API.MaxMissAttempts)
		cfg.API.MaxMissAttempts = 0
	}
}

// validateTrustedNetworks fails fast on an invalid CIDR in either
// [server.trusted_networks] list. Parsing happens at load (not lazily at first
// request) so a misconfiguration is a clear startup error rather than silently
// failing open. The parsed networks are rebuilt by the serve listener; here we
// only assert they are well-formed.
func validateTrustedNetworks(cfg Config) error {
	if _, err := trustnet.ParseCIDRs(cfg.Server.TrustedNetworks.Cidrs); err != nil {
		return fmt.Errorf("config: server.trusted_networks.cidrs: %w", err)
	}
	if _, err := trustnet.ParseCIDRs(cfg.Server.TrustedNetworks.TrustedProxies); err != nil {
		return fmt.Errorf("config: server.trusted_networks.trusted_proxies: %w", err)
	}
	return nil
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
