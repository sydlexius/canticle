package config

import (
	"testing"
)

func TestIsSensitiveConfigKey(t *testing.T) {
	sensitive := []string{"api.token", "server.webhook_api_keys"}
	for _, k := range sensitive {
		if !IsSensitiveConfigKey(k) {
			t.Errorf("IsSensitiveConfigKey(%q) = false; want true", k)
		}
	}

	nonsensitive := []string{
		"api.cooldown",
		"output.dir",
		"server.addr",
		"logging.level",
		"providers.primary",
		"token",           // bare key without section is not matched
		"webhook_api_key", // logging key form is not matched
	}
	for _, k := range nonsensitive {
		if IsSensitiveConfigKey(k) {
			t.Errorf("IsSensitiveConfigKey(%q) = true; want false", k)
		}
	}
}

func TestRedactValue(t *testing.T) {
	tests := []struct {
		key   string
		value string
		want  string
	}{
		{"api.token", "supersecret", "[REDACTED]"},
		{"api.token", "", ""}, // empty passes through
		{"server.webhook_api_keys", "key123", "[REDACTED]"},
		{"server.webhook_api_keys", "", ""}, // empty passes through
		{"api.cooldown", "15", "15"},        // non-sensitive passes through
		{"output.dir", "/lyrics", "/lyrics"},
		{"logging.level", "debug", "debug"},
	}
	for _, tt := range tests {
		got := RedactValue(tt.key, tt.value)
		if got != tt.want {
			t.Errorf("RedactValue(%q, %q) = %q; want %q", tt.key, tt.value, got, tt.want)
		}
	}
}
