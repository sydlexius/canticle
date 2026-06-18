package config

import (
	"fmt"
	"os"
	"strconv"
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
// exists and is readable. It is used only for TLS cert/key and ffmpeg paths;
// db.path is deliberately excluded (the SQLite file is created on first boot).
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

// validatorFor derives the validator for a field from its path (enum / path /
// unit-interval specifics) and otherwise from its type. Deriving here keeps the
// registry table readable; the drift test guarantees path coverage. Returns nil
// when the only constraint is being well-formed for the type (handled at write
// time by the writer's type serialization).
func validatorFor(f FieldSpec) Validator {
	switch f.Path {
	case "output.embedded_lyrics":
		return ValidateEnum("off", "respect", "extract")
	case "providers.mode":
		return ValidateEnum("ordered", "parallel")
	case "logging.level":
		return ValidateEnum("debug", "info", "warn", "error")
	case "logging.format":
		return ValidateEnum("text", "json")
	case "server.tls.cert_file", "server.tls.key_file",
		"verification.ffmpeg_path", "instrumental_detector.ffmpeg_path":
		return ValidatePathExists()
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
		if err := SetValue(doc, path, f.Type, value); err != nil {
			return err
		}
	}
	return WriteAtomic(configPath, doc)
}
