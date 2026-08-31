package commands

import (
	"slices"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/schedule"
)

// TestConfigGetSetScanSchedule covers the `config get` / `config set` arms for
// the new keys. A missing arm is invisible in normal use -- get renders blank
// and set silently reports success while changing nothing -- so each key is
// asserted through a set-then-get round trip rather than by inspection. It
// also asserts each key appears in configKeys(): configValue and
// setConfigValue handling a key is not enough for `config list` to show it,
// which is exactly the gap CodeRabbit found -- these four keys were fully
// wired into the get/set switches but absent from configKeys.
func TestConfigGetSetScanSchedule(t *testing.T) {
	cfg := config.Config{}
	cases := []struct{ key, set, want string }{
		{"server.scan_schedule.frequency", "weekly", "weekly"},
		{"server.scan_schedule.at", "04:30", "04:30"},
		{"server.scan_schedule.day", "sunday", "sunday"},
		{"server.scan_schedule.scan_on_start", "true", "true"},
	}
	for _, tc := range cases {
		if err := setConfigValue(&cfg, tc.key, tc.set); err != nil {
			t.Fatalf("setConfigValue(%s, %q): %v", tc.key, tc.set, err)
		}
		got, ok := configValue(cfg, tc.key)
		if !ok {
			t.Errorf("configValue(%s) reported the key as unknown", tc.key)
			continue
		}
		if got != tc.want {
			t.Errorf("configValue(%s) = %q; want %q", tc.key, got, tc.want)
		}
		if !slices.Contains(configKeys(), tc.key) {
			t.Errorf("configKeys missing %q (config list cannot show a key it does not enumerate)", tc.key)
		}
	}
	// The round trip must leave a config that actually boots.
	if err := config.ValidateScanSchedule(cfg); err != nil {
		t.Fatalf("config assembled via config set does not validate: %v", err)
	}
}

// TestConfigSetScanScheduleNormalizes confirms the CLI folds case and spacing
// the same way the loader does, so `config set ... Weekly` cannot write a value
// the next boot has to re-normalize.
func TestConfigSetScanScheduleNormalizes(t *testing.T) {
	cfg := config.Config{}
	if err := setConfigValue(&cfg, "server.scan_schedule.frequency", " Weekly "); err != nil {
		t.Fatalf("setConfigValue: %v", err)
	}
	if got := cfg.Server.ScanSchedule.Frequency; got != "weekly" {
		t.Errorf("frequency = %q; want %q", got, "weekly")
	}
	if err := setConfigValue(&cfg, "server.scan_schedule.day", " Sunday "); err != nil {
		t.Fatalf("setConfigValue day: %v", err)
	}
	if got := cfg.Server.ScanSchedule.Day; got != "sunday" {
		t.Errorf("day = %q; want %q", got, "sunday")
	}
}

// TestConfigSetScanScheduleRejectsBadValues confirms the CLI shares the
// loader's vocabulary rather than accepting anything and failing at boot.
func TestConfigSetScanScheduleRejectsBadValues(t *testing.T) {
	bad := []struct{ key, value string }{
		{"server.scan_schedule.frequency", "fortnightly"},
		{"server.scan_schedule.day", "someday"},
		{"server.scan_schedule.at", "25:00"},
		{"server.scan_schedule.at", "4pm"},
		{"server.scan_schedule.scan_on_start", "maybe"},
	}
	for _, tc := range bad {
		cfg := config.Config{}
		if err := setConfigValue(&cfg, tc.key, tc.value); err == nil {
			t.Errorf("setConfigValue(%s, %q) = nil; want an error", tc.key, tc.value)
		}
	}
}

// TestConfigSetScanScheduleFrequencyAcceptsBlank pins the documented escape
// hatch: blank reverts to the deprecated interval key. Rejecting it would make
// that state reachable only by hand-editing the TOML.
func TestConfigSetScanScheduleFrequencyAcceptsBlank(t *testing.T) {
	cfg := config.Config{}
	cfg.Server.ScanSchedule.Frequency = "daily"
	if err := setConfigValue(&cfg, "server.scan_schedule.frequency", ""); err != nil {
		t.Fatalf("setConfigValue blank frequency: %v", err)
	}
	if cfg.Server.ScanSchedule.Frequency != "" {
		t.Fatalf("frequency = %q; want blank", cfg.Server.ScanSchedule.Frequency)
	}
	cfg.Server.ScanIntervalSeconds = 600
	if got := config.ResolveScanSchedule(cfg, nil); got.Frequency != schedule.FrequencyInterval || got.Interval != 600*time.Second {
		t.Fatalf("resolved = %+v; want the deprecated interval path at 10m", got)
	}
}
