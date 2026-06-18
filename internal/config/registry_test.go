package config

import (
	"os"
	"regexp"
	"testing"
)

// appliedKeyRe matches the provenance keys applyEnvOverrides records, e.g.
//
//	applied["api.token"] = true
//
// It is a fixed string-literal pattern (not control-flow parsing), so it stays
// stable as the function's logic changes.
var appliedKeyRe = regexp.MustCompile(`applied\["([^"]+)"\]`)

// appliedPathsFromSource scans config.go for every applied["<path>"] key that
// applyEnvOverrides records. This is the independent enumeration of what the
// env-override ladder covers, used to catch a field added to applyEnvOverrides
// but forgotten in the registry.
func appliedPathsFromSource(t *testing.T) map[string]bool {
	t.Helper()
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatalf("read config.go: %v", err)
	}
	paths := map[string]bool{}
	for _, m := range appliedKeyRe.FindAllStringSubmatch(string(src), -1) {
		paths[m[1]] = true
	}
	if len(paths) == 0 {
		t.Fatal("found no applied[\"...\"] keys in config.go; the drift scan is broken")
	}
	return paths
}

// TestRegistryDrift_EveryAppliedPathInRegistry asserts every path
// applyEnvOverrides can set has a registry entry (Direction B: applyEnvOverrides
// -> registry). Fails loudly if a field is added to the env ladder but not the
// registry.
func TestRegistryDrift_EveryAppliedPathInRegistry(t *testing.T) {
	for path := range appliedPathsFromSource(t) {
		if _, ok := FieldByPath(path); !ok {
			t.Errorf("applyEnvOverrides sets %q but the registry has no entry for it", path)
		}
	}
}

// TestRegistryDrift_EveryEnvVarApplies asserts every registry field that names
// env vars is actually wired into applyEnvOverrides (Direction A: registry ->
// applyEnvOverrides). For each field, set its primary env var to a
// type-appropriate valid value, run applyEnvOverrides directly (avoiding
// LoadWithSources' cross-field validation), and assert the provenance map
// records the field's path. This also proves the env var NAME in the registry
// is correct.
func TestRegistryDrift_EveryEnvVarApplies(t *testing.T) {
	for _, f := range fields {
		if len(f.EnvVars) == 0 {
			t.Errorf("registry field %q has no EnvVars; every config field is env-overridable", f.Path)
			continue
		}
		t.Run(f.Path, func(t *testing.T) {
			isolateEnv(t)
			t.Setenv(f.EnvVars[0], validEnvValue(f))

			cfg := defaults()
			applied := map[string]bool{}
			applyEnvOverrides(&cfg, applied)

			if !applied[f.Path] {
				t.Errorf("setting %s did not record provenance for %q; registry env var name or path is wrong",
					f.EnvVars[0], f.Path)
			}
		})
	}
}

// TestRegistryDrift_EnvVarAliasesApply asserts every env var name in the
// registry (not just the primary) actually drives its field, so an alias listed
// in the registry can never be a dead string.
func TestRegistryDrift_EnvVarAliasesApply(t *testing.T) {
	for _, f := range fields {
		for _, env := range f.EnvVars {
			t.Run(f.Path+"/"+env, func(t *testing.T) {
				isolateEnv(t)
				t.Setenv(env, validEnvValue(f))

				cfg := defaults()
				applied := map[string]bool{}
				applyEnvOverrides(&cfg, applied)

				if !applied[f.Path] {
					t.Errorf("env var %s is listed for %q but did not apply", env, f.Path)
				}
			})
		}
	}
}

// validEnvValue returns a value that applyEnvOverrides will accept for the
// field's type and bounds, so the provenance bit is set (invalid values are
// rejected and leave provenance false).
func validEnvValue(f FieldSpec) string {
	switch f.Path {
	case "providers.mode":
		return "ordered"
	case "output.embedded_lyrics":
		return "off"
	case "logging.level":
		return "info"
	case "logging.format":
		return "json"
	}
	switch f.Type {
	case TypeInt:
		return "1"
	case TypeBool:
		return "true"
	case TypeFloat64:
		return "0.5"
	case TypeStringSlice:
		return "a,b"
	default: // TypeString
		return "x"
	}
}
