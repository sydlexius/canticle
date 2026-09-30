package config

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestExampleCoversRegistry fails when a Registry key has no entry (set or
// commented out) in config.example.toml, so the example cannot drift (#1144).
func TestExampleCoversRegistry(t *testing.T) {
	data, err := os.ReadFile("../../config.example.toml")
	if err != nil {
		t.Fatalf("read config.example.toml: %v", err)
	}
	section := regexp.MustCompile(`^#?\s*\[([A-Za-z0-9_.]+)\]\s*$`)
	present := map[string]bool{}
	cur := ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if m := section.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if key, ok := exampleEntryKey(line); ok && cur != "" {
			present[cur+"."+key] = true
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

// exampleEntryKey reports the key of a set or commented-out entry. The line,
// minus one leading "#", must decode as a single TOML key/value pair, so prose
// such as "# enabled = true turns this on" does not count as an entry.
func exampleEntryKey(line string) (string, bool) {
	body := strings.TrimSpace(strings.TrimPrefix(line, "#"))
	var kv map[string]any
	if _, err := toml.Decode(body, &kv); err != nil || len(kv) != 1 {
		return "", false
	}
	for k, v := range kv {
		if _, table := v.(map[string]any); table {
			return "", false
		}
		return k, true
	}
	return "", false
}

func TestExampleEntryKey(t *testing.T) {
	for _, tc := range []struct {
		line string
		key  string
		ok   bool
	}{
		{`enabled = true`, "enabled", true},
		{`# enabled = false`, "enabled", true},
		{`#enabled = false`, "enabled", true},
		{`# hosts = ["a.lan", "10.0.0.1"]`, "hosts", true},
		{`# enabled = true turns this on`, "", false},
		{`# Default 100 = the batch size`, "", false},
		{`# a.b = 1`, "", false},
		{`# plain prose`, "", false},
	} {
		key, ok := exampleEntryKey(tc.line)
		if key != tc.key || ok != tc.ok {
			t.Errorf("exampleEntryKey(%q) = %q, %v; want %q, %v", tc.line, key, ok, tc.key, tc.ok)
		}
	}
}
