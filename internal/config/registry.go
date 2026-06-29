package config

// FieldType is the Go kind of a config field's value, used by the settings
// writer and UI to render and serialize the field correctly.
type FieldType int

const (
	// TypeString is a plain string field.
	TypeString FieldType = iota
	// TypeInt is an integer field.
	TypeInt
	// TypeBool is a boolean field.
	TypeBool
	// TypeFloat64 is a floating-point field.
	TypeFloat64
	// TypeStringSlice is a comma-separated list field ([]string).
	TypeStringSlice
)

// Criticality is the risk tier of a config field. It drives the settings
// page's save affordance: Safe fields hot-save, Caution fields need an
// explicit Save, Critical fields need Save plus a confirmation prompt. The
// tier governs write-friction only; no change takes effect until restart
// (there is no config hot-reload).
type Criticality int

const (
	// Safe fields are low-risk: a wrong value degrades but does not break or
	// lock out the daemon.
	Safe Criticality = iota
	// Caution fields can break serving or a core function if wrong, but do not
	// lock the operator out.
	Caution
	// Critical fields can lock the operator out or break boot.
	Critical
)

// FieldSpec describes one editable (or read-only) config field. It is the
// single source of truth for per-field metadata: where the value lives, the
// env var(s) that override it, its type, and how risky it is to change. The
// settings page binds to this registry; the read-side provenance map keys off
// the same dotted Path values that applyEnvOverrides records.
type FieldSpec struct {
	// Path is the dotted config key, e.g. "api.token". It MUST match the key
	// applyEnvOverrides records in its provenance map (enforced by the drift
	// test).
	Path string
	// Section is the top-level TOML section, e.g. "server" for
	// "server.tls.cert_file".
	Section string
	// Type is the Go kind of the value.
	Type FieldType
	// EnvVars lists the env var names that override this field, primary first.
	// CLI > env > file precedence is unchanged; this just names the env vars.
	EnvVars []string
	// Sensitive marks secrets that must be redacted and never echoed to a
	// client (matches IsSensitiveConfigKey).
	Sensitive bool
	// Criticality is the field's risk tier (drives the save affordance).
	Criticality Criticality
	// Editable is false for fields the settings UI/writer must never rewrite
	// (e.g. secrets.key_file, whose change is a key-rotation operation).
	Editable bool
	// Description is the plain-language, one-line help text for the field. It is
	// the single source of truth for both the settings UI help line and the
	// "# mxlrc: ..." comment stamped above the key in config.toml on save. It
	// lives on the registry (not in internal/web) so the config package can read
	// it on the write path without importing internal/web (#291).
	Description string
}

// fields is the canonical registry. Every entry's Path MUST appear in
// applyEnvOverrides and vice versa (enforced by TestRegistryDrift). Tiers
// follow the locked design: Safe fields hot-save, Caution fields need an
// explicit Save, Critical fields need Save + confirm. Only secrets.key_file is
// read-only (Editable: false).
var fields = []FieldSpec{
	// [api]
	{Path: "api.token", Section: "api", Type: TypeString, EnvVars: []string{"MUSIXMATCH_TOKEN", "MXLRC_API_TOKEN"}, Sensitive: true, Criticality: Critical, Editable: true, Description: "Musixmatch API token. Required to fetch lyrics."},
	{Path: "api.cooldown", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_COOLDOWN", "MXLRC_COOLDOWN"}, Criticality: Safe, Editable: true, Description: "Minimum seconds between Musixmatch requests."},
	{Path: "api.circuit_open_duration", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_CIRCUIT_OPEN_DURATION"}, Criticality: Safe, Editable: true, Description: "Maximum circuit-breaker pause (seconds) after repeated throttling."},
	{Path: "api.circuit_backoff_base_seconds", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_CIRCUIT_BACKOFF_BASE"}, Criticality: Safe, Editable: true, Description: "Initial circuit-breaker pause (seconds); doubles on each trip."},
	{Path: "api.miss_backoff_base_hours", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MISS_BACKOFF_BASE_HOURS"}, Criticality: Safe, Editable: true, Description: "Initial re-check delay (hours) for a benign miss."},
	{Path: "api.miss_backoff_cap_hours", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MISS_BACKOFF_CAP_HOURS"}, Criticality: Safe, Editable: true, Description: "Maximum re-check delay (hours) for a benign miss."},
	{Path: "api.max_miss_attempts", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MAX_MISS_ATTEMPTS"}, Criticality: Safe, Editable: true, Description: "Retire a queue row after this many misses. 0 disables the cap."},

	// [output]
	{Path: "output.dir", Section: "output", Type: TypeString, EnvVars: []string{"MXLRC_OUTPUT_DIR"}, Criticality: Safe, Editable: true, Description: "Default directory for .lrc output files."},
	{Path: "output.embedded_lyrics", Section: "output", Type: TypeString, EnvVars: []string{"MXLRC_EMBEDDED_LYRICS"}, Criticality: Safe, Editable: true, Description: "How to handle embedded lyrics: off, respect, or extract."},
	{Path: "output.bilingual_output", Section: "output", Type: TypeBool, EnvVars: []string{"MXLRC_BILINGUAL_OUTPUT"}, Criticality: Safe, Editable: true, Description: "Interleave original and translation lines in one .lrc."},

	// [db]
	{Path: "db.path", Section: "db", Type: TypeString, EnvVars: []string{"MXLRC_DB_PATH"}, Criticality: Caution, Editable: true, Description: "SQLite database file path."},

	// [secrets]
	{Path: "secrets.key_file", Section: "secrets", Type: TypeString, EnvVars: []string{"MXLRC_SECRETS_KEY_FILE"}, Criticality: Critical, Editable: false, Description: "Path to the 32-byte master key that encrypts stored secrets (the API token and webhook keys). Defaults to .mxlrcgo.key beside the database, or set MXLRC_MASTER_KEY to supply the key directly. Read-only here: the key cannot be rotated while the server is running."},

	// [server]
	{Path: "server.addr", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_SERVER_ADDR"}, Criticality: Caution, Editable: true, Description: "Listen address for serve mode (host:port)."},
	{Path: "server.web_ui_enabled", Section: "server", Type: TypeBool, EnvVars: []string{"MXLRC_WEB_UI_ENABLED"}, Criticality: Caution, Editable: true},
	{Path: "server.webhook_api_keys", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_WEBHOOK_API_KEY"}, Sensitive: true, Criticality: Critical, Editable: true, Description: "Keys that Lidarr sends to authenticate its webhook to this server (POST /api/v1/webhooks/lidarr). Only Lidarr is supported today."},
	{Path: "server.scan_interval_seconds", Section: "server", Type: TypeInt, EnvVars: []string{"MXLRC_SCAN_INTERVAL"}, Criticality: Caution, Editable: true, Description: "Seconds between library scans in serve mode."},
	{Path: "server.work_interval_seconds", Section: "server", Type: TypeInt, EnvVars: []string{"MXLRC_WORK_INTERVAL"}, Criticality: Caution, Editable: true, Description: "Seconds between work-queue drains in serve mode."},
	{Path: "server.trusted_networks.cidrs", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TRUSTED_CIDRS"}, Criticality: Critical, Editable: true, Description: "Client CIDRs allowed to reach the server."},
	{Path: "server.trusted_networks.trusted_proxies", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TRUSTED_PROXIES"}, Criticality: Critical, Editable: true, Description: "Proxy CIDRs whose forwarded-for header is trusted."},
	{Path: "server.tls.cert_file", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_CERT_FILE"}, Criticality: Critical, Editable: true, Description: "TLS certificate file path (PEM-encoded, typically .pem or .crt). Enter this together with the private key below and click Save on either - the pair is written in one step. Leave both blank (and turn on self-signed) if you don't have a custom certificate."},
	{Path: "server.tls.key_file", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_KEY_FILE"}, Criticality: Critical, Editable: true, Description: "TLS private key file path (PEM-encoded, typically .key). Enter this together with the certificate above and click Save on either - the pair is written in one step."},
	{Path: "server.tls.self_signed", Section: "server", Type: TypeBool, EnvVars: []string{"MXLRC_TLS_SELF_SIGNED"}, Criticality: Critical, Editable: true, Description: "Generate and use a self-signed certificate."},
	{Path: "server.tls.redirect_http", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_REDIRECT_HTTP"}, Criticality: Critical, Editable: true, Description: "HTTP listen address to redirect to HTTPS (blank disables)."},
	{Path: "server.tls.self_signed_hosts", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TLS_SELF_SIGNED_HOSTS"}, Criticality: Critical, Editable: true, Description: "Hostnames to include in the self-signed certificate."},

	// [providers]
	{Path: "providers.primary", Section: "providers", Type: TypeString, EnvVars: []string{"MXLRC_PROVIDER_PRIMARY"}, Criticality: Caution, Editable: true, Description: "Lyrics provider tried first."},
	{Path: "providers.disabled", Section: "providers", Type: TypeStringSlice, EnvVars: []string{"MXLRC_PROVIDERS_DISABLED"}, Criticality: Caution, Editable: true, Description: "Check each lyrics source to enable it. Either or both may run; checking Musixmatch un-greys its API token field. An unchecked source is never used. The primary source can't be disabled here - change the main source first."},
	{Path: "providers.mode", Section: "providers", Type: TypeString, EnvVars: []string{"MXLRC_PROVIDERS_MODE"}, Criticality: Safe, Editable: true, Description: "How to use multiple sources: ordered (try in turn) or parallel (race them)."},
	{Path: "providers.race_wait_seconds", Section: "providers", Type: TypeInt, EnvVars: []string{"MXLRC_PROVIDERS_RACE_WAIT_SECONDS"}, Criticality: Safe, Editable: true, Description: "Grace seconds the race winner waits for a richer result."},
	{Path: "providers.fallback_order", Section: "providers", Type: TypeStringSlice, EnvVars: []string{"MXLRC_PROVIDERS_FALLBACK_ORDER"}, Criticality: Caution, Editable: true, Description: "Provider order tried in fallback mode."},

	// [verification]
	{Path: "verification.enabled", Section: "verification", Type: TypeBool, EnvVars: []string{"MXLRC_VERIFICATION_ENABLED"}, Criticality: Safe, Editable: true, Description: "Verify fetched lyrics against the audio."},
	{Path: "verification.whisper_url", Section: "verification", Type: TypeString, EnvVars: []string{"MXLRC_VERIFICATION_WHISPER_URL", "MXLRC_WHISPER_URL"}, Criticality: Caution, Editable: true, Description: "Whisper transcription service URL."},
	{Path: "verification.ffmpeg_path", Section: "verification", Type: TypeString, EnvVars: []string{"MXLRC_VERIFICATION_FFMPEG_PATH"}, Criticality: Caution, Editable: true, Description: "Path to the ffmpeg binary used to sample audio."},
	{Path: "verification.sample_duration_seconds", Section: "verification", Type: TypeInt, EnvVars: []string{"MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "MXLRC_VERIFICATION_SAMPLE_DURATION"}, Criticality: Safe, Editable: true, Description: "Seconds of audio sampled for verification."},
	{Path: "verification.min_confidence", Section: "verification", Type: TypeFloat64, EnvVars: []string{"MXLRC_VERIFICATION_MIN_CONFIDENCE"}, Criticality: Safe, Editable: true, Description: "Minimum transcription confidence to accept (0-1)."},
	{Path: "verification.min_similarity", Section: "verification", Type: TypeFloat64, EnvVars: []string{"MXLRC_VERIFICATION_MIN_SIMILARITY"}, Criticality: Safe, Editable: true, Description: "Minimum lyric/transcript similarity to accept (0-1)."},

	// [instrumental_detector]
	{Path: "instrumental_detector.enabled", Section: "instrumental_detector", Type: TypeBool, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_ENABLED"}, Criticality: Safe, Editable: true, Description: "Detect instrumental tracks via an audio classifier."},
	{Path: "instrumental_detector.classifier_url", Section: "instrumental_detector", Type: TypeString, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL"}, Criticality: Caution, Editable: true, Description: "Audio-classifier service URL."},
	{Path: "instrumental_detector.ffmpeg_path", Section: "instrumental_detector", Type: TypeString, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH"}, Criticality: Caution, Editable: true, Description: "Path to the ffmpeg binary used to sample audio."},
	{Path: "instrumental_detector.sample_duration_seconds", Section: "instrumental_detector", Type: TypeInt, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS"}, Criticality: Safe, Editable: true, Description: "Seconds of audio sampled for detection."},
	{Path: "instrumental_detector.min_confidence", Section: "instrumental_detector", Type: TypeFloat64, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE"}, Criticality: Safe, Editable: true, Description: "Minimum classifier confidence to mark instrumental (0-1)."},
	{Path: "instrumental_detector.instrumental_classes", Section: "instrumental_detector", Type: TypeStringSlice, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_CLASSES"}, Criticality: Safe, Editable: true, Description: "Google AudioSet classifier labels that count as instrumental (defaults: Music, Musical instrument)."},
	{Path: "instrumental_detector.cooldown_seconds", Section: "instrumental_detector", Type: TypeInt, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS"}, Criticality: Safe, Editable: true, Description: "Seconds to wait between detector calls."},
	{Path: "instrumental_detector.vocal_classes", Section: "instrumental_detector", Type: TypeStringSlice, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES"}, Criticality: Safe, Editable: true, Description: "AudioSet vocal labels whose peak score blocks an instrumental marking (defaults: Singing, Vocal music, Choir, ...)."},
	{Path: "instrumental_detector.vocal_max_confidence", Section: "instrumental_detector", Type: TypeFloat64, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE"}, Criticality: Safe, Editable: true, Description: "Maximum vocal-class peak before a track is excluded from instrumental (0-1; default 0.03)."},
	{Path: "instrumental_detector.speech_classes", Section: "instrumental_detector", Type: TypeStringSlice, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_CLASSES"}, Criticality: Safe, Editable: true, Description: "AudioSet speech labels gated on sustained mean (not peak) so brief incidental speech does not block instrumental (default: Speech)."},
	{Path: "instrumental_detector.speech_max_confidence", Section: "instrumental_detector", Type: TypeFloat64, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE"}, Criticality: Safe, Editable: true, Description: "Maximum summed speech-class frame mean before a track is excluded from instrumental (0-1; provisional default 0.20)."},
	{Path: "instrumental_detector.spread_samples", Section: "instrumental_detector", Type: TypeInt, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES"}, Criticality: Safe, Editable: true, Description: "Number of short windows spread across the track per detection (default 6; <2 disables spreading)."},
	{Path: "instrumental_detector.ffprobe_path", Section: "instrumental_detector", Type: TypeString, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH"}, Criticality: Caution, Editable: true, Description: "Path to the ffprobe binary used to read track duration (empty = auto-discover)."},

	// [enrichment]
	{Path: "enrichment.enabled", Section: "enrichment", Type: TypeBool, EnvVars: []string{"MXLRC_ENRICHMENT_ENABLED"}, Criticality: Safe, Editable: true, Description: "Look up recording IDs (ISRC, MusicBrainz MBID, Spotify ID) before fetching, and feed them to the matcher to improve match accuracy."},

	// [guard]
	{Path: "guard.accepted_scripts", Section: "guard", Type: TypeStringSlice, EnvVars: []string{"MXLRC_GUARD_ACCEPTED_SCRIPTS"}, Criticality: Safe, Editable: true, Description: "The foreign-script guard skips or flags lyrics whose share of characters outside these accepted writing systems exceeds the sensitivity threshold below. Valid buckets: Latin, Han, Kana, Hangul, Other. Empty disables the guard."},
	{Path: "guard.script_guard_threshold", Section: "guard", Type: TypeFloat64, EnvVars: []string{"MXLRC_GUARD_THRESHOLD"}, Criticality: Safe, Editable: true, Description: "The fraction of non-accepted-script characters that trips the guard (0-1). Higher is more tolerant (more foreign text allowed); lower is stricter."},

	// [queue]
	{Path: "queue.randomize", Section: "queue", Type: TypeBool, EnvVars: []string{"MXLRC_QUEUE_RANDOMIZE"}, Criticality: Safe, Editable: true, Description: "Process queued tracks in random order."},

	// [watcher]
	{Path: "watcher.enabled", Section: "watcher", Type: TypeBool, EnvVars: []string{"MXLRCGO_WATCH_ENABLED"}, Criticality: Safe, Editable: true, Description: "Watch library folders and trigger a targeted scan when files change. The periodic scan still runs; the watcher only lowers latency. Off by default. Takes effect on restart."},
	{Path: "watcher.debounce_ms", Section: "watcher", Type: TypeInt, EnvVars: []string{"MXLRCGO_WATCH_DEBOUNCE_MS"}, Criticality: Safe, Editable: true, Description: "Milliseconds to wait after the last file change before scanning, so a burst of edits (a tagger rewriting an album) coalesces into one scan. Default 2000."},
	{Path: "watcher.max_dirs", Section: "watcher", Type: TypeInt, EnvVars: []string{"MXLRCGO_WATCH_MAX_DIRS"}, Criticality: Safe, Editable: true, Description: "Safety cap on how many folders the watcher may track. Startup fails fast above this instead of exhausting the OS watch budget. Default 100000."},

	// [logging]
	{Path: "logging.level", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_LEVEL"}, Criticality: Safe, Editable: true, Description: "Log verbosity: debug, info, warn, or error. This single level governs both console and file output (there is no separate per-sink level)."},
	{Path: "logging.format", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_FORMAT"}, Criticality: Safe, Editable: true, Description: "Log format: text or json."},
	{Path: "logging.file", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_FILE"}, Criticality: Safe, Editable: true, Description: "Where to write logs. Leave blank to log to the console (stderr)."},
	{Path: "logging.max_size_mb", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_SIZE_MB"}, Criticality: Safe, Editable: true, Description: "Rotate the log file after this many megabytes."},
	{Path: "logging.max_files", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_FILES"}, Criticality: Safe, Editable: true, Description: "Number of rotated log files to keep."},
	{Path: "logging.max_age_days", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_AGE_DAYS"}, Criticality: Safe, Editable: true, Description: "Delete rotated log files older than this many days."},
	{Path: "logging.compress", Section: "logging", Type: TypeBool, EnvVars: []string{"MXLRC_LOG_COMPRESS"}, Criticality: Safe, Editable: true, Description: "Gzip rotated log files."},
}

// cloneFieldSpec returns a copy of f whose EnvVars slice is independent of the
// package-global backing storage, so a caller cannot mutate f.EnvVars[...] and
// silently alter the registry at runtime.
func cloneFieldSpec(f FieldSpec) FieldSpec {
	out := f
	out.EnvVars = append([]string(nil), f.EnvVars...)
	return out
}

// Registry returns a copy of the full field registry. Returning copies (with
// EnvVars deep-copied) keeps the package-global table immutable to callers, so
// a consumer cannot flip Editable/Sensitive or rename an env var at runtime and
// desync validation / ApplyChanges.
func Registry() []FieldSpec {
	out := make([]FieldSpec, len(fields))
	for i := range fields {
		out[i] = cloneFieldSpec(fields[i])
	}
	return out
}

// FieldByPath looks a field up by its dotted path. The returned spec is a copy;
// mutating it does not affect the registry.
func FieldByPath(path string) (FieldSpec, bool) {
	for _, f := range fields {
		if f.Path == path {
			return cloneFieldSpec(f), true
		}
	}
	return FieldSpec{}, false
}

// FieldByEnvVar looks a field up by any of its env var names. The returned spec
// is a copy; mutating it does not affect the registry.
func FieldByEnvVar(envVar string) (FieldSpec, bool) {
	for _, f := range fields {
		for _, e := range f.EnvVars {
			if e == envVar {
				return cloneFieldSpec(f), true
			}
		}
	}
	return FieldSpec{}, false
}

// AllPaths returns every registry field's dotted path, in registry order.
func AllPaths() []string {
	paths := make([]string, len(fields))
	for i, f := range fields {
		paths[i] = f.Path
	}
	return paths
}
