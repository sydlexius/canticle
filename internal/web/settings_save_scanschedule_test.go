package web

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

// scheduleTestUI seeds a minimal config and returns a writable settings handler
// plus the config path, mirroring the TLS section-save tests.
func scheduleTestUI(t *testing.T, seed string) (http.Handler, string) {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(seed), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	u := NewUI(config.Config{}, "v0", WithConfigPath(cfgPath), WithSecretStore(newFakeSecretStore()))
	mux := http.NewServeMux()
	u.Register(mux)
	return mux, cfgPath
}

const scheduleSeed = "[server]\naddr = \"127.0.0.1:3876\"\n"

// TestSaveSectionScanScheduleWeeklyPair is the ordinary way an operator sets
// this up: frequency, time and day arrive in ONE batch. A per-field check would
// reject "weekly" for missing an anchor that is in the same request, so this is
// the case the multi-field invariant exists for.
func TestSaveSectionScanScheduleWeeklyPair(t *testing.T) {
	mux, cfgPath := scheduleTestUI(t, scheduleSeed)

	rec := postSection(t, mux, [][2]string{
		{"server.scan_schedule.frequency", "weekly"},
		{"server.scan_schedule.at", "04:00"},
		{"server.scan_schedule.day", "sunday"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (weekly + anchors in one batch); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := cfg.Server.ScanSchedule
	if got.Frequency != "weekly" || got.At != "04:00" || got.Day != "sunday" {
		t.Fatalf("saved schedule = %+v; want weekly/04:00/sunday", got)
	}
}

// TestSaveSectionScanScheduleRejectsAnAnchorlessSchedule is the point of the
// whole invariant: the page must refuse to write a config the next restart
// would refuse to boot, and it must leave the file untouched when it does.
func TestSaveSectionScanScheduleRejectsAnAnchorlessSchedule(t *testing.T) {
	mux, cfgPath := scheduleTestUI(t, scheduleSeed)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // reason: G304: test temp path
	rec := postSection(t, mux, [][2]string{
		{"server.scan_schedule.frequency", "weekly"},
		{"server.scan_schedule.at", "04:00"},
		{"server.scan_schedule.day", ""},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (weekly with no day); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // reason: G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected section save:\n%s", after)
	}

	// The saved config must still boot. That is the actual contract: reject
	// exactly what Load would reject, and never leave a half-written schedule.
	if _, err := config.Load(cfgPath); err != nil {
		t.Fatalf("config no longer loads after a rejected save: %v", err)
	}
}

// TestSaveFieldScanScheduleRejectsDailyWithoutATime covers the single-field
// lane of the same invariant: switching an unanchored config to "daily" on its
// own is refused, since the resulting state has no start time.
func TestSaveFieldScanScheduleRejectsDailyWithoutATime(t *testing.T) {
	mux, cfgPath := scheduleTestUI(t, scheduleSeed)

	before, _ := os.ReadFile(cfgPath) //nolint:gosec // reason: G304: test temp path
	rec := postField(t, mux, url.Values{
		"path":  {"server.scan_schedule.frequency"},
		"value": {"daily"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (daily with no time); body=%s", rec.Code, rec.Body.String())
	}
	after, _ := os.ReadFile(cfgPath) //nolint:gosec // reason: G304: test temp path
	if !bytes.Equal(before, after) {
		t.Errorf("config mutated on a rejected field save:\n%s", after)
	}
}

// TestSaveFieldScanScheduleAcceptsDailyOnceAnchored is the positive control:
// with the time already on disk, the same frequency save succeeds. Without
// this, the rejection above could equally mean "schedule saves never work".
func TestSaveFieldScanScheduleAcceptsDailyOnceAnchored(t *testing.T) {
	mux, cfgPath := scheduleTestUI(t, scheduleSeed+"\n[server.scan_schedule]\nat = \"04:00\"\n")

	rec := postField(t, mux, url.Values{
		"path":  {"server.scan_schedule.frequency"},
		"value": {"daily"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (daily with the time already set); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.ScanSchedule.Frequency != "daily" || cfg.Server.ScanSchedule.At != "04:00" {
		t.Fatalf("saved schedule = %+v; want daily/04:00", cfg.Server.ScanSchedule)
	}
}

// TestSaveFieldScanScheduleOffNeedsNoAnchor confirms "off" is always
// acceptable: an operator must be able to stop the periodic scan without first
// inventing a start time for a scan that will not run.
func TestSaveFieldScanScheduleOffNeedsNoAnchor(t *testing.T) {
	mux, cfgPath := scheduleTestUI(t, scheduleSeed)

	rec := postField(t, mux, url.Values{
		"path":  {"server.scan_schedule.frequency"},
		"value": {"off"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (off needs no anchor); body=%s", rec.Code, rec.Body.String())
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if cfg.Server.ScanSchedule.Frequency != "off" {
		t.Fatalf("frequency = %q; want off", cfg.Server.ScanSchedule.Frequency)
	}
}
