package commands

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestConfigSetRejectsWeeklyWithoutDay verifies that `config set` revalidates
// the MERGED [server.scan_schedule] before persisting, mirroring
// TestConfigSetRejectsOrderingFrontWhenModeParallel for the ordering/mode
// pair. Setting frequency=weekly on a config with no day ever set produces the
// incomplete schedule that config.ValidateScanSchedule rejects at load; without
// a re-check here the CLI writes a config the server refuses to boot from at
// the NEXT startup -- the same defect CodeRabbit found and #817 fixed on the
// web settings-save path (checkScanScheduleInvariant /
// checkScanScheduleInvariantChanges), just on the CLI path instead.
func TestConfigSetRejectsWeeklyWithoutDay(t *testing.T) {
	path := writeConfigTOML(t, "[server]\naddr = \":8080\"\n")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.frequency", Value: "weekly", ConfigPath: path,
	}})

	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (weekly with no day/at must be rejected before writing)", code)
	}
	if !strings.Contains(out.String(), "server.scan_schedule.at is required") {
		t.Fatalf("output = %q; want the ValidateScanSchedule error naming the missing anchor", out.String())
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read config: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("config file was rewritten despite the rejection:\nbefore=%q\nafter=%q", before, after)
	}
}

// TestConfigSetRejectsDailyWithoutAt is the "daily" half of the same gap:
// "daily" needs an "at" but not a "day", so it exercises a different branch of
// ValidateScanSchedule than the weekly case above.
func TestConfigSetRejectsDailyWithoutAt(t *testing.T) {
	path := writeConfigTOML(t, "[server]\naddr = \":8080\"\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.frequency", Value: "daily", ConfigPath: path,
	}})

	if code != 2 {
		t.Fatalf("exit code = %d; want 2 (daily with no at must be rejected before writing)", code)
	}
	if !strings.Contains(out.String(), "server.scan_schedule.at is required") {
		t.Fatalf("output = %q; want the ValidateScanSchedule error naming the missing anchor", out.String())
	}
}

// TestConfigSetAllowsValidScanScheduleCombinations is the positive control:
// the merged re-check must reject only an incomplete schedule, not every
// write to these keys. It also pins that setting the anchor FIRST, then the
// frequency that needs it, is a supported ordering for the CLI (each `config
// set` invocation only ever changes one key, so the anchor from a prior
// invocation must already be on disk when frequency is set).
func TestConfigSetAllowsValidScanScheduleCombinations(t *testing.T) {
	path := writeConfigTOML(t, "[server]\naddr = \":8080\"\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.at", Value: "04:00", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 setting at alone", code, out.String())
	}

	out.Reset()
	code = runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.day", Value: "sunday", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 setting day alone", code, out.String())
	}

	out.Reset()
	code = runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.frequency", Value: "weekly", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 - at and day are already on disk", code, out.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(saved), "weekly") {
		t.Fatalf("saved config = %q; want the accepted frequency=weekly persisted", saved)
	}
}

// TestConfigSetAllowsBlankFrequencyAndLegacyScanInterval pins the deliberate
// escape hatch through the merged re-check specifically: a blank frequency
// (the deprecated scan_interval_seconds path) must persist even though it is
// nowhere close to a complete daily/weekly schedule, and a legacy config that
// has never touched [server.scan_schedule] at all must keep saving other
// fields without tripping the new validation.
func TestConfigSetAllowsBlankFrequencyAndLegacyScanInterval(t *testing.T) {
	path := writeConfigTOML(t, "[server]\naddr = \":8080\"\nscan_interval_seconds = 600\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_interval_seconds", Value: "1200", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 - a legacy config with blank scan_schedule.frequency must still persist", code, out.String())
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(saved), "1200") {
		t.Fatalf("saved config = %q; want scan_interval_seconds=1200 persisted", saved)
	}

	out.Reset()
	code = runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.frequency", Value: "", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 - blank frequency reverting to the deprecated interval key must persist", code, out.String())
	}
}

// TestConfigSetRejectsHourlyStaysUnaffected is a narrow negative control:
// "hourly" and "off" need no anchor at all, so setting frequency=hourly with
// nothing else configured must NOT be caught by the new merged check.
func TestConfigSetAllowsHourlyWithNoAnchor(t *testing.T) {
	path := writeConfigTOML(t, "[server]\naddr = \":8080\"\n")

	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{
		Key: "server.scan_schedule.frequency", Value: "hourly", ConfigPath: path,
	}})
	if code != 0 {
		t.Fatalf("exit code = %d (output %q); want 0 - hourly requires no at/day", code, out.String())
	}
}
