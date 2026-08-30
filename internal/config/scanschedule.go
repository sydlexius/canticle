package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/schedule"
)

// normalizeScanSchedule lowercases and trims the two vocabulary fields so the
// rest of the stack compares against one canonical spelling. It does NOT decide
// legality -- ValidateScanSchedule does, after env overrides land, so the
// verdict is the same whichever source set the value.
func normalizeScanSchedule(cfg *Config) {
	s := &cfg.Server.ScanSchedule
	s.Frequency = strings.ToLower(strings.TrimSpace(s.Frequency))
	s.Day = strings.ToLower(strings.TrimSpace(s.Day))
	s.At = strings.TrimSpace(s.At)
}

// ValidateScanSchedule fails fast on a [server.scan_schedule] that names a
// frequency without the anchor that frequency needs (#726). It is the single
// source of truth shared by the loader and the settings write path, so the UI
// rejects exactly what boot rejects -- the arrangement ValidateTLSSelection
// already uses for the cert/key pair.
//
// An empty frequency is legal and means "not configured": the deprecated
// server.scan_interval_seconds still drives the scheduler. That is deliberate.
// A config key an operator currently sets must not stop working silently; it
// keeps working and says so once, in a log line, at resolution time.
func ValidateScanSchedule(cfg Config) error {
	s := cfg.Server.ScanSchedule
	freq := strings.ToLower(strings.TrimSpace(s.Frequency))
	switch freq {
	case "":
		return nil
	case string(schedule.FrequencyOff), string(schedule.FrequencyHourly):
		return nil
	case string(schedule.FrequencyDaily), string(schedule.FrequencyWeekly):
		if strings.TrimSpace(s.At) == "" {
			return fmt.Errorf("config: server.scan_schedule.at is required when server.scan_schedule.frequency is %q (e.g. at = \"04:00\")", freq)
		}
		if _, err := schedule.ParseTimeOfDay(s.At); err != nil {
			return fmt.Errorf("config: server.scan_schedule.at: %w", err)
		}
		if freq == string(schedule.FrequencyWeekly) {
			if strings.TrimSpace(s.Day) == "" {
				return fmt.Errorf("config: server.scan_schedule.day is required when server.scan_schedule.frequency is \"weekly\" (e.g. day = \"sunday\")")
			}
			if _, err := schedule.ParseWeekday(s.Day); err != nil {
				return fmt.Errorf("config: server.scan_schedule.day: %w", err)
			}
		}
		return nil
	default:
		return fmt.Errorf("config: unsupported server.scan_schedule.frequency %q (supported: %s)", s.Frequency, strings.Join(schedule.FrequencyNames(), ", "))
	}
}

// ResolveScanSchedule turns config plus the CLI interval override into the
// schedule the scheduler runs. cliIntervalSeconds is nil when --scan-interval
// was not passed.
//
// PRECEDENCE AND THE MIGRATION DECISION. [server.scan_schedule] wins when its
// frequency is set. Otherwise the deprecated interval path is taken --
// --scan-interval over server.scan_interval_seconds, exactly as before -- and a
// single deprecation warning is logged. Nothing an operator has configured
// today changes behavior on upgrade, and nothing they have configured is
// silently discarded either: the one case where both are set logs that the
// schedule is winning, so a stale interval key cannot look effective.
//
// Caller must have validated cfg (ValidateScanSchedule runs at load), so the
// anchors parse here; a parse failure would mean a schedule that fires at a
// time nobody chose, so it degrades to "off" and says so rather than guessing.
func ResolveScanSchedule(cfg Config, cliIntervalSeconds *int) schedule.Schedule {
	s := cfg.Server.ScanSchedule
	freq := strings.ToLower(strings.TrimSpace(s.Frequency))
	if freq == "" {
		seconds := cfg.Server.ScanIntervalSeconds
		if cliIntervalSeconds != nil {
			seconds = *cliIntervalSeconds
		}
		slog.Warn("server.scan_interval_seconds is deprecated and does not survive a restart; configure [server.scan_schedule] instead (see docs/CONFIGURATION.md)",
			"interval_seconds", seconds)
		return schedule.Schedule{
			Frequency: schedule.FrequencyInterval,
			Interval:  time.Duration(seconds) * time.Second,
			// The legacy path always walked at startup; keeping that is what
			// makes it a compatibility path rather than a behavior change.
			ScanOnStart: true,
		}
	}
	if cliIntervalSeconds != nil || cfg.Server.ScanIntervalSeconds != defaultScanIntervalSeconds {
		slog.Warn("server.scan_interval_seconds is ignored because [server.scan_schedule] is configured; remove the deprecated key",
			"schedule", freq)
	}
	out := schedule.Schedule{Frequency: schedule.Frequency(freq), ScanOnStart: s.ScanOnStart}
	if freq == string(schedule.FrequencyDaily) || freq == string(schedule.FrequencyWeekly) {
		at, err := schedule.ParseTimeOfDay(s.At)
		if err != nil {
			slog.Error("server.scan_schedule.at is unparsable; disabling the periodic scan", "error", err)
			return schedule.Schedule{Frequency: schedule.FrequencyOff, ScanOnStart: s.ScanOnStart}
		}
		out.At = at
	}
	if freq == string(schedule.FrequencyWeekly) {
		day, err := schedule.ParseWeekday(s.Day)
		if err != nil {
			slog.Error("server.scan_schedule.day is unparsable; disabling the periodic scan", "error", err)
			return schedule.Schedule{Frequency: schedule.FrequencyOff, ScanOnStart: s.ScanOnStart}
		}
		out.Day = day
	}
	return out
}

// applyScanScheduleEnv overlays the [server.scan_schedule] env vars, following
// the file's warn-and-keep-current style: an unusable value never silently
// changes when the library is walked.
func applyScanScheduleEnv(cfg *Config, applied map[string]bool) {
	if v := os.Getenv("MXLRC_SCAN_SCHEDULE"); v != "" {
		cfg.Server.ScanSchedule.Frequency = v
		applied["server.scan_schedule.frequency"] = true
	}
	if v := os.Getenv("MXLRC_SCAN_SCHEDULE_AT"); v != "" {
		if _, err := schedule.ParseTimeOfDay(v); err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SCAN_SCHEDULE_AT", "error", err) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.ScanSchedule.At = v
			applied["server.scan_schedule.at"] = true
		}
	}
	if v := os.Getenv("MXLRC_SCAN_SCHEDULE_DAY"); v != "" {
		if _, err := schedule.ParseWeekday(v); err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SCAN_SCHEDULE_DAY", "error", err) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.ScanSchedule.Day = v
			applied["server.scan_schedule.day"] = true
		}
	}
	if v := os.Getenv("MXLRC_SCAN_SCHEDULE_ON_START"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			slog.Warn("env var is invalid; using current value", "var", "MXLRC_SCAN_SCHEDULE_ON_START", "current", cfg.Server.ScanSchedule.ScanOnStart) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); no log-injection vector since slog escapes values
		} else {
			cfg.Server.ScanSchedule.ScanOnStart = b
			applied["server.scan_schedule.scan_on_start"] = true
		}
	}
}
