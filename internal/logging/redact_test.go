package logging

import (
	"log/slog"
	"testing"
)

// TestRedactingReplaceAttr_SensitiveKeysAreRedacted verifies each known
// sensitive key has its value replaced with [REDACTED].
func TestRedactingReplaceAttr_SensitiveKeysAreRedacted(t *testing.T) {
	keys := []string{
		"token",
		"api_key",
		"apikey",
		"authorization",
		"secret",
		"password",
		"webhook_api_key",
	}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			a := slog.Attr{Key: k, Value: slog.StringValue("super-secret")}
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != "[REDACTED]" {
				t.Errorf("key %q: got %q; want [REDACTED]", k, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_CaseInsensitive verifies that key matching is
// case-insensitive.
func TestRedactingReplaceAttr_CaseInsensitive(t *testing.T) {
	cases := []string{"TOKEN", "Token", "API_KEY", "Api_Key", "PASSWORD"}
	for _, k := range cases {
		t.Run(k, func(t *testing.T) {
			a := slog.Attr{Key: k, Value: slog.StringValue("secret-value")}
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != "[REDACTED]" {
				t.Errorf("key %q: got %q; want [REDACTED]", k, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_NonSensitiveKeysPassThrough verifies that unrelated
// field keys are left untouched.
func TestRedactingReplaceAttr_NonSensitiveKeysPassThrough(t *testing.T) {
	keys := []string{"artist", "title", "library", "error", "path", "count", "key"}
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			a := slog.Attr{Key: k, Value: slog.StringValue("visible-value")}
			got := RedactingReplaceAttr(nil, a)
			if got.Value.String() != "visible-value" {
				t.Errorf("key %q: got %q; want visible-value", k, got.Value.String())
			}
		})
	}
}

// TestRedactingReplaceAttr_EmptyValuesPassThrough verifies that empty string
// values are not redacted, even for sensitive keys.
func TestRedactingReplaceAttr_EmptyValuesPassThrough(t *testing.T) {
	a := slog.Attr{Key: "token", Value: slog.StringValue("")}
	got := RedactingReplaceAttr(nil, a)
	if got.Value.String() == "[REDACTED]" {
		t.Error("empty string value was redacted; want pass-through")
	}
}

// TestRedactingReplaceAttr_GroupContextDoesNotPreventRedaction verifies that a
// non-nil groups slice does not bypass redaction.
func TestRedactingReplaceAttr_GroupContextDoesNotPreventRedaction(t *testing.T) {
	a := slog.Attr{Key: "secret", Value: slog.StringValue("classified")}
	got := RedactingReplaceAttr([]string{"request", "auth"}, a)
	if got.Value.String() != "[REDACTED]" {
		t.Errorf("got %q; want [REDACTED]", got.Value.String())
	}
}

// TestKeyIsNotSensitive verifies that the bare "key" field is not redacted,
// because it is too broad.
func TestKeyIsNotSensitive(t *testing.T) {
	a := slog.Attr{Key: "key", Value: slog.StringValue("some-id")}
	got := RedactingReplaceAttr(nil, a)
	if got.Value.String() != "some-id" {
		t.Errorf("'key' field was redacted; want pass-through (it is not in sensitiveKeys)")
	}
}
