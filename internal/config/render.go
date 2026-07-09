package config

import (
	"fmt"
	"log/slog"
	"strings"
)

// FormatConfigText returns a human-readable TOML-style snapshot of cfg with
// sensitive fields redacted. envSrc and cliSrc are best-effort source-hint
// maps keyed by dotted config field path (e.g. "api.token"); a nil map means
// no hints for that source. Fields present in cliSrc are annotated "(cli)";
// fields present in envSrc are annotated "(env)".
func FormatConfigText(cfg Config, envSrc, cliSrc map[string]bool) string {
	var b strings.Builder

	ann := func(path string) string {
		if cliSrc[path] {
			return " (cli)"
		}
		if envSrc[path] {
			return " (env)"
		}
		return ""
	}

	// p is a write helper; strings.Builder writes never error.
	p := func(format string, args ...any) {
		_, _ = fmt.Fprintf(&b, format, args...)
	}

	strVal := func(path, val string) string {
		if val == "" && IsSensitiveConfigKey(path) {
			return "(not set)"
		}
		return RedactValue(path, val)
	}

	sliceVal := func(path string, vals []string) string {
		if IsSensitiveConfigKey(path) {
			if len(vals) > 0 {
				return "[REDACTED]"
			}
			return "[]"
		}
		if len(vals) == 0 {
			return "[]"
		}
		return "[" + strings.Join(vals, ", ") + "]"
	}

	// [api]
	p("[api]\n")
	p("token = %s%s\n", strVal("api.token", cfg.API.Token), ann("api.token"))
	p("cooldown = %d%s\n", cfg.API.Cooldown, ann("api.cooldown"))
	p("circuit_open_duration = %d%s\n", cfg.API.CircuitOpenDuration, ann("api.circuit_open_duration"))
	p("circuit_backoff_base_seconds = %d%s\n", cfg.API.CircuitBackoffBase, ann("api.circuit_backoff_base_seconds"))
	p("miss_backoff_base_hours = %d%s\n", cfg.API.MissBackoffBaseHours, ann("api.miss_backoff_base_hours"))
	p("miss_backoff_cap_hours = %d%s\n", cfg.API.MissBackoffCapHours, ann("api.miss_backoff_cap_hours"))
	p("max_miss_attempts = %d%s\n", cfg.API.MaxMissAttempts, ann("api.max_miss_attempts"))
	p("\n")

	// [output]
	p("[output]\n")
	p("dir = %s%s\n", cfg.Output.Dir, ann("output.dir"))
	p("embedded_lyrics = %s%s\n", cfg.Output.EmbeddedLyrics, ann("output.embedded_lyrics"))
	p("bilingual_output = %t%s\n", cfg.Output.BilingualOutput, ann("output.bilingual_output"))
	p("\n")

	// [db]
	p("[db]\n")
	p("path = %s%s\n", cfg.DB.Path, ann("db.path"))
	p("\n")

	// [server]
	p("[server]\n")
	p("addr = %s%s\n", cfg.Server.Addr, ann("server.addr"))
	p("webhook_api_keys = %s%s\n", sliceVal("server.webhook_api_keys", cfg.Server.WebhookAPIKeys), ann("server.webhook_api_keys"))
	p("scan_interval_seconds = %d%s\n", cfg.Server.ScanIntervalSeconds, ann("server.scan_interval_seconds"))
	p("sweep_interval_seconds = %d%s\n", cfg.Server.SweepIntervalSeconds, ann("server.sweep_interval_seconds"))
	p("work_interval_seconds = %d%s\n", cfg.Server.WorkIntervalSeconds, ann("server.work_interval_seconds"))
	p("\n")

	// [providers]
	p("[providers]\n")
	p("primary = %s%s\n", cfg.Providers.Primary, ann("providers.primary"))
	p("disabled = %s%s\n", sliceVal("providers.disabled", cfg.Providers.Disabled), ann("providers.disabled"))
	p("mode = %s%s\n", cfg.Providers.Mode, ann("providers.mode"))
	p("race_wait_seconds = %d%s\n", cfg.Providers.RaceWaitSeconds, ann("providers.race_wait_seconds"))
	p("fallback_order = %s%s\n", sliceVal("providers.fallback_order", cfg.Providers.FallbackOrder), ann("providers.fallback_order"))
	p("\n")

	// [verification]
	p("[verification]\n")
	p("enabled = %t%s\n", cfg.Verification.Enabled, ann("verification.enabled"))
	p("whisper_url = %s%s\n", cfg.Verification.WhisperURL, ann("verification.whisper_url"))
	p("ffmpeg_path = %s%s\n", cfg.Verification.FFmpegPath, ann("verification.ffmpeg_path"))
	p("sample_duration_seconds = %d%s\n", cfg.Verification.SampleDurationSeconds, ann("verification.sample_duration_seconds"))
	p("min_confidence = %g%s\n", cfg.Verification.MinConfidence, ann("verification.min_confidence"))
	p("min_similarity = %g%s\n", cfg.Verification.MinSimilarity, ann("verification.min_similarity"))
	p("\n")

	// [instrumental_detector]
	p("[instrumental_detector]\n")
	p("enabled = %t%s\n", cfg.InstrumentalDetector.Enabled, ann("instrumental_detector.enabled"))
	p("classifier_url = %s%s\n", cfg.InstrumentalDetector.ClassifierURL, ann("instrumental_detector.classifier_url"))
	p("ffmpeg_path = %s%s\n", cfg.InstrumentalDetector.FFmpegPath, ann("instrumental_detector.ffmpeg_path"))
	p("sample_duration_seconds = %d%s\n", cfg.InstrumentalDetector.SampleDurationSeconds, ann("instrumental_detector.sample_duration_seconds"))
	p("min_confidence = %g%s\n", cfg.InstrumentalDetector.MinConfidence, ann("instrumental_detector.min_confidence"))
	p("instrumental_classes = %s%s\n", sliceVal("instrumental_detector.instrumental_classes", cfg.InstrumentalDetector.InstrumentalClasses), ann("instrumental_detector.instrumental_classes"))
	p("vocal_classes = %s%s\n", sliceVal("instrumental_detector.vocal_classes", cfg.InstrumentalDetector.VocalClasses), ann("instrumental_detector.vocal_classes"))
	p("vocal_max_confidence = %g%s\n", cfg.InstrumentalDetector.VocalMaxConfidence, ann("instrumental_detector.vocal_max_confidence"))
	p("speech_classes = %s%s\n", sliceVal("instrumental_detector.speech_classes", cfg.InstrumentalDetector.SpeechClasses), ann("instrumental_detector.speech_classes"))
	p("speech_max_confidence = %g%s\n", cfg.InstrumentalDetector.SpeechMaxConfidence, ann("instrumental_detector.speech_max_confidence"))
	p("spread_samples = %d%s\n", cfg.InstrumentalDetector.SpreadSamples, ann("instrumental_detector.spread_samples"))
	p("ffprobe_path = %s%s\n", cfg.InstrumentalDetector.FFprobePath, ann("instrumental_detector.ffprobe_path"))
	p("cooldown_seconds = %d%s\n", cfg.InstrumentalDetector.CooldownSeconds, ann("instrumental_detector.cooldown_seconds"))
	p("\n")

	// [enrichment]
	p("[enrichment]\n")
	p("enabled = %t%s\n", cfg.Enrichment.Enabled, ann("enrichment.enabled"))
	p("\n")

	// [realign]
	p("[realign]\n")
	p("enabled = %t%s\n", cfg.Realign.Enabled, ann("realign.enabled"))
	p("on_scan = %t%s\n", cfg.Realign.OnScan, ann("realign.on_scan"))
	p("require_provenance = %t%s\n", cfg.Realign.RequireProvenance, ann("realign.require_provenance"))
	p("cross_directory = %t%s\n", cfg.Realign.CrossDirectory, ann("realign.cross_directory"))
	p("identity_keys = %s%s\n", sliceVal("realign.identity_keys", cfg.Realign.IdentityKeys), ann("realign.identity_keys"))
	p("min_confidence = %g%s\n", cfg.Realign.MinConfidence, ann("realign.min_confidence"))
	p("\n")

	// [guard]
	p("[guard]\n")
	p("accepted_scripts = %s%s\n", sliceVal("guard.accepted_scripts", cfg.Guard.AcceptedScripts), ann("guard.accepted_scripts"))
	p("script_guard_threshold = %g%s\n", cfg.Guard.Threshold, ann("guard.script_guard_threshold"))
	p("\n")

	// [queue]
	p("[queue]\n")
	p("randomize = %t%s\n", cfg.Queue.Randomize, ann("queue.randomize"))
	p("\n")

	// [watcher]
	p("[watcher]\n")
	p("enabled = %t%s\n", cfg.Watcher.Enabled, ann("watcher.enabled"))
	p("debounce_ms = %d%s\n", cfg.Watcher.DebounceMS, ann("watcher.debounce_ms"))
	p("max_dirs = %d%s\n", cfg.Watcher.MaxDirs, ann("watcher.max_dirs"))
	p("\n")

	// [secrets]
	// Only the key-file PATH is shown; the key bytes it contains are never read
	// into Config and never logged.
	p("[secrets]\n")
	p("key_file = %s%s\n", cfg.Secrets.KeyFile, ann("secrets.key_file"))
	p("\n")

	// [logging]
	p("[logging]\n")
	p("level = %s%s\n", cfg.Logging.Level, ann("logging.level"))
	p("format = %s%s\n", cfg.Logging.Format, ann("logging.format"))
	p("file = %s%s\n", cfg.Logging.File, ann("logging.file"))
	p("max_size_mb = %d%s\n", cfg.Logging.MaxSizeMB, ann("logging.max_size_mb"))
	p("max_files = %d%s\n", cfg.Logging.MaxFiles, ann("logging.max_files"))
	p("max_age_days = %d%s\n", cfg.Logging.MaxAgeDays, ann("logging.max_age_days"))
	p("compress = %t%s\n", cfg.Logging.Compress, ann("logging.compress"))

	return b.String()
}

// ConfigToSlogAttrs returns a slice of slog.Attr groups representing cfg with
// sensitive fields redacted. Each element is a named group corresponding to a
// config section. Source hints follow the same convention as FormatConfigText:
// nil maps mean no hints for that source.
func ConfigToSlogAttrs(cfg Config, envSrc, cliSrc map[string]bool) []slog.Attr {
	ann := func(path string) string {
		if cliSrc[path] {
			return " (cli)"
		}
		if envSrc[path] {
			return " (env)"
		}
		return ""
	}

	// src returns the bare source token ("cli"/"env"/"") for a path, mirroring
	// ann's precedence. Typed slog attrs (int/bool/float) keep their native type
	// and emit this as a sibling "<key>_source" attr instead of inlining the
	// annotation, so the value key's type never varies across runs.
	src := func(path string) string {
		if cliSrc[path] {
			return "cli"
		}
		if envSrc[path] {
			return "env"
		}
		return ""
	}

	// strAttr renders a string field, redacting sensitive values and appending
	// the source annotation when present. String values do not type-vary, so the
	// annotation stays inline (no sibling "_source" attr).
	strAttr := func(key, path, val string) []slog.Attr {
		display := RedactValue(path, val)
		if a := ann(path); a != "" {
			display += a
		}
		return []slog.Attr{slog.String(key, display)}
	}

	// intAttr renders an integer field, preserving the slog.Int type. When a
	// source applies, a sibling "<key>_source" attr carries the bare source.
	intAttr := func(key, path string, val int) []slog.Attr {
		attrs := []slog.Attr{slog.Int(key, val)}
		if s := src(path); s != "" {
			attrs = append(attrs, slog.String(key+"_source", s))
		}
		return attrs
	}

	// boolAttr renders a boolean field, preserving the slog.Bool type. When a
	// source applies, a sibling "<key>_source" attr carries the bare source.
	boolAttr := func(key, path string, val bool) []slog.Attr {
		attrs := []slog.Attr{slog.Bool(key, val)}
		if s := src(path); s != "" {
			attrs = append(attrs, slog.String(key+"_source", s))
		}
		return attrs
	}

	// floatAttr renders a float64 field, preserving the slog.Float64 type. When
	// a source applies, a sibling "<key>_source" attr carries the bare source.
	floatAttr := func(key, path string, val float64) []slog.Attr {
		attrs := []slog.Attr{slog.Float64(key, val)}
		if s := src(path); s != "" {
			attrs = append(attrs, slog.String(key+"_source", s))
		}
		return attrs
	}

	// sliceAttr renders a slice field, redacting sensitive values. An empty
	// slice renders as "[]" (matching FormatConfigText's sliceVal) for both
	// sensitive and non-sensitive paths, since an empty sensitive slice has
	// nothing to redact.
	sliceAttr := func(key, path string, vals []string) []slog.Attr {
		var display string
		switch {
		case len(vals) == 0:
			display = "[]"
		case IsSensitiveConfigKey(path):
			display = "[REDACTED]"
		default:
			display = strings.Join(vals, ", ")
		}
		if a := ann(path); a != "" {
			display += a
		}
		return []slog.Attr{slog.String(key, display)}
	}

	// tokenAttr handles the api.token field specially so empty shows as empty
	// rather than "[REDACTED]" (lets caller distinguish unset from redacted).
	tokenAttr := func() []slog.Attr {
		val := ""
		if cfg.API.Token != "" {
			val = "[REDACTED]"
		}
		if a := ann("api.token"); a != "" {
			val += a
		}
		return []slog.Attr{slog.String("token", val)}
	}

	// group flattens the per-field []slog.Attr results into a single named group.
	group := func(name string, fields ...[]slog.Attr) slog.Attr {
		var attrs []any
		for _, f := range fields {
			for _, a := range f {
				attrs = append(attrs, a)
			}
		}
		return slog.Group(name, attrs...)
	}

	return []slog.Attr{
		group("api",
			tokenAttr(),
			intAttr("cooldown", "api.cooldown", cfg.API.Cooldown),
			intAttr("circuit_open_duration", "api.circuit_open_duration", cfg.API.CircuitOpenDuration),
			intAttr("circuit_backoff_base_seconds", "api.circuit_backoff_base_seconds", cfg.API.CircuitBackoffBase),
			intAttr("miss_backoff_base_hours", "api.miss_backoff_base_hours", cfg.API.MissBackoffBaseHours),
			intAttr("miss_backoff_cap_hours", "api.miss_backoff_cap_hours", cfg.API.MissBackoffCapHours),
			intAttr("max_miss_attempts", "api.max_miss_attempts", cfg.API.MaxMissAttempts),
		),
		group("output",
			strAttr("dir", "output.dir", cfg.Output.Dir),
			strAttr("embedded_lyrics", "output.embedded_lyrics", cfg.Output.EmbeddedLyrics),
			boolAttr("bilingual_output", "output.bilingual_output", cfg.Output.BilingualOutput),
		),
		group("db",
			strAttr("path", "db.path", cfg.DB.Path),
		),
		group("server",
			strAttr("addr", "server.addr", cfg.Server.Addr),
			sliceAttr("webhook_api_keys", "server.webhook_api_keys", cfg.Server.WebhookAPIKeys),
			intAttr("scan_interval_seconds", "server.scan_interval_seconds", cfg.Server.ScanIntervalSeconds),
			intAttr("sweep_interval_seconds", "server.sweep_interval_seconds", cfg.Server.SweepIntervalSeconds),
			intAttr("work_interval_seconds", "server.work_interval_seconds", cfg.Server.WorkIntervalSeconds),
		),
		group("providers",
			strAttr("primary", "providers.primary", cfg.Providers.Primary),
			sliceAttr("disabled", "providers.disabled", cfg.Providers.Disabled),
			strAttr("mode", "providers.mode", cfg.Providers.Mode),
			intAttr("race_wait_seconds", "providers.race_wait_seconds", cfg.Providers.RaceWaitSeconds),
			sliceAttr("fallback_order", "providers.fallback_order", cfg.Providers.FallbackOrder),
		),
		group("verification",
			boolAttr("enabled", "verification.enabled", cfg.Verification.Enabled),
			strAttr("whisper_url", "verification.whisper_url", cfg.Verification.WhisperURL),
			strAttr("ffmpeg_path", "verification.ffmpeg_path", cfg.Verification.FFmpegPath),
			intAttr("sample_duration_seconds", "verification.sample_duration_seconds", cfg.Verification.SampleDurationSeconds),
			floatAttr("min_confidence", "verification.min_confidence", cfg.Verification.MinConfidence),
			floatAttr("min_similarity", "verification.min_similarity", cfg.Verification.MinSimilarity),
		),
		group("instrumental_detector",
			boolAttr("enabled", "instrumental_detector.enabled", cfg.InstrumentalDetector.Enabled),
			strAttr("classifier_url", "instrumental_detector.classifier_url", cfg.InstrumentalDetector.ClassifierURL),
			strAttr("ffmpeg_path", "instrumental_detector.ffmpeg_path", cfg.InstrumentalDetector.FFmpegPath),
			intAttr("sample_duration_seconds", "instrumental_detector.sample_duration_seconds", cfg.InstrumentalDetector.SampleDurationSeconds),
			floatAttr("min_confidence", "instrumental_detector.min_confidence", cfg.InstrumentalDetector.MinConfidence),
			sliceAttr("instrumental_classes", "instrumental_detector.instrumental_classes", cfg.InstrumentalDetector.InstrumentalClasses),
			sliceAttr("vocal_classes", "instrumental_detector.vocal_classes", cfg.InstrumentalDetector.VocalClasses),
			floatAttr("vocal_max_confidence", "instrumental_detector.vocal_max_confidence", cfg.InstrumentalDetector.VocalMaxConfidence),
			sliceAttr("speech_classes", "instrumental_detector.speech_classes", cfg.InstrumentalDetector.SpeechClasses),
			floatAttr("speech_max_confidence", "instrumental_detector.speech_max_confidence", cfg.InstrumentalDetector.SpeechMaxConfidence),
			intAttr("spread_samples", "instrumental_detector.spread_samples", cfg.InstrumentalDetector.SpreadSamples),
			strAttr("ffprobe_path", "instrumental_detector.ffprobe_path", cfg.InstrumentalDetector.FFprobePath),
			intAttr("cooldown_seconds", "instrumental_detector.cooldown_seconds", cfg.InstrumentalDetector.CooldownSeconds),
		),
		group("enrichment",
			boolAttr("enabled", "enrichment.enabled", cfg.Enrichment.Enabled),
		),
		group("realign",
			boolAttr("enabled", "realign.enabled", cfg.Realign.Enabled),
			boolAttr("on_scan", "realign.on_scan", cfg.Realign.OnScan),
			boolAttr("require_provenance", "realign.require_provenance", cfg.Realign.RequireProvenance),
			boolAttr("cross_directory", "realign.cross_directory", cfg.Realign.CrossDirectory),
			sliceAttr("identity_keys", "realign.identity_keys", cfg.Realign.IdentityKeys),
			floatAttr("min_confidence", "realign.min_confidence", cfg.Realign.MinConfidence),
		),
		group("guard",
			sliceAttr("accepted_scripts", "guard.accepted_scripts", cfg.Guard.AcceptedScripts),
			floatAttr("script_guard_threshold", "guard.script_guard_threshold", cfg.Guard.Threshold),
		),
		group("queue",
			boolAttr("randomize", "queue.randomize", cfg.Queue.Randomize),
		),
		group("watcher",
			boolAttr("enabled", "watcher.enabled", cfg.Watcher.Enabled),
			intAttr("debounce_ms", "watcher.debounce_ms", cfg.Watcher.DebounceMS),
			intAttr("max_dirs", "watcher.max_dirs", cfg.Watcher.MaxDirs),
		),
		group("secrets",
			// Only the key-file path; key bytes are never read into Config.
			strAttr("key_file", "secrets.key_file", cfg.Secrets.KeyFile),
		),
		group("logging",
			strAttr("level", "logging.level", cfg.Logging.Level),
			strAttr("format", "logging.format", cfg.Logging.Format),
			strAttr("file", "logging.file", cfg.Logging.File),
			intAttr("max_size_mb", "logging.max_size_mb", cfg.Logging.MaxSizeMB),
			intAttr("max_files", "logging.max_files", cfg.Logging.MaxFiles),
			intAttr("max_age_days", "logging.max_age_days", cfg.Logging.MaxAgeDays),
			boolAttr("compress", "logging.compress", cfg.Logging.Compress),
		),
	}
}
