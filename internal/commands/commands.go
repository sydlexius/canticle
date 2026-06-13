package commands

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	arg "github.com/alexflint/go-arg"
	"github.com/sydlexius/mxlrcgo-svc/internal/app"
	"github.com/sydlexius/mxlrcgo-svc/internal/auth"
	"github.com/sydlexius/mxlrcgo-svc/internal/cache"
	"github.com/sydlexius/mxlrcgo-svc/internal/config"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
	"github.com/sydlexius/mxlrcgo-svc/internal/detector"
	"github.com/sydlexius/mxlrcgo-svc/internal/langguard"
	"github.com/sydlexius/mxlrcgo-svc/internal/library"
	"github.com/sydlexius/mxlrcgo-svc/internal/logging"
	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/petitlyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/scan"
	"github.com/sydlexius/mxlrcgo-svc/internal/scanner"
	"github.com/sydlexius/mxlrcgo-svc/internal/server"
	"github.com/sydlexius/mxlrcgo-svc/internal/verification"
	"github.com/sydlexius/mxlrcgo-svc/internal/watcher"
	"github.com/sydlexius/mxlrcgo-svc/internal/worker"
)

// Args defines the CLI arguments for the application.
type Args struct {
	Fetch      *FetchCmd      `arg:"subcommand:fetch" help:"fetch lyrics once without HTTP server or DB queue"`
	Serve      *ServeCmd      `arg:"subcommand:serve" help:"run HTTP server, worker, and library scheduler"`
	Scan       *ScanCmd       `arg:"subcommand:scan" help:"scan configured libraries and enqueue missing lyrics"`
	Library    *LibraryCmd    `arg:"subcommand:library" help:"manage library roots"`
	Keys       *KeysCmd       `arg:"subcommand:keys" help:"manage API keys"`
	Config     *ConfigCmd     `arg:"subcommand:config" help:"inspect or update configuration"`
	Queue      *QueueCmd      `arg:"subcommand:queue" help:"inspect or maintain the durable work queue"`
	Completion *CompletionCmd `arg:"subcommand:completion" help:"output a shell completion script (bash, zsh, or fish)"`
}

// LegacyArgs preserves the pre-subcommand CLI surface.
type LegacyArgs struct {
	Song       []string `arg:"positional" help:"song information in [ artist,title ] format, a .txt file, or a directory path"`
	Outdir     *string  `arg:"-o,--outdir" help:"output directory (default: from config or 'lyrics')"`
	Cooldown   *int     `arg:"-c,--cooldown" help:"cooldown time in seconds (default: from config or 15)"`
	Depth      int      `arg:"-d,--depth" help:"(directory mode) maximum recursion depth" default:"100"`
	Update     bool     `arg:"-u,--update" help:"(directory mode) re-fetch and overwrite existing .lrc files"`
	Upgrade    bool     `arg:"--upgrade" help:"(directory mode) re-fetch songs with .txt (unsynced) to promote to .lrc if synced lyrics are now available; implied by --update"`
	BFS        bool     `arg:"--bfs" help:"(directory mode) use breadth-first-search traversal"`
	Serve      bool     `arg:"--serve" help:"run HTTP server mode"`
	Listen     *string  `arg:"--listen" help:"HTTP listen address (default: from config or 127.0.0.1:3876)"`
	Token      string   `arg:"-t,--token" help:"musixmatch token (or MUSIXMATCH_TOKEN / MXLRC_API_TOKEN env var, or config file)" default:""`
	ConfigPath string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// FetchCmd runs the legacy one-shot fetch path.
type FetchCmd struct {
	Song       []string `arg:"positional,required" help:"song information in [ artist,title ] format, a .txt file, or a directory path"`
	Outdir     *string  `arg:"-o,--outdir" help:"output directory (default: from config or 'lyrics')"`
	Cooldown   *int     `arg:"-c,--cooldown" help:"cooldown time in seconds (default: from config or 15)"`
	Depth      int      `arg:"-d,--depth" help:"(directory mode) maximum recursion depth" default:"100"`
	Update     bool     `arg:"-u,--update" help:"(directory mode) re-fetch and overwrite existing .lrc files"`
	Upgrade    bool     `arg:"--upgrade" help:"(directory mode) re-fetch songs with .txt lyrics to promote to .lrc"`
	BFS        bool     `arg:"--bfs" help:"(directory mode) use breadth-first-search traversal"`
	Token      string   `arg:"-t,--token" help:"musixmatch token" default:""`
	ConfigPath string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
	Album      string   `arg:"--album" help:"album name passed to the matcher (also a hint for --probe)" default:""`
	Probe      bool     `arg:"--probe" help:"query the provider once for the first [artist,title] and print the matched result without writing files (diagnostic for matcher behavior)"`
	ISRC       string   `arg:"--isrc" help:"(--probe diagnostic) ISRC passed to the matcher as track_isrc" default:""`
	Duration   int      `arg:"--duration" help:"(--probe diagnostic) track duration in seconds passed to the matcher as q_duration" default:"0"`
	SpotifyID  string   `arg:"--spotify-id" help:"(--probe diagnostic) Spotify track id passed to the matcher as track_spotify_id" default:""`
}

// ServeCmd runs the daemon.
type ServeCmd struct {
	Listen         *string `arg:"--listen" help:"HTTP listen address (default: from config or 127.0.0.1:3876)"`
	Outdir         *string `arg:"-o,--outdir" help:"output directory (default: from config or 'lyrics')"`
	Token          string  `arg:"-t,--token" help:"musixmatch token" default:""`
	ConfigPath     string  `arg:"--config" help:"path to config file (default: XDG)" default:""`
	Depth          int     `arg:"-d,--depth" help:"scheduler maximum recursion depth" default:"100"`
	Update         bool    `arg:"-u,--update" help:"scheduler re-fetches existing .lrc files"`
	Upgrade        bool    `arg:"--upgrade" help:"scheduler re-fetches .txt lyrics to promote them"`
	BFS            bool    `arg:"--bfs" help:"scheduler uses breadth-first traversal"`
	EmbeddedLyrics *string `arg:"--embedded-lyrics" help:"embedded unsynced lyrics handling: off, respect, or extract (default: output.embedded_lyrics or off)"`
	ScanInterval   *int    `arg:"--scan-interval" help:"scheduler interval in seconds (default: server.scan_interval_seconds or 900; 0 disables repeat)"`
	WorkInterval   *int    `arg:"--work-interval" help:"worker poll interval in seconds (default: server.work_interval_seconds or api.cooldown; minimum 15)"`
}

// ScanCmd scans libraries once and enqueues cache misses. It also hosts
// nested inspection subcommands (results, clear). When neither nested
// subcommand is set, the legacy run-once scan path is taken.
type ScanCmd struct {
	ConfigPath     string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
	Depth          int      `arg:"-d,--depth" help:"maximum recursion depth" default:"100"`
	Update         bool     `arg:"-u,--update" help:"re-fetch and overwrite existing .lrc files"`
	Upgrade        bool     `arg:"--upgrade" help:"re-fetch .txt lyrics to promote them"`
	BFS            bool     `arg:"--bfs" help:"use breadth-first traversal"`
	EmbeddedLyrics *string  `arg:"--embedded-lyrics" help:"embedded unsynced lyrics handling: off, respect, or extract (default: output.embedded_lyrics or off)"`
	Enrich         bool     `arg:"--enrich" help:"force recording enrichment (ISRC/MBID/duration) on for this scan, overriding per-library and global settings; mutually exclusive with --no-enrich"`
	NoEnrich       bool     `arg:"--no-enrich" help:"force recording enrichment off for this scan, overriding per-library and global settings; mutually exclusive with --enrich"`
	Libraries      []string `arg:"--only,separate" help:"limit scan to named or numeric libraries; repeat to select more than one. Distinct from subcommand --library flags (which target a single library row)"`

	Results *ScanResultsCmd `arg:"subcommand:results" help:"list persisted scan_results rows"`
	Clear   *ScanClearCmd   `arg:"subcommand:clear" help:"delete persisted scan_results rows for a library"`
}

// ScanResultsCmd lists persisted scan results, optionally filtered.
type ScanResultsCmd struct {
	Library    string `arg:"--library" help:"library name or numeric id" default:""`
	Status     string `arg:"--status" help:"filter by status (pending, processing, done)" default:""`
	Limit      int    `arg:"--limit" help:"maximum number of rows to return (0 = unlimited)" default:"0"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanClearCmd deletes scan_results rows for the named library only.
type ScanClearCmd struct {
	Library    string `arg:"--library,required" help:"library name or numeric id"`
	Yes        bool   `arg:"--yes" help:"actually delete (without it, prints what would be deleted)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueCmd contains nested queue inspection and maintenance subcommands.
type QueueCmd struct {
	List     *QueueListCmd     `arg:"subcommand:list" help:"list work_queue rows"`
	Failed   *QueueFailedCmd   `arg:"subcommand:failed" help:"list failed work_queue rows"`
	Deferred *QueueDeferredCmd `arg:"subcommand:deferred" help:"list deferred (benign-miss cooldown) work_queue rows"`
	Retry    *QueueRetryCmd    `arg:"subcommand:retry" help:"reset a failed work item back to pending"`
	Clear    *QueueClearCmd    `arg:"subcommand:clear" help:"delete completed work_queue rows"`
	Recheck  *QueueRecheckCmd  `arg:"subcommand:recheck" help:"revive deferred or retired rows for another pass"`
}

// QueueRecheckCmd revives work_queue rows for another processing pass.
type QueueRecheckCmd struct {
	Deferred   bool   `arg:"--deferred" help:"revive deferred (benign-miss cooldown) rows for an immediate re-check"`
	Retired    bool   `arg:"--retired" help:"revive rows retired after hitting the miss-attempt cap"`
	Library    string `arg:"--library" help:"limit to a single library (name or id)" default:""`
	Yes        bool   `arg:"--yes" help:"actually revive (without it, prints what would be revived)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueListCmd lists work_queue rows.
type QueueListCmd struct {
	Status     string `arg:"--status" help:"filter by status (pending, processing, failed, deferred, done)" default:""`
	Limit      int    `arg:"--limit" help:"maximum number of rows to return" default:"50"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueFailedCmd is a convenience for `queue list --status failed`.
type QueueFailedCmd struct {
	Limit      int    `arg:"--limit" help:"maximum number of rows to return" default:"50"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueDeferredCmd is a convenience for `queue list --status deferred`.
// Deferred rows are benign-miss cooldowns: no matching track or usable lyrics
// was found, and the row is scheduled for re-check after a fixed window. Use
// `queue retry` on a failed row to reset it immediately; deferred rows cannot
// be retried via that path -- use a webhook-priority enqueue instead.
type QueueDeferredCmd struct {
	Limit      int    `arg:"--limit" help:"maximum number of rows to return" default:"50"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueRetryCmd resets a single failed row back to pending.
type QueueRetryCmd struct {
	ID         int64  `arg:"positional,required" help:"work_queue row id"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// QueueClearCmd deletes completed (status=done) work_queue rows.
type QueueClearCmd struct {
	Done       bool   `arg:"--done,required" help:"delete rows whose status is done"`
	Yes        bool   `arg:"--yes" help:"actually delete (without it, prints what would be deleted)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// LibraryCmd contains nested library subcommands.
type LibraryCmd struct {
	Add    *LibraryAddCmd    `arg:"subcommand:add" help:"add a library root"`
	List   *LibraryListCmd   `arg:"subcommand:list" help:"list library roots"`
	Remove *LibraryRemoveCmd `arg:"subcommand:remove" help:"remove a library root"`
	Update *LibraryUpdateCmd `arg:"subcommand:update" help:"update a library root"`
}

// LibraryAddCmd adds a library root.
type LibraryAddCmd struct {
	Path               string `arg:"positional,required" help:"library root path"`
	Name               string `arg:"--name" help:"display name (default: directory base)" default:""`
	Enrich             *bool  `arg:"--enrich" help:"recording enrichment for this library; omit to inherit the global default, --enrich / --enrich=false to set"`
	DetectInstrumental *bool  `arg:"--detect-instrumental" help:"instrumental detection for this library; omit to inherit the global default, --detect-instrumental / --detect-instrumental=false to set"`
	ConfigPath         string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// LibraryListCmd lists library roots.
type LibraryListCmd struct {
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// LibraryRemoveCmd removes a library root.
type LibraryRemoveCmd struct {
	ID         int64  `arg:"positional,required" help:"library id"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// LibraryUpdateCmd updates a library root.
type LibraryUpdateCmd struct {
	ID                 int64  `arg:"positional,required" help:"library id"`
	Path               string `arg:"--path" help:"new library root path" default:""`
	Name               string `arg:"--name" help:"new display name" default:""`
	Enrich             *bool  `arg:"--enrich" help:"set recording enrichment for this library (--enrich / --enrich=false); omit to leave unchanged"`
	DetectInstrumental *bool  `arg:"--detect-instrumental" help:"set instrumental detection for this library (--detect-instrumental / --detect-instrumental=false); omit to leave unchanged"`
	ConfigPath         string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// KeysCmd contains nested key subcommands.
type KeysCmd struct {
	Create *KeysCreateCmd `arg:"subcommand:create" help:"create an API key"`
	List   *KeysListCmd   `arg:"subcommand:list" help:"list API keys"`
	Revoke *KeysRevokeCmd `arg:"subcommand:revoke" help:"revoke an API key"`
}

// KeysCreateCmd creates an API key.
type KeysCreateCmd struct {
	Name       string   `arg:"--name" help:"key name" default:""`
	Scopes     []string `arg:"--scope,separate" help:"scope to grant: webhook or admin"`
	ConfigPath string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// KeysListCmd lists API keys.
type KeysListCmd struct {
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// KeysRevokeCmd revokes an API key.
type KeysRevokeCmd struct {
	Key        string `arg:"positional,required" help:"raw API key to revoke"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ConfigCmd contains nested config subcommands.
type ConfigCmd struct {
	Get  *ConfigGetCmd  `arg:"subcommand:get" help:"get a config value"`
	Set  *ConfigSetCmd  `arg:"subcommand:set" help:"set a config value"`
	List *ConfigListCmd `arg:"subcommand:list" help:"list config values"`
}

// ConfigGetCmd gets a config value.
type ConfigGetCmd struct {
	Key        string `arg:"positional,required" help:"config key"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ConfigSetCmd sets a config value.
type ConfigSetCmd struct {
	Key        string `arg:"positional,required" help:"config key"`
	Value      string `arg:"positional,required" help:"config value"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ConfigListCmd lists config values.
type ConfigListCmd struct {
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// AppRunner executes the one-shot fetch application.
type AppRunner interface {
	Run(ctx context.Context) error
}

// Deps contains command dependencies that main can replace in tests.
type Deps struct {
	LoadDotenv func() error
	NewFetcher func(token string) musixmatch.Fetcher
	NewWriter  func(roots ...string) lyrics.Writer
	NewApp     func(fetcher musixmatch.Fetcher, writer lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) AppRunner
}

// Run parses rawArgs, dispatches the selected command, and returns a process exit code.
func Run(ctx context.Context, rawArgs []string, out io.Writer, deps Deps) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if out == nil {
		out = os.Stdout
	}
	// __complete is the hidden handler the generated completion scripts invoke.
	// It is intercepted before flag parsing so it never appears in help output.
	if len(rawArgs) > 0 && rawArgs[0] == "__complete" {
		return runComplete(ctx, out, rawArgs[1:])
	}
	if deps.LoadDotenv == nil {
		deps.LoadDotenv = func() error { return nil }
	}
	if deps.NewFetcher == nil {
		deps.NewFetcher = func(token string) musixmatch.Fetcher { return musixmatch.NewClient(token) }
	}
	if deps.NewWriter == nil {
		deps.NewWriter = func(roots ...string) lyrics.Writer { return lyrics.NewLRCWriter(roots...) }
	}
	if deps.NewApp == nil {
		deps.NewApp = func(fetcher musixmatch.Fetcher, writer lyrics.Writer, inputs *queue.InputsQueue, cooldown int, mode string) AppRunner {
			return app.NewApp(fetcher, writer, inputs, cooldown, mode)
		}
	}

	var args Args
	var legacy LegacyArgs
	parseTarget := any(&args)
	if !usesSubcommand(rawArgs) {
		parseTarget = &legacy
	}
	parser, err := arg.NewParser(arg.Config{Program: "mxlrcgo-svc", Out: out}, parseTarget)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	if err := parser.Parse(rawArgs); err != nil {
		if err == arg.ErrHelp {
			if err := parser.WriteHelpForSubcommand(out, parser.SubcommandNames()...); err != nil {
				_, _ = fmt.Fprintln(out, err)
				return 2
			}
			return 0
		}
		if err == arg.ErrVersion {
			_, _ = fmt.Fprintln(out, VersionString())
			return 0
		}
		if usageErr := parser.WriteUsageForSubcommand(out, parser.SubcommandNames()...); usageErr != nil {
			_, _ = fmt.Fprintln(out, usageErr)
			return 2
		}
		_, _ = fmt.Fprintln(out, err)
		return 2
	}

	_ = deps.LoadDotenv()
	applyLogLevel()

	switch {
	case !usesSubcommand(rawArgs):
		if legacy.Serve {
			return runServe(ctx, legacyServe(legacy), deps.NewFetcher, deps.NewWriter)
		}
		return runFetch(ctx, out, legacyFetch(legacy), deps.NewFetcher, deps.NewWriter, deps.NewApp)
	case args.Fetch != nil:
		return runFetch(ctx, out, *args.Fetch, deps.NewFetcher, deps.NewWriter, deps.NewApp)
	case args.Serve != nil:
		return runServe(ctx, *args.Serve, deps.NewFetcher, deps.NewWriter)
	case args.Scan != nil:
		return runScanCmd(ctx, out, *args.Scan)
	case args.Library != nil:
		return runLibrary(ctx, out, *args.Library)
	case args.Keys != nil:
		return runKeys(ctx, out, *args.Keys)
	case args.Config != nil:
		return runConfig(out, *args.Config)
	case args.Queue != nil:
		return runQueueCmd(ctx, out, *args.Queue)
	case args.Completion != nil:
		return runCompletion(out, *args.Completion)
	default:
		_, _ = fmt.Fprintln(out, "missing subcommand")
		return 2
	}
}

func usesSubcommand(rawArgs []string) bool {
	if len(rawArgs) == 0 {
		return false
	}
	if rawArgs[0] == "-h" || rawArgs[0] == "--help" {
		return true
	}
	commands := map[string]bool{
		"fetch": true, "serve": true, "scan": true, "library": true, "keys": true, "config": true, "queue": true, "completion": true,
	}
	return commands[rawArgs[0]]
}

func legacyFetch(args LegacyArgs) FetchCmd {
	return FetchCmd{
		Song:       args.Song,
		Outdir:     args.Outdir,
		Cooldown:   args.Cooldown,
		Depth:      args.Depth,
		Update:     args.Update,
		Upgrade:    args.Upgrade,
		BFS:        args.BFS,
		Token:      args.Token,
		ConfigPath: args.ConfigPath,
	}
}

func legacyServe(args LegacyArgs) ServeCmd {
	return ServeCmd{
		Listen:     args.Listen,
		Outdir:     args.Outdir,
		Token:      args.Token,
		ConfigPath: args.ConfigPath,
		Depth:      args.Depth,
		Update:     args.Update,
		Upgrade:    args.Upgrade,
		BFS:        args.BFS,
	}
}

// applyLogLevel adjusts the default slog level from MXLRC_LOG_LEVEL (debug,
// info, warn, error). It uses SetLogLoggerLevel so the existing default-handler
// output format is unchanged; only the threshold moves. Unset/unknown leaves the
// default (info). DEBUG exposes the worker idle-poll and watcher event lines.
// This bootstrap handler is active during dotenv load and config.Load(); once
// config.Load() succeeds, initLogging installs the full handler (level, format,
// optional file output, redaction) and takes over.
func applyLogLevel() {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MXLRC_LOG_LEVEL"))) {
	case "debug":
		slog.SetLogLoggerLevel(slog.LevelDebug)
	case "warn", "warning":
		slog.SetLogLoggerLevel(slog.LevelWarn)
	case "error":
		slog.SetLogLoggerLevel(slog.LevelError)
	case "info", "":
		slog.SetLogLoggerLevel(slog.LevelInfo)
	}
}

// Log-level rubric:
//
//	DEBUG: routine, high-frequency, normal-operation detail (per-request logs, per-item processing, cache hits).
//	INFO:  meaningful, lower-frequency events (startup/shutdown, scan summaries, successful writes).
//	WARN:  recoverable problems / degraded conditions (circuit open, rate-limit backoff, retries, fallbacks, giving up on a track, misconfig).
//	ERROR: unrecoverable failures requiring attention.

// initLogging installs the fully configured slog handler after config.Load()
// succeeds. It maps cfg.Logging fields to a logging.Config and calls
// logging.Init, which replaces the bootstrap handler set by applyLogLevel.
func initLogging(cfg config.Config) {
	logging.Init(logging.Config{
		Level:      cfg.Logging.Level,
		Format:     cfg.Logging.Format,
		FilePath:   cfg.Logging.File,
		MaxSizeMB:  cfg.Logging.MaxSizeMB,
		MaxFiles:   cfg.Logging.MaxFiles,
		MaxAgeDays: cfg.Logging.MaxAgeDays,
		Compress:   cfg.Logging.Compress,
	})
}

func runFetch(ctx context.Context, out io.Writer, args FetchCmd, newFetcher func(string) musixmatch.Fetcher, newWriter func(roots ...string) lyrics.Writer, newApp func(musixmatch.Fetcher, lyrics.Writer, *queue.InputsQueue, int, string) AppRunner) int {
	if len(args.Song) == 0 {
		_, _ = fmt.Fprintln(out, "missing required positional argument: Song")
		return 2
	}
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	initLogging(cfg)
	token := args.Token
	if token == "" {
		token = cfg.API.Token
	}
	cooldown := cfg.API.Cooldown
	if args.Cooldown != nil {
		cooldown = *args.Cooldown
	}
	outdir := cfg.Output.Dir
	if args.Outdir != nil {
		outdir = *args.Outdir
	}
	fetcher, err := selectedProvider(cfg, token, newFetcher)
	if err != nil {
		slog.Error("failed to configure lyrics provider", "error", err)
		return 1
	}

	if args.Probe {
		artist, title := parseArtistTitle(args.Song[0])
		return fetchProbe(ctx, out, models.Track{
			ArtistName:  artist,
			TrackName:   title,
			AlbumName:   args.Album,
			TrackLength: args.Duration,
			ISRC:        args.ISRC,
			SpotifyID:   args.SpotifyID,
		}, fetcher)
	}

	inputs := queue.NewInputsQueue()
	sc := scanner.NewScanner()
	mode, err := sc.ParseInput(args.Song, outdir, args.Update, args.Upgrade, args.Depth, args.BFS, inputs)
	if err != nil {
		slog.Error("failed to parse input", "error", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "\n%d lyrics to fetch\n\n", inputs.Len())
	if mode != "dir" {
		if err := os.MkdirAll(outdir, 0750); err != nil { //nolint:gosec // user-specified output directory
			slog.Error("failed to create output directory", "error", err)
			return 1
		}
	}

	writer := newWriter()
	configureWriterBilingual(writer, cfg)
	application := newApp(fetcher, writer, inputs, cooldown, mode)
	if err := application.Run(ctx); err != nil {
		slog.Error("application error", "error", err)
		return 1
	}
	return 0
}

// fetchProbe runs a single live query against the fetcher and prints the matched
// result without writing any files. It is a diagnostic for the (undocumented)
// Musixmatch macro matcher: it shows what artist/title/album the query resolved
// to and what lyrics came back, so matcher behavior (album-artist vs a
// concatenated multi-artist string, album steering between lyric versions) can be
// characterized from a local build. A no-match is a valid outcome, not an error,
// so it returns 0.
func fetchProbe(ctx context.Context, out io.Writer, track models.Track, fetcher musixmatch.Fetcher) int {
	_, _ = fmt.Fprintf(out, "query:   artist=%q title=%q album=%q isrc=%q duration=%d spotify_id=%q\n",
		track.ArtistName, track.TrackName, track.AlbumName, track.ISRC, track.TrackLength, track.SpotifyID)
	song, err := fetcher.FindLyrics(ctx, track)
	if err != nil {
		_, _ = fmt.Fprintf(out, "result:  MISS (%v)\n", err)
		return 0
	}
	_, _ = fmt.Fprintf(out, "matched: artist=%q title=%q album=%q\n",
		song.Track.ArtistName, song.Track.TrackName, song.Track.AlbumName)
	_, _ = fmt.Fprintf(out, "lyrics:  synced_lines=%d unsynced=%t instrumental=%t\n",
		len(song.Subtitles.Lines),
		strings.TrimSpace(song.Lyrics.LyricsBody) != "",
		song.Track.Instrumental == 1)
	if preview := lyricPreview(song, 5); preview != "" {
		_, _ = fmt.Fprintf(out, "preview:\n%s\n", preview)
	}
	return 0
}

// parseArtistTitle splits a single "artist,title" probe argument. Extra commas
// fold into the title, so "a,b,c" yields artist "a" and title "b,c".
func parseArtistTitle(s string) (artist, title string) {
	parts := strings.SplitN(s, ",", 2)
	artist = strings.TrimSpace(parts[0])
	if len(parts) > 1 {
		title = strings.TrimSpace(parts[1])
	}
	return artist, title
}

// lyricPreview returns up to n non-blank lines of the matched lyrics, preferring
// synced subtitle text and falling back to the unsynced body.
func lyricPreview(song models.Song, n int) string {
	var lines []string
	for _, l := range song.Subtitles.Lines {
		if t := strings.TrimSpace(l.Text); t != "" {
			lines = append(lines, t)
		}
		if len(lines) >= n {
			break
		}
	}
	if len(lines) == 0 {
		for _, l := range strings.Split(song.Lyrics.LyricsBody, "\n") {
			if t := strings.TrimSpace(l); t != "" {
				lines = append(lines, t)
			}
			if len(lines) >= n {
				break
			}
		}
	}
	return strings.Join(lines, "\n")
}

func runServe(ctx context.Context, args ServeCmd, newFetcher func(string) musixmatch.Fetcher, newWriter func(roots ...string) lyrics.Writer) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	initLogging(cfg)
	token := args.Token
	if token == "" {
		token = cfg.API.Token
	}
	outdir := cfg.Output.Dir
	if args.Outdir != nil {
		outdir = *args.Outdir
	}
	fetcher, err := selectedProvider(cfg, token, newFetcher)
	if err != nil {
		slog.Error("failed to configure lyrics provider", "error", err)
		return 1
	}
	verifier, err := newVerifier(cfg)
	if err != nil {
		slog.Error("failed to configure verification", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}

	authSvc, err := keyService(ctx, sqlDB, cfg.Server.WebhookAPIKeys)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to configure authentication", "error", err)
		return 1
	}
	workQ := queue.NewDBQueue(sqlDB)
	workQ.SetRandomized(cfg.Queue.Randomize)
	// Snapshot the configured library roots once at startup. They confine both
	// the webhook handler's raw payload paths (path-injection guard) and the
	// worker's write-time output, so a symlink swapped in below a root after the
	// handler check cannot redirect the .lrc write outside the root. Listing
	// failure is not fatal: confinement is simply skipped (the writer falls back
	// to its unconfined path) and the handler degrades as documented below.
	allowedRoots := webhookAllowedRoots(ctx, sqlDB)
	writer := newWriter(allowedRoots...)
	configureWriterBilingual(writer, cfg)
	w := worker.New(workQ, cache.New(sqlDB), fetcher, writer)
	w.SetCircuitOpenDuration(time.Duration(cfg.API.CircuitOpenDuration) * time.Second)
	w.SetCircuitBackoff(time.Duration(cfg.API.CircuitBackoffBase)*time.Second, time.Duration(cfg.API.CircuitOpenDuration)*time.Second)
	// Dispatch strategy and the parallel-mode synced-upgrade window. Set before the
	// fallback lanes for clarity; the worker rebuilds the orchestrator on every
	// setter, so the ordering is not load-bearing.
	w.SetProvidersMode(cfg.Providers.Mode)
	w.SetRaceWait(time.Duration(cfg.Providers.RaceWaitSeconds) * time.Second)
	// Register fallback lanes (each with its own breaker) and stamp the active
	// provider set's generation onto both the queue (write-on-enqueue) and the
	// worker (compare-on-lookup) so a provider-set change invalidates stale cached
	// results. Fallbacks are configured after the circuit setters so the lanes
	// inherit the configured window via the worker's stored parameters.
	fallbacks := fallbackProviders(cfg, token, fetcher.Name(), newFetcher)
	w.SetFallbackProviders(fallbacks...)
	gen := providerGeneration(fetcher.Name(), fallbacks)
	workQ.SetProvidersVersion(gen)
	w.SetProvidersVersion(gen)
	w.SetMissBackoff(time.Duration(cfg.API.MissBackoffBaseHours)*time.Hour, time.Duration(cfg.API.MissBackoffCapHours)*time.Hour)
	w.SetMaxMissAttempts(cfg.API.MaxMissAttempts)
	configureWorkerVerification(w, cfg, verifier)
	audioDetector, err := newAudioDetector(cfg)
	if err != nil {
		slog.Error("failed to configure instrumental detector", "error", err)
		return 1
	}
	configureWorkerAudioDetector(w, audioDetector)
	configureWorkerGuard(w, newGuard(cfg))

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		runWorkerLoop(runCtx, w, serveWorkerInterval(cfg, args))
	}()
	go func() {
		defer wg.Done()
		runScheduler(runCtx, sqlDB, cfg, args)
	}()
	if watchCfg := watcher.ConfigFromEnv(); watchCfg.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWatcher(runCtx, sqlDB, args, watchCfg, cfg)
		}()
	}

	addr := cfg.Server.Addr
	if args.Listen != nil {
		addr = *args.Listen
	}
	srv := &http.Server{
		Addr: addr,
		Handler: server.NewHandler(authSvc, workQ, outdir,
			server.WithReadiness(sqlDB),
			server.WithStatusReporter(workQ),
			server.WithInventory(scan.New(sqlDB)),
			server.WithAllowedRoots(allowedRoots),
		),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	go func() {
		<-runCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("HTTP server shutdown failed", "error", err)
		}
	}()
	slog.Info("starting HTTP server", "addr", addr)
	code := 0
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server failed", "error", err)
		code = 1
	}
	cancel()
	wg.Wait()
	if err := sqlDB.Close(); err != nil {
		slog.Warn("failed to close database", "error", err)
	}
	return code
}

func runWorkerLoop(ctx context.Context, w *worker.Worker, interval time.Duration) {
	interval = normalizeWorkerInterval(interval)
	slog.Info("worker loop started", "interval", interval)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.RunPaced(ctx, interval); err != nil && !errors.Is(err, context.Canceled) {
			slog.Warn("worker run failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// serveWorkerInterval resolves the worker poll interval. Precedence:
// CLI --work-interval > config server.work_interval_seconds > api.cooldown.
// A zero server.work_interval_seconds means "fall back to api.cooldown".
func serveWorkerInterval(cfg config.Config, args ServeCmd) time.Duration {
	interval := cfg.API.Cooldown
	if cfg.Server.WorkIntervalSeconds > 0 {
		interval = cfg.Server.WorkIntervalSeconds
	}
	if args.WorkInterval != nil {
		interval = *args.WorkInterval
	}
	return time.Duration(interval) * time.Second
}

// serveScanInterval resolves the scheduler scan interval. Precedence:
// CLI --scan-interval > config server.scan_interval_seconds (default 900).
// A zero result disables repeat scanning (scan once).
func serveScanInterval(cfg config.Config, args ServeCmd) time.Duration {
	seconds := cfg.Server.ScanIntervalSeconds
	if args.ScanInterval != nil {
		seconds = *args.ScanInterval
	}
	return time.Duration(seconds) * time.Second
}

func normalizeWorkerInterval(interval time.Duration) time.Duration {
	if interval < 15*time.Second {
		return 15 * time.Second
	}
	return interval
}

// buildProvider constructs the named provider's adapter with the per-request
// pacing floor applied, or nil for an unknown name. Test-injected fake fetchers
// do not satisfy *musixmatch.Client so the pacer is a no-op for them, preserving
// the injection seam. Petit Lyrics needs no token; it gets the same pacing floor
// as Musixmatch for now (independent per-provider tuning is future work).
func buildProvider(name string, cfg config.Config, token string, newFetcher func(string) musixmatch.Fetcher) providers.LyricsProvider {
	switch providers.NormalizeName(name) {
	case providers.Musixmatch:
		fetcher := newFetcher(token)
		if mc, ok := fetcher.(*musixmatch.Client); ok {
			mc.WithMinInterval(time.Duration(cfg.API.Cooldown) * time.Second)
		}
		return providers.New(providers.Musixmatch, fetcher)
	case providers.PetitLyrics:
		petit := petitlyrics.NewClient()
		petit.WithMinInterval(time.Duration(cfg.API.Cooldown) * time.Second)
		return providers.New(providers.PetitLyrics, petit)
	default:
		return nil
	}
}

func selectedProvider(cfg config.Config, token string, newFetcher func(string) musixmatch.Fetcher) (providers.LyricsProvider, error) {
	p, err := providers.Select(
		cfg.Providers.Primary,
		cfg.Providers.Disabled,
		buildProvider(providers.Musixmatch, cfg, token, newFetcher),
		buildProvider(providers.PetitLyrics, cfg, token, newFetcher),
	)
	if err != nil {
		return nil, err
	}
	// The API token requirement is provider-specific: only Musixmatch needs one.
	// petitlyrics is tokenless, so a missing token must not block it.
	if p.Name() == providers.Musixmatch && strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("no API token provided for the musixmatch provider: use --token, MUSIXMATCH_TOKEN, MXLRC_API_TOKEN, or config file")
	}
	return p, nil
}

// fallbackProviders builds the ordered fallback lanes from cfg.Providers.
// FallbackOrder, skipping any entry that is disabled, duplicates the primary, or
// is a Musixmatch lane with no token (which would only emit 401s). It returns the
// providers in configured priority order.
func fallbackProviders(cfg config.Config, token, primaryName string, newFetcher func(string) musixmatch.Fetcher) []providers.LyricsProvider {
	primaryName = providers.NormalizeName(primaryName)
	var out []providers.LyricsProvider
	for _, name := range cfg.Providers.FallbackOrder {
		n := providers.NormalizeName(name)
		if n == primaryName {
			continue // already the primary lane
		}
		if providerDisabledIn(n, cfg.Providers.Disabled) {
			continue
		}
		if n == providers.Musixmatch && strings.TrimSpace(token) == "" {
			slog.Warn("skipping musixmatch fallback lane: no API token configured", "provider", n)
			continue
		}
		if p := buildProvider(n, cfg, token, newFetcher); p != nil {
			out = append(out, p)
		}
	}
	return out
}

// providerGeneration is the cache-invalidation generation for the active lane
// set (primary + fallbacks), computed over their provider names.
func providerGeneration(primaryName string, fallbacks []providers.LyricsProvider) int {
	names := make([]string, 0, len(fallbacks)+1)
	names = append(names, primaryName)
	for _, p := range fallbacks {
		names = append(names, p.Name())
	}
	return providers.Generation(names)
}

func providerDisabledIn(name string, disabled []string) bool {
	name = providers.NormalizeName(name)
	for _, d := range disabled {
		if providers.NormalizeName(d) == name {
			return true
		}
	}
	return false
}

func newVerifier(cfg config.Config) (verification.Verifier, error) {
	if !cfg.Verification.Enabled {
		return nil, nil
	}
	return verification.NewHTTPVerifier(
		cfg.Verification.WhisperURL,
		cfg.Verification.SampleDurationSeconds,
		cfg.Verification.MinSimilarity,
		cfg.Verification.FFmpegPath,
	)
}

func configureWorkerVerification(w *worker.Worker, cfg config.Config, verifier verification.Verifier) {
	if verifier == nil {
		return
	}
	w.EnableVerification(verifier, cfg.Verification.MinConfidence)
}

// newAudioDetector builds the instrumental detector from config. Returns (nil, nil)
// when the detector is disabled (the default). Callers must treat a nil return as
// the disabled/no-op state.
func newAudioDetector(cfg config.Config) (detector.Detector, error) {
	if !cfg.InstrumentalDetector.Enabled {
		return nil, nil
	}
	return detector.NewHTTPDetector(
		cfg.InstrumentalDetector.ClassifierURL,
		cfg.InstrumentalDetector.SampleDurationSeconds,
		cfg.InstrumentalDetector.MinConfidence,
		cfg.InstrumentalDetector.InstrumentalClasses,
		cfg.InstrumentalDetector.FFmpegPath,
		cfg.InstrumentalDetector.CooldownSeconds,
	)
}

// configureWorkerAudioDetector wires the detector into the worker. It is a
// no-op when d is nil (detector disabled).
func configureWorkerAudioDetector(w *worker.Worker, d detector.Detector) {
	if d == nil {
		return
	}
	w.EnableAudioDetector(d)
}

// newGuard builds the language/script guard from config. It returns an UNTYPED
// nil interface when no allowlist is configured (guard disabled). Returning a
// typed-nil *langguard.Guard through the interface would be non-nil and defeat
// the nil check in configureWorkerGuard, so the disabled path must return a bare
// nil rather than a (*langguard.Guard)(nil).
func newGuard(cfg config.Config) worker.ScriptGuard {
	if len(cfg.Guard.AcceptedScripts) == 0 {
		return nil
	}
	return langguard.NewGuard(cfg.Guard.AcceptedScripts, cfg.Guard.Threshold)
}

func configureWorkerGuard(w *worker.Worker, g worker.ScriptGuard) {
	if g == nil {
		return
	}
	w.EnableGuard(g)
}

// configureWriterBilingual enables interleaved bilingual output on an LRC writer
// when configured. Fake writers in tests do not satisfy *lyrics.LRCWriter, so
// this is a no-op for them (mirrors how the Musixmatch pacer is applied via a
// type assertion in selectedProvider).
func configureWriterBilingual(w lyrics.Writer, cfg config.Config) {
	if lw, ok := w.(*lyrics.LRCWriter); ok {
		lw.SetBilingual(cfg.Output.BilingualOutput)
	}
}

func runScheduler(ctx context.Context, sqlDB *sql.DB, cfg config.Config, args ServeCmd) {
	s := scheduler(sqlDB, scanner.ScanOptions{
		Update:         args.Update,
		Upgrade:        args.Upgrade,
		MaxDepth:       args.Depth,
		BFS:            args.BFS,
		EmbeddedLyrics: embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
	})
	// serve has no per-run enrichment override; resolve per library against the
	// global default (and the per-library setting) inside the scheduler.
	s.GlobalEnrichDefault = cfg.Enrichment.Enabled
	s.Interval = serveScanInterval(cfg, args)
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("scheduler failed", "error", err)
	}
}

// runWatcher runs the optional filesystem watcher, triggering a targeted scan of
// the changed directory under the owning library. The periodic scheduler remains
// the reconciliation backstop; the watcher only lowers latency for new files.
func runWatcher(ctx context.Context, sqlDB *sql.DB, args ServeCmd, watchCfg watcher.Config, cfg config.Config) {
	sched := scheduler(sqlDB, scanner.ScanOptions{
		Update:         args.Update,
		Upgrade:        args.Upgrade,
		MaxDepth:       args.Depth,
		BFS:            args.BFS,
		EmbeddedLyrics: embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
	})
	sched.GlobalEnrichDefault = cfg.Enrichment.Enabled
	wch := watcher.New(watchCfg, library.New(sqlDB), func(ctx context.Context, lib models.Library, path string) error {
		return sched.RunOnceForPath(ctx, lib, path)
	})
	// The watcher is best-effort and explicitly never a replacement for the
	// periodic scheduler (see README), so a setup or runtime failure must not
	// take down serve. Surface it at error level so an operator who enabled the
	// watcher sees the failure loudly while the scheduler keeps reconciling.
	if err := wch.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		slog.Error("watcher stopped; continuing without it (periodic scheduler remains the source of truth)", "error", err)
	}
}

// webhookAllowedRoots returns the configured library root paths used to confine
// webhook payload paths. A listing failure is logged and treated as "no roots",
// which disables raw-payload-path resolution rather than failing startup.
func webhookAllowedRoots(ctx context.Context, sqlDB *sql.DB) []string {
	libs, err := library.New(sqlDB).List(ctx)
	if err != nil {
		slog.Warn("failed to list libraries for path confinement; confinement disabled for both the webhook handler (raw payload-path resolution) and worker writes (write-time output confinement)", "error", err)
		return nil
	}
	roots := make([]string, 0, len(libs))
	for _, lib := range libs {
		if lib.Path != "" {
			roots = append(roots, lib.Path)
		}
	}
	return roots
}

// runScanCmd dispatches the scan subcommand. When neither nested subcommand
// is set, it runs the legacy one-shot scan. Otherwise it routes to the
// requested inspection or maintenance subcommand. The parent --config flag
// is forwarded to the nested subcommand when the subcommand did not specify
// its own --config value.
func runScanCmd(ctx context.Context, out io.Writer, args ScanCmd) int {
	switch {
	case args.Results != nil:
		sub := *args.Results
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runScanResults(ctx, out, sub)
	case args.Clear != nil:
		sub := *args.Clear
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runScanClear(ctx, out, sub)
	default:
		return runScan(ctx, out, args)
	}
}

func runScan(ctx context.Context, out io.Writer, args ScanCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	s := scheduler(sqlDB, scanner.ScanOptions{
		Update:         args.Update,
		Upgrade:        args.Upgrade,
		MaxDepth:       args.Depth,
		BFS:            args.BFS,
		EmbeddedLyrics: embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
	})
	enrichOverride, err := resolveEnrichOverride(args.Enrich, args.NoEnrich)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	s.EnrichOverride = enrichOverride
	s.GlobalEnrichDefault = cfg.Enrichment.Enabled
	if len(args.Libraries) > 0 {
		libRepo := library.New(sqlDB)
		libs := make([]models.Library, 0, len(args.Libraries))
		for _, ref := range args.Libraries {
			lib, err := resolveLibrary(ctx, libRepo, ref)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					_, _ = fmt.Fprintf(out, "library %q not found\n", ref)
					return 1
				}
				_, _ = fmt.Fprintf(out, "library %q: %v\n", ref, err)
				return 1
			}
			libs = append(libs, lib)
		}
		if err := s.RunOnceFor(ctx, libs); err != nil {
			slog.Error("scan failed", "error", err)
			return 1
		}
		return 0
	}
	if err := s.RunOnce(ctx); err != nil {
		slog.Error("scan failed", "error", err)
		return 1
	}
	return 0
}

// embeddedLyricsMode resolves the embedded-lyrics mode: the CLI flag wins over
// the (already-normalized) config value, and any unrecognized value clamps to
// "off" so a typo can never silently enable extraction or skip fetching.
func embeddedLyricsMode(flag *string, cfgVal string) string {
	v := cfgVal
	if flag != nil {
		v = *flag
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "respect":
		return "respect"
	case "extract":
		return "extract"
	default:
		return "off"
	}
}

// resolveEnrichOverride converts the mutually-exclusive scan --enrich/--no-enrich
// flags into a tri-state override for recording enrichment: nil = no override
// (fall back to per-library then global), &true = force on, &false = force off.
// Passing both flags is a usage error.
func resolveEnrichOverride(enrich, noEnrich bool) (*bool, error) {
	switch {
	case enrich && noEnrich:
		return nil, fmt.Errorf("--enrich and --no-enrich are mutually exclusive")
	case enrich:
		v := true
		return &v, nil
	case noEnrich:
		v := false
		return &v, nil
	default:
		return nil, nil
	}
}

func scheduler(sqlDB *sql.DB, opts scanner.ScanOptions) scan.Scheduler {
	results := scan.New(sqlDB)
	enq := scan.Enqueuer{
		Results:  results,
		Cache:    cache.New(sqlDB),
		Queue:    queue.NewDBQueue(sqlDB),
		Priority: queue.PriorityScan,
	}
	return scan.Scheduler{
		Libraries: library.New(sqlDB),
		Results:   results,
		Scanner:   scanner.NewScanner(),
		Options:   opts,
		OnScanComplete: func(ctx context.Context, lib models.Library, found []models.ScanResult) error {
			enqueued, cacheHits, err := enq.EnqueuePending(ctx, lib.ID)
			if err != nil {
				// Counts are partial on an aborted enqueue; don't log "complete".
				slog.Warn("scheduled scan incomplete (enqueue aborted)",
					"library", lib.Name, "found", len(found), "enqueued", enqueued, "cache_hits", cacheHits, "error", err)
				return err
			}
			slog.Info("scheduled scan complete",
				"library", lib.Name, "found", len(found), "enqueued", enqueued, "cache_hits", cacheHits)
			return nil
		},
	}
}

func runLibrary(ctx context.Context, out io.Writer, args LibraryCmd) int {
	path := libraryConfigPath(args)
	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	repo := library.New(sqlDB)
	switch {
	case args.Add != nil:
		name := args.Add.Name
		if name == "" {
			name = filepath.Base(filepath.Clean(args.Add.Path))
		}
		lib, err := repo.Add(ctx, args.Add.Path, name, models.LibrarySettings{
			EnrichRecording:    args.Add.Enrich,
			DetectInstrumental: args.Add.DetectInstrumental,
		})
		if err != nil {
			slog.Error("failed to add library", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "%d\t%s\t%s\n", lib.ID, lib.Name, lib.Path)
	case args.List != nil:
		libs, err := repo.List(ctx)
		if err != nil {
			slog.Error("failed to list libraries", "error", err)
			return 1
		}
		for _, v := range libs {
			_, _ = fmt.Fprintf(out, "%d\t%s\t%s\n", v.ID, v.Name, v.Path)
		}
	case args.Remove != nil:
		if err := repo.Remove(ctx, args.Remove.ID); err != nil {
			slog.Error("failed to remove library", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "removed library %d\n", args.Remove.ID)
	case args.Update != nil:
		if args.Update.Path == "" && args.Update.Name == "" &&
			args.Update.Enrich == nil && args.Update.DetectInstrumental == nil {
			_, _ = fmt.Fprintln(out, "library update requires --path, --name, --enrich, or --detect-instrumental")
			return 2
		}
		lib, err := repo.Get(ctx, args.Update.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				slog.Error("failed to find library", "library_id", args.Update.ID, "error", err)
				_, _ = fmt.Fprintf(out, "library %d not found\n", args.Update.ID)
				return 1
			}
			slog.Error("failed to find library", "error", err)
			return 1
		}
		path := lib.Path
		if args.Update.Path != "" {
			path = args.Update.Path
		}
		name := lib.Name
		if args.Update.Name != "" {
			name = args.Update.Name
		}
		lib, err = repo.Update(ctx, args.Update.ID, path, name, models.LibrarySettings{
			EnrichRecording:    args.Update.Enrich,
			DetectInstrumental: args.Update.DetectInstrumental,
		})
		if err != nil {
			slog.Error("failed to update library", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "%d\t%s\t%s\n", lib.ID, lib.Name, lib.Path)
	default:
		_, _ = fmt.Fprintln(out, "missing library subcommand")
		return 2
	}
	return 0
}

func libraryConfigPath(args LibraryCmd) string {
	switch {
	case args.Add != nil:
		return args.Add.ConfigPath
	case args.List != nil:
		return args.List.ConfigPath
	case args.Remove != nil:
		return args.Remove.ConfigPath
	case args.Update != nil:
		return args.Update.ConfigPath
	default:
		return ""
	}
}

func runKeys(ctx context.Context, out io.Writer, args KeysCmd) int {
	path := keysConfigPath(args)
	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	svc := auth.NewService(auth.NewSQLStore(sqlDB))
	switch {
	case args.Create != nil:
		scopes, err := parseScopes(args.Create.Scopes)
		if err != nil {
			slog.Error("failed to create API key", "error", err)
			return 1
		}
		created, err := svc.CreateKey(ctx, args.Create.Name, scopes)
		if err != nil {
			slog.Error("failed to create API key", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "%s\n", created.Raw)
	case args.List != nil:
		keys, err := svc.ListKeys(ctx)
		if err != nil {
			slog.Error("failed to list API keys", "error", err)
			return 1
		}
		for _, v := range keys {
			revoked := ""
			if v.RevokedAt != nil {
				revoked = v.RevokedAt.Format(time.RFC3339)
			}
			_, _ = fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", v.ID, v.Name, encodeScopes(v.Scopes), revoked)
		}
	case args.Revoke != nil:
		key, err := svc.RevokeKey(ctx, args.Revoke.Key)
		if err != nil {
			slog.Error("failed to revoke API key", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "revoked %s\n", key.ID)
	default:
		_, _ = fmt.Fprintln(out, "missing keys subcommand")
		return 2
	}
	return 0
}

func keysConfigPath(args KeysCmd) string {
	switch {
	case args.Create != nil:
		return args.Create.ConfigPath
	case args.List != nil:
		return args.List.ConfigPath
	case args.Revoke != nil:
		return args.Revoke.ConfigPath
	default:
		return ""
	}
}

func parseScopes(values []string) ([]auth.Scope, error) {
	if len(values) == 0 {
		values = []string{string(auth.ScopeWebhook)}
	}
	scopes := make([]auth.Scope, 0, len(values))
	for _, v := range values {
		scopes = append(scopes, auth.Scope(v))
	}
	return auth.NormalizeScopes(scopes)
}

func keyService(ctx context.Context, sqlDB *sql.DB, rawKeys []string) (*auth.Service, error) {
	store := auth.NewSQLStore(sqlDB)
	for i, raw := range rawKeys {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if !strings.HasPrefix(raw, auth.KeyPrefix) {
			return nil, fmt.Errorf("webhook API key %d: invalid format", i+1)
		}
		hash, err := auth.HashKey(raw)
		if err != nil {
			return nil, err
		}
		if len(hash) < 16 {
			return nil, fmt.Errorf("webhook API key %d: derived hash is too short", i+1)
		}
		if _, err := store.CreateIfNotExists(ctx, auth.Key{
			ID:        hash[:16],
			Name:      fmt.Sprintf("webhook-%d", i+1),
			Hash:      hash,
			Scopes:    []auth.Scope{auth.ScopeWebhook},
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			return nil, err
		}
	}
	return auth.NewService(store), nil
}

func runConfig(out io.Writer, args ConfigCmd) int {
	path := configCommandPath(args)
	cfg, err := config.Load(path)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	switch {
	case args.Get != nil:
		v, ok := configValue(cfg, args.Get.Key)
		if !ok {
			_, _ = fmt.Fprintf(out, "unknown config key: %s\n", args.Get.Key)
			return 2
		}
		_, _ = fmt.Fprintln(out, v)
	case args.List != nil:
		for _, k := range configKeys() {
			v, _ := configValue(cfg, k)
			_, _ = fmt.Fprintf(out, "%s=%s\n", k, v)
		}
	case args.Set != nil:
		if err := setConfigValue(&cfg, args.Set.Key, args.Set.Value); err != nil {
			_, _ = fmt.Fprintln(out, err)
			return 2
		}
		if path == "" {
			path = defaultConfigPath()
		}
		if err := os.MkdirAll(filepath.Dir(path), 0750); err != nil {
			slog.Error("failed to create config directory", "error", err)
			return 1
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600) //nolint:gosec // user-selected config path
		if err != nil {
			slog.Error("failed to open config file", "error", err)
			return 1
		}
		if err := toml.NewEncoder(f).Encode(cfg); err != nil {
			_ = f.Close()
			slog.Error("failed to write config", "error", err)
			return 1
		}
		if err := f.Close(); err != nil {
			slog.Error("failed to close config", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "%s=%s\n", args.Set.Key, args.Set.Value)
	default:
		_, _ = fmt.Fprintln(out, "missing config subcommand")
		return 2
	}
	return 0
}

func configCommandPath(args ConfigCmd) string {
	switch {
	case args.Get != nil:
		return args.Get.ConfigPath
	case args.Set != nil:
		return args.Set.ConfigPath
	case args.List != nil:
		return args.List.ConfigPath
	default:
		return ""
	}
}

func configKeys() []string {
	return []string{
		"api.token",
		"api.cooldown",
		"api.circuit_open_duration",
		"api.miss_backoff_base_hours",
		"api.miss_backoff_cap_hours",
		"api.max_miss_attempts",
		"output.dir",
		"output.embedded_lyrics",
		"output.bilingual_output",
		"db.path",
		"server.addr",
		"server.webhook_api_keys",
		"server.scan_interval_seconds",
		"server.work_interval_seconds",
		"providers.primary",
		"providers.disabled",
		"providers.mode",
		"providers.race_wait_seconds",
		"verification.enabled",
		"verification.whisper_url",
		"verification.ffmpeg_path",
		"verification.sample_duration_seconds",
		"verification.min_confidence",
		"verification.min_similarity",
		"guard.accepted_scripts",
		"guard.script_guard_threshold",
	}
}

func configValue(cfg config.Config, key string) (string, bool) {
	switch key {
	case "api.token":
		return cfg.API.Token, true
	case "api.cooldown":
		return strconv.Itoa(cfg.API.Cooldown), true
	case "api.circuit_open_duration":
		return strconv.Itoa(cfg.API.CircuitOpenDuration), true
	case "api.miss_backoff_base_hours":
		return strconv.Itoa(cfg.API.MissBackoffBaseHours), true
	case "api.miss_backoff_cap_hours":
		return strconv.Itoa(cfg.API.MissBackoffCapHours), true
	case "api.max_miss_attempts":
		return strconv.Itoa(cfg.API.MaxMissAttempts), true
	case "output.dir":
		return cfg.Output.Dir, true
	case "output.embedded_lyrics":
		return cfg.Output.EmbeddedLyrics, true
	case "output.bilingual_output":
		return strconv.FormatBool(cfg.Output.BilingualOutput), true
	case "db.path":
		return cfg.DB.Path, true
	case "server.addr":
		return cfg.Server.Addr, true
	case "server.webhook_api_keys":
		return strings.Join(cfg.Server.WebhookAPIKeys, ","), true
	case "server.scan_interval_seconds":
		return strconv.Itoa(cfg.Server.ScanIntervalSeconds), true
	case "server.work_interval_seconds":
		return strconv.Itoa(cfg.Server.WorkIntervalSeconds), true
	case "providers.primary":
		return cfg.Providers.Primary, true
	case "providers.disabled":
		return strings.Join(cfg.Providers.Disabled, ","), true
	case "providers.mode":
		return cfg.Providers.Mode, true
	case "providers.race_wait_seconds":
		return strconv.Itoa(cfg.Providers.RaceWaitSeconds), true
	case "verification.enabled":
		return strconv.FormatBool(cfg.Verification.Enabled), true
	case "verification.whisper_url":
		return cfg.Verification.WhisperURL, true
	case "verification.ffmpeg_path":
		return cfg.Verification.FFmpegPath, true
	case "verification.sample_duration_seconds":
		return strconv.Itoa(cfg.Verification.SampleDurationSeconds), true
	case "verification.min_confidence":
		return strconv.FormatFloat(cfg.Verification.MinConfidence, 'f', -1, 64), true
	case "verification.min_similarity":
		return strconv.FormatFloat(cfg.Verification.MinSimilarity, 'f', -1, 64), true
	case "guard.accepted_scripts":
		return strings.Join(cfg.Guard.AcceptedScripts, ","), true
	case "guard.script_guard_threshold":
		return strconv.FormatFloat(cfg.Guard.Threshold, 'f', -1, 64), true
	default:
		return "", false
	}
}

func setConfigValue(cfg *config.Config, key string, value string) error {
	switch key {
	case "api.token":
		cfg.API.Token = value
	case "api.cooldown":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("api.cooldown must be a non-negative integer")
		}
		cfg.API.Cooldown = n
	case "api.circuit_open_duration":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("api.circuit_open_duration must be a positive integer (seconds)")
		}
		cfg.API.CircuitOpenDuration = n
	case "api.miss_backoff_base_hours":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("api.miss_backoff_base_hours must be a positive integer (hours; minimum 1)")
		}
		cfg.API.MissBackoffBaseHours = n
	case "api.miss_backoff_cap_hours":
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 {
			return fmt.Errorf("api.miss_backoff_cap_hours must be a positive integer (hours; minimum 1)")
		}
		cfg.API.MissBackoffCapHours = n
	case "api.max_miss_attempts":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("api.max_miss_attempts must be a non-negative integer (0 means no cap)")
		}
		cfg.API.MaxMissAttempts = n
	case "output.dir":
		cfg.Output.Dir = value
	case "output.embedded_lyrics":
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "off", "respect", "extract":
			cfg.Output.EmbeddedLyrics = strings.ToLower(strings.TrimSpace(value))
		default:
			return fmt.Errorf("invalid value %q for output.embedded_lyrics (want off, respect, or extract)", value)
		}
	case "output.bilingual_output":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("output.bilingual_output must be a boolean")
		}
		cfg.Output.BilingualOutput = v
	case "db.path":
		cfg.DB.Path = value
	case "server.addr":
		cfg.Server.Addr = value
	case "server.webhook_api_keys":
		cfg.Server.WebhookAPIKeys = splitCSV(value)
	case "server.scan_interval_seconds":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("server.scan_interval_seconds must be a non-negative integer (seconds; 0 disables repeat)")
		}
		cfg.Server.ScanIntervalSeconds = n
	case "server.work_interval_seconds":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("server.work_interval_seconds must be a non-negative integer (seconds; 0 uses api.cooldown)")
		}
		cfg.Server.WorkIntervalSeconds = n
	case "providers.primary":
		cfg.Providers.Primary = value
	case "providers.disabled":
		cfg.Providers.Disabled = splitCSV(value)
	case "providers.mode":
		m := strings.ToLower(strings.TrimSpace(value))
		if m != "ordered" && m != "parallel" {
			return fmt.Errorf("providers.mode must be \"ordered\" or \"parallel\"")
		}
		cfg.Providers.Mode = m
	case "providers.race_wait_seconds":
		// Accept any integer and let config normalization clamp it: a non-positive
		// value is the "use the default" sentinel in the config stack, so rejecting
		// it here would make that contract unreachable from the CLI.
		n, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("providers.race_wait_seconds must be an integer (seconds; non-positive uses the default)")
		}
		cfg.Providers.RaceWaitSeconds = n
	case "verification.enabled":
		v, err := strconv.ParseBool(value)
		if err != nil {
			return fmt.Errorf("verification.enabled must be a boolean")
		}
		cfg.Verification.Enabled = v
	case "verification.whisper_url":
		cfg.Verification.WhisperURL = value
	case "verification.ffmpeg_path":
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("verification.ffmpeg_path must not be empty")
		}
		cfg.Verification.FFmpegPath = value
	case "verification.sample_duration_seconds":
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return fmt.Errorf("verification.sample_duration_seconds must be a positive integer")
		}
		cfg.Verification.SampleDurationSeconds = n
	case "verification.min_confidence":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || n > 1 {
			return fmt.Errorf("verification.min_confidence must be a number between 0 and 1")
		}
		cfg.Verification.MinConfidence = n
	case "verification.min_similarity":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || n > 1 {
			return fmt.Errorf("verification.min_similarity must be a number between 0 and 1")
		}
		cfg.Verification.MinSimilarity = n
	case "guard.accepted_scripts":
		// An empty value is valid: it clears the allowlist and disables the guard.
		cfg.Guard.AcceptedScripts = splitCSV(value)
	case "guard.script_guard_threshold":
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || n <= 0 || n > 1 {
			return fmt.Errorf("guard.script_guard_threshold must be a number between 0 and 1")
		}
		cfg.Guard.Threshold = n
	default:
		return fmt.Errorf("unknown config key: %s", key)
	}
	return nil
}

func defaultConfigPath() string {
	if base := os.Getenv("XDG_CONFIG_HOME"); base != "" {
		return filepath.Join(base, "mxlrcgo-svc", "config.toml")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join("config.toml")
	}
	return filepath.Join(home, ".config", "mxlrcgo-svc", "config.toml")
}

func splitCSV(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func encodeScopes(scopes []auth.Scope) string {
	parts := make([]string, 0, len(scopes))
	for _, v := range scopes {
		parts = append(parts, string(v))
	}
	slices.Sort(parts)
	return strings.Join(parts, ",")
}

// resolveLibrary looks up a library by either numeric ID or name.
func resolveLibrary(ctx context.Context, repo *library.Repo, ref string) (models.Library, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return models.Library{}, fmt.Errorf("library reference must not be empty")
	}
	id, parseErr := strconv.ParseInt(ref, 10, 64)
	if parseErr != nil {
		return repo.GetByName(ctx, ref)
	}
	// All-digit ref: query both interpretations so a library literally named
	// "123" can never silently mask the ID match (and vice versa).
	byID, idErr := repo.Get(ctx, id)
	byName, nameErr := repo.GetByName(ctx, ref)
	idFound := idErr == nil
	nameFound := nameErr == nil
	switch {
	case idFound && nameFound:
		if byID.ID == byName.ID {
			return byID, nil
		}
		return models.Library{}, fmt.Errorf("library reference %q is ambiguous: matches id %d and name %q (id %d); pass an unambiguous form", ref, byID.ID, byName.Name, byName.ID)
	case idFound:
		if !errors.Is(nameErr, sql.ErrNoRows) {
			return models.Library{}, nameErr
		}
		return byID, nil
	case nameFound:
		if !errors.Is(idErr, sql.ErrNoRows) {
			return models.Library{}, idErr
		}
		return byName, nil
	case errors.Is(idErr, sql.ErrNoRows) && errors.Is(nameErr, sql.ErrNoRows):
		return models.Library{}, fmt.Errorf("library reference %q: %w", ref, sql.ErrNoRows)
	case !errors.Is(idErr, sql.ErrNoRows):
		return models.Library{}, idErr
	default:
		return models.Library{}, nameErr
	}
}

// validateQueueStatus checks --status for queue commands.
func validateQueueStatus(s string) error {
	if s == "" {
		return nil
	}
	switch s {
	case queue.StatusPending, queue.StatusProcessing, queue.StatusFailed, queue.StatusDone, queue.StatusDeferred:
		return nil
	default:
		return fmt.Errorf("invalid status %q (want pending, processing, failed, deferred, or done)", s)
	}
}

// validateScanStatus checks --status for scan results commands. scan_results
// rows only transition pending -> processing -> done; no code path writes
// "failed" to a scan_results row, so we deliberately reject it here even
// though the constant exists in the scan package.
func validateScanStatus(s string) error {
	if s == "" {
		return nil
	}
	switch s {
	case scan.StatusPending, scan.StatusProcessing, scan.StatusDone, scan.StatusDeferred:
		return nil
	default:
		return fmt.Errorf("invalid status %q (want pending, processing, done, or deferred)", s)
	}
}

func truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 3 {
		return s[:max]
	}
	return s[:max-3] + "..."
}

func runQueueCmd(ctx context.Context, out io.Writer, args QueueCmd) int {
	switch {
	case args.List != nil:
		return runQueueList(ctx, out, *args.List)
	case args.Failed != nil:
		return runQueueList(ctx, out, QueueListCmd{
			Status:     queue.StatusFailed,
			Limit:      args.Failed.Limit,
			ConfigPath: args.Failed.ConfigPath,
		})
	case args.Deferred != nil:
		return runQueueList(ctx, out, QueueListCmd{
			Status:     queue.StatusDeferred,
			Limit:      args.Deferred.Limit,
			ConfigPath: args.Deferred.ConfigPath,
		})
	case args.Retry != nil:
		return runQueueRetry(ctx, out, *args.Retry)
	case args.Clear != nil:
		return runQueueClear(ctx, out, *args.Clear)
	case args.Recheck != nil:
		return runQueueRecheck(ctx, out, *args.Recheck)
	default:
		_, _ = fmt.Fprintln(out, "missing queue subcommand")
		return 2
	}
}

func runQueueList(ctx context.Context, out io.Writer, args QueueListCmd) int {
	if err := validateQueueStatus(args.Status); err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)
	limit := args.Limit
	if limit < 0 {
		limit = 0
	}
	items, err := q.List(ctx, queue.ListFilter{Status: args.Status, Limit: limit})
	if err != nil {
		slog.Error("failed to list queue", "error", err)
		return 1
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tStatus\tPriority\tAttempts\tNextAttempt\tArtist\tTitle\tLastError")
	for _, item := range items {
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%d\t%d\t%s\t%s\t%s\t%s\n",
			item.ID,
			item.Status,
			item.Priority,
			item.Attempts,
			item.NextAttemptAt.UTC().Format(time.RFC3339),
			item.Inputs.Track.ArtistName,
			item.Inputs.Track.TrackName,
			truncate(item.LastError, 80),
		)
	}
	if err := tw.Flush(); err != nil {
		slog.Error("failed to write queue list", "error", err)
		return 1
	}
	return 0
}

func runQueueRetry(ctx context.Context, out io.Writer, args QueueRetryCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)
	item, err := q.Retry(ctx, args.ID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, _ = fmt.Fprintf(out, "queue: work item %d not found\n", args.ID)
		return 1
	case errors.Is(err, queue.ErrNotRetryable):
		_, _ = fmt.Fprintf(out, "queue: work item %d is not in failed status; refusing to retry\n"+
			"  (deferred rows are benign-miss cooldowns -- use `queue deferred` to inspect them;\n"+
			"   force an immediate re-check by re-enqueueing via the webhook path)\n", args.ID)
		return 1
	case err != nil:
		slog.Error("failed to retry queue item", "error", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "retried %d (status=%s, attempts=%d)\n", item.ID, item.Status, item.Attempts)
	return 0
}

func runQueueClear(ctx context.Context, out io.Writer, args QueueClearCmd) int {
	if !args.Done {
		_, _ = fmt.Fprintln(out, "queue clear requires --done")
		return 2
	}
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)
	if !args.Yes {
		count, err := q.CountDone(ctx)
		if err != nil {
			slog.Error("failed to count done queue rows", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "would delete %d completed queue rows; pass --yes to confirm\n", count)
		return 0
	}
	deleted, err := q.ClearDone(ctx)
	if err != nil {
		slog.Error("failed to clear done queue rows", "error", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "deleted %d completed queue rows\n", deleted)
	return 0
}

func runQueueRecheck(ctx context.Context, out io.Writer, args QueueRecheckCmd) int {
	if !args.Deferred && !args.Retired {
		_, _ = fmt.Fprintln(out, "queue recheck requires --deferred and/or --retired")
		return 2
	}

	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	q := queue.NewDBQueue(sqlDB)

	var libID *int64
	var libLabel string
	if strings.TrimSpace(args.Library) != "" {
		libRepo := library.New(sqlDB)
		lib, err := resolveLibrary(ctx, libRepo, args.Library)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", err)
			return 1
		}
		libID = &lib.ID
		libLabel = fmt.Sprintf(" for library %q (id=%d)", lib.Name, lib.ID)
	}

	if !args.Yes {
		if args.Deferred {
			count, err := q.CountRecheckDeferred(ctx, libID)
			if err != nil {
				slog.Error("failed to count deferred rows", "error", err)
				return 1
			}
			_, _ = fmt.Fprintf(out, "would revive %d deferred row(s)%s\n", count, libLabel)
		}
		if args.Retired {
			count, err := q.CountRecheckRetired(ctx, libID)
			if err != nil {
				slog.Error("failed to count retired rows", "error", err)
				return 1
			}
			_, _ = fmt.Fprintf(out, "would revive %d retired row(s)%s\n", count, libLabel)
		}
		_, _ = fmt.Fprintln(out, "pass --yes to confirm")
		return 0
	}

	if args.Deferred {
		n, err := q.RecheckDeferred(ctx, libID)
		if err != nil {
			slog.Error("failed to revive deferred rows", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "revived %d deferred row(s)%s\n", n, libLabel)
	}
	if args.Retired {
		n, err := q.RecheckRetired(ctx, libID)
		if err != nil {
			slog.Error("failed to revive retired rows", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "revived %d retired row(s)%s\n", n, libLabel)
	}
	return 0
}

func runScanResults(ctx context.Context, out io.Writer, args ScanResultsCmd) int {
	if err := validateScanStatus(args.Status); err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 2
	}
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	libRepo := library.New(sqlDB)
	scanRepo := scan.New(sqlDB)

	filter := scan.Filter{Status: args.Status, Limit: args.Limit}
	libNames := map[int64]string{}
	if strings.TrimSpace(args.Library) != "" {
		lib, err := resolveLibrary(ctx, libRepo, args.Library)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
				return 1
			}
			slog.Error("failed to resolve library", "error", err)
			return 1
		}
		filter.LibraryID = &lib.ID
		libNames[lib.ID] = lib.Name
	} else {
		libs, err := libRepo.List(ctx)
		if err != nil {
			slog.Error("failed to list libraries", "error", err)
			return 1
		}
		for _, v := range libs {
			libNames[v.ID] = v.Name
		}
	}

	// "deferred" is not a scan_results status value; it means scan_results whose
	// linked work_queue row is in benign-miss cooldown (they sit in 'processing'
	// on the scan side). Resolve those via the work_queue join.
	var results []models.ScanResult
	if args.Status == scan.StatusDeferred {
		results, err = scanRepo.ListDeferred(ctx, filter)
	} else {
		results, err = scanRepo.List(ctx, filter)
	}
	if err != nil {
		slog.Error("failed to list scan results", "error", err)
		return 1
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tLibrary\tStatus\tArtist\tTitle\tFilePath\tOutDir\tFilename")
	for _, r := range results {
		name, ok := libNames[r.LibraryID]
		if !ok {
			name = strconv.FormatInt(r.LibraryID, 10)
		}
		_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID,
			name,
			r.Status,
			r.Track.ArtistName,
			r.Track.TrackName,
			r.FilePath,
			r.Outdir,
			r.Filename,
		)
	}
	if err := tw.Flush(); err != nil {
		slog.Error("failed to write scan results", "error", err)
		return 1
	}
	return 0
}

func runScanClear(ctx context.Context, out io.Writer, args ScanClearCmd) int {
	cfg, err := config.Load(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}
	defer sqlDB.Close() //nolint:errcheck // best-effort close on shutdown

	libRepo := library.New(sqlDB)
	lib, err := resolveLibrary(ctx, libRepo, args.Library)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			_, _ = fmt.Fprintf(out, "library %q not found\n", args.Library)
			return 1
		}
		slog.Error("failed to resolve library", "error", err)
		return 1
	}

	scanRepo := scan.New(sqlDB)
	workQueue := queue.NewDBQueue(sqlDB)
	workQueue.SetRandomized(cfg.Queue.Randomize)
	if !args.Yes {
		count, err := scanRepo.CountByLibrary(ctx, lib.ID)
		if err != nil {
			slog.Error("failed to count scan results", "error", err)
			return 1
		}
		qDel, qUpd, err := workQueue.CountCancelByLibrary(ctx, lib.ID)
		if err != nil {
			slog.Error("failed to count work_queue cancellations", "error", err)
			return 1
		}
		_, _ = fmt.Fprintf(out, "would delete %d scan_results rows and cancel %d / update %d work_queue rows for library %q (id=%d); pass --yes to confirm\n", count, qDel, qUpd, lib.Name, lib.ID)
		return 0
	}
	// One transaction wraps both mutations so a failure on the scan_results
	// delete cannot leave the queue canceled while scan_results survive.
	// Cancel first: it reads the junction (work_queue_scan_results) which
	// cascades away when ClearByLibraryTx then deletes the scan_results.
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		slog.Error("failed to begin scan clear tx", "error", err)
		return 1
	}
	defer func() { _ = tx.Rollback() }() //nolint:errcheck // rollback is a no-op after commit
	qDel, qUpd, err := workQueue.CancelByLibraryTx(ctx, tx, lib.ID)
	if err != nil {
		slog.Error("failed to cancel work_queue rows", "error", err)
		return 1
	}
	deleted, err := scanRepo.ClearByLibraryTx(ctx, tx, lib.ID)
	if err != nil {
		slog.Error("failed to clear scan results", "error", err)
		return 1
	}
	if err := tx.Commit(); err != nil {
		slog.Error("failed to commit scan clear tx", "error", err)
		return 1
	}
	_, _ = fmt.Fprintf(out, "deleted %d scan_results rows and canceled %d / updated %d work_queue rows for library %q (id=%d)\n", deleted, qDel, qUpd, lib.Name, lib.ID)
	return 0
}
