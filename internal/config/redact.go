package config

// sensitiveConfigKeys is the authoritative list of dotted TOML-path keys
// whose values must never appear in logs or diagnostic output. It is the
// single source of truth shared by the startup banner (this feature) and the
// planned serve-mode config view (#210).
var sensitiveConfigKeys = []string{
	"api.token",
	"server.webhook_api_keys",
}

// IsSensitiveConfigKey reports whether key is a known sensitive config field
// path (e.g. "api.token").
func IsSensitiveConfigKey(key string) bool {
	for _, k := range sensitiveConfigKeys {
		if k == key {
			return true
		}
	}
	return false
}

// RedactValue returns "[REDACTED]" when key is a sensitive config field path
// and value is non-empty. An empty value passes through unchanged so callers
// can distinguish "field not set" from "field redacted".
func RedactValue(key, value string) string {
	if value != "" && IsSensitiveConfigKey(key) {
		return "[REDACTED]"
	}
	return value
}
