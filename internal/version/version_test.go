package version

import (
	"strings"
	"testing"
)

func TestVersionString(t *testing.T) {
	got := VersionString()
	if !strings.HasPrefix(got, "mxlrcgo-svc ") {
		t.Errorf("VersionString() = %q, want prefix \"mxlrcgo-svc \"", got)
	}
	for _, sub := range []string{Version, Commit, Date} {
		if !strings.Contains(got, sub) {
			t.Errorf("VersionString() = %q, does not contain %q", got, sub)
		}
	}
}
