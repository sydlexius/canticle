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
		"[guard]", "[queue]", "[logging]", "[watcher]",
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
	cfg.Output.Dir = "custom-dir"
	envSrc := map[string]bool{"output.dir": true}
	cliSrc := map[string]bool{"output.dir": true}
	got := FormatConfigText(cfg, envSrc, cliSrc)

	// Find the specific "dir = ..." line under [output] and assert it carries
	// (cli) and NOT (env): CLI takes precedence over env for the same field.
	var dirLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "dir = ") {
			dirLine = line
			break
		}
	}
	if dirLine == "" {
		t.Fatalf("FormatConfigText: no output dir line found; got:\n%s", got)
	}
	if !strings.Contains(dirLine, "(cli)") {
		t.Errorf("FormatConfigText: output.dir line missing (cli): %q", dirLine)
	}
	if strings.Contains(dirLine, "(env)") {
		t.Errorf("FormatConfigText: output.dir line should not carry (env) when cli is set: %q", dirLine)
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
	// Positive assertion: the token must be redacted, not merely absent/blank.
	if !strings.Contains(got, "api.token=[REDACTED]") {
		t.Errorf("ConfigToSlogAttrs: token not rendered as [REDACTED]: %s", got)
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
	// Positive assertion: the keys must be redacted, not merely absent/blank.
	if !strings.Contains(got, "server.webhook_api_keys=[REDACTED]") {
		t.Errorf("ConfigToSlogAttrs: webhook keys not rendered as [REDACTED]: %s", got)
	}
}

// findGroupAttr walks the top-level groups returned by ConfigToSlogAttrs and
// returns the attr with the given key inside the named group (zero Attr + false
// if absent).
func findGroupAttr(attrs []slog.Attr, group, key string) (slog.Attr, bool) {
	for _, a := range attrs {
		if a.Key != group || a.Value.Kind() != slog.KindGroup {
			continue
		}
		for _, inner := range a.Value.Group() {
			if inner.Key == key {
				return inner, true
			}
		}
	}
	return slog.Attr{}, false
}

// TestConfigToSlogAttrs_TypedValuesStableWithSourceSibling verifies F2: typed
// int/bool/float values keep their native slog type even when env/cli-sourced,
// and a sibling "<key>_source" attr appears only when annotated.
func TestConfigToSlogAttrs_TypedValuesStableWithSourceSibling(t *testing.T) {
	cfg := defaults()
	cfg.Server.ScanIntervalSeconds = 42

	// Not annotated: typed value present, no sibling.
	attrs := ConfigToSlogAttrs(cfg, nil, nil)
	val, ok := findGroupAttr(attrs, "server", "scan_interval_seconds")
	if !ok {
		t.Fatal("scan_interval_seconds attr not found")
	}
	if val.Value.Kind() != slog.KindInt64 {
		t.Errorf("scan_interval_seconds kind = %v; want Int64", val.Value.Kind())
	}
	if val.Value.Int64() != 42 {
		t.Errorf("scan_interval_seconds = %d; want 42", val.Value.Int64())
	}
	if _, ok := findGroupAttr(attrs, "server", "scan_interval_seconds_source"); ok {
		t.Error("scan_interval_seconds_source present when not annotated; want absent")
	}

	// Env-annotated: typed value still Int64, sibling "_source" = "env".
	envSrc := map[string]bool{"server.scan_interval_seconds": true}
	attrs = ConfigToSlogAttrs(cfg, envSrc, nil)
	val, ok = findGroupAttr(attrs, "server", "scan_interval_seconds")
	if !ok {
		t.Fatal("scan_interval_seconds attr not found (annotated)")
	}
	if val.Value.Kind() != slog.KindInt64 {
		t.Errorf("annotated scan_interval_seconds kind = %v; want Int64 (type must not vary)", val.Value.Kind())
	}
	if val.Value.Int64() != 42 {
		t.Errorf("annotated scan_interval_seconds = %d; want 42", val.Value.Int64())
	}
	src, ok := findGroupAttr(attrs, "server", "scan_interval_seconds_source")
	if !ok {
		t.Fatal("scan_interval_seconds_source absent when annotated; want present")
	}
	if src.Value.String() != "env" {
		t.Errorf("scan_interval_seconds_source = %q; want \"env\"", src.Value.String())
	}

	// CLI takes precedence over env: sibling = "cli".
	cliSrc := map[string]bool{"server.scan_interval_seconds": true}
	attrs = ConfigToSlogAttrs(cfg, envSrc, cliSrc)
	src, ok = findGroupAttr(attrs, "server", "scan_interval_seconds_source")
	if !ok {
		t.Fatal("scan_interval_seconds_source absent when cli-annotated; want present")
	}
	if src.Value.String() != "cli" {
		t.Errorf("scan_interval_seconds_source = %q; want \"cli\"", src.Value.String())
	}

	// Float field keeps Float64 when annotated.
	fattrs := ConfigToSlogAttrs(cfg, map[string]bool{"verification.min_confidence": true}, nil)
	fv, ok := findGroupAttr(fattrs, "verification", "min_confidence")
	if !ok {
		t.Fatal("min_confidence attr not found")
	}
	if fv.Value.Kind() != slog.KindFloat64 {
		t.Errorf("annotated min_confidence kind = %v; want Float64", fv.Value.Kind())
	}
	if _, ok := findGroupAttr(fattrs, "verification", "min_confidence_source"); !ok {
		t.Error("min_confidence_source absent when annotated; want present")
	}

	// Bool field keeps Bool when annotated.
	battrs := ConfigToSlogAttrs(cfg, map[string]bool{"queue.randomize": true}, nil)
	bv, ok := findGroupAttr(battrs, "queue", "randomize")
	if !ok {
		t.Fatal("randomize attr not found")
	}
	if bv.Value.Kind() != slog.KindBool {
		t.Errorf("annotated randomize kind = %v; want Bool", bv.Value.Kind())
	}
	if _, ok := findGroupAttr(battrs, "queue", "randomize_source"); !ok {
		t.Error("randomize_source absent when annotated; want present")
	}
}

// TestConfigToSlogAttrs_EmptySliceRendersBrackets verifies F3: an empty slice
// renders as "[]" for both sensitive and non-sensitive paths, and an annotation
// appends to it (e.g. "[] (cli)") rather than replacing it.
func TestConfigToSlogAttrs_EmptySliceRendersBrackets(t *testing.T) {
	cfg := defaults()
	cfg.Server.WebhookAPIKeys = nil                    // sensitive, empty
	cfg.InstrumentalDetector.InstrumentalClasses = nil // non-sensitive, empty

	// No annotation: both render bare "[]".
	attrs := ConfigToSlogAttrs(cfg, nil, nil)
	sens, ok := findGroupAttr(attrs, "server", "webhook_api_keys")
	if !ok {
		t.Fatal("webhook_api_keys attr not found")
	}
	if sens.Value.String() != "[]" {
		t.Errorf("empty sensitive slice = %q; want \"[]\"", sens.Value.String())
	}
	nonSensitive, ok := findGroupAttr(attrs, "instrumental_detector", "instrumental_classes")
	if !ok {
		t.Fatal("instrumental_classes attr not found")
	}
	if nonSensitive.Value.String() != "[]" {
		t.Errorf("empty non-sensitive slice = %q; want \"[]\"", nonSensitive.Value.String())
	}

	// Annotated: the annotation appends to "[]" rather than replacing it.
	cliSrc := map[string]bool{
		"server.webhook_api_keys":                    true,
		"instrumental_detector.instrumental_classes": true,
	}
	attrs = ConfigToSlogAttrs(cfg, nil, cliSrc)
	sens, _ = findGroupAttr(attrs, "server", "webhook_api_keys")
	if sens.Value.String() != "[] (cli)" {
		t.Errorf("annotated empty sensitive slice = %q; want \"[] (cli)\"", sens.Value.String())
	}
	nonSensitive, _ = findGroupAttr(attrs, "instrumental_detector", "instrumental_classes")
	if nonSensitive.Value.String() != "[] (cli)" {
		t.Errorf("annotated empty non-sensitive slice = %q; want \"[] (cli)\"", nonSensitive.Value.String())
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
