package commands

import appversion "github.com/sydlexius/mxlrcgo-svc/internal/version"

// version, commit, and date mirror internal/version's build-time vars so
// callers within this package (e.g. server.WithWebUIIf, version_test.go) can
// use them without importing internal/version directly. Values are injected via
// ldflags into internal/version at release time; see .goreleaser.yml.
var (
	version = appversion.Version
	commit  = appversion.Commit
	date    = appversion.Date
)

// VersionString delegates to internal/version so the rest of the codebase
// can read the app version without importing internal/commands.
func VersionString() string { return appversion.VersionString() }

// Version implements go-arg's Versioned interface for the subcommand-aware
// parser, so `mxlrcgo-svc <cmd> --version` is recognized.
func (Args) Version() string { return appversion.VersionString() }

// Version implements go-arg's Versioned interface for the legacy (no
// subcommand) parser, so top-level `mxlrcgo-svc --version` works too.
func (LegacyArgs) Version() string { return appversion.VersionString() }
