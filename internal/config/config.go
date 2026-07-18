package config

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/secrets"
	"github.com/sydlexius/canticle/internal/trustnet"
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
	Realign              RealignConfig              `toml:"realign"`
	Guard                GuardConfig                `toml:"guard"`
	Queue                QueueConfig                `toml:"queue"`
	Watcher              WatcherConfig              `toml:"watcher"`
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
	// SweepIntervalSeconds is the interval in seconds for the serve-mode path
	// reconciliation sweep, which deletes queue/scan rows whose source file has
	// vanished (renamed/merged/deleted). Default 21600 (6h): deliberately lazier
	// than the content scan because the filesystem watcher reconciles deletions
	// reactively and this sweep is only a backstop, so it need not spin disks
	// often. A value of 0 disables the periodic sweep (the reactive watcher prune
	// still runs).
	SweepIntervalSeconds int `toml:"sweep_interval_seconds"`
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
	// TLS configures optional transport security for the serve listener (issue
	// #204, Area 4). Off by default (plain HTTP), preserving pre-#204 behavior.
	TLS TLSConfig `toml:"tls"`
}

// TLSConfig holds the optional TLS settings for the serve listener (issue #204,
// Area 4). TLS is off by default. Two modes are supported: bring-your-own
// certificate (CertFile + KeyFile, both required together) and a self-signed
// bootstrap (SelfSigned, mutually exclusive with cert/key). Both are validated at
// load (a contradictory combination is a fatal startup error). ACME autocert is a
// deferred follow-up (lane 6); the listener wires a CertManager seam so it drops
// in without a rewrite.
type TLSConfig struct {
	// CertFile is the PEM-encoded certificate path. With KeyFile set, the serve
	// listener terminates TLS itself (MinVersion TLS 1.2). Override:
	// MXLRC_TLS_CERT_FILE.
	CertFile string `toml:"cert_file"`
	// KeyFile is the PEM-encoded private key path. Required together with
	// CertFile. Override: MXLRC_TLS_KEY_FILE.
	KeyFile string `toml:"key_file"`
	// SelfSigned generates and persists a self-signed certificate on first run
	// (ECDSA P-256, CN=mxlrcgo-svc, ~365-day validity) under <dir(db_path)>/tls/,
	// regenerating when missing or expired. Mutually exclusive with
	// CertFile/KeyFile. Browsers show an untrusted-certificate prompt. Override:
	// MXLRC_TLS_SELF_SIGNED.
	SelfSigned bool `toml:"self_signed"`
	// RedirectHTTP is an optional plain-HTTP listen address (e.g. ":80") whose
	// every request 301-redirects to the HTTPS address. Empty (the default) means
	// no redirect listener. Only honored when TLS is enabled. Override:
	// MXLRC_TLS_REDIRECT_HTTP.
	RedirectHTTP string `toml:"redirect_http"`
	// SelfSignedHosts lists extra hostnames and IP literals to include as Subject
	// Alternative Names in the generated self-signed certificate, in addition to the
	// built-in SANs (localhost, canticle, 127.0.0.1, ::1). Allows browsers on
	// LAN hosts to reach https://<lan-ip-or-hostname> without a name-mismatch error
	// when they trust the cert. Each entry must be a valid hostname or IP literal;
	// invalid entries are a startup error. Duplicates and entries already covered by
	// the built-ins are silently ignored. Only honored when self_signed is true.
	// Override: MXLRC_TLS_SELF_SIGNED_HOSTS (comma-separated).
	SelfSignedHosts []string `toml:"self_signed_hosts"`
}

// Enabled reports whether TLS termination is configured (bring-your-own cert or
// self-signed bootstrap). When false the serve listener stays plain HTTP.
func (t TLSConfig) Enabled() bool {
	return (t.CertFile != "" && t.KeyFile != "") || t.SelfSigned
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

// defaultSweepIntervalSeconds is the built-in path-reconciliation sweep interval
// (6h). Lazier than the content scan by design: the watcher reconciles deletions
// reactively, so the sweep is only a backstop and need not spin disks often.
const defaultSweepIntervalSeconds = 21600

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

// detectorOrderingDemoted and detectorOrderingFront are the supported
// instrumental_detector.ordering values. Demoted (the default) consults the
// detector lane only after a provider miss, preserving today's behavior;
// front settles a high-confidence instrumental before any provider lane runs.
const (
	detectorOrderingDemoted = "demoted"
	detectorOrderingFront   = "front"
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
	// VocalClasses is the list of SUNG-vocal AudioSet class names whose PEAK
	// (max-over-frames) score gates the instrumental decision: a track is never
	// marked instrumental when any of these peaks at or above VocalMaxConfidence.
	// Default is the verified singing/vocal set (Speech is NOT included -- it is
	// gated separately on sustained mean, see SpeechClasses). A class also listed
	// in SpeechClasses is de-duplicated out of the effective peak set at detector
	// construction. Override: MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES (CSV).
	VocalClasses []string `toml:"vocal_classes"`
	// VocalMaxConfidence is the maximum tolerated sung-vocal-class peak before a
	// track is excluded from being marked instrumental. Conservative (biased
	// toward "not instrumental"). Values outside (0, 1] reset to the default 0.03.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE.
	VocalMaxConfidence float64 `toml:"vocal_max_confidence"`
	// SpeechClasses is the list of AudioSet class names gated on SUSTAINED
	// presence -- their summed frame MEAN (not peak) -- against
	// SpeechMaxConfidence. This is a separate gate from the sung-vocal peak gate
	// so brief incidental speech (crowd, announcer, a line of dialog: high peak,
	// near-zero mean) no longer blocks an instrumental marking, while sustained
	// spoken word (high mean) still does. Default ["Speech"].
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_CLASSES (CSV).
	SpeechClasses []string `toml:"speech_classes"`
	// SpeechMaxConfidence is the maximum tolerated summed Speech-class frame MEAN
	// before a track is excluded from being marked instrumental. Conservative
	// (biased toward "not instrumental"). Values outside (0, 1] reset to the
	// default 0.20. The default is a PROVISIONAL placeholder pending a #384-style
	// calibration sweep; because the key is configurable, calibration refines it
	// without a code change. Override: MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE.
	SpeechMaxConfidence float64 `toml:"speech_max_confidence"`
	// SpreadSamples is the number of short segments evenly distributed across the
	// track and concatenated into one classifier sample, so late-entering vocals
	// are captured. Total sampled audio stays SampleDurationSeconds; each segment
	// is SampleDurationSeconds/SpreadSamples long. A value < 2 disables spreading
	// (single contiguous window). Default 6.
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES.
	SpreadSamples int `toml:"spread_samples"`
	// FFprobePath is the path to the ffprobe binary used to read track duration
	// for spread-sample placement. Empty (default) means auto-discover (sibling of
	// ffmpeg, then PATH). Set this when ffmpeg was auto-provisioned (which ships no
	// ffprobe). Override: MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH.
	FFprobePath string `toml:"ffprobe_path"`
	// Ordering places the detector lane first ("front") so a high-confidence
	// instrumental settles with zero provider requests, or last ("demoted",
	// default) so it is consulted only on a provider miss (today's behavior).
	// Override: MXLRC_INSTRUMENTAL_DETECTOR_ORDERING.
	Ordering string `toml:"ordering"`
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

// RealignConfig governs the realign feature, which re-attaches orphaned
// .lrc/.txt sidecars (left behind when audio files are renamed) to their audio
// via a four-tier confidence resolver. The fields gate which tiers are eligible
// to apply. Defaults are conservative: the feature is off, provenance is not
// required, matches are confined to the orphan's directory, and identity is
// matched MBID-first then ISRC.
type RealignConfig struct {
	// Enabled is the master switch for the realign feature. Default false. Not
	// yet active: the realign CLI command runs regardless of this flag; it is
	// reserved for the serve-mode auto-realign integration (#450) and UI surfacing.
	// Override: MXLRC_REALIGN_ENABLED.
	Enabled bool `toml:"enabled"`
	// OnScan runs realign automatically after each library scan. Not yet active:
	// reserved for the serve-mode auto-realign integration (#450, the scheduler
	// wiring is not yet built); default false.
	// Override: MXLRC_REALIGN_ON_SCAN.
	OnScan bool `toml:"on_scan"`
	// RequireProvenance restricts applied moves to the exact (ISRC/MBID) tier;
	// heuristic candidates are reported but never renamed. Default false.
	// Override: MXLRC_REALIGN_REQUIRE_PROVENANCE.
	RequireProvenance bool `toml:"require_provenance"`
	// CrossDirectory lets an exact provenance match move a sidecar to an audio
	// file outside the orphan's directory (still within the library root).
	// Default false. Override: MXLRC_REALIGN_CROSS_DIRECTORY.
	CrossDirectory bool `toml:"cross_directory"`
	// IdentityKeys is the ordered list of provenance identifiers the exact tier
	// matches on, most authoritative first. Valid values: "mbid", "isrc".
	// Default ["mbid", "isrc"]. Override: MXLRC_REALIGN_IDENTITY_KEYS (comma-separated).
	IdentityKeys []string `toml:"identity_keys"`
	// MinConfidence is the Jaro-Winkler name-similarity floor (0-1) a heuristic
	// rename must clear: the orphan's [ar:]/[ti:] header (or sidecar stem) vs the
	// candidate audio's artist/title. Default 0.75. Values outside (0,1] are reset
	// to the default. Override: MXLRC_REALIGN_MIN_CONFIDENCE.
	MinConfidence float64 `toml:"min_confidence"`
	// AutoApplyHeuristic governs whether serve mode's reactive realign (watcher /
	// post-scan / webhook) is allowed to auto-apply heuristic-tier matches. Default
	// false (exact/provenance-verified matches only) so an unattended pass never
	// renames on a name-similarity guess without an explicit opt-in; the manual
	// CLI is unaffected (it keeps applying heuristic matches unless
	// require_provenance is set). Override: MXLRC_REALIGN_AUTO_APPLY_HEURISTIC.
	AutoApplyHeuristic bool `toml:"auto_apply_heuristic"`
}

// realignMinConfidenceDefault is the default Jaro-Winkler name-similarity floor
// for a heuristic-tier realign rename. Conservative: biased toward reporting a
// borderline pair rather than renaming it.
const realignMinConfidenceDefault = 0.75

// realignIdentityKeysDefault is the default provenance match order for the exact
// tier: MusicBrainz recording MBID first (globally unique per recording), then
// ISRC. Kept in sync with the [realign] rendering and docs.
func realignIdentityKeysDefault() []string { return []string{"mbid", "isrc"} }

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

// DefaultOutputDir is the default output directory name used in both the
// config defaults() path and the serve-mode fallback so the value is defined
// once and stays in sync.
const DefaultOutputDir = "lyrics"

// guardThresholdDefault is the default foreign-script share threshold. It
// mirrors langguard's built-in default so an empty config and an empty
// allowlist agree.
const guardThresholdDefault = 0.20

// detectorVocalMaxConfidenceDefault is the default vocal-class peak threshold
// for the instrumental detector's vocal gate. Pinned by the issue #384
// calibration sweep over the full instrumental-marked corpus (184/352 flipped,
// 0 known-vocal false-positives). Conservative: biased toward "not instrumental".
const detectorVocalMaxConfidenceDefault = 0.03

// detectorSpeechMaxConfidenceDefault is the default summed-frame-MEAN threshold
// for the instrumental detector's speech gate (sustained spoken-word presence).
// PROVISIONAL: a conservatively low placeholder biased toward "not instrumental",
// pending a #384-style calibration sweep to pin the final value. Mirrors the
// detector package's defaultSpeechMaxConfidence by convention.
const detectorSpeechMaxConfidenceDefault = 0.20

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

// WatcherConfig holds the optional filesystem-watcher tuning. The watcher is a
// latency optimization layered over the periodic scheduler (see the watcher
// package doc); it is off by default. These values mirror the watcher package's
// own defaults (defaultDebounceMS / defaultMaxDirs) so the central config and a
// standalone ConfigFromEnv agree.
type WatcherConfig struct {
	// Enabled is the master switch for the watcher. Default false (off).
	// Override: MXLRCGO_WATCH_ENABLED.
	Enabled bool `toml:"enabled"`
	// DebounceMS is the quiet period in milliseconds after the last filesystem
	// event before a targeted scan fires; it coalesces event storms (taggers
	// rewrite albums in bursts). Default 2000. A non-positive value is clamped to
	// the watcher package default when the watcher is constructed.
	// Override: MXLRCGO_WATCH_DEBOUNCE_MS.
	DebounceMS int `toml:"debounce_ms"`
	// MaxDirs caps how many directories may be watched before startup fails, a
	// safety valve so a misconfigured root fails fast instead of silently
	// exhausting the kernel inotify watch budget. Default 100000. A non-positive
	// value is clamped to the watcher package default at construction.
	// Override: MXLRCGO_WATCH_MAX_DIRS.
	MaxDirs int `toml:"max_dirs"`
}

// watcherDebounceMSDefault and watcherMaxDirsDefault mirror the watcher
// package's defaultDebounceMS / defaultMaxDirs. They are duplicated here (rather
// than imported) to keep config free of an import on the watcher package;
// TestWatcherDefaultsMatchConfigPackage (in the watcher package) guards drift.
const (
	watcherDebounceMSDefault = 2000
	watcherMaxDirsDefault    = 100000
)

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
		Output:       OutputConfig{Dir: DefaultOutputDir, EmbeddedLyrics: "off"},
		DB:           DBConfig{Path: xdgDataPath("mxlrcgo-svc", "mxlrcgo.db")},
		Server:       ServerConfig{Addr: "127.0.0.1:3876", ScanIntervalSeconds: defaultScanIntervalSeconds, SweepIntervalSeconds: defaultSweepIntervalSeconds},
		Providers:    ProvidersConfig{Primary: "musixmatch", Mode: providersModeDefault, RaceWaitSeconds: raceWaitSecondsDefault},
		Verification: VerificationConfig{FFmpegPath: "ffmpeg", SampleDurationSeconds: 30, MinConfidence: 0.85, MinSimilarity: 0.35},
		InstrumentalDetector: InstrumentalDetectorConfig{
			FFmpegPath:            "ffmpeg",
			SampleDurationSeconds: 30,
			MinConfidence:         0.90,
			InstrumentalClasses:   []string{"Music", "Musical instrument"},
			VocalClasses:          []string{"Singing", "Vocal music", "Choir", "A capella", "Chant", "Rapping", "Child singing", "Synthetic singing", "Yodeling", "Humming"},
			VocalMaxConfidence:    detectorVocalMaxConfidenceDefault,
			SpeechClasses:         []string{"Speech"},
			SpeechMaxConfidence:   detectorSpeechMaxConfidenceDefault,
			SpreadSamples:         6,
			CooldownSeconds:       5,
			Ordering:              detectorOrderingDemoted,
		},
		Enrichment: EnrichmentConfig{Enabled: true},
		Realign: RealignConfig{
			Enabled:            false,
			OnScan:             false,
			RequireProvenance:  false,
			CrossDirectory:     false,
			IdentityKeys:       realignIdentityKeysDefault(),
			MinConfidence:      realignMinConfidenceDefault,
			AutoApplyHeuristic: false,
		},
		Guard:   GuardConfig{Threshold: guardThresholdDefault},
		Queue:   QueueConfig{Randomize: true},
		Watcher: WatcherConfig{Enabled: false, DebounceMS: watcherDebounceMSDefault, MaxDirs: watcherMaxDirsDefault},
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
	path = ResolveConfigPath(path)
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
			if len(cfg.InstrumentalDetector.VocalClasses) == 0 {
				cfg.InstrumentalDetector.VocalClasses = d.InstrumentalDetector.VocalClasses
			}
			if cfg.InstrumentalDetector.VocalMaxConfidence <= 0 || cfg.InstrumentalDetector.VocalMaxConfidence > 1 {
				cfg.InstrumentalDetector.VocalMaxConfidence = d.InstrumentalDetector.VocalMaxConfidence
			}
			if len(cfg.InstrumentalDetector.SpeechClasses) == 0 {
				cfg.InstrumentalDetector.SpeechClasses = d.InstrumentalDetector.SpeechClasses
			}
			if cfg.InstrumentalDetector.SpeechMaxConfidence <= 0 || cfg.InstrumentalDetector.SpeechMaxConfidence > 1 {
				cfg.InstrumentalDetector.SpeechMaxConfidence = d.InstrumentalDetector.SpeechMaxConfidence
			}
			// Realign.MinConfidence: an out-of-range value from TOML would silently
			// disable the name guard (<=0) or block every heuristic realign (>1);
			// reset it to the default just as the env-override path does.
			if cfg.Realign.MinConfidence <= 0 || cfg.Realign.MinConfidence > 1 {
				cfg.Realign.MinConfidence = d.Realign.MinConfidence
			}
			// SpreadSamples is intentionally NOT re-defaulted: defaults() seeds 6 and
			// the TOML decode preserves it when the key is omitted, so an explicit
			// spread_samples = 0 or 1 (single window) survives and is honored.
			// CooldownSeconds=0 is a valid user value (disable cooldown), so it is
			// not re-defaulted. Negative values are clamped to 0.
			if cfg.InstrumentalDetector.CooldownSeconds < 0 {
				cfg.InstrumentalDetector.CooldownSeconds = 0
			}
			// Ordering: blank (key omitted from TOML) restores the default; an
			// unrecognized value is also reset to the default rather than left to
			// silently behave as "demoted" via the worker's any-other-value
			// fallback, so a typo in the file is visibly corrected rather than
			// hidden.
			if cfg.InstrumentalDetector.Ordering != detectorOrderingFront && cfg.InstrumentalDetector.Ordering != detectorOrderingDemoted {
				cfg.InstrumentalDetector.Ordering = d.InstrumentalDetector.Ordering
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
			// Watcher: Enabled defaults to false (the bool zero-value), so it is
			// never re-defaulted (an explicit enabled=false is honored). The two int
			// fields use 0 == not-set-in-file: restore the default so a blank
			// config.example.toml copy keeps the documented debounce/cap. New also
			// clamps any non-positive value at construction, so a user-set 0 still
			// works, but restoring the default here keeps the Raw config tab and
			// provenance display sensible.
			if cfg.Watcher.DebounceMS == 0 {
				cfg.Watcher.DebounceMS = d.Watcher.DebounceMS
			}
			if cfg.Watcher.MaxDirs == 0 {
				cfg.Watcher.MaxDirs = d.Watcher.MaxDirs
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
	if err := validateServerTLS(cfg); err != nil {
		return cfg, appliedEnv, err
	}
	if err := validateInstrumentalDetectorOrdering(cfg); err != nil {
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
// Supported: MUSIXMATCH_TOKEN, MXLRC_API_TOKEN, MXLRC_API_COOLDOWN, MXLRC_COOLDOWN, MXLRC_API_CIRCUIT_OPEN_DURATION, MXLRC_API_CIRCUIT_BACKOFF_BASE, MXLRC_MISS_BACKOFF_BASE_HOURS, MXLRC_MISS_BACKOFF_CAP_HOURS, MXLRC_MAX_MISS_ATTEMPTS, MXLRC_OUTPUT_DIR, MXLRC_BILINGUAL_OUTPUT, MXLRC_DB_PATH, MXLRC_SECRETS_KEY_FILE, MXLRC_SERVER_ADDR, MXLRC_WEB_UI_ENABLED, MXLRC_WEBHOOK_API_KEY, MXLRC_SCAN_INTERVAL, MXLRC_WORK_INTERVAL, MXLRC_TRUSTED_CIDRS, MXLRC_TRUSTED_PROXIES, MXLRC_TLS_CERT_FILE, MXLRC_TLS_KEY_FILE, MXLRC_TLS_SELF_SIGNED, MXLRC_TLS_REDIRECT_HTTP, MXLRC_TLS_SELF_SIGNED_HOSTS, MXLRC_PROVIDER_PRIMARY, MXLRC_PROVIDERS_DISABLED, MXLRC_PROVIDERS_MODE, MXLRC_PROVIDERS_RACE_WAIT_SECONDS, MXLRC_PROVIDERS_FALLBACK_ORDER, MXLRC_VERIFICATION_ENABLED, MXLRC_VERIFICATION_WHISPER_URL, MXLRC_WHISPER_URL, MXLRC_VERIFICATION_FFMPEG_PATH, MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS, MXLRC_VERIFICATION_SAMPLE_DURATION, MXLRC_VERIFICATION_MIN_CONFIDENCE, MXLRC_VERIFICATION_MIN_SIMILARITY, MXLRC_INSTRUMENTAL_DETECTOR_ENABLED, MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL, MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH, MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS, MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE, MXLRC_INSTRUMENTAL_DETECTOR_CLASSES, MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS, MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES, MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE, MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_CLASSES, MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE, MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES, MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH, MXLRC_INSTRUMENTAL_DETECTOR_ORDERING, MXLRC_ENRICHMENT_ENABLED, MXLRC_REALIGN_ENABLED, MXLRC_REALIGN_ON_SCAN, MXLRC_REALIGN_REQUIRE_PROVENANCE, MXLRC_REALIGN_CROSS_DIRECTORY, MXLRC_REALIGN_IDENTITY_KEYS, MXLRC_REALIGN_MIN_CONFIDENCE, MXLRC_GUARD_ACCEPTED_SCRIPTS, MXLRC_GUARD_THRESHOLD, MXLRC_QUEUE_RANDOMIZE, MXLRCGO_WATCH_ENABLED, MXLRCGO_WATCH_DEBOUNCE_MS, MXLRCGO_WATCH_MAX_DIRS, MXLRC_LOG_LEVEL, MXLRC_LOG_FORMAT, MXLRC_LOG_FILE, MXLRC_LOG_MAX_SIZE_MB, MXLRC_LOG_MAX_FILES, MXLRC_LOG_MAX_AGE_DAYS, MXLRC_LOG_COMPRESS
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
	if v := os.Getenv("MXLRC_WEB_UI_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_WEB_UI_ENABLED", "value", v, "current", cfg.Server.WebUIEnabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.WebUIEnabled = enabled
			applied["server.web_ui_enabled"] = true
		}
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
	if v := os.Getenv("MXLRC_SWEEP_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SWEEP_INTERVAL", "value", v, "current", cfg.Server.SweepIntervalSeconds) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.SweepIntervalSeconds = n
			applied["server.sweep_interval_seconds"] = true
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
	if v := os.Getenv("MXLRC_TLS_CERT_FILE"); v != "" {
		cfg.Server.TLS.CertFile = v
		applied["server.tls.cert_file"] = true
	}
	if v := os.Getenv("MXLRC_TLS_KEY_FILE"); v != "" {
		cfg.Server.TLS.KeyFile = v
		applied["server.tls.key_file"] = true
	}
	if v := os.Getenv("MXLRC_TLS_SELF_SIGNED"); v != "" {
		selfSigned, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_TLS_SELF_SIGNED", "value", v, "current", cfg.Server.TLS.SelfSigned) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.TLS.SelfSigned = selfSigned
			applied["server.tls.self_signed"] = true
		}
	}
	if v := os.Getenv("MXLRC_TLS_REDIRECT_HTTP"); v != "" {
		cfg.Server.TLS.RedirectHTTP = v
		applied["server.tls.redirect_http"] = true
	}
	if v := os.Getenv("MXLRC_TLS_SELF_SIGNED_HOSTS"); v != "" {
		cfg.Server.TLS.SelfSignedHosts = splitCSV(v)
		applied["server.tls.self_signed_hosts"] = true
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
	if v := os.Getenv("MXLRCGO_WATCH_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRCGO_WATCH_ENABLED", "value", v, "current", cfg.Watcher.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Watcher.Enabled = enabled
			applied["watcher.enabled"] = true
		}
	}
	if v := os.Getenv("MXLRCGO_WATCH_DEBOUNCE_MS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRCGO_WATCH_DEBOUNCE_MS", "value", v, "current", cfg.Watcher.DebounceMS) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Watcher.DebounceMS = n
			applied["watcher.debounce_ms"] = true
		}
	}
	if v := os.Getenv("MXLRCGO_WATCH_MAX_DIRS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRCGO_WATCH_MAX_DIRS", "value", v, "current", cfg.Watcher.MaxDirs) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Watcher.MaxDirs = n
			applied["watcher.max_dirs"] = true
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
	if v := os.Getenv("MXLRC_REALIGN_ENABLED"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_ENABLED", "value", v, "current", cfg.Realign.Enabled) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.Enabled = enabled
			applied["realign.enabled"] = true
		}
	}
	if v := os.Getenv("MXLRC_REALIGN_ON_SCAN"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_ON_SCAN", "value", v, "current", cfg.Realign.OnScan) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.OnScan = enabled
			applied["realign.on_scan"] = true
		}
	}
	if v := os.Getenv("MXLRC_REALIGN_REQUIRE_PROVENANCE"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_REQUIRE_PROVENANCE", "value", v, "current", cfg.Realign.RequireProvenance) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.RequireProvenance = enabled
			applied["realign.require_provenance"] = true
		}
	}
	if v := os.Getenv("MXLRC_REALIGN_CROSS_DIRECTORY"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_CROSS_DIRECTORY", "value", v, "current", cfg.Realign.CrossDirectory) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.CrossDirectory = enabled
			applied["realign.cross_directory"] = true
		}
	}
	if v := os.Getenv("MXLRC_REALIGN_IDENTITY_KEYS"); v != "" {
		cfg.Realign.IdentityKeys = splitCSV(v)
		applied["realign.identity_keys"] = true
	}
	if v := os.Getenv("MXLRC_REALIGN_AUTO_APPLY_HEURISTIC"); v != "" {
		enabled, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_AUTO_APPLY_HEURISTIC", "value", v, "current", cfg.Realign.AutoApplyHeuristic) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.AutoApplyHeuristic = enabled
			applied["realign.auto_apply_heuristic"] = true
		}
	}
	if v := os.Getenv("MXLRC_REALIGN_MIN_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_REALIGN_MIN_CONFIDENCE", "value", v, "current", cfg.Realign.MinConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Realign.MinConfidence = n
			applied["realign.min_confidence"] = true
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
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES"); v != "" {
		cfg.InstrumentalDetector.VocalClasses = splitCSV(v)
		applied["instrumental_detector.vocal_classes"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE", "value", v, "current", cfg.InstrumentalDetector.VocalMaxConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.VocalMaxConfidence = n
			applied["instrumental_detector.vocal_max_confidence"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_CLASSES"); v != "" {
		cfg.InstrumentalDetector.SpeechClasses = splitCSV(v)
		applied["instrumental_detector.speech_classes"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 || n > 1 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE", "value", v, "current", cfg.InstrumentalDetector.SpeechMaxConfidence) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.SpeechMaxConfidence = n
			applied["instrumental_detector.speech_max_confidence"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES", "value", v, "current", cfg.InstrumentalDetector.SpreadSamples) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.SpreadSamples = n
			applied["instrumental_detector.spread_samples"] = true
		}
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH"); v != "" {
		cfg.InstrumentalDetector.FFprobePath = v
		applied["instrumental_detector.ffprobe_path"] = true
	}
	if v := os.Getenv("MXLRC_INSTRUMENTAL_DETECTOR_ORDERING"); v != "" {
		normalized := strings.ToLower(strings.TrimSpace(v))
		if normalized != detectorOrderingFront && normalized != detectorOrderingDemoted {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_INSTRUMENTAL_DETECTOR_ORDERING", "value", v, "current", cfg.InstrumentalDetector.Ordering) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.InstrumentalDetector.Ordering = normalized
			applied["instrumental_detector.ordering"] = true
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

// validateServerTLS fails fast on a contradictory [server.tls] configuration
// (issue #204, Area 4): self_signed cannot be combined with an explicit
// cert_file/key_file, and cert_file and key_file must be supplied together.
// Each entry in self_signed_hosts must parse as a valid IP literal or hostname.
// Validation happens at load so a misconfiguration is a clear startup error
// rather than a confusing listener failure.
func validateServerTLS(cfg Config) error {
	t := cfg.Server.TLS
	if err := ValidateTLSSelection(t.SelfSigned, t.CertFile, t.KeyFile); err != nil {
		return fmt.Errorf("config: server.tls: %w", err)
	}
	for _, h := range t.SelfSignedHosts {
		if net.ParseIP(h) == nil && !isValidHostname(h) {
			return fmt.Errorf("config: server.tls: self_signed_hosts: %q is not a valid hostname or IP address", h)
		}
	}
	return nil
}

// validateInstrumentalDetectorOrdering fails fast on a contradictory
// combination of instrumental_detector.ordering="front" and
// providers.mode="parallel". "front" exists to let a high-confidence
// instrumental verdict settle a track with zero provider requests, which only
// holds in ordered mode: findOrdered walks lanes in slice order and a
// terminal-suitable result short-circuits the rest. In parallel mode
// findParallel launches every lane concurrently, so lane position does not
// determine dispatch and the provider lanes are already in flight before the
// detector can settle anything - the combination silently fails to deliver
// the guarantee its name promises. Staged dispatch that would make "front"
// meaningful under parallel mode is tracked separately (issue #528) and is
// out of scope here; this only rejects the contradictory configuration at
// load so the failure is a clear startup error rather than a silent
// no-op. Validation happens after both TOML and env overrides are resolved,
// so it fires regardless of which source set either value.
func validateInstrumentalDetectorOrdering(cfg Config) error {
	if cfg.InstrumentalDetector.Ordering == detectorOrderingFront && cfg.Providers.Mode == providersModeParallel {
		return fmt.Errorf("config: instrumental_detector.ordering=front requires providers.mode=ordered: " +
			"providers.mode=parallel dispatches all lanes concurrently, so a detector-first ordering cannot " +
			"prevent provider requests; set providers.mode=ordered or instrumental_detector.ordering=demoted")
	}
	return nil
}

// ValidateTLSSelection enforces the two cross-field [server.tls] invariants the
// daemon needs to boot: self_signed is mutually exclusive with an explicit
// cert_file/key_file, and cert_file and key_file must be set together. It is the
// single source of truth shared by validateServerTLS (boot) and the settings
// write path's checkTLSInvariant (a per-field validator cannot see these
// resulting-state rules), so the UI rejects exactly what boot would reject.
func ValidateTLSSelection(selfSigned bool, certFile, keyFile string) error {
	if selfSigned && (certFile != "" || keyFile != "") {
		return fmt.Errorf("self_signed is mutually exclusive with cert_file/key_file")
	}
	if (certFile == "") != (keyFile == "") {
		return fmt.Errorf("cert_file and key_file must be set together")
	}
	return nil
}

// isValidHostname reports whether s is a valid RFC 1123 hostname. Each dot-separated
// label must be 1-63 characters of [a-zA-Z0-9-] with no leading or trailing hyphen.
func isValidHostname(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		n := len(label)
		if n == 0 || n > 63 {
			return false
		}
		if label[0] == '-' || label[n-1] == '-' {
			return false
		}
		for _, ch := range label {
			if (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') && (ch < '0' || ch > '9') && ch != '-' {
				return false
			}
		}
	}
	return true
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

// ResolveConfigPath resolves the effective config file path: the caller-supplied
// path when non-empty, otherwise the XDG default (config.toml under the app's
// XDG config dir, or /config in Docker). It is the single resolver shared by
// LoadWithSources and the settings write path, so the web UI writes to the exact
// file the daemon loaded. May return "" only if no path was given and the home
// directory cannot be determined outside Docker.
func ResolveConfigPath(path string) string {
	if path != "" {
		return path
	}
	return xdgConfigPath("mxlrcgo-svc", "config.toml")
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
