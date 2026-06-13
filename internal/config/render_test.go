package config

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestFormatConfigText_ContainsAllSections(t *testing.T) {
	cfg := defaults()
	got := FormatConfigText(cfg, nil, nil)

	sections := []string{
		"[api]", "[output]", "[db]", "[server]", "[providers]",
		"[verification]", "[instrumental_detector]", "[enrichment]",
		"[guard]", "[queue]", "[logging]",
	}
	for _, s := range sections {
		if !strings.Contains(got, s) {
			t.Errorf("FormatConfigText: missing section %q", s)
		}
	}
}

func TestFormatConfigText_RedactsToken(t *testing.T) {
	cfg := defaults()
	cfg.API.Token = "supersecret"
	got := FormatConfigText(cfg, nil, nil)

	if strings.Contains(got, "supersecret") {
		t.Error("FormatConfigText: token appears in plaintext")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Error("FormatConfigText: no [REDACTED] marker for token")
	}
}

func TestFormatConfigText_RedactsWebhookKeys(t *testing.T) {
	cfg := defaults()
	cfg.Server.WebhookAPIKeys = []string{"webhookkey1", "webhookkey2"}
	got := FormatConfigText(cfg, nil, nil)

	if strings.Contains(got, "webhookkey1") || strings.Contains(got, "webhookkey2") {
		t.Error("FormatConfigText: webhook key appears in plaintext")
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Error("FormatConfigText: no [REDACTED] marker for webhook keys")
	}
}

func TestFormatConfigText_EmptyTokenShowsNotSet(t *testing.T) {
	cfg := defaults()
	cfg.API.Token = ""
	got := FormatConfigText(cfg, nil, nil)

	if strings.Contains(got, "[REDACTED]") {
		t.Error("FormatConfigText: empty token should not be redacted")
	}
	if !strings.Contains(got, "(not set)") {
		t.Error("FormatConfigText: empty token should show '(not set)'")
	}
}

func TestFormatConfigText_SourceAnnotations(t *testing.T) {
	cfg := defaults()
	envSrc := map[string]bool{"api.cooldown": true}
	cliSrc := map[string]bool{"output.dir": true}
	got := FormatConfigText(cfg, envSrc, cliSrc)

	if !strings.Contains(got, "(env)") {
		t.Errorf("FormatConfigText: missing (env) annotation; got:\n%s", got)
	}
	if !strings.Contains(got, "(cli)") {
		t.Errorf("FormatConfigText: missing (cli) annotation; got:\n%s", got)
	}
}

func TestFormatConfigText_CLIAnnotationTakesPrecedenceOverEnv(t *testing.T) {
	cfg := defaults()
	envSrc := map[string]bool{"output.dir": true}
	cliSrc := map[string]bool{"output.dir": true}
	got := FormatConfigText(cfg, envSrc, cliSrc)

	// CLI overrides env: only "(cli)" should appear for this field.
	if !strings.Contains(got, "(cli)") {
		t.Error("FormatConfigText: missing (cli) annotation when both env and cli are set")
	}
}

func TestConfigToSlogAttrs_RedactsToken(t *testing.T) {
	cfg := defaults()
	cfg.API.Token = "supersecret"
	attrs := ConfigToSlogAttrs(cfg, nil, nil)

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	r := slog.NewRecord(time.Time{}, slog.LevelDebug, "test", 0)
	r.AddAttrs(attrs...)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "supersecret") {
		t.Errorf("ConfigToSlogAttrs: token in plaintext in slog output: %s", got)
	}
}

func TestConfigToSlogAttrs_RedactsWebhookKeys(t *testing.T) {
	cfg := defaults()
	cfg.Server.WebhookAPIKeys = []string{"webhookkey1"}
	attrs := ConfigToSlogAttrs(cfg, nil, nil)

	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	r := slog.NewRecord(time.Time{}, slog.LevelDebug, "test", 0)
	r.AddAttrs(attrs...)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "webhookkey1") {
		t.Errorf("ConfigToSlogAttrs: webhook key in plaintext in slog output: %s", got)
	}
}

func TestConfigToSlogAttrs_ContainsAllSections(t *testing.T) {
	cfg := defaults()
	attrs := ConfigToSlogAttrs(cfg, nil, nil)

	// Render to text and verify all section groups appear.
	var buf bytes.Buffer
	h := slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})
	r := slog.NewRecord(time.Time{}, slog.LevelDebug, "test", 0)
	r.AddAttrs(attrs...)
	if err := h.Handle(context.Background(), r); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := buf.String()

	// Text handler renders groups as "group.key=value". Check a field from each section.
	checks := []string{
		"api.cooldown=",
		"output.dir=",
		"db.path=",
		"server.addr=",
		"providers.primary=",
		"verification.enabled=",
		"instrumental_detector.enabled=",
		"enrichment.enabled=",
		"guard.script_guard_threshold=",
		"queue.randomize=",
		"logging.level=",
	}
	for _, c := range checks {
		if !strings.Contains(got, c) {
			t.Errorf("ConfigToSlogAttrs: missing field %q in slog output: %s", c, got)
		}
	}
}
