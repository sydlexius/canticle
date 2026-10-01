package scanner

import (
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/failsig"
)

// Every format the scanner reads must also be one failsig's path rules treat as
// a trailing extension, or a failure on that format keeps its library path in
// the failure signature and the /metrics reason label (#1167). Lives here, not
// in failsig, because scanner already depends on failsig.
func TestFailsigCoversEverySupportedFileType(t *testing.T) {
	for _, ext := range supportedFileTypes {
		in := "worker: /srv/Private Folder/track" + ext
		got := failsig.Normalize(in)
		if strings.Contains(got, "Private Folder") {
			t.Errorf("supported type %s: path survived normalization: %q", ext, got)
		}
	}
}
