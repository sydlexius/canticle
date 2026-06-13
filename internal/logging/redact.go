package logging

import (
	"log/slog"
	"strings"
)

// sensitiveKeys is the set of field names whose values must be redacted from
// log output. Matching is case-insensitive. "key" is intentionally excluded
// because it is over-broad and would redact unrelated structured fields.
var sensitiveKeys = map[string]struct{}{
	"token":            {},
	"api_key":          {},
	"apikey":           {},
	"authorization":    {},
	"secret":           {},
	"password":         {},
	"webhook_api_key":  {},
	"webhook_api_keys": {},
}

// isSensitiveKey reports whether name is a known sensitive field name.
// Comparison is case-insensitive.
func isSensitiveKey(name string) bool {
	_, ok := sensitiveKeys[strings.ToLower(name)]
	return ok
}

// RedactingReplaceAttr is a slog ReplaceAttr function that redacts the values
// of known sensitive field names. Wire it into every slog.HandlerOptions.ReplaceAttr
// to ensure credentials never appear in log output.
//
// Empty or zero-value attributes are left unchanged: there is no value to
// protect and preserving the absence helps distinguish "field not set" from
// "field redacted".
func RedactingReplaceAttr(groups []string, a slog.Attr) slog.Attr {
	// Resolve the attr to its final form before inspecting it.
	a.Value = a.Value.Resolve()

	// Never redact empty/zero values -- there is nothing sensitive to hide and
	// preserving zero values lets callers distinguish "unset" from "redacted".
	if a.Value.Equal(slog.Value{}) || a.Value.String() == "" {
		return a
	}

	if isSensitiveKey(a.Key) {
		return slog.Attr{Key: a.Key, Value: slog.StringValue("[REDACTED]")}
	}

	return a
}
