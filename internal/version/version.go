package version

import "fmt"

// Build-time version metadata. These defaults are overridden at release time by
// GoReleaser via -ldflags "-X .../internal/version.Version=..." (see .goreleaser.yml).
// A plain `go build` or `go run` leaves the defaults, so a non-release binary
// reports a clear "dev" marker instead of masquerading as a tagged version.
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// VersionString renders the single line printed for --version.
func VersionString() string {
	return fmt.Sprintf("mxlrcgo-svc %s (commit %s, built %s)", Version, Commit, Date)
}
