package config

import (
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/trustnet"
)

// ValidationError is a per-field rejection: the field, the offending value, and
// a human-readable reason. The settings page surfaces these next to the field.
type ValidationError struct {
	Path    string
	Value   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("config: %s = %q: %s", e.Path, e.Value, e.Message)
}

// Validator checks a single string-form field value, returning a reason if it
// is invalid.
type Validator func(value string) error

// ValidateNonNegativeInt accepts an integer >= 0.
func ValidateNonNegativeInt() Validator {
	return func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		if n < 0 {
			return fmt.Errorf("must be zero or greater")
		}
		return nil
	}
}

// ValidatePositiveInt accepts an integer > 0. It mirrors the watcher.max_dirs
// env-override rule (n <= 0 rejected): a non-positive cap would reject every
// watch root, so the settings page refuses it at save time rather than letting
// it abort the watcher at next boot.
func ValidatePositiveInt() Validator {
	return func(value string) error {
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("must be an integer")
		}
		if n <= 0 {
			return fmt.Errorf("must be greater than zero")
		}
		return nil
	}
}

// ValidateBool accepts a TOML boolean.
func ValidateBool() Validator {
	return func(value string) error {
		if _, err := strconv.ParseBool(value); err != nil {
			return fmt.Errorf("must be true or false")
		}
		return nil
	}
}

// ValidateUnitInterval accepts a float in (0, 1], matching the daemon's
// confidence/similarity/threshold bounds.
func ValidateUnitInterval() Validator {
	return func(value string) error {
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("must be a number")
		}
		if f <= 0 || f > 1 {
			return fmt.Errorf("must be greater than 0 and at most 1")
		}
		return nil
	}
}

// enumValues is the single source of truth for the fixed-choice (enum) fields'
// allowed values. validatorFor builds its ValidateEnum from this map and
// AllowedValues exposes it to the settings UI, so the dropdown options on the
// page can never drift from what validation accepts.
var enumValues = map[string][]string{
	"output.embedded_lyrics": {"off", "respect", "extract"},
	"providers.mode":         {"ordered", "parallel"},
	"logging.level":          {"debug", "info", "warn", "error"},
	"logging.format":         {"text", "json"},
}

// AllowedValues returns the allowed values for a fixed-choice config field, or
// nil if the field is not a closed enum. The slice is a copy, so a caller cannot
// mutate the registry's backing values. It is the single source the settings UI
// reads to render dropdowns, kept in lockstep with validation via enumValues.
func AllowedValues(path string) []string {
	v, ok := enumValues[path]
	if !ok {
		return nil
	}
	return append([]string(nil), v...)
}

// ValidateEnum accepts only one of the allowed values.
func ValidateEnum(allowed ...string) Validator {
	return func(value string) error {
		for _, a := range allowed {
			if value == a {
				return nil
			}
		}
		return fmt.Errorf("must be one of %v", allowed)
	}
}

// ValidatePathExists accepts an empty value (meaning "unset") or a path that
// exists and is readable. It is used only for ffmpeg paths; db.path is
// deliberately excluded (the SQLite file is created on first boot).
func ValidatePathExists() Validator {
	return func(value string) error {
		if value == "" {
			return nil
		}
		if _, err := os.Stat(value); err != nil {
			return fmt.Errorf("path does not exist or is not readable")
		}
		return nil
	}
}

// ValidateHTTPURL returns a non-nil error if s is not an http(s) URL with a
// non-empty host. Used by both the settings-save validator (ValidateURL) and
// the boot-time constructors in internal/verification and internal/detector, so
// the UI rejects exactly the same inputs that boot would later reject.
func ValidateHTTPURL(s string) error {
	u, err := url.Parse(s)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("must be a valid http(s) URL with host (e.g. https://host/path)")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("must be a valid http(s) URL with host (e.g. https://host/path)")
	}
	return nil
}

// ValidateURL accepts an empty value (meaning "unset") or a string that passes
// ValidateHTTPURL (non-empty scheme and host), mirroring the boot-time check in
// internal/verification/verification.go and internal/detector/http.go.
func ValidateURL() Validator {
	return func(value string) error {
		if value == "" {
			return nil
		}
		return ValidateHTTPURL(value)
	}
}

// ValidatePEMFile accepts an empty value (meaning "unset"), rejects a path that
// does not exist or is not readable, and rejects a file whose content does not
// contain a valid PEM block. Used for TLS cert and key paths.
func ValidatePEMFile() Validator {
	return func(value string) error {
		if value == "" {
			return nil
		}
		data, err := os.ReadFile(value) //nolint:gosec // G304: value is operator-supplied config path, not untrusted user input
		if err != nil {
			return fmt.Errorf("path does not exist or is not readable")
		}
		if b, _ := pem.Decode(data); b == nil {
			return fmt.Errorf("file does not contain valid PEM data")
		}
		return nil
	}
}

// ValidateCIDRList accepts a comma-separated list (possibly empty) whose every
// entry parses as a CIDR, using the SAME parser the config loader applies at
// boot (trustnet.ParseCIDRs), so the UI rejects exactly what boot would reject.
func ValidateCIDRList() Validator {
	return func(value string) error {
		if _, err := trustnet.ParseCIDRs(splitCSV(value)); err != nil {
			return err
		}
		return nil
	}
}

// ValidateSelfSignedHosts accepts a comma-separated list whose every entry is a
// valid IP literal or RFC 1123 hostname, matching the loader's validateServerTLS
// check so a UI-accepted value can never break the listener at boot.
func ValidateSelfSignedHosts() Validator {
	return func(value string) error {
		for _, h := range splitCSV(value) {
			if net.ParseIP(h) == nil && !isValidHostname(h) {
				return fmt.Errorf("%q is not a valid hostname or IP address", h)
			}
		}
		return nil
	}
}

// ValidateKnownProviders accepts a comma-separated list (or single value) whose
// every non-empty entry is a known provider, mirroring the loader's
// providers.IsKnown gate (normalizeProvidersFallback / provider selection) so
// the UI rejects exactly the names that would abort boot. An empty value is
// valid (the loader restores defaults / treats it as "no entries").
func ValidateKnownProviders() Validator {
	return func(value string) error {
		for _, name := range splitCSV(value) {
			n := providers.NormalizeName(name)
			if n == "" {
				continue
			}
			if !providers.IsKnown(n) {
				return fmt.Errorf("unknown provider %q (known: %v)", name, providers.Known())
			}
		}
		return nil
	}
}

// ValidateListenAddr accepts an empty value (the loader restores the default
// listen address) or a host:port that parses, so a value that would fail the
// serve listener (net.Listen) is rejected at save time rather than at next boot.
func ValidateListenAddr() Validator {
	return func(value string) error {
		if value == "" {
			return nil
		}
		if _, _, err := net.SplitHostPort(value); err != nil {
			return fmt.Errorf("must be host:port")
		}
		return nil
	}
}

// validatorFor derives the validator for a field from its path (enum / path /
// unit-interval specifics) and otherwise from its type. Deriving here keeps the
// registry table readable; the drift test guarantees path coverage. Returns nil
// when the only constraint is being well-formed for the type (handled at write
// time by the writer's type serialization). The semantic validators reuse the
// loader's own primitives so the write path accepts exactly the set boot does.
func validatorFor(f FieldSpec) Validator {
	switch f.Path {
	case "output.embedded_lyrics", "providers.mode", "logging.level", "logging.format":
		return ValidateEnum(enumValues[f.Path]...)
	case "server.tls.cert_file", "server.tls.key_file":
		return ValidatePEMFile()
	case "verification.ffmpeg_path", "instrumental_detector.ffmpeg_path":
		return ValidatePathExists()
	case "verification.whisper_url", "instrumental_detector.classifier_url":
		return ValidateURL()
	case "server.trusted_networks.cidrs", "server.trusted_networks.trusted_proxies":
		return ValidateCIDRList()
	case "server.tls.self_signed_hosts":
		return ValidateSelfSignedHosts()
	case "providers.primary", "providers.disabled", "providers.fallback_order":
		return ValidateKnownProviders()
	case "server.addr":
		return ValidateListenAddr()
	case "watcher.max_dirs":
		// Strictly positive: a non-positive cap rejects every watch root (matches
		// the MXLRCGO_WATCH_MAX_DIRS env rule). watcher.debounce_ms falls through to
		// the default TypeInt non-negative validator (>= 0), matching its env rule.
		return ValidatePositiveInt()
	}
	switch f.Type {
	case TypeInt:
		return ValidateNonNegativeInt()
	case TypeBool:
		return ValidateBool()
	case TypeFloat64:
		return ValidateUnitInterval()
	default:
		return nil
	}
}

// ValidateAndSet validates a single change without touching the file. It rejects
// unknown keys, read-only fields, and values that fail the field's validator. It
// is the validate-only counterpart to ApplyChanges (which is the single write
// entry point).
func ValidateAndSet(path, value string) error {
	f, ok := FieldByPath(path)
	if !ok {
		return &ValidationError{Path: path, Value: value, Message: "unknown config key"}
	}
	if !f.Editable {
		return &ValidationError{Path: path, Value: value, Message: "field is read-only"}
	}
	if v := validatorFor(f); v != nil {
		if err := v(value); err != nil {
			return &ValidationError{Path: path, Value: value, Message: err.Error()}
		}
	}
	return nil
}

// ApplyChanges validates every change first and only then performs a single
// atomic write of all of them. If any value is invalid it returns the
// ValidationError and the on-disk config is left byte-identical (no temp file,
// no .bak). It is the single write entry point for both one-field hot-saves
// (a one-element map) and multi-field section saves.
func ApplyChanges(configPath string, changes map[string]string) error {
	for path, value := range changes {
		if err := ValidateAndSet(path, value); err != nil {
			return err
		}
	}
	if len(changes) == 0 {
		return nil
	}
	doc, err := LoadDocument(configPath)
	if err != nil {
		return err
	}
	for path, value := range changes {
		f, _ := FieldByPath(path)
		if err := SetValue(doc, path, f.Type, value, f.Description); err != nil {
			return err
		}
	}
	return WriteAtomic(configPath, doc)
}
