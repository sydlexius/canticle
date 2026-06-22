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
}

// fields is the canonical registry. Every entry's Path MUST appear in
// applyEnvOverrides and vice versa (enforced by TestRegistryDrift). Tiers
// follow the locked design: Safe fields hot-save, Caution fields need an
// explicit Save, Critical fields need Save + confirm. Only secrets.key_file is
// read-only (Editable: false).
var fields = []FieldSpec{
	// [api]
	{Path: "api.token", Section: "api", Type: TypeString, EnvVars: []string{"MUSIXMATCH_TOKEN", "MXLRC_API_TOKEN"}, Sensitive: true, Criticality: Critical, Editable: true},
	{Path: "api.cooldown", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_COOLDOWN", "MXLRC_COOLDOWN"}, Criticality: Safe, Editable: true},
	{Path: "api.circuit_open_duration", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_CIRCUIT_OPEN_DURATION"}, Criticality: Safe, Editable: true},
	{Path: "api.circuit_backoff_base_seconds", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_API_CIRCUIT_BACKOFF_BASE"}, Criticality: Safe, Editable: true},
	{Path: "api.miss_backoff_base_hours", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MISS_BACKOFF_BASE_HOURS"}, Criticality: Safe, Editable: true},
	{Path: "api.miss_backoff_cap_hours", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MISS_BACKOFF_CAP_HOURS"}, Criticality: Safe, Editable: true},
	{Path: "api.max_miss_attempts", Section: "api", Type: TypeInt, EnvVars: []string{"MXLRC_MAX_MISS_ATTEMPTS"}, Criticality: Safe, Editable: true},

	// [output]
	{Path: "output.dir", Section: "output", Type: TypeString, EnvVars: []string{"MXLRC_OUTPUT_DIR"}, Criticality: Safe, Editable: true},
	{Path: "output.embedded_lyrics", Section: "output", Type: TypeString, EnvVars: []string{"MXLRC_EMBEDDED_LYRICS"}, Criticality: Safe, Editable: true},
	{Path: "output.bilingual_output", Section: "output", Type: TypeBool, EnvVars: []string{"MXLRC_BILINGUAL_OUTPUT"}, Criticality: Safe, Editable: true},

	// [db]
	{Path: "db.path", Section: "db", Type: TypeString, EnvVars: []string{"MXLRC_DB_PATH"}, Criticality: Caution, Editable: true},

	// [secrets]
	{Path: "secrets.key_file", Section: "secrets", Type: TypeString, EnvVars: []string{"MXLRC_SECRETS_KEY_FILE"}, Criticality: Critical, Editable: false},

	// [server]
	{Path: "server.addr", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_SERVER_ADDR"}, Criticality: Caution, Editable: true},
	{Path: "server.web_ui_enabled", Section: "server", Type: TypeBool, EnvVars: []string{"MXLRC_WEB_UI_ENABLED"}, Criticality: Caution, Editable: true},
	{Path: "server.webhook_api_keys", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_WEBHOOK_API_KEY"}, Sensitive: true, Criticality: Critical, Editable: true},
	{Path: "server.scan_interval_seconds", Section: "server", Type: TypeInt, EnvVars: []string{"MXLRC_SCAN_INTERVAL"}, Criticality: Caution, Editable: true},
	{Path: "server.work_interval_seconds", Section: "server", Type: TypeInt, EnvVars: []string{"MXLRC_WORK_INTERVAL"}, Criticality: Caution, Editable: true},
	{Path: "server.trusted_networks.cidrs", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TRUSTED_CIDRS"}, Criticality: Critical, Editable: true},
	{Path: "server.trusted_networks.trusted_proxies", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TRUSTED_PROXIES"}, Criticality: Critical, Editable: true},
	{Path: "server.tls.cert_file", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_CERT_FILE"}, Criticality: Critical, Editable: true},
	{Path: "server.tls.key_file", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_KEY_FILE"}, Criticality: Critical, Editable: true},
	{Path: "server.tls.self_signed", Section: "server", Type: TypeBool, EnvVars: []string{"MXLRC_TLS_SELF_SIGNED"}, Criticality: Critical, Editable: true},
	{Path: "server.tls.redirect_http", Section: "server", Type: TypeString, EnvVars: []string{"MXLRC_TLS_REDIRECT_HTTP"}, Criticality: Critical, Editable: true},
	{Path: "server.tls.self_signed_hosts", Section: "server", Type: TypeStringSlice, EnvVars: []string{"MXLRC_TLS_SELF_SIGNED_HOSTS"}, Criticality: Critical, Editable: true},

	// [providers]
	{Path: "providers.primary", Section: "providers", Type: TypeString, EnvVars: []string{"MXLRC_PROVIDER_PRIMARY"}, Criticality: Caution, Editable: true},
	{Path: "providers.disabled", Section: "providers", Type: TypeStringSlice, EnvVars: []string{"MXLRC_PROVIDERS_DISABLED"}, Criticality: Caution, Editable: true},
	{Path: "providers.mode", Section: "providers", Type: TypeString, EnvVars: []string{"MXLRC_PROVIDERS_MODE"}, Criticality: Safe, Editable: true},
	{Path: "providers.race_wait_seconds", Section: "providers", Type: TypeInt, EnvVars: []string{"MXLRC_PROVIDERS_RACE_WAIT_SECONDS"}, Criticality: Safe, Editable: true},
	{Path: "providers.fallback_order", Section: "providers", Type: TypeStringSlice, EnvVars: []string{"MXLRC_PROVIDERS_FALLBACK_ORDER"}, Criticality: Caution, Editable: true},

	// [verification]
	{Path: "verification.enabled", Section: "verification", Type: TypeBool, EnvVars: []string{"MXLRC_VERIFICATION_ENABLED"}, Criticality: Safe, Editable: true},
	{Path: "verification.whisper_url", Section: "verification", Type: TypeString, EnvVars: []string{"MXLRC_VERIFICATION_WHISPER_URL", "MXLRC_WHISPER_URL"}, Criticality: Caution, Editable: true},
	{Path: "verification.ffmpeg_path", Section: "verification", Type: TypeString, EnvVars: []string{"MXLRC_VERIFICATION_FFMPEG_PATH"}, Criticality: Caution, Editable: true},
	{Path: "verification.sample_duration_seconds", Section: "verification", Type: TypeInt, EnvVars: []string{"MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS", "MXLRC_VERIFICATION_SAMPLE_DURATION"}, Criticality: Safe, Editable: true},
	{Path: "verification.min_confidence", Section: "verification", Type: TypeFloat64, EnvVars: []string{"MXLRC_VERIFICATION_MIN_CONFIDENCE"}, Criticality: Safe, Editable: true},
	{Path: "verification.min_similarity", Section: "verification", Type: TypeFloat64, EnvVars: []string{"MXLRC_VERIFICATION_MIN_SIMILARITY"}, Criticality: Safe, Editable: true},

	// [instrumental_detector]
	{Path: "instrumental_detector.enabled", Section: "instrumental_detector", Type: TypeBool, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_ENABLED"}, Criticality: Safe, Editable: true},
	{Path: "instrumental_detector.classifier_url", Section: "instrumental_detector", Type: TypeString, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL"}, Criticality: Caution, Editable: true},
	{Path: "instrumental_detector.ffmpeg_path", Section: "instrumental_detector", Type: TypeString, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH"}, Criticality: Caution, Editable: true},
	{Path: "instrumental_detector.sample_duration_seconds", Section: "instrumental_detector", Type: TypeInt, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS"}, Criticality: Safe, Editable: true},
	{Path: "instrumental_detector.min_confidence", Section: "instrumental_detector", Type: TypeFloat64, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE"}, Criticality: Safe, Editable: true},
	{Path: "instrumental_detector.instrumental_classes", Section: "instrumental_detector", Type: TypeStringSlice, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_CLASSES"}, Criticality: Safe, Editable: true},
	{Path: "instrumental_detector.cooldown_seconds", Section: "instrumental_detector", Type: TypeInt, EnvVars: []string{"MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS"}, Criticality: Safe, Editable: true},

	// [enrichment]
	{Path: "enrichment.enabled", Section: "enrichment", Type: TypeBool, EnvVars: []string{"MXLRC_ENRICHMENT_ENABLED"}, Criticality: Safe, Editable: true},

	// [guard]
	{Path: "guard.accepted_scripts", Section: "guard", Type: TypeStringSlice, EnvVars: []string{"MXLRC_GUARD_ACCEPTED_SCRIPTS"}, Criticality: Safe, Editable: true},
	{Path: "guard.script_guard_threshold", Section: "guard", Type: TypeFloat64, EnvVars: []string{"MXLRC_GUARD_THRESHOLD"}, Criticality: Safe, Editable: true},

	// [queue]
	{Path: "queue.randomize", Section: "queue", Type: TypeBool, EnvVars: []string{"MXLRC_QUEUE_RANDOMIZE"}, Criticality: Safe, Editable: true},

	// [watcher]
	{Path: "watcher.enabled", Section: "watcher", Type: TypeBool, EnvVars: []string{"MXLRCGO_WATCH_ENABLED"}, Criticality: Safe, Editable: true},
	{Path: "watcher.debounce_ms", Section: "watcher", Type: TypeInt, EnvVars: []string{"MXLRCGO_WATCH_DEBOUNCE_MS"}, Criticality: Safe, Editable: true},
	{Path: "watcher.max_dirs", Section: "watcher", Type: TypeInt, EnvVars: []string{"MXLRCGO_WATCH_MAX_DIRS"}, Criticality: Safe, Editable: true},

	// [logging]
	{Path: "logging.level", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_LEVEL"}, Criticality: Safe, Editable: true},
	{Path: "logging.format", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_FORMAT"}, Criticality: Safe, Editable: true},
	{Path: "logging.file", Section: "logging", Type: TypeString, EnvVars: []string{"MXLRC_LOG_FILE"}, Criticality: Safe, Editable: true},
	{Path: "logging.max_size_mb", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_SIZE_MB"}, Criticality: Safe, Editable: true},
	{Path: "logging.max_files", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_FILES"}, Criticality: Safe, Editable: true},
	{Path: "logging.max_age_days", Section: "logging", Type: TypeInt, EnvVars: []string{"MXLRC_LOG_MAX_AGE_DAYS"}, Criticality: Safe, Editable: true},
	{Path: "logging.compress", Section: "logging", Type: TypeBool, EnvVars: []string{"MXLRC_LOG_COMPRESS"}, Criticality: Safe, Editable: true},
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
