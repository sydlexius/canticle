package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoad_BumpAudioMtime pins output.bump_audio_mtime (#505): off by default,
// an omitted key restores the default, the file value and the env override
// work, and an invalid env value is ignored.
func TestLoad_BumpAudioMtime(t *testing.T) {
	for _, tc := range []struct {
		name, toml, env string
		want            bool
	}{
		{"default", "", "", false},
		{"omitted key", "[output]\nbilingual_output = true\n", "", false},
		{"file true", "[output]\nbump_audio_mtime = true\n", "", true},
		{"env beats file", "[output]\nbump_audio_mtime = false\n", "true", true},
		{"invalid env ignored", "[output]\nbump_audio_mtime = true\n", "notabool", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateEnv(t)
			if tc.env != "" {
				t.Setenv("MXLRC_OUTPUT_BUMP_AUDIO_MTIME", tc.env)
			}
			path := filepath.Join(t.TempDir(), "config.toml")
			if tc.toml != "" {
				if err := os.WriteFile(path, []byte(tc.toml), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Output.BumpAudioMtime != tc.want {
				t.Errorf("BumpAudioMtime = %v; want %v", cfg.Output.BumpAudioMtime, tc.want)
			}
		})
	}
}
