package commands

import (
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

func TestDefaultConfigPathMatchesReader(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Run("docker mode writes where serve reads", func(t *testing.T) {
		t.Setenv("MXLRC_DOCKER", "true")
		want := filepath.Join("/config", "config.toml")
		if got := defaultConfigPath(); got != want {
			t.Errorf("defaultConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("native follows the shared resolver", func(t *testing.T) {
		t.Setenv("MXLRC_DOCKER", "")
		if got, want := defaultConfigPath(), config.ResolveConfigPath(""); got != want {
			t.Errorf("defaultConfigPath() = %q, want %q", got, want)
		}
	})
}
