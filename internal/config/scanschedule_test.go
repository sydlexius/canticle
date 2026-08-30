package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/schedule"
)

// writeScanScheduleConfig writes a TOML fixture and returns its path.
func writeScanScheduleConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestValidateScanSchedule(t *testing.T) {
	cases := []struct {
		name      string
		freq      string
		at        string
		day       string
		wantErr   bool
		wantPaths []string
	}{
		{name: "blank is legal (keeps the deprecated interval)", freq: ""},
		{name: "off needs no anchor", freq: "off"},
		{name: "hourly needs no anchor", freq: "hourly"},
		{name: "daily with a time", freq: "daily", at: "04:00"},
		{name: "weekly with a time and a day", freq: "weekly", at: "04:00", day: "sunday"},
		{
			name: "daily without a time", freq: "daily",
			wantErr: true, wantPaths: []string{"server.scan_schedule.at", "server.scan_schedule.frequency"},
		},
		{
			name: "daily with a malformed time", freq: "daily", at: "4am",
			wantErr: true, wantPaths: []string{"server.scan_schedule.at"},
		},
		{
			name: "weekly without a day", freq: "weekly", at: "04:00",
			wantErr: true, wantPaths: []string{"server.scan_schedule.day"},
		},
		{
			name: "weekly with a bogus day", freq: "weekly", at: "04:00", day: "someday",
			wantErr: true, wantPaths: []string{"server.scan_schedule.day"},
		},
		{
			name: "unknown frequency", freq: "fortnightly",
			wantErr: true, wantPaths: []string{"server.scan_schedule.frequency"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{}
			cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: tc.freq, At: tc.at, Day: tc.day}
			err := ValidateScanSchedule(cfg)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("ValidateScanSchedule: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("ValidateScanSchedule = nil; want an error")
			}
			// The message must NAME the offending key. An operator reading a
			// startup failure needs the line to edit, not just "invalid".
			for _, p := range tc.wantPaths {
				if !strings.Contains(err.Error(), p) {
					t.Errorf("error %q does not name %q", err, p)
				}
			}
		})
	}
}

// TestResolveScanScheduleLegacyFallback is the migration guarantee: with no
// [server.scan_schedule], the deprecated key still drives the scheduler AND
// still walks at startup, so upgrading changes nothing for an existing config.
func TestResolveScanScheduleLegacyFallback(t *testing.T) {
	cfg := Config{}
	cfg.Server.ScanIntervalSeconds = 600

	got := ResolveScanSchedule(cfg, nil)
	if got.Frequency != schedule.FrequencyInterval {
		t.Errorf("frequency = %q; want %q", got.Frequency, schedule.FrequencyInterval)
	}
	if got.Interval != 600*time.Second {
		t.Errorf("interval = %v; want 10m", got.Interval)
	}
	if !got.ScanOnStart {
		t.Error("legacy mode must keep its historical startup walk")
	}

	// The CLI flag still outranks the config value on the legacy path.
	flag := 120
	if got := ResolveScanSchedule(cfg, &flag); got.Interval != 120*time.Second {
		t.Errorf("interval with --scan-interval = %v; want 2m", got.Interval)
	}

	// A zero interval remains "scan once and stop", which NextFire reports as
	// no next occurrence.
	cfg.Server.ScanIntervalSeconds = 0
	if _, ok := ResolveScanSchedule(cfg, nil).NextFire(time.Now()); ok {
		t.Error("a zero interval returned a next fire; want none (scan once)")
	}
}

func TestResolveScanScheduleWallClock(t *testing.T) {
	cfg := Config{}
	cfg.Server.ScanIntervalSeconds = defaultScanIntervalSeconds
	cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: "weekly", At: "04:30", Day: "sunday", ScanOnStart: true}

	got := ResolveScanSchedule(cfg, nil)
	if got.Frequency != schedule.FrequencyWeekly {
		t.Errorf("frequency = %q; want weekly", got.Frequency)
	}
	if got.At != (schedule.TimeOfDay{Hour: 4, Minute: 30}) {
		t.Errorf("at = %v; want 04:30", got.At)
	}
	if got.Day != time.Sunday {
		t.Errorf("day = %v; want Sunday", got.Day)
	}
	if !got.ScanOnStart {
		t.Error("scan_on_start was not carried through")
	}
}

// TestResolveScanScheduleWallClockDefaultsScanOnStartOff pins the decision at
// the heart of #726: under a wall-clock schedule a restart must NOT imply a
// full library walk.
func TestResolveScanScheduleWallClockDefaultsScanOnStartOff(t *testing.T) {
	cfg := Config{}
	cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: "daily", At: "04:00"}
	if ResolveScanSchedule(cfg, nil).ScanOnStart {
		t.Fatal("scan_on_start defaulted to true; a restart must not imply a full walk")
	}
}

// TestResolveScanScheduleBeatsTheDeprecatedInterval confirms precedence: with
// both configured, the schedule wins and the interval is not consulted.
func TestResolveScanScheduleBeatsTheDeprecatedInterval(t *testing.T) {
	cfg := Config{}
	cfg.Server.ScanIntervalSeconds = 60
	cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: "hourly"}

	got := ResolveScanSchedule(cfg, nil)
	if got.Frequency != schedule.FrequencyHourly {
		t.Fatalf("frequency = %q; want hourly (the schedule outranks the interval)", got.Frequency)
	}
	if got.Interval != 0 {
		t.Errorf("interval = %v; want 0 (the deprecated value must not leak through)", got.Interval)
	}
}

func TestNormalizeScanSchedule(t *testing.T) {
	cfg := Config{}
	cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: "  DAILY ", At: " 04:00 ", Day: " Sunday "}
	normalizeScanSchedule(&cfg)
	got := cfg.Server.ScanSchedule
	if got.Frequency != "daily" || got.Day != "sunday" || got.At != "04:00" {
		t.Fatalf("normalized = %+v; want daily/sunday/04:00", got)
	}
	// Normalizing must not widen what is legal.
	if err := ValidateScanSchedule(cfg); err != nil {
		t.Fatalf("normalized config failed validation: %v", err)
	}
}

// TestScanScheduleLoadsFromFile exercises the real loader end to end, which is
// what proves the TOML tags, the normalizer and the cross-field check are all
// actually wired into Load rather than merely present.
func TestScanScheduleLoadsFromFile(t *testing.T) {
	isolateEnv(t)
	path := writeScanScheduleConfig(t, `
[server]
addr = "127.0.0.1:3876"

[server.scan_schedule]
frequency = "Weekly"
at = "04:00"
day = "Sunday"
scan_on_start = true
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	got := cfg.Server.ScanSchedule
	if got.Frequency != "weekly" || got.Day != "sunday" || got.At != "04:00" || !got.ScanOnStart {
		t.Fatalf("loaded schedule = %+v; want weekly/sunday/04:00/true", got)
	}
}

// TestScanScheduleLoadRejectsAMissingAnchor confirms the cross-field rule is a
// STARTUP ERROR, not a warning: a schedule with no anchor would otherwise fire
// at midnight, a time nobody chose.
func TestScanScheduleLoadRejectsAMissingAnchor(t *testing.T) {
	isolateEnv(t)
	path := writeScanScheduleConfig(t, `
[server.scan_schedule]
frequency = "daily"
`)
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted a daily schedule with no time; want an error")
	}
}

// TestScanScheduleEnvOverrides covers the env lane, including the
// warn-and-keep-current contract for an unusable value: a typo must never
// silently move when the library is walked.
func TestScanScheduleEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("MXLRC_SCAN_SCHEDULE", "daily")
	t.Setenv("MXLRC_SCAN_SCHEDULE_AT", "05:15")
	t.Setenv("MXLRC_SCAN_SCHEDULE_ON_START", "true")

	cfg := defaults()
	applied := map[string]bool{}
	applyEnvOverrides(&cfg, applied)
	if cfg.Server.ScanSchedule.Frequency != "daily" || cfg.Server.ScanSchedule.At != "05:15" {
		t.Fatalf("env overrides = %+v; want daily/05:15", cfg.Server.ScanSchedule)
	}
	if !cfg.Server.ScanSchedule.ScanOnStart {
		t.Error("MXLRC_SCAN_SCHEDULE_ON_START did not apply")
	}

	t.Setenv("MXLRC_SCAN_SCHEDULE_AT", "half four")
	t.Setenv("MXLRC_SCAN_SCHEDULE_DAY", "someday")
	cfg2 := defaults()
	cfg2.Server.ScanSchedule.At = "04:00"
	applyEnvOverrides(&cfg2, map[string]bool{})
	if cfg2.Server.ScanSchedule.At != "04:00" {
		t.Errorf("at = %q; an invalid env value must keep the current one", cfg2.Server.ScanSchedule.At)
	}
	if cfg2.Server.ScanSchedule.Day != "" {
		t.Errorf("day = %q; an invalid env value must keep the current one", cfg2.Server.ScanSchedule.Day)
	}
}

// TestScanScheduleValidateAndSet exercises the settings/CLI write path, which
// must accept exactly what boot accepts.
func TestScanScheduleValidateAndSet(t *testing.T) {
	ok := []struct{ path, value string }{
		{"server.scan_schedule.frequency", "daily"},
		{"server.scan_schedule.frequency", " Weekly "},
		{"server.scan_schedule.day", "sunday"},
		{"server.scan_schedule.at", "04:00"},
		{"server.scan_schedule.at", ""},
		{"server.scan_schedule.scan_on_start", "true"},
	}
	for _, tc := range ok {
		if err := ValidateAndSet(tc.path, tc.value); err != nil {
			t.Errorf("ValidateAndSet(%s, %q) = %v; want nil", tc.path, tc.value, err)
		}
	}
	bad := []struct{ path, value string }{
		{"server.scan_schedule.frequency", "fortnightly"},
		{"server.scan_schedule.day", "someday"},
		{"server.scan_schedule.at", "25:00"},
		{"server.scan_schedule.at", "4:00"},
		{"server.scan_schedule.scan_on_start", "maybe"},
	}
	for _, tc := range bad {
		if err := ValidateAndSet(tc.path, tc.value); err == nil {
			t.Errorf("ValidateAndSet(%s, %q) = nil; want an error", tc.path, tc.value)
		}
	}
}

// TestScanScheduleRegistryEntries asserts the four keys are registered with the
// section, type and env var the rest of the stack expects.
func TestScanScheduleRegistryEntries(t *testing.T) {
	want := map[string]struct {
		ftype FieldType
		env   string
	}{
		"server.scan_schedule.frequency":     {TypeString, "MXLRC_SCAN_SCHEDULE"},
		"server.scan_schedule.at":            {TypeString, "MXLRC_SCAN_SCHEDULE_AT"},
		"server.scan_schedule.day":           {TypeString, "MXLRC_SCAN_SCHEDULE_DAY"},
		"server.scan_schedule.scan_on_start": {TypeBool, "MXLRC_SCAN_SCHEDULE_ON_START"},
	}
	for path, w := range want {
		f, ok := FieldByPath(path)
		if !ok {
			t.Errorf("%s is not in the registry", path)
			continue
		}
		if f.Section != "server" {
			t.Errorf("%s section = %q; want server", path, f.Section)
		}
		if f.Type != w.ftype {
			t.Errorf("%s type = %v; want %v", path, f.Type, w.ftype)
		}
		if len(f.EnvVars) == 0 || f.EnvVars[0] != w.env {
			t.Errorf("%s env = %v; want %s", path, f.EnvVars, w.env)
		}
		if !f.Editable {
			t.Errorf("%s should be editable", path)
		}
	}
}

// TestScanScheduleEnumsMatchTheScheduler is the drift guard between the values
// the settings dropdown offers and the values the scheduler implements.
func TestScanScheduleEnumsMatchTheScheduler(t *testing.T) {
	freqs := AllowedValues("server.scan_schedule.frequency")
	if len(freqs) != len(schedule.FrequencyNames()) {
		t.Fatalf("frequency enum = %v; want %v", freqs, schedule.FrequencyNames())
	}
	for _, f := range freqs {
		cfg := Config{}
		cfg.Server.ScanSchedule = ScanScheduleConfig{Frequency: f, At: "04:00", Day: "sunday"}
		if err := ValidateScanSchedule(cfg); err != nil {
			t.Errorf("offered frequency %q is rejected by the loader: %v", f, err)
		}
	}
	for _, d := range AllowedValues("server.scan_schedule.day") {
		if _, err := schedule.ParseWeekday(d); err != nil {
			t.Errorf("offered day %q does not parse: %v", d, err)
		}
	}
}
