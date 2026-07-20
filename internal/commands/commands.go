package commands

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	arg "github.com/alexflint/go-arg"
	"github.com/sydlexius/canticle/internal/app"
	"github.com/sydlexius/canticle/internal/auth"
	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/ffmpeg"
	"github.com/sydlexius/canticle/internal/langguard"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/logging"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/petitlyrics"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/prune"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/realign"
	"github.com/sydlexius/canticle/internal/scan"
	"github.com/sydlexius/canticle/internal/scanfail"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/secrets"
	"github.com/sydlexius/canticle/internal/server"
	"github.com/sydlexius/canticle/internal/servetls"
	"github.com/sydlexius/canticle/internal/trustnet"
	"github.com/sydlexius/canticle/internal/verification"
	"github.com/sydlexius/canticle/internal/watcher"
	"github.com/sydlexius/canticle/internal/web"
	"github.com/sydlexius/canticle/internal/webauth"
	"github.com/sydlexius/canticle/internal/worker"
)

// Args defines the CLI arguments for the application.
type Args struct {
	Fetch      *FetchCmd      `arg:"subcommand:fetch" help:"fetch lyrics once without HTTP server or DB queue"`
	Serve      *ServeCmd      `arg:"subcommand:serve" help:"run HTTP server, worker, and library scheduler"`
	Scan       *ScanCmd       `arg:"subcommand:scan" help:"scan configured libraries and enqueue missing lyrics"`
	Library    *LibraryCmd    `arg:"subcommand:library" help:"manage library roots"`
	Keys       *KeysCmd       `arg:"subcommand:keys" help:"manage API keys"`
	Admin      *AdminCmd      `arg:"subcommand:admin" help:"manage the web-UI admin account"`
	Secrets    *SecretsCmd    `arg:"subcommand:secrets" help:"manage encrypted-at-rest secrets"`
	Config     *ConfigCmd     `arg:"subcommand:config" help:"inspect or update configuration"`
	Queue      *QueueCmd      `arg:"subcommand:queue" help:"inspect or maintain the durable work queue"`
	Provenance *ProvenanceCmd `arg:"subcommand:provenance" help:"embed or inspect provenance tags in .lrc files"`
	Realign    *RealignCmd    `arg:"subcommand:realign" help:"re-attach orphaned .lrc/.txt sidecars to renamed audio files"`
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
	SweepInterval  *int    `arg:"--sweep-interval" help:"path-reconciliation sweep interval in seconds (default: server.sweep_interval_seconds or 21600; 0 disables the periodic sweep)"`
	WorkInterval   *int    `arg:"--work-interval" help:"worker poll interval in seconds (default: server.work_interval_seconds or api.cooldown; minimum 15)"`
}

// ScanCmd scans libraries once and enqueues cache misses. It also hosts
// nested inspection subcommands (results, clear). When neither nested
// subcommand is set, the legacy run-once scan path is taken.
type ScanCmd struct {
	ConfigPath           string   `arg:"--config" help:"path to config file (default: XDG)" default:""`
	Depth                int      `arg:"-d,--depth" help:"maximum recursion depth" default:"100"`
	Update               bool     `arg:"-u,--update" help:"re-fetch and overwrite existing .lrc files"`
	Upgrade              bool     `arg:"--upgrade" help:"re-fetch .txt lyrics to promote them"`
	BFS                  bool     `arg:"--bfs" help:"use breadth-first traversal"`
	EmbeddedLyrics       *string  `arg:"--embedded-lyrics" help:"embedded unsynced lyrics handling: off, respect, or extract (default: output.embedded_lyrics or off)"`
	Enrich               bool     `arg:"--enrich" help:"force recording enrichment (ISRC/MBID/duration) on for this scan, overriding per-library and global settings; mutually exclusive with --no-enrich"`
	NoEnrich             bool     `arg:"--no-enrich" help:"force recording enrichment off for this scan, overriding per-library and global settings; mutually exclusive with --enrich"`
	DetectInstrumental   bool     `arg:"--detect-instrumental" help:"force instrumental detection on for tracks enqueued by this scan, overriding per-library and global settings; mutually exclusive with --no-detect-instrumental"`
	NoDetectInstrumental bool     `arg:"--no-detect-instrumental" help:"force instrumental detection off for tracks enqueued by this scan, overriding per-library and global settings; mutually exclusive with --detect-instrumental"`
	Libraries            []string `arg:"--only,separate" help:"limit scan to named or numeric libraries; repeat to select more than one. Distinct from subcommand --library flags (which target a single library row)"`

	Results                          *ScanResultsCmd                          `arg:"subcommand:results" help:"list persisted scan_results rows"`
	Clear                            *ScanClearCmd                            `arg:"subcommand:clear" help:"delete persisted scan_results rows for a library"`
	Reconcile                        *ScanReconcileCmd                        `arg:"subcommand:reconcile" help:"re-validate instrumental markers against the current detector and clear stale ones"`
	ReconcileInstrumental            *ScanReconcileInstrumentalCmd            `arg:"subcommand:reconcile-instrumental" help:"classify deferred rows the detector has never scored and write markers where it agrees; makes no provider requests (issue #499)"`
	ReconcileInstrumentalRecalibrate *ScanReconcileInstrumentalRecalibrateCmd `arg:"subcommand:reconcile-instrumental-recalibrate" help:"re-decide stamped not-instrumental rows against the current detector thresholds using stored telemetry; makes no provider or detector-sidecar requests (issue #510)"`
	ReconcilePaths                   *ScanReconcilePathsCmd                   `arg:"subcommand:reconcile-paths" help:"delete queue/scan rows whose source audio file has vanished (renamed/merged/deleted)"`
	ReconcileIdentity                *ScanReconcileIdentityCmd                `arg:"subcommand:reconcile-identity" help:"re-read tags and correct run-together multi-value artist rows ingested before the fix (issue #466)"`
	ReconcileLRC                     *ScanReconcileLRCCmd                     `arg:"subcommand:reconcile-lrc" help:"rewrite existing .lrc sidecars that stack multiple timestamps on one line into the expanded, universally-readable form (issue #470)"`
	ReconcileMarkerProvenance        *ScanReconcileMarkerProvenanceCmd        `arg:"subcommand:reconcile-marker-provenance" help:"backfill provenance headers onto detector-written instrumental markers (#502)"`
	ReconcileDetectorStats           *ScanReconcileDetectorStatsCmd           `arg:"subcommand:reconcile-detector-stats" help:"attribute historical audio detections to the detector lane's statistics; reads recorded verdicts only, makes no detector-sidecar requests (issue #537)"`
}

// ScanReconcileDetectorStatsCmd attributes historical audio detections to the
// detector lane in lane_attempts, correcting an under-count left by instrumentals
// that settled before the detector became a lane (#537). Driven entirely from
// work_queue.instrumental_result; never re-runs detection. Dry-run unless --yes.
//
// There is no --library flag: work_queue rows reach a library only through the
// work_queue_scan_results junction, and a row whose scan_results link has since
// been cleared would silently drop out of a library-scoped run. Since the
// counters this corrects are global, scoping would introduce a partial-fill
// hazard for no operator benefit.
type ScanReconcileDetectorStatsCmd struct {
	Yes        bool   `arg:"--yes" help:"actually write the attributions (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of attributed rows (default: <db-dir>/reconcile-detector-stats-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileLRCCmd walks the configured library roots and rewrites any .lrc
// sidecar carrying compressed multi-timestamp lines (e.g. [t1][t2]text) into the
// expanded one-cue-per-line form, backing up the pristine original to
// <file>.lrc.orig. Dry-run unless --yes.
type ScanReconcileLRCCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default reconciles every library"`
	Yes        bool   `arg:"--yes" help:"actually rewrite files (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL record of rewritten files (default: <db-dir>/reconcile-lrc-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileMarkerProvenanceCmd backfills provenance headers onto detector-
// written instrumental markers so the scanner treats them as re-checkable (#502).
type ScanReconcileMarkerProvenanceCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default all"`
	Yes        bool   `arg:"--yes" help:"actually stamp markers (without it, prints what would change)"`
	Limit      int    `arg:"--limit" help:"cap the number of detector rows considered (0 = no cap)" default:"0"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of stamped markers (default: <db-dir>/reconcile-marker-provenance-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileIdentityCmd re-reads each scan_results row's file tags and
// corrects the stored artist / album-artist (and the coupled work_queue row)
// where a multi-value ID3v2.4 frame was ingested run-together before the fix.
// Dry-run unless --yes.
type ScanReconcileIdentityCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default reconciles every library"`
	Yes        bool   `arg:"--yes" help:"actually apply corrections (without it, prints what would change)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of corrected rows (default: <db-dir>/reconcile-identity-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcilePathsCmd deletes work_queue/scan_results rows whose source audio
// file no longer exists on disk, at Exact granularity. Dry-run unless --yes.
type ScanReconcilePathsCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default reconciles every library"`
	Yes        bool   `arg:"--yes" help:"actually delete rows (without it, prints what would be deleted)"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of pruned rows (default: <db-dir>/reconcile-paths-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileCmd re-runs the audio detector over instrumental-tagged tracks and,
// for tracks the current detector no longer classifies as instrumental, deletes the
// exact-match instrumental .txt marker and re-queues the row. Dry-run unless --yes.
type ScanReconcileCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id)" default:""`
	All        bool   `arg:"--all" help:"re-infer every instrumental-tagged row; default re-infers only borderline / cross-version / un-scored rows via stored telemetry"`
	Yes        bool   `arg:"--yes" help:"actually delete markers and re-queue rows (without it, prints what would change)"`
	Limit      int    `arg:"--limit" help:"maximum number of candidate rows to process (0 = unlimited)" default:"0"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of cleared rows (default: <db-dir>/reconcile-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileInstrumentalCmd classifies work_queue rows the detector has never
// scored (instrumental_result IS NULL) that are parked on a benign provider miss,
// writing an instrumental marker and completing the row where the detector agrees.
// Dry-run unless --yes.
//
// The inverse of ScanReconcileCmd, which re-checks already-confirmed verdicts and
// clears a marker on disagreement. Nothing previously reached the never-scored
// population, so a queue drained before the detector shipped stayed permanently
// unexamined (issue #499). Makes no provider requests.
type ScanReconcileInstrumentalCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default covers every library"`
	Yes        bool   `arg:"--yes" help:"actually write markers and settle rows (without it, prints what would change)"`
	Limit      int    `arg:"--limit" help:"maximum number of candidate rows to process (0 = unlimited)" default:"0"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of classified rows (default: <db-dir>/reconcile-instrumental-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// ScanReconcileInstrumentalRecalibrateCmd re-decides work_queue rows already
// stamped not-instrumental (instrumental_result = 0, a "vocal-gate rejection")
// against the CURRENT detector thresholds, using each row's stored telemetry
// (music_sum/vocal_peak/speech_mean) rather than re-scanning audio. A row a
// tightened-then-loosened threshold buried is settled and marked; a row
// scored by a different detector version is reset to never-classified for a
// real re-scan instead of being trusted on stale telemetry. Makes no
// provider or detector-sidecar requests. Dry-run unless --yes.
//
// The counterpart of ScanReconcileInstrumentalCmd, which classifies rows the
// detector has never scored at all; this one re-opens rows it already
// scored negative (issue #510).
type ScanReconcileInstrumentalRecalibrateCmd struct {
	Library    string `arg:"--library" help:"limit to a single library (name or numeric id); default covers every library"`
	Yes        bool   `arg:"--yes" help:"actually write markers and settle/reset rows (without it, prints what would change)"`
	Limit      int    `arg:"--limit" help:"maximum number of candidate rows to process (0 = unlimited)" default:"0"`
	Backup     string `arg:"--backup" help:"path for the JSONL backup of changed rows (default: <db-dir>/reconcile-instrumental-recalibrate-backup-<ts>.jsonl)" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
	// Reverse selects the TIGHTENING direction: re-decide rows already settled
	// instrumental and revert the ones a lowered vocal_max_confidence now
	// rejects, removing their detector-written markers. The default (loosening)
	// direction only ever promotes rows the old thresholds buried.
	Reverse bool `arg:"--reverse" help:"re-decide CONFIRMED instrumentals under tightened thresholds, reverting the ones that no longer qualify (default direction promotes buried rows instead)"`
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

// AdminCmd manages the web-UI admin account.
type AdminCmd struct {
	SetPassword *AdminSetPasswordCmd `arg:"subcommand:set-password" help:"set the web-UI admin password"`
}

// AdminSetPasswordCmd sets an existing web-UI admin's password (#545).
//
// This is the supported rotation path and the escape hatch when the UI is
// unreachable: /setup is loopback/trusted-CIDR only (#461), and the env
// bootstrap refuses to overwrite an existing admin, so without this the only
// way to change the password was deleting the user row from the database.
//
// The password is read from stdin rather than taken as a flag, so it never
// lands in shell history, the process table, or a CI log.
type AdminSetPasswordCmd struct {
	User       string `arg:"--user" help:"username whose password to set" default:""`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
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

// SecretsCmd contains nested encrypted-secret-store subcommands.
type SecretsCmd struct {
	Import *SecretsImportCmd `arg:"subcommand:import" help:"encrypt the current effective secret(s) into the DB store"`
	Set    *SecretsSetCmd    `arg:"subcommand:set" help:"set one secret by name from stdin (never on argv)"`
	List   *SecretsListCmd   `arg:"subcommand:list" help:"list stored secret names and updated_at (never values)"`
}

// SecretsImportCmd encrypts the currently effective plaintext secret(s) into the
// DB store, resolving the normal precedence but skipping the DB tier as a source.
type SecretsImportCmd struct {
	Token      bool   `arg:"--token" help:"import only the Musixmatch token"`
	Webhook    bool   `arg:"--webhook" help:"import only the webhook API key"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// SecretsSetCmd sets one secret by name. The value is read from stdin (prompt or
// pipe), never from argv, since argv lands in shell history and ps.
type SecretsSetCmd struct {
	Name       string `arg:"positional,required" help:"secret name: musixmatch_token or webhook_api_key"`
	Value      string `arg:"positional" help:"DO NOT pass the value here; it is rejected (use stdin)"`
	ConfigPath string `arg:"--config" help:"path to config file (default: XDG)" default:""`
}

// SecretsListCmd lists stored secret names and their updated_at, never values.
type SecretsListCmd struct {
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
	parser, err := arg.NewParser(arg.Config{Program: "canticle", Out: out}, parseTarget)
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
			return runServe(ctx, out, legacyServe(legacy), deps.NewFetcher, deps.NewWriter)
		}
		return runFetch(ctx, out, legacyFetch(legacy), deps.NewFetcher, deps.NewWriter, deps.NewApp)
	case args.Fetch != nil:
		return runFetch(ctx, out, *args.Fetch, deps.NewFetcher, deps.NewWriter, deps.NewApp)
	case args.Serve != nil:
		return runServe(ctx, out, *args.Serve, deps.NewFetcher, deps.NewWriter)
	case args.Scan != nil:
		return runScanCmd(ctx, out, *args.Scan)
	case args.Library != nil:
		return runLibrary(ctx, out, *args.Library)
	case args.Keys != nil:
		return runKeys(ctx, out, *args.Keys)
	case args.Admin != nil:
		return runAdmin(ctx, out, os.Stdin, *args.Admin)
	case args.Secrets != nil:
		return runSecrets(ctx, out, *args.Secrets)
	case args.Config != nil:
		return runConfig(out, *args.Config)
	case args.Queue != nil:
		return runQueueCmd(ctx, out, *args.Queue)
	case args.Provenance != nil:
		return runProvenance(ctx, out, *args.Provenance)
	case args.Realign != nil:
		return runRealign(ctx, out, *args.Realign)
	case args.Completion != nil:
		return runCompletion(out, *args.Completion)
	default:
		_, _ = fmt.Fprintln(out, "missing subcommand")
		return 2
	}
}

// topLevelSubcommands is the set of subcommand names declared on Args, derived
// from its struct tags rather than hand-maintained.
//
// It used to be a hardcoded map, and that shipped a broken command in v1.20.0:
// `admin` was added to Args, to the dispatch switch, and to the completion
// tables, but not to the map. Because this function decides whether to parse the
// modern subcommand tree or the legacy CLI, an unlisted command fell through to
// the legacy parser and died with "unknown argument --user" -- it could not be
// invoked at all. Unit tests missed it because they called the handlers
// directly, never through the parser.
//
// Deriving the set from the same struct the parser uses removes the drift
// entirely: a new subcommand is recognized the moment it is declared, with no
// second place to remember. Computed once at init; Args is static.
var topLevelSubcommands = func() map[string]bool {
	out := make(map[string]bool)
	t := reflect.TypeOf(Args{})
	for i := range t.NumField() {
		if name := subcommandTagName(t.Field(i).Tag.Get("arg")); name != "" {
			out[name] = true
		}
	}
	return out
}()

// subcommandTagName extracts the name from a go-arg `subcommand:<name>` tag,
// returning "" for any tag that does not declare one.
func subcommandTagName(tag string) string {
	for part := range strings.SplitSeq(tag, ",") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(part), "subcommand:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

func usesSubcommand(rawArgs []string) bool {
	if len(rawArgs) == 0 {
		return false
	}
	if rawArgs[0] == "-h" || rawArgs[0] == "--help" {
		return true
	}
	return topLevelSubcommands[rawArgs[0]]
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

// logStartupBanner emits a startup banner to the console (out) and via slog.
// The console receives the build version line followed by the full
// FormatConfigText dump. slog INFO receives the version and key diagnostic
// settings; slog DEBUG receives the complete structured config via
// ConfigToSlogAttrs. Sensitive fields are redacted in all output paths.
// cliSrc is a set of config field paths that were overridden on the command
// line; envSrc is the set whose environment override actually applied (from
// config.LoadWithSources). Both may be nil.
func logStartupBanner(ctx context.Context, cfg config.Config, ver string, out io.Writer, envSrc, cliSrc map[string]bool) {
	// Console: version line + full TOML-style config dump.
	_, _ = fmt.Fprintf(out, "%s\n\n", ver)
	_, _ = fmt.Fprint(out, config.FormatConfigText(cfg, envSrc, cliSrc))
	_, _ = fmt.Fprintln(out)

	// slog INFO: version + key diagnostic settings (always visible at INFO level).
	slog.InfoContext(ctx, "startup",
		"version", ver,
		"api_token_set", cfg.API.Token != "",
		"output_dir", cfg.Output.Dir,
		"log_level", cfg.Logging.Level,
		"server_addr", cfg.Server.Addr,
	)

	// slog DEBUG: full structured config dump.
	attrs := config.ConfigToSlogAttrs(cfg, envSrc, cliSrc)
	slog.LogAttrs(ctx, slog.LevelDebug, "startup config", attrs...)
}

func runFetch(ctx context.Context, out io.Writer, args FetchCmd, newFetcher func(string) musixmatch.Fetcher, newWriter func(roots ...string) lyrics.Writer, newApp func(musixmatch.Fetcher, lyrics.Writer, *queue.InputsQueue, int, string) AppRunner) int {
	if len(args.Song) == 0 {
		_, _ = fmt.Fprintln(out, "missing required positional argument: Song")
		return 2
	}
	cfg, envSrc, err := config.LoadWithSources(args.ConfigPath)
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

	// Emit startup banner after initLogging (log file is ready) and after all
	// CLI flag overrides are applied so the banner reflects effective values.
	fetchBannerCfg := cfg
	fetchBannerCfg.API.Token = token
	fetchBannerCfg.API.Cooldown = cooldown
	fetchBannerCfg.Output.Dir = outdir
	fetchCLISrc := map[string]bool{}
	if args.Token != "" {
		fetchCLISrc["api.token"] = true
	}
	if args.Cooldown != nil {
		fetchCLISrc["api.cooldown"] = true
	}
	if args.Outdir != nil {
		fetchCLISrc["output.dir"] = true
	}
	logStartupBanner(ctx, fetchBannerCfg, VersionString(), out, envSrc, fetchCLISrc)
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

func runServe(ctx context.Context, out io.Writer, args ServeCmd, newFetcher func(string) musixmatch.Fetcher, newWriter func(roots ...string) lyrics.Writer) int {
	cfg, envSrc, err := config.LoadWithSources(args.ConfigPath)
	if err != nil {
		slog.Error("failed to load config", "error", err)
		return 1
	}
	initLogging(cfg)
	token := args.Token
	if token == "" {
		token = cfg.API.Token
	}
	// serve mode always writes next to the audio file; output.dir/MXLRC_OUTPUT_DIR/--outdir
	// are fetch-mode-only and are ignored here. The fixed default is used as the
	// metadata-only webhook fallback when no library path is resolvable.
	outdir := config.DefaultOutputDir
	if args.Outdir != nil || envSrc["output.dir"] || cfg.Output.Dir != config.DefaultOutputDir {
		slog.Warn("output.dir/MXLRC_OUTPUT_DIR/--outdir are ignored in serve mode; " +
			"lyrics are written next to the audio file, and the metadata-only webhook fallback " +
			"uses the internal default (\"lyrics\"); use CLI --outdir for fetch mode")
	}
	addr := cfg.Server.Addr
	if args.Listen != nil {
		addr = *args.Listen
	}

	// Open the DB before the banner so the encrypted secret store can serve as
	// the lowest-precedence source for the token and webhook key, and the banner
	// reflects the effective (redacted) values.
	sqlDB, err := db.Open(ctx, cfg.DB.Path)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		return 1
	}

	// Resolve the master key and build the encrypted secret store. The key is
	// auto-generated on first use (0600 key file) on all platforms including
	// Docker; MXLRC_MASTER_KEY is an optional override for key/data separation.
	// Any resolution failure (malformed key, unreadable key file) is fatal.
	store, err := resolveSecretStore(cfg, sqlDB)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to initialize secret store", "error", err)
		return 1
	}

	// DB is the lowest-precedence source for both secrets: consulted only when the
	// higher tiers (CLI/env/TOML) are empty, and a DB-sourced value is never
	// auto-persisted back.
	token, tokenFromDB, err := resolveTokenWithStore(ctx, token, store)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to resolve musixmatch token", "error", err)
		return 1
	}
	webhookKeys, webhookFromDB, err := resolveWebhookKeysWithStore(ctx, cfg.Server.WebhookAPIKeys, store)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to resolve webhook API key", "error", err)
		return 1
	}

	// Emit startup banner after initLogging (log file is ready) and after all
	// CLI flag overrides and DB-tier fallbacks are applied so the banner reflects
	// effective values. DB-sourced secrets are redacted exactly like plaintext.
	bannerCfg := cfg
	bannerCfg.API.Token = token
	bannerCfg.Server.WebhookAPIKeys = webhookKeys
	bannerCfg.Server.Addr = addr
	serveCLISrc := map[string]bool{}
	if args.Token != "" {
		serveCLISrc["api.token"] = true
	}
	if args.Listen != nil {
		serveCLISrc["server.addr"] = true
	}
	if tokenFromDB {
		slog.Info("musixmatch token sourced from encrypted secret store")
	}
	if webhookFromDB {
		slog.Info("webhook API key sourced from encrypted secret store")
	}
	logStartupBanner(ctx, bannerCfg, VersionString(), out, envSrc, serveCLISrc)
	// Select the primary lyrics provider. When Musixmatch is primary and no token
	// is configured, do NOT abort: degrade so the web UI still serves and the
	// operator can add a token (or switch the primary provider) in Settings (#385).
	// resolveServeProvider returns a no-op provider and signals lyricsDisabled so
	// the worker/scheduler never start (the queue is left untouched rather than
	// churned into permanent miss retirements).
	fetcher, selErr := selectedProvider(cfg, token, newFetcher)
	fetcher, musixmatchInactive, lyricsDisabled, err := resolveServeProvider(fetcher, selErr)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to configure lyrics provider", "error", err)
		return 1
	}
	// Resolve ffmpeg once for both the verifier and the instrumental detector.
	// Resolution auto-provisions a checksum-pinned static build when ffmpeg is
	// neither configured nor on PATH; it is skipped entirely unless verification
	// is enabled or a classifier is configured (the nil-guards below also short-
	// circuit, but resolving here avoids a redundant download per constructor).
	var ffmpegPath string
	if cfg.Verification.Enabled || strings.TrimSpace(cfg.InstrumentalDetector.ClassifierURL) != "" {
		ffmpegPath, err = resolveFFmpeg(ctx, cfg)
		if err != nil {
			_ = sqlDB.Close()
			slog.Error("failed to resolve ffmpeg", "error", err)
			return 1
		}
	}
	verifier, err := newVerifier(cfg, ffmpegPath)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to configure verification", "error", err)
		return 1
	}

	authSvc, err := keyService(ctx, sqlDB, webhookKeys)
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
	// One shared cache repo across the worker, scheduler, and watcher so the
	// /metrics cache hit/lookup counters (#308) cover every cache read in serve
	// mode, not just the worker's. Its hit/lookup counters are process-lifetime
	// and exposed via CacheStats.
	cacheRepo := cache.New(sqlDB)
	w := worker.New(workQ, cacheRepo, fetcher, writer)
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
	audioDetector, err := newAudioDetector(cfg, ffmpegPath)
	if err != nil {
		slog.Error("failed to configure instrumental detector", "error", err)
		return 1
	}
	configureWorkerAudioDetector(w, audioDetector)
	w.SetInstrumentalDetectionDefault(cfg.InstrumentalDetector.Enabled)
	w.SetDetectorOrdering(cfg.InstrumentalDetector.Ordering)
	configureWorkerGuard(w, newGuard(cfg))
	// Wire the DB-backed provider outcome recorder so hits and misses are persisted
	// and exposed via GET /metrics (mxlrcgo_provider_hits_total{lane},
	// mxlrcgo_provider_misses_total{lane}).
	configureWorkerProviderRecorder(w, workQ)

	// Trusted-network policy gates GET /metrics (#204, S3). CIDRs were already
	// validated at config load; rebuild here for the listener. A build failure
	// would mean validation was bypassed, so it is a fatal startup error. Built
	// before the cancelable run context so an early return cannot leak it.
	trustPolicy, err := trustnet.NewPolicy(cfg.Server.TrustedNetworks.Cidrs, cfg.Server.TrustedNetworks.TrustedProxies)
	if err != nil {
		slog.Error("failed to build trusted-network policy", "error", err)
		return 1
	}

	// Build the authenticated web UI subsystem (#204, lane 4) only when the UI is
	// enabled. It backs both the browser-session login and the first-run
	// onboarding flow, and bootstraps an env-provided admin (#259). When the UI
	// is disabled all returns are nil and serve mode keeps its pre-#204 behavior
	// (webhook + health only).
	webAuthSvc, webAuth, onboarding, err := buildWebAuth(ctx, cfg, sqlDB, store, trustPolicy, version)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to initialize web authentication", "error", err)
		return 1
	}

	// Optional TLS (#204, lane 5): build the certificate manager from
	// [server.tls]. Returns nil when TLS is disabled (plain HTTP, the default).
	// The self-signed bootstrap persists under <dir(db_path)>/tls/. Built before
	// the run context so a bad cert/key or generation failure is a clean early
	// return rather than a leaked listener.
	certMgr, err := servetls.BuildCertManager(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile, cfg.Server.TLS.SelfSigned, filepath.Dir(cfg.DB.Path), cfg.Server.TLS.SelfSignedHosts)
	if err != nil {
		_ = sqlDB.Close()
		slog.Error("failed to initialize TLS", "error", err)
		return 1
	}

	// Bind the redirect listener eagerly (before starting goroutines) so a bind
	// failure is a clean startup error. An operator who explicitly configured
	// redirect_http expects it to work; silently skipping it would be confusing.
	var redirectLn net.Listener
	if certMgr != nil && cfg.Server.TLS.RedirectHTTP != "" {
		ln, listenErr := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Server.TLS.RedirectHTTP)
		if listenErr != nil {
			_ = sqlDB.Close()
			slog.Error("HTTP redirect listener failed to bind; aborting startup", "addr", cfg.Server.TLS.RedirectHTTP, "error", listenErr)
			return 1
		}
		redirectLn = ln
	}

	runCtx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// Start the worker and scheduler only when a lyrics provider is active. When
	// lyricsDisabled (Musixmatch primary, no token, no fallback; #385) the queue
	// must stay untouched: starting the worker would retry every track and retire
	// it as a permanent miss, so the operator who later adds a token would find
	// nothing left to fetch. The web server, watcher, and session sweeper still
	// start so the UI is reachable to add the token.
	// Credit historical detector settles to provider_outcomes (#548) BEFORE the
	// worker or scheduler can start. Ordering is load-bearing, not cosmetic: the
	// live writer's two counter writes are non-transactional (see the KNOWN
	// IMPRECISION note in internal/detectorbackfill/provider_outcomes.go), so a
	// row caught mid-write -- already counted, not yet provider_lane-stamped --
	// would be seen as uncredited by this pass and permanently double-credited.
	// Running before any live writer exists closes that window entirely, which
	// running synchronously AFTER the worker goroutine does not.
	runProviderOutcomesBackfill(runCtx, sqlDB)

	if !lyricsDisabled {
		wg.Add(2)
		go func() {
			defer wg.Done()
			runWorkerLoop(runCtx, w, serveWorkerInterval(cfg, args))
		}()
		go func() {
			defer wg.Done()
			runScheduler(runCtx, sqlDB, cfg, args, cacheRepo)
		}()
	}
	// Build the watcher config from the central config (TOML + env, env > file)
	// rather than reading the environment directly, so the [watcher] section and
	// the settings UI drive it. New clamps a non-positive Debounce/MaxDirs to the
	// watcher package default, so DebounceMS <= 0 keeps the documented behavior.
	if watchCfg := watcherConfigFromCentral(cfg); watchCfg.Enabled {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runWatcher(runCtx, sqlDB, args, watchCfg, cfg, cacheRepo)
		}()
	}
	// Periodic path-reconciliation sweep (#453): deletes queue/scan rows whose
	// source file vanished. It is the disk-cheap backstop to the watcher's
	// reactive prune, so it runs regardless of the watcher toggle. A zero interval
	// disables it (the reactive prune still runs when the watcher is enabled).
	if sweepInterval := serveSweepInterval(cfg, args); sweepInterval > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSweeper(runCtx, sqlDB, sweepInterval)
		}()
	}
	// One-shot identity-repair backfill (#466): correct run-together multi-value
	// artist rows ingested before the tag fix. It re-reads every scan_results
	// file's tags, so it runs once in the background (marker-gated, non-blocking)
	// rather than delaying startup; the idempotent CLI `scan reconcile-identity`
	// covers reruns and fetch-only deployments.
	wg.Add(1)
	go func() {
		defer wg.Done()
		runIdentityBackfill(runCtx, sqlDB)
	}()
	// Background session sweeper: periodically delete expired/revoked sessions,
	// mirroring the worker/scheduler goroutine + context-cancel pattern. Only
	// runs when the authenticated UI is mounted (there are no sessions otherwise).
	if webAuthSvc != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runSessionSweeper(runCtx, webAuthSvc)
		}()
	}

	handlerOpts := []server.Option{
		server.WithReadiness(sqlDB),
		server.WithStatusReporter(workQ),
		// WithMetricsReporter is required: omitting it causes GET /metrics to return 500.
		// Decorate the queue reporter with the shared cache repo so /metrics also
		// exposes the lyrics-cache hit/lookup counters (#308).
		server.WithMetricsReporter(server.WithCacheStats(workQ, cacheRepo)),
		// GET /metrics is gated by the trusted-network allowlist (loopback
		// implicitly trusted); no API key or session is required (#204, S3).
		server.WithTrustedNetworks(trustPolicy),
		server.WithInventory(scan.New(sqlDB)),
		server.WithAllowedRoots(allowedRoots),
	}
	// Reactive realign on Lidarr rename/import/upgrade webhooks (#450), gated by
	// realign.enabled. Self-heals a sidecar stranded by a rename/move without a
	// manual `realign` run; confined to the payload's directories.
	if cfg.Realign.Enabled {
		handlerOpts = append(handlerOpts, server.WithRealigner(serverRealigner{
			realigner:  realign.New(library.New(sqlDB), cfg.Realign),
			backupPath: realignServeBackupPath(cfg),
		}))
	}
	if cfg.Server.WebUIEnabled {
		// Mount the authenticated web UI (#204, lane 4): session login gates the
		// page routes, and onboarding redirects them to /setup until an admin
		// exists. bannerCfg is the effective config snapshot (resolved
		// token/webhook keys/outdir/addr) so the Config view matches the startup
		// banner. Default is OFF (the #210 gate is unchanged).
		handlerOpts = append(handlerOpts,
			server.WithWebUIAuth(bannerCfg, version, webAuth),
			server.WithOnboarding(onboarding),
			// Back the Reports workspace with the same DB the rest of serve mode
			// uses; the handler builds a read-only reports.Repo from it (#211).
			server.WithReportsDB(sqlDB),
			// Enable the settings write path (#288 Phase 2): writes go to the
			// RESOLVED config file (never ""), and secret-field saves route to the
			// encrypted store rather than the TOML.
			server.WithSettingsWriter(config.ResolveConfigPath(args.ConfigPath), store),
			// Back the webhook key management page (#300) with the same managed key
			// service that authenticates webhook calls, so keys created in the UI are
			// immediately usable for webhook auth.
			server.WithKeyManagerUI(authSvc),
			// Surface the tokenless-Musixmatch banner on every authenticated shell
			// page when serve started without a usable Musixmatch token (#385).
			server.WithMusixmatchInactive(musixmatchInactive),
		)
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           server.NewHandler(authSvc, workQ, outdir, handlerOpts...),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if certMgr != nil {
		srv.TLSConfig = servetls.TLSConfig(certMgr)
	}
	go func() {
		<-runCtx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
		defer shutdownCancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			slog.Warn("HTTP server shutdown failed", "error", err)
		}
	}()

	// Optional plain-HTTP redirect listener (#204, lane 5): listener already bound
	// above (fail-fast on error); here we just start serving on it.
	if redirectLn != nil {
		redirectSrv := buildRedirectServer(cfg.Server.TLS.RedirectHTTP, addr)
		wg.Add(1)
		go func() {
			defer wg.Done()
			go func() {
				<-runCtx.Done()
				shutdownCtx, shutdownCancel := context.WithTimeout(context.WithoutCancel(runCtx), 10*time.Second)
				defer shutdownCancel()
				if err := redirectSrv.Shutdown(shutdownCtx); err != nil {
					slog.Warn("HTTP redirect server shutdown failed", "error", err)
				}
			}()
			slog.Info("starting HTTP->HTTPS redirect listener", "addr", redirectSrv.Addr, "target", addr)
			if err := redirectSrv.Serve(redirectLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("HTTP redirect server failed", "error", err)
			}
		}()
	}

	code := 0
	if certMgr != nil {
		slog.Info("starting HTTPS server", "addr", addr)
		// Empty cert/key paths: the certificate is supplied via TLSConfig.GetCertificate.
		if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTPS server failed", "error", err)
			code = 1
		}
	} else {
		slog.Info("starting HTTP server", "addr", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server failed", "error", err)
			code = 1
		}
	}
	cancel()
	wg.Wait()
	if err := sqlDB.Close(); err != nil {
		slog.Warn("failed to close database", "error", err)
	}
	return code
}

// buildRedirectServer constructs the HTTP->HTTPS redirect http.Server. Extracted so
// the timeout values can be verified in tests without starting a live listener.
func buildRedirectServer(redirectAddr, tlsAddr string) *http.Server {
	return &http.Server{
		Addr:              redirectAddr,
		Handler:           servetls.RedirectHandler(tlsAddr),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
}

// buildWebAuth constructs the authenticated web UI subsystem for serve mode
// (#204, lane 4): the session/onboarding service over the DB, the auth
// middleware, and the onboarding flow. It also bootstraps an env-provided admin
// (#259) before returning. When the web UI is disabled it is a no-op returning
// all-nil components (and no error), so the caller keeps the pre-#204 behavior.
// A bootstrap failure (e.g. a too-short env password) is returned as a fatal
// error.
func buildWebAuth(ctx context.Context, cfg config.Config, sqlDB *sql.DB, store secrets.Store, policy *trustnet.Policy, version string) (*webauth.Service, *web.Auth, *web.Onboarding, error) {
	if !cfg.Server.WebUIEnabled {
		// Credentials were supplied for a feature that is switched off. Without
		// this warning the operator gets a healthy startup, no admin account, and
		// no explanation: the bootstrap below is never reached, so the variables
		// silently do nothing. That is the same failure shape as editing the
		// password variable after first run and believing a rotation happened
		// (#545), and it is worth naming rather than inferring from an absence.
		//
		// Deliberately NOT bootstrapping anyway: an admin account is meaningless
		// with no UI to sign in to, and creating one would make the later
		// idempotent skip fire on a genuine first run once the UI is enabled.
		if os.Getenv(envWebAdminUser) != "" || os.Getenv(envWebAdminPass) != "" {
			slog.Warn("web admin bootstrap IGNORED: " + envWebAdminUser + "/" + envWebAdminPass +
				" are set but the web UI is disabled, so no admin account is created. " +
				"Enable it with web_ui_enabled = true under [server] (or MXLRC_WEB_UI_ENABLED=true), " +
				"or remove the variables to keep the password out of the environment")
		}
		return nil, nil, nil, nil
	}
	svc := webauth.NewService(webauth.NewSQLUserStore(sqlDB), webauth.NewSQLSessionStore(sqlDB))
	// #259: bootstrap the first admin from the environment before serving so a
	// Docker deployment can come up with credentials and no interactive setup.
	if err := bootstrapAdminFromEnv(ctx, svc); err != nil {
		return nil, nil, nil, fmt.Errorf("bootstrap web admin from environment: %w", err)
	}
	auth := web.NewAuth(svc, policy, version)
	onboarding := web.NewOnboarding(svc, store, auth, policy, version)
	return svc, auth, onboarding, nil
}

// Environment variables for the #259 web-admin bootstrap. Both must be set for a
// bootstrap to occur; the password is read directly here and never copied into
// the Config struct (keeping it isolated, per the #259 plan).
const (
	envWebAdminUser = "MXLRC_WEBAUTH_ADMIN_USER"
	envWebAdminPass = "MXLRC_WEBAUTH_ADMIN_PASSWORD" //nolint:gosec // G101: this is an env var NAME, not a hardcoded credential
)

// bootstrapAdminFromEnv creates the first web-UI admin from MXLRC_WEBAUTH_ADMIN_USER
// and MXLRC_WEBAUTH_ADMIN_PASSWORD when no admin exists yet (#259). It is:
//   - opt-in: a no-op when neither var is set;
//   - idempotent: a no-op (never an overwrite) when an admin already exists;
//   - strict: only one var set logs a warning and skips; a too-short password is
//     a fatal startup error (returned), never a silent skip.
//
// The password is never logged. On success it logs a non-sensitive line naming
// only the username.
func bootstrapAdminFromEnv(ctx context.Context, svc *webauth.Service) error {
	user := strings.TrimSpace(os.Getenv(envWebAdminUser))
	pass := os.Getenv(envWebAdminPass)
	if user == "" && pass == "" {
		return nil // not requested
	}
	if user == "" || pass == "" {
		slog.Warn("web admin env-bootstrap skipped: both " + envWebAdminUser + " and " + envWebAdminPass + " are required")
		return nil
	}
	has, err := svc.HasUsers(ctx)
	if err != nil {
		return fmt.Errorf("check for existing admin: %w", err)
	}
	if has {
		// WARN, not INFO. An operator who edited the bootstrap password and
		// restarted expecting a rotation gets a healthy service and, previously, a
		// reassuring INFO line -- while the OLD password stayed live. Every
		// outward sign said the credential had been rotated when it had not
		// (#545). Say plainly that the value is being ignored, and name the
		// command that actually rotates it.
		slog.Warn("web admin env-bootstrap SKIPPED: an admin already exists, so "+
			envWebAdminPass+" is being IGNORED. If you changed it expecting a password "+
			"rotation, it did NOT take effect and the previous password is still active. "+
			"Rotate with: canticle admin set-password --user <name>",
			"user", user)
		return nil
	}
	if len(pass) < webauth.MinPasswordLength {
		return fmt.Errorf("%s must be at least %d characters", envWebAdminPass, webauth.MinPasswordLength)
	}
	if _, err := svc.Setup(ctx, user, pass); err != nil {
		if errors.Is(err, webauth.ErrUserExists) {
			// Lost a race to another bootstrap path; the account now exists.
			slog.Info("web admin env-bootstrap skipped: an admin account already exists")
			return nil
		}
		return fmt.Errorf("create admin: %w", err)
	}
	slog.Info("bootstrapped web admin from environment", "user", user)
	return nil
}

// sessionSweepInterval is how often the background sweeper deletes expired and
// revoked sessions.
const sessionSweepInterval = time.Hour

// runSessionSweeper periodically purges expired/revoked sessions until ctx is
// canceled, reusing the worker/scheduler goroutine cadence pattern. It sweeps
// once at startup so a long-running process does not wait a full interval to
// reclaim sessions that expired while it was down.
func runSessionSweeper(ctx context.Context, svc *webauth.Service) {
	sweepSessions(ctx, svc)
	ticker := time.NewTicker(sessionSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweepSessions(ctx, svc)
		}
	}
}

// sweepSessions runs one cleanup pass, logging a non-fatal warning on failure
// (a canceled context during shutdown is expected and not warned).
func sweepSessions(ctx context.Context, svc *webauth.Service) {
	n, err := svc.CleanExpiredSessions(ctx)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			slog.Warn("session sweep failed", "error", err)
		}
		return
	}
	if n > 0 {
		slog.Info("cleaned expired sessions", "count", n)
	}
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

// serveSweepInterval resolves the path-reconciliation sweep interval. Precedence:
// CLI --sweep-interval > config server.sweep_interval_seconds (default 21600).
// A zero result disables the periodic sweep (the reactive watcher prune remains).
func serveSweepInterval(cfg config.Config, args ServeCmd) time.Duration {
	seconds := cfg.Server.SweepIntervalSeconds
	if args.SweepInterval != nil {
		seconds = *args.SweepInterval
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

// errNoMusixmatchToken is the typed sentinel selectedProvider returns when
// Musixmatch is the primary provider and no API token is configured. serve mode
// detects it via errors.Is to degrade gracefully (serve the web UI so the
// operator can add a token in Settings) instead of aborting startup (#385).
var errNoMusixmatchToken = errors.New("no API token provided for the musixmatch provider: use --token, MUSIXMATCH_TOKEN, MXLRC_API_TOKEN, or config file")

// noopFetcher is a lyrics fetcher that always reports a miss. It backs the no-op
// provider serve mode constructs when Musixmatch is primary, no token is set,
// and no fallback is available (#385): the worker is built nil-safe even though
// its loop is never started in that state (lyricsDisabled), so the fetcher is
// never actually invoked.
type noopFetcher struct{}

func (noopFetcher) FindLyrics(context.Context, models.Track) (models.Song, error) {
	return models.Song{}, musixmatch.ErrNotFound
}

// resolveServeProvider decides the effective lyrics fetcher and the serve-mode
// degrade flags from the selectedProvider result (#385). It is the testable core
// of the startup degrade decision:
//
//   - no error: pass the primary through; lyrics enabled.
//   - errNoMusixmatchToken: Musixmatch is the primary provider and no token is
//     configured. Return a no-op provider so worker construction stays nil-safe,
//     and signal musixmatchInactive (the token banner shows) AND lyricsDisabled
//     (the worker/scheduler must not start, so the queue is never churned into
//     permanent miss retirements). The operator adds a token, or switches the
//     primary provider, in Settings and restarts.
//   - any other error: propagated unchanged (a real, fatal misconfiguration).
//
// It deliberately does NOT promote a configured fallback into the primary slot:
// worker.New labels the injected fetcher's lane as Musixmatch, so a promoted
// PetitLyrics fetcher would be metriced and cache-keyed inconsistently. A user
// who wants tokenless fetching sets that provider as the primary (the banner
// points them there), which is the no-error path above.
func resolveServeProvider(primary providers.LyricsProvider, selErr error) (fetcher providers.LyricsProvider, musixmatchInactive, lyricsDisabled bool, err error) {
	if selErr == nil {
		return primary, false, false, nil
	}
	if !errors.Is(selErr, errNoMusixmatchToken) {
		return nil, false, false, selErr
	}
	slog.Warn("no lyrics provider configured: Musixmatch is the primary provider and needs an API token. Serving the web UI so you can add one in Settings (or switch to a tokenless provider like PetitLyrics), then restart.")
	return providers.New(providers.Musixmatch, noopFetcher{}), true, true, nil
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
		return nil, errNoMusixmatchToken
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

// resolveFFmpeg picks the ffmpeg override from the existing config keys
// (verification's path takes precedence, then the detector's) and resolves it
// to an absolute executable path, auto-provisioning a pinned static build when
// neither a configured path nor a PATH ffmpeg is available. The provisioned
// cache lives beside the database so it inherits the same data directory
// (including the Docker /config short-circuit).
func resolveFFmpeg(ctx context.Context, cfg config.Config) (string, error) {
	override := strings.TrimSpace(cfg.Verification.FFmpegPath)
	if override == "" {
		override = strings.TrimSpace(cfg.InstrumentalDetector.FFmpegPath)
	}
	cacheDir := filepath.Join(filepath.Dir(cfg.DB.Path), "ffmpeg")
	return ffmpeg.Resolve(ctx, override, ffmpeg.Options{CacheDir: cacheDir})
}

func newVerifier(cfg config.Config, ffmpegPath string) (verification.Verifier, error) {
	if !cfg.Verification.Enabled {
		return nil, nil
	}
	return verification.NewHTTPVerifier(
		cfg.Verification.WhisperURL,
		cfg.Verification.SampleDurationSeconds,
		cfg.Verification.MinSimilarity,
		ffmpegPath,
	)
}

func configureWorkerVerification(w *worker.Worker, cfg config.Config, verifier verification.Verifier) {
	if verifier == nil {
		return
	}
	w.EnableVerification(verifier, cfg.Verification.MinConfidence)
}

// newAudioDetector builds the instrumental detector from config. It is built
// whenever a classifier URL is configured - decoupled from the global enable flag
// (#218) so per-library detection works even when the global default is off. The
// global default and per-item decisions gate whether the worker actually calls it.
// Returns (nil, nil) when no classifier URL is set; callers treat a nil return as
// "no classifier configured".
func newAudioDetector(cfg config.Config, ffmpegPath string) (detector.Detector, error) {
	if strings.TrimSpace(cfg.InstrumentalDetector.ClassifierURL) == "" {
		return nil, nil
	}
	return detector.NewHTTPDetector(detector.Config{
		ClassifierURL:         cfg.InstrumentalDetector.ClassifierURL,
		SampleDurationSeconds: cfg.InstrumentalDetector.SampleDurationSeconds,
		MinConfidence:         cfg.InstrumentalDetector.MinConfidence,
		InstrumentalClasses:   cfg.InstrumentalDetector.InstrumentalClasses,
		VocalClasses:          cfg.InstrumentalDetector.VocalClasses,
		VocalMaxConfidence:    cfg.InstrumentalDetector.VocalMaxConfidence,
		SpeechClasses:         cfg.InstrumentalDetector.SpeechClasses,
		SpeechMaxConfidence:   cfg.InstrumentalDetector.SpeechMaxConfidence,
		SpreadSamples:         cfg.InstrumentalDetector.SpreadSamples,
		FFmpegPath:            ffmpegPath,
		FFprobePath:           cfg.InstrumentalDetector.FFprobePath,
		CooldownSeconds:       cfg.InstrumentalDetector.CooldownSeconds,
		Version:               version, // version is the package-level var in version.go mirroring appversion.Version
	})
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

// configureWorkerProviderRecorder installs the provider-outcome recorder on the
// worker. A nil recorder is valid and leaves the worker in no-op mode (default).
func configureWorkerProviderRecorder(w *worker.Worker, r worker.ProviderRecorder) {
	w.SetProviderRecorder(r)
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

// detectorScanVersion returns the app version to stamp into a scan's ScanOptions
// for detector-marker version invalidation (#502), or "" when the audio detector
// is disabled -- so a disabled detector never reopens provisional markers on a
// version bump. It is the same value fed to detector.Config.Version.
func detectorScanVersion(cfg config.Config) string {
	if cfg.InstrumentalDetector.Enabled {
		return version
	}
	return ""
}

func runScheduler(ctx context.Context, sqlDB *sql.DB, cfg config.Config, args ServeCmd, cacheRepo *cache.CacheRepo) {
	// serve has no per-run detect override; resolve per library against the global
	// default (and the per-library setting) at enqueue time.
	// Periodic scans realign only when realign.enabled AND realign.on_scan.
	rlg, rlgBackup := serveRealigner(sqlDB, cfg, true)
	s := scheduler(sqlDB, scanner.ScanOptions{
		Update:          args.Update,
		Upgrade:         args.Upgrade,
		MaxDepth:        args.Depth,
		BFS:             args.BFS,
		EmbeddedLyrics:  embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
		DetectorVersion: detectorScanVersion(cfg),
	}, nil, cfg.InstrumentalDetector.Enabled, cacheRepo, rlg, rlgBackup)
	// serve has no per-run enrichment override; resolve per library against the
	// global default (and the per-library setting) inside the scheduler.
	s.GlobalEnrichDefault = cfg.Enrichment.Enabled
	s.Interval = serveScanInterval(cfg, args)
	if err := s.Run(ctx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		slog.Warn("scheduler failed", "error", err)
	}
}

// runSweeper runs the periodic path-reconciliation sweep: on each tick it prunes
// queue/scan rows whose source file has vanished, at Directory granularity (one
// stat per directory) so the unattended backstop stays disk-cheap. Its first run
// also reconciles any rows that predate this feature. Caller guarantees interval
// > 0. A per-run failure is logged and the loop continues; the sweep is a
// backstop and must never take down serve.
func runSweeper(ctx context.Context, sqlDB *sql.DB, interval time.Duration) {
	pruner := prune.New(sqlDB)
	sweep := func() {
		res, err := pruner.Sweep(ctx, prune.SweepOptions{Granularity: prune.Directory})
		if err != nil {
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				slog.Warn("path-reconciliation sweep failed; will retry next interval", "error", err)
			}
			return
		}
		if res.ScanResults > 0 || res.WorkItems > 0 {
			slog.Info("path-reconciliation sweep pruned rows for vanished sources", "scan_results", res.ScanResults, "work_items", res.WorkItems)
		}
	}
	slog.Info("path-reconciliation sweeper started", "interval", interval)
	sweep() // reconcile at startup, including any pre-existing dead-path rows
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}

// runWatcher runs the optional filesystem watcher, triggering a targeted scan of
// the changed directory under the owning library. The periodic scheduler remains
// the reconciliation backstop; the watcher only lowers latency for new files.
// watcherConfigFromCentral maps the central [watcher] config onto the watcher
// package's Config. DebounceMS (milliseconds) becomes a time.Duration. The
// non-positive clamp is intentionally left to watcher.New (a 0 or negative
// Debounce/MaxDirs is raised to the watcher package default there), so this
// mapping is a straight translation and the clamp lives in exactly one place.
func watcherConfigFromCentral(cfg config.Config) watcher.Config {
	return watcher.Config{
		Enabled:  cfg.Watcher.Enabled,
		Debounce: time.Duration(cfg.Watcher.DebounceMS) * time.Millisecond,
		MaxDirs:  cfg.Watcher.MaxDirs,
	}
}

func runWatcher(ctx context.Context, sqlDB *sql.DB, args ServeCmd, watchCfg watcher.Config, cfg config.Config, cacheRepo *cache.CacheRepo) {
	// The watcher's ScanFunc is a subtree (RunOnceForPath) scan, so realign is
	// scoped to the changed directory. It is gated by realign.enabled alone (not
	// on_scan, which governs full periodic/manual scans).
	rlg, rlgBackup := serveRealigner(sqlDB, cfg, false)
	sched := scheduler(sqlDB, scanner.ScanOptions{
		Update:          args.Update,
		Upgrade:         args.Upgrade,
		MaxDepth:        args.Depth,
		BFS:             args.BFS,
		EmbeddedLyrics:  embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
		DetectorVersion: detectorScanVersion(cfg),
	}, nil, cfg.InstrumentalDetector.Enabled, cacheRepo, rlg, rlgBackup)
	sched.GlobalEnrichDefault = cfg.Enrichment.Enabled
	pruner := prune.New(sqlDB)
	wch := watcher.New(watchCfg, library.New(sqlDB), func(ctx context.Context, lib models.Library, path string) error {
		return sched.RunOnceForPath(ctx, lib, path)
	}, func(ctx context.Context, path string) error {
		// Reactive, disk-cheap reconciliation: the watcher already learned path
		// vanished, so delete its rows without a rescan (Exact granularity).
		_, err := pruner.PrunePath(ctx, path)
		return err
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
	case args.Reconcile != nil:
		sub := *args.Reconcile
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runScanReconcile(ctx, out, sub)
	case args.ReconcileInstrumental != nil:
		sub := *args.ReconcileInstrumental
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileInstrumental(ctx, out, sub)
	case args.ReconcileInstrumentalRecalibrate != nil:
		sub := *args.ReconcileInstrumentalRecalibrate
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileInstrumentalRecalibrate(ctx, out, sub)
	case args.ReconcilePaths != nil:
		sub := *args.ReconcilePaths
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcilePaths(ctx, out, sub)
	case args.ReconcileIdentity != nil:
		sub := *args.ReconcileIdentity
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileIdentity(ctx, out, sub)
	case args.ReconcileLRC != nil:
		sub := *args.ReconcileLRC
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileLRC(ctx, out, sub)
	case args.ReconcileMarkerProvenance != nil:
		sub := *args.ReconcileMarkerProvenance
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileMarkerProvenance(ctx, out, sub)
	case args.ReconcileDetectorStats != nil:
		sub := *args.ReconcileDetectorStats
		if sub.ConfigPath == "" {
			sub.ConfigPath = args.ConfigPath
		}
		return runReconcileDetectorStats(ctx, out, sub)
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

	enrichOverride, err := resolveEnrichOverride(args.Enrich, args.NoEnrich)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	detectOverride, err := resolveDetectOverride(args.DetectInstrumental, args.NoDetectInstrumental)
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	// A manual scan realigns only when realign.enabled AND realign.on_scan (both
	// off by default, so no behavior change unless opted in).
	rlg, rlgBackup := serveRealigner(sqlDB, cfg, true)
	s := scheduler(sqlDB, scanner.ScanOptions{
		Update:          args.Update,
		Upgrade:         args.Upgrade,
		MaxDepth:        args.Depth,
		BFS:             args.BFS,
		EmbeddedLyrics:  embeddedLyricsMode(args.EmbeddedLyrics, cfg.Output.EmbeddedLyrics),
		DetectorVersion: detectorScanVersion(cfg),
	}, detectOverride, cfg.InstrumentalDetector.Enabled, nil, rlg, rlgBackup)
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

// resolveFlagOverride converts a mutually-exclusive on/off bool flag pair into a
// tri-state override: nil = no override (fall back to per-library then global),
// &true = force on, &false = force off. label names the flag pair for the
// conflict error. Passing both flags is a usage error.
func resolveFlagOverride(on, off bool, label string) (*bool, error) {
	switch {
	case on && off:
		return nil, fmt.Errorf("%s are mutually exclusive", label)
	case on:
		v := true
		return &v, nil
	case off:
		v := false
		return &v, nil
	default:
		return nil, nil
	}
}

// resolveEnrichOverride resolves the scan --enrich/--no-enrich override (#217).
func resolveEnrichOverride(enrich, noEnrich bool) (*bool, error) {
	return resolveFlagOverride(enrich, noEnrich, "--enrich and --no-enrich")
}

// resolveDetectOverride resolves the scan --detect-instrumental/--no-detect-instrumental
// override (#218).
func resolveDetectOverride(detect, noDetect bool) (*bool, error) {
	return resolveFlagOverride(detect, noDetect, "--detect-instrumental and --no-detect-instrumental")
}

// scheduler builds the scan scheduler. cacheRepo is the cache used to skip
// already-cached tracks at enqueue time; pass the worker's shared repo in serve
// mode so the /metrics hit-rate covers scheduler lookups too (#308),
// or nil for a one-shot scan (a fresh, uncounted repo is created).
func scheduler(sqlDB *sql.DB, opts scanner.ScanOptions, detectOverride *bool, globalDetectDefault bool, cacheRepo *cache.CacheRepo, realigner *realign.Realigner, realignBackupPath string) scan.Scheduler {
	results := scan.New(sqlDB)
	if cacheRepo == nil {
		cacheRepo = cache.New(sqlDB)
	}
	enq := scan.Enqueuer{
		Results:             results,
		Cache:               cacheRepo,
		Queue:               queue.NewDBQueue(sqlDB),
		Priority:            queue.PriorityScan,
		DetectOverride:      detectOverride,
		GlobalDetectDefault: globalDetectDefault,
	}
	return scan.Scheduler{
		Libraries: library.New(sqlDB),
		Results:   results,
		// Persist metadata-read failures so a permanently-malformed file is not
		// re-read (and re-warned about) on every scheduled/watched scan (#376).
		Scanner: scanner.NewScanner(scanner.WithMetadataFailureStore(scanfail.New(sqlDB))),
		Options: opts,
		OnScanComplete: func(ctx context.Context, lib models.Library, found []models.ScanResult) error {
			enqueued, cacheHits, err := enq.EnqueuePending(ctx, lib)
			if err != nil {
				// Counts are partial on an aborted enqueue; don't log "complete".
				slog.Warn("scheduled scan incomplete (enqueue aborted)",
					"library", lib.Name, "found", len(found), "enqueued", enqueued, "cache_hits", cacheHits, "error", err)
				return err
			}
			slog.Info("scheduled scan complete",
				"library", lib.Name, "found", len(found), "enqueued", enqueued, "cache_hits", cacheHits)
			reactiveRealign(ctx, realigner, realignBackupPath, lib, found)
			return nil
		},
	}
}

// reactiveRealign runs a scoped realign over the directories touched by a scan's
// results, self-healing orphaned sidecars left by a rename. It is best-effort:
// realign failures are logged and never abort the scan (the scan already
// succeeded). A nil realigner disables it.
// reactiveRealignMu serializes every serve-mode reactive realign pass (watcher /
// post-scan / webhook). All three write the one shared backup file
// (realignServeBackupPath) and Apply rolls a failed rename back by truncating it;
// concurrent writers would let one pass truncate away another's committed record.
// Serializing also makes a later pass observe an earlier pass's applied move
// (its plan-time destinationBlocked then trips), so two concurrent passes cannot
// both target the same audio and clobber. Realign passes are bounded and
// best-effort, so the coarse lock is cheap.
var reactiveRealignMu sync.Mutex

func reactiveRealign(ctx context.Context, realigner *realign.Realigner, backupPath string, lib models.Library, found []models.ScanResult) {
	if realigner == nil {
		return
	}
	dirs := make(map[string]struct{})
	for _, r := range found {
		if r.FilePath != "" {
			dirs[filepath.Dir(r.FilePath)] = struct{}{}
		}
	}
	var moved, errored int
	for dir := range dirs {
		if err := ctx.Err(); err != nil {
			return
		}
		reactiveRealignMu.Lock()
		_, applied, err := realigner.ReactiveDir(lib, dir, backupPath)
		reactiveRealignMu.Unlock()
		if err != nil {
			slog.Warn("reactive realign failed for directory; continuing", "library", lib.Name, "dir", dir, "error", err)
			continue
		}
		m, _, e := realign.CountApplied(applied)
		moved += m
		errored += e
	}
	if moved > 0 || errored > 0 {
		slog.Info("reactive realign re-attached orphaned sidecars", "library", lib.Name, "moved", moved, "errors", errored)
	}
}

// realignServeBackupPath is the append-only JSONL backup that serve-mode reactive
// realign records applied moves to, next to the database.
func realignServeBackupPath(cfg config.Config) string {
	return filepath.Join(filepath.Dir(cfg.DB.Path), "realign-serve-backup.jsonl")
}

// serverRealigner adapts internal/realign to the server.Realigner interface for
// the Lidarr webhook trigger: it resolves the owning library for a confined
// directory and applies a scoped realign there.
type serverRealigner struct {
	realigner  *realign.Realigner
	backupPath string
}

func (s serverRealigner) RealignDir(ctx context.Context, dir string) error {
	// Serialize with the scheduler/watcher reactive passes so the shared backup
	// file is never truncated by a racing writer (see reactiveRealignMu).
	reactiveRealignMu.Lock()
	defer reactiveRealignMu.Unlock()
	_, _, err := s.realigner.ResolveAndRealignDir(ctx, dir, s.backupPath)
	return err
}

// serveRealigner builds the reactive realigner for a scan trigger, or (nil, "")
// when reactive realign is disabled. realign.enabled is the master gate;
// requireOnScan additionally requires realign.on_scan (for the periodic and manual
// scan triggers -- the watcher path passes false so realign.enabled alone gates it).
func serveRealigner(sqlDB *sql.DB, cfg config.Config, requireOnScan bool) (*realign.Realigner, string) {
	if !cfg.Realign.Enabled || (requireOnScan && !cfg.Realign.OnScan) {
		return nil, ""
	}
	return realign.New(library.New(sqlDB), cfg.Realign), realignServeBackupPath(cfg)
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

// resolveSecretStore resolves the AES-256 master key and constructs the
// SQL-backed encrypted secret store. The key is resolved from MXLRC_MASTER_KEY
// (env, never logged) > the resolved key file (auto-generated 0600 on first
// use, universal default on all platforms including Docker), via
// secrets.ResolveKey. Any resolution failure is returned as a wrapped error
// and is fatal at the call site. The key bytes never touch slog.
func resolveSecretStore(cfg config.Config, sqlDB *sql.DB) (secrets.Store, error) {
	opts := cfg.SecretsKeyOptions()
	opts.MasterKeyB64 = os.Getenv("MXLRC_MASTER_KEY")
	key, err := secrets.ResolveKey(opts)
	if err != nil {
		return nil, fmt.Errorf("resolve secrets master key: %w", err)
	}
	return secrets.NewSQLStore(sqlDB, key), nil
}

// resolveTokenWithStore appends the encrypted DB store as the LOWEST-precedence
// Musixmatch token source. higher is the already-resolved value from the higher
// tiers (--token CLI > MUSIXMATCH_TOKEN > MXLRC_API_TOKEN > TOML api.token). The
// DB is consulted only when higher is empty; a present higher tier is used as-is
// and is NEVER auto-persisted to the DB (import is an explicit operator action).
func resolveTokenWithStore(ctx context.Context, higher string, store secrets.Store) (token string, fromDB bool, err error) {
	if strings.TrimSpace(higher) != "" || store == nil {
		return higher, false, nil
	}
	v, ok, err := store.Get(ctx, secrets.NameMusixmatchToken)
	if err != nil {
		return "", false, fmt.Errorf("read musixmatch token from secret store: %w", err)
	}
	if !ok {
		return higher, false, nil
	}
	return v, true, nil
}

// resolveWebhookKeysWithStore appends the encrypted DB store as the
// LOWEST-precedence webhook API key source. higher is the already-resolved value
// from the higher tiers (CLI/env MXLRC_WEBHOOK_API_KEY > TOML
// server.webhook_api_keys). The DB is consulted only when higher is empty; a
// present higher tier is used as-is and is NEVER auto-persisted.
func resolveWebhookKeysWithStore(ctx context.Context, higher []string, store secrets.Store) (keys []string, fromDB bool, err error) {
	if len(higher) > 0 || store == nil {
		return higher, false, nil
	}
	v, ok, err := store.Get(ctx, secrets.NameWebhookAPIKey)
	if err != nil {
		return nil, false, fmt.Errorf("read webhook API key from secret store: %w", err)
	}
	if !ok || strings.TrimSpace(v) == "" {
		return higher, false, nil
	}
	return []string{v}, true, nil
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
		// Re-check the cross-field invariants against the MERGED config before
		// persisting. setConfigValue validates one key in isolation, but
		// instrumental_detector.ordering and providers.mode are separately
		// settable and only contradict each other in combination: setting either
		// one alone can move the file into the rejected front+parallel state,
		// which load-time validation would then fail on at the NEXT startup -
		// after the write, so the operator gets a broken config file and a
		// delayed, confusing boot error rather than an immediate rejection. This
		// is the same share-one-validator arrangement ValidateTLSSelection uses
		// across boot and the settings-save path.
		if err := config.ValidateInstrumentalDetectorOrdering(cfg); err != nil {
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
		"server.sweep_interval_seconds",
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
		"instrumental_detector.ordering",
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
	case "server.sweep_interval_seconds":
		return strconv.Itoa(cfg.Server.SweepIntervalSeconds), true
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
	case "instrumental_detector.ordering":
		return cfg.InstrumentalDetector.Ordering, true
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
	case "server.sweep_interval_seconds":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("server.sweep_interval_seconds must be a non-negative integer (seconds; 0 disables the periodic sweep)")
		}
		cfg.Server.SweepIntervalSeconds = n
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
	case "instrumental_detector.ordering":
		m := strings.ToLower(strings.TrimSpace(value))
		if m != "front" && m != "demoted" {
			return fmt.Errorf("instrumental_detector.ordering must be \"front\" or \"demoted\"")
		}
		cfg.InstrumentalDetector.Ordering = m
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

// runScanReconcile re-runs the audio detector over instrumental-tagged tracks and,
// for any the current detector no longer classifies as instrumental, deletes the
// exact-match instrumental .txt marker and re-queues the work_queue row so the
// scheduler re-fetches it behind foreground work. Dry-run by default; --yes
// applies. By default it re-infers only the telemetry-narrowed candidate set
// (borderline / cross-version / un-scored rows); --all re-infers every tagged row.
func runScanReconcile(ctx context.Context, out io.Writer, args ScanReconcileCmd) int {
	env, code := openDetectorEnv(ctx, out, args.ConfigPath, args.Library, "reconcile")
	if env == nil {
		return code
	}
	defer env.Close()

	cfg, libraryID, libLabel := env.cfg, env.libraryID, env.libLabel
	det, workQueue := env.detector, env.queue

	total, err := workQueue.CountInstrumental(ctx)
	if err != nil {
		slog.Error("failed to count instrumental rows", "error", err)
		return 1
	}
	candidates, err := workQueue.ListInstrumental(ctx, queue.ListInstrumentalOptions{
		LibraryID:      libraryID,
		Limit:          args.Limit,
		All:            args.All,
		CurrentVersion: version,
	})
	if err != nil {
		slog.Error("failed to list instrumental rows", "error", err)
		return 1
	}
	mode := "telemetry-narrowed"
	if args.All {
		mode = "full"
	}
	suffix := ""
	if !args.Yes {
		suffix = " [dry run; pass --yes to apply]"
	}
	_, _ = fmt.Fprintf(out, "reconcile%s: %d instrumental-tagged rows total; %d candidate(s) to re-infer (%s mode)%s\n",
		libLabel, total, len(candidates), mode, suffix)

	var (
		checked, confirmed, disagreed    int
		sidecarsDeleted, rowsReset       int
		skippedNoSource, skippedNoMarker int
		errCount                         int
	)

	backupPath := args.Backup
	if backupPath == "" {
		backupPath = filepath.Join(filepath.Dir(cfg.DB.Path), fmt.Sprintf("reconcile-backup-%s.jsonl", time.Now().UTC().Format("20060102-150405")))
	}
	var backup *os.File
	defer func() {
		if backup != nil {
			_ = backup.Close()
		}
	}()

	for _, item := range candidates {
		if err := ctx.Err(); err != nil {
			_, _ = fmt.Fprintf(out, "interrupted: %v\n", err)
			break
		}
		src := strings.TrimSpace(item.Inputs.SourcePath)
		if src == "" {
			skippedNoSource++
			continue
		}
		res, derr := det.Detect(ctx, src)
		if derr != nil {
			slog.Warn("reconcile: detection failed; skipping row", "id", item.ID, "error", derr)
			errCount++
			continue
		}
		checked++
		if res.Instrumental {
			confirmed++
			continue
		}
		disagreed++

		markerPaths := exactInstrumentalSidecars(item)
		if len(markerPaths) == 0 {
			// Disagreement, but no on-disk file is exactly the instrumental marker
			// (missing, edited, or genuine unsynced lyrics). Leave the row untouched
			// so reconcile never clears a verdict whose marker it did not remove and
			// never clobbers real lyrics.
			skippedNoMarker++
			slog.Info("reconcile: disagreement but no exact instrumental-marker sidecar; leaving row untouched", "id", item.ID, "source", src)
			continue
		}

		if !args.Yes {
			_, _ = fmt.Fprintf(out, "would clear: id=%d  %s  -> delete %d marker(s) + re-queue\n", item.ID, src, len(markerPaths))
			continue
		}

		// Apply order: backup first (abort the row's mutation if it fails), then
		// delete markers, then reset the row.
		if backup == nil {
			f, err := os.OpenFile(backupPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // G304: backupPath is operator-supplied (--backup) or derived from the configured db dir, not untrusted input
			if err != nil {
				slog.Error("failed to open reconcile backup file", "path", backupPath, "error", err)
				return 1
			}
			backup = f
		}
		if err := appendReconcileBackup(backup, item, markerPaths, res); err != nil {
			slog.Error("reconcile: backup write failed; skipping row mutation", "id", item.ID, "error", err)
			errCount++
			continue
		}
		deleteFailed := false
		for _, p := range markerPaths {
			if err := os.Remove(p); err != nil {
				if os.IsNotExist(err) {
					// Already gone (removed out-of-band since the check); not a failure.
					continue
				}
				slog.Warn("reconcile: failed to delete marker; leaving row tagged", "id", item.ID, "path", p, "error", err)
				errCount++
				deleteFailed = true
				continue
			}
			sidecarsDeleted++
		}
		if deleteFailed {
			// A marker remains on disk: do NOT clear the verdict, or the row would say
			// "not instrumental" while the on-disk marker still says it is. Leave the
			// row tagged so a later reconcile retries it.
			continue
		}
		if n, err := workQueue.ResetInstrumental(ctx, item.ID); err != nil {
			slog.Warn("reconcile: failed to reset row (marker already deleted)", "id", item.ID, "error", err)
			errCount++
		} else {
			rowsReset += int(n)
		}
	}

	_, _ = fmt.Fprintf(out, "reconcile done: checked=%d confirmed=%d disagreed=%d markers-deleted=%d rows-reset=%d skipped(no-source=%d,no-marker=%d) errors=%d\n",
		checked, confirmed, disagreed, sidecarsDeleted, rowsReset, skippedNoSource, skippedNoMarker, errCount)
	if args.Yes && rowsReset > 0 {
		_, _ = fmt.Fprintf(out, "backup of cleared rows written to %s\n", backupPath)
	}
	if errCount > 0 {
		return 1
	}
	return 0
}

// exactInstrumentalSidecars returns the .txt sidecar paths for item whose on-disk
// content is exactly the instrumental marker. Paths derived via lyrics.SidecarName
// (the same logic the writer uses) that are missing, unreadable, or do not match
// the marker exactly are omitted, so genuine unsynced .txt lyrics are never
// returned for deletion.
func exactInstrumentalSidecars(item queue.WorkItem) []string {
	paths := item.Inputs.OutputPaths
	if len(paths) == 0 {
		paths = []models.OutputPath{{Outdir: item.Inputs.Outdir, Filename: item.Inputs.Filename}}
	}
	var matched []string
	for _, op := range paths {
		name, err := lyrics.SidecarName(item.Inputs.Track.ArtistName, item.Inputs.Track.TrackName, op.Filename, false)
		if err != nil {
			continue
		}
		p := filepath.Join(op.Outdir, name)
		if isExactInstrumentalMarker(p) {
			matched = append(matched, p)
		}
	}
	return matched
}

// isExactInstrumentalMarker reports whether the file at path exists and its content
// trimmed of trailing whitespace equals lyrics.InstrumentalMarker exactly - a
// stricter guard than a substring check so reconcile never deletes a .txt that
// carries real unsynced lyrics.
func isExactInstrumentalMarker(path string) bool {
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is derived from the queue row's stored output dir + the writer's own SidecarName; content is verified here before any deletion
	if err != nil {
		return false
	}
	return strings.TrimRight(string(b), " \t\r\n") == lyrics.InstrumentalMarker
}

// reconcileBackupRecord is one JSONL line capturing a cleared row so the operation
// is restorable.
type reconcileBackupRecord struct {
	WorkItemID   int64                  `json:"work_item_id"`
	Inputs       models.Inputs          `json:"inputs"`
	DeletedPaths []string               `json:"deleted_paths"`
	NewResult    reconcileVerdictRecord `json:"new_result"`
}

// reconcileVerdictRecord is the detector verdict that justified clearing a row.
type reconcileVerdictRecord struct {
	Instrumental      bool    `json:"instrumental"`
	MusicSum          float64 `json:"music_sum"`
	VocalPeak         float64 `json:"vocal_peak"`
	SpeechMean        float64 `json:"speech_mean"`
	WinningVocalClass string  `json:"winning_vocal_class"`
}

func appendReconcileBackup(f *os.File, item queue.WorkItem, deleted []string, res detector.Result) error {
	rec := reconcileBackupRecord{
		WorkItemID:   item.ID,
		Inputs:       item.Inputs,
		DeletedPaths: deleted,
		NewResult: reconcileVerdictRecord{
			Instrumental:      res.Instrumental,
			MusicSum:          res.Confidence,
			VocalPeak:         res.VocalConfidence,
			SpeechMean:        res.SpeechConfidence,
			WinningVocalClass: res.WinningVocalClass,
		},
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal reconcile backup record: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write reconcile backup record: %w", err)
	}
	return nil
}
