package config

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestExampleCoversRegistry fails when a Registry key has no entry (set or
// commented out) in config.example.toml, so the example cannot drift (#1144).
func TestExampleCoversRegistry(t *testing.T) {
	data, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatalf("read config.example.toml: %v", err)
	}
	section := regexp.MustCompile(`^#?\s*\[([A-Za-z0-9_.]+)\]\s*$`)
	// An entry is an optional comment marker, a bare identifier, then "=".
	// Prose comments ("# Default 100 = ...") do not match.
	entry := regexp.MustCompile(`^#?\s*([A-Za-z_][A-Za-z0-9_]*)\s*=`)
	present := map[string]bool{}
	cur := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := section.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if m := entry.FindStringSubmatch(line); m != nil && cur != "" {
			present[cur+"."+m[1]] = true
		}
	}
	var missing []string
	for _, e := range Registry() {
		if !present[e.Path] {
			missing = append(missing, e.Path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("config.example.toml lacks entries for %d registry keys: %s", len(missing), strings.Join(missing, ", "))
	}
}
