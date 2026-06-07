# mxlrcgo-svc

[![CI](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/ci.yml/badge.svg)](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/ci.yml)
[![Release](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/release.yml/badge.svg)](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/sydlexius/mxlrcgo-svc/branch/main/graph/badge.svg)](https://codecov.io/gh/sydlexius/mxlrcgo-svc)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/sydlexius/mxlrcgo-svc/badge)](https://securityscorecards.dev/viewer/?uri=github.com/sydlexius/mxlrcgo-svc)

Command line tool to fetch synced lyrics from [Musixmatch](https://www.musixmatch.com/) and save it as *.lrc file.

## Download
### Standalone binary
Versioned binaries are published on the [GitHub Releases](https://github.com/sydlexius/mxlrcgo-svc/releases) page for Linux, macOS, and Windows on amd64/arm64 where supported.

This fork starts its release line at `v1.0.0`. The upstream `fashni/mxlrc-go` repository does not publish semver release tags, so `v1.0.0` is reserved as the first `mxlrcgo-svc` version.

### Build from source
Required Go 1.26.2+
```sh
go install github.com/sydlexius/mxlrcgo-svc/cmd/mxlrcgo-svc@latest
```

---

## Usage
```text
Usage: mxlrcgo-svc [fetch|serve|scan|library|keys|config|queue]

Commands:
  fetch     fetch lyrics once without HTTP server or DB queue
  serve     run HTTP server, worker, and library scheduler
  scan      scan configured libraries and enqueue missing lyrics
  library   manage library roots
  keys      manage API keys
  config    inspect or update configuration
  queue     inspect or maintain the durable work queue

Global flags:
  --version  print the build version and exit
  --help     show help for the program or a subcommand

Legacy flag-only invocation is still supported:
  mxlrcgo-svc [--outdir OUTDIR] [--cooldown COOLDOWN] [--depth DEPTH] [--update] [--upgrade] [--bfs] [--serve] [--listen LISTEN] [--token TOKEN] [--config CONFIG] [SONG ...]
```

`mxlrcgo-svc --version` prints the embedded build metadata, for example
`mxlrcgo-svc v1.1.0 (commit 1a2b3c4, built 2026-06-05T00:00:00Z)`. Release
binaries and the published Docker images carry the real tag; a `go build` or
`go install` from source reports `dev` unless you inject the ldflags yourself.

## Example:
### One song
```sh
mxlrcgo-svc adele,hello
mxlrcgo-svc fetch adele,hello
```
### Multiple song and custom output directory
```sh
mxlrcgo-svc adele,hello "the killers,mr. brightside" -o some_directory
```
### With a text file and custom cooldown time
```sh
mxlrcgo-svc example_input.txt -c 20
```
### Directory Mode (recursive)
```sh
mxlrcgo-svc "Dream Theater"
```
> **_This option overrides the `-o/--outdir` argument which means the lyrics will be saved in the same directory as the given input._**
>
> **_The output extension depends on the lyric type: `.lrc` when synced lyrics are found, and `.txt` when only unsynced lyrics or an instrumental marker is written._**
>
> **_The `-d/--depth` argument limits the depth of subdirectories to scan; use `-d 0` or `--depth 0` to only scan the specified directory._**

### Lidarr webhook server
```sh
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc --serve --listen 127.0.0.1:3876
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc serve --listen 127.0.0.1:3876
```

The server listens on `MXLRC_SERVER_ADDR` when `--listen` is not provided. Configure one or more webhook keys with `MXLRC_WEBHOOK_API_KEY`, use `mxlrcgo-svc keys create`, or put the server address and webhook keys in a config file and start with `mxlrcgo-svc serve --config path/to/config.toml`.

Webhook events are enqueued at high priority. If a webhook arrives for an artist/title that previously failed and is waiting out a retry backoff, the high-priority enqueue resets its retry timer so it becomes eligible immediately, jumping the queue. Scan-enqueued duplicates keep their existing backoff, so bulk scan traffic stays rate-limit protected. The worker's circuit breaker still pauses dequeuing globally when the upstream API signals rate limiting.

#### Path resolution (Docker/Unraid)

Configured library scans are the source of truth for filesystem paths. When a Lidarr webhook arrives, `mxlrcgo-svc` resolves the target file in this order:

1. **Scanned inventory.** The webhook artist/title is matched against persisted scan results (using the same normalization as the cache), and a match reuses the exact container-visible source path and output destination the scan recorded. This is why you should add and scan your libraries (`mxlrcgo-svc library add ...`, then `mxlrcgo-svc scan`) before relying on webhooks.
2. **Direct payload path.** If there is no inventory match but the webhook payload carries a `trackFiles` path that, after cleaning, lies inside one of your configured library roots and exists inside the `mxlrcgo-svc` container, that path is used directly. Payload paths outside every configured root are never used as a write target; they fall back to metadata. This confinement is a security guard: it stops a webhook from directing a lyric write to an arbitrary location. As a result, raw payload-path resolution requires at least one configured library; with no libraries configured, step 2 is disabled and resolution goes straight from inventory to metadata.
3. **Metadata fallback.** Otherwise the lyrics are written to the configured `output.dir` using the webhook metadata.

On Unraid, Lidarr and `mxlrcgo-svc` often see the same media through different mount paths. Because resolution prefers the scanned inventory, you do not need to maintain host-to-container path mappings: a payload path that is not visible inside the container, or that falls outside your configured library roots, simply falls back to metadata rather than failing.

Two operational notes:

- The library roots used to confine payload paths (step 2) are snapshotted when `serve` starts. A library added with `mxlrcgo-svc library add ...` while `serve` is running is not recognized for raw-payload-path resolution until `serve` is restarted. (The periodic scheduler and watcher still pick up new libraries without a restart; only the webhook payload-path confinement uses the startup snapshot.)
- Inventory matching for tracks with non-ASCII artist/title metadata converges after one rescan following an upgrade. The key-backfill migration applies a best-effort ASCII fold to pre-existing rows; the exact normalized keys are written on the next library scan, so run `mxlrcgo-svc scan` once after upgrading to make non-ASCII webhook matches reliable.

The scheduler scan interval and worker poll interval are configurable for Docker/Unraid deployments. Set `scan_interval_seconds` and `work_interval_seconds` under `[server]` in the config file, or override with `MXLRC_SCAN_INTERVAL` and `MXLRC_WORK_INTERVAL`. Precedence is CLI flag (`--scan-interval`, `--work-interval`) > environment variable > config file > default. Defaults preserve current behavior: scan interval 900 seconds, and worker interval falls back to `api.cooldown` (clamped to a 15-second floor). A scan interval of 0 scans once without repeating.

### Health and status endpoints

`serve` exposes lightweight endpoints for container orchestration:

- `GET /healthz` (unauthenticated) returns `200` with `{"status":"ok"}` whenever the HTTP server is accepting requests. Use it for Docker/Unraid liveness probes.
- `GET /readyz` (unauthenticated) verifies database reachability and returns `200` when ready or `503` when the database is unavailable. Error detail is omitted so it never leaks paths or connection strings.
- `GET /api/v1/status` (requires an `admin`-scoped API key) returns a queue summary grouped by status, for example `{"status":"ok","queue":{"pending":3,"failed":1}}`. It exposes no tokens, webhook keys, or filesystem paths.

Example Docker healthcheck: `curl -fsS http://127.0.0.1:3876/readyz`.

### Provider and verification config

Musixmatch is currently the only supported lyrics provider. The config file still exposes provider selection so future providers can be added without changing the fetch and worker paths:

```toml
[providers]
primary = "musixmatch"
disabled = []

[verification]
enabled = false
whisper_url = ""
ffmpeg_path = "ffmpeg"
sample_duration_seconds = 30
min_confidence = 0.85
min_similarity = 0.35
```

Environment variables override the TOML file: `MXLRC_PROVIDER_PRIMARY`, `MXLRC_PROVIDERS_DISABLED`, `MXLRC_VERIFICATION_ENABLED`, `MXLRC_VERIFICATION_WHISPER_URL`, `MXLRC_VERIFICATION_FFMPEG_PATH`, `MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS`, `MXLRC_VERIFICATION_MIN_CONFIDENCE`, and `MXLRC_VERIFICATION_MIN_SIMILARITY`. `MXLRC_WHISPER_URL` and `MXLRC_VERIFICATION_SAMPLE_DURATION` remain accepted as legacy aliases.

When verification is enabled, `ffmpeg` must be installed or `ffmpeg_path` must point to an executable ffmpeg binary. The worker extracts a bounded mono 16 kHz WAV sample using `sample_duration_seconds`, then sends that sample to a Whisper-compatible `/v1/audio/transcriptions` sidecar for scanned audio whose Musixmatch metadata confidence is below `min_confidence`. The transcript must overlap the candidate lyrics by at least `min_similarity`.

### Library and key management
```sh
mxlrcgo-svc library add /data/media/music --name Music
mxlrcgo-svc library list
mxlrcgo-svc scan
mxlrcgo-svc keys create --name lidarr --scope webhook
mxlrcgo-svc keys list
mxlrcgo-svc config get db.path
```

### Filesystem watcher (optional, low-latency scans)

By default, `serve` only scans on the scheduler's tick (`--scan-interval`, default 900s), so a new track dropped into the library waits up to that interval before lyrics are fetched. An optional filesystem watcher reacts within seconds for the common single-host case. It is disabled by default and configured entirely through environment variables:

| Variable | Default | Purpose |
|----------|---------|---------|
| `MXLRCGO_WATCH_ENABLED` | `false` | Master switch. When unset/false, behavior is exactly as before. |
| `MXLRCGO_WATCH_DEBOUNCE_MS` | `2000` | Quiet period after the last event before a directory is scanned. Coalesces the event storms that taggers (Beets, Picard) produce when rewriting an album. |
| `MXLRCGO_WATCH_MAX_DIRS` | `100000` | Safety cap. Startup fails loudly if the configured roots contain more directories than this, rather than silently exceeding the kernel watch budget. |

When a file appears or changes, the watcher scans the affected directory (and its subtree, up to the configured scan depth) under the owning library and enqueues any cache misses at scan priority.

The watcher is **best-effort and in addition to** the periodic scan, never a replacement:

- Bind-mounted volumes, NFS, SMB, and Docker Desktop on macOS frequently drop or never emit filesystem events.
- Events that fire while the container is down are lost; there is no replay. The periodic scan reconciles them.
- On Linux, very large libraries may require raising the inotify watch limit, e.g. `sysctl fs.inotify.max_user_watches=524288`.

#### Watcher-primary mode

Because the periodic scheduler remains the source of truth, you can run the watcher as the primary trigger and demote the periodic scan to a long reconcile backstop. Enable the watcher and raise the interval, e.g.:

```sh
MXLRCGO_WATCH_ENABLED=1
MXLRC_SCAN_INTERVAL=21600   # 6h reconcile backstop (seconds)
```

The startup scan always runs regardless of the interval, so initial reconciliation is guaranteed. Do **not** set the interval to `0` (scan-once) unless you have verified the watcher actually delivers events on your filesystem, because then nothing reconciles missed events.

#### Verifying watcher events

The watcher emits `INFO "watcher started"` at boot (with library and directory counts). To confirm it is actually receiving events, enable debug logging (`MXLRC_LOG_LEVEL=debug`) and `touch` a file under a library root, then watch for `DEBUG "watcher: event received"` and a follow-up scan. If nothing appears, your filesystem is not delivering inotify events to the container and you must keep the periodic scan as the source of truth. Common offenders: **Unraid `/mnt/user` (FUSE/shfs) bind mounts**, NFS without NFSv4.1 delegations, SMB/CIFS, and Docker Desktop's virtualized mounts.

### Shell completion

`mxlrcgo-svc completion <bash|zsh|fish>` prints a sourceable completion script that completes subcommands, flags, and configured library names (the last queried live from the database, degrading gracefully when it is absent):

```bash
source <(mxlrcgo-svc completion bash)                 # bash (e.g. in ~/.bashrc)
source <(mxlrcgo-svc completion zsh)                  # zsh  (e.g. in ~/.zshrc)
mxlrcgo-svc completion fish > ~/.config/fish/completions/mxlrcgo-svc.fish
```

The scripts call a hidden `__complete` handler; library-name completion never creates the database.

### Inspection commands

The `queue` and `scan` subcommands expose the durable work queue and persisted
scan results so you can debug what the service is doing without opening the
SQLite database by hand.

```sh
# List the next 50 work_queue rows.
mxlrcgo-svc queue list

# Filter by status; failed and deferred are also exposed as convenience subcommands.
mxlrcgo-svc queue list --status pending --limit 100
mxlrcgo-svc queue failed

# List deferred rows: benign misses (a track Musixmatch has no lyrics for yet)
# waiting out a fixed cooldown before re-check. These are NOT failures and are
# kept out of `queue failed`.
mxlrcgo-svc queue deferred

# Reset a single failed row back to pending. Refused if the row is not failed
# (a deferred row is refused; let it re-check on its own, or re-trigger via webhook).
mxlrcgo-svc queue retry 42

# Delete completed rows. Without --yes, prints what would be deleted.
mxlrcgo-svc queue clear --done
mxlrcgo-svc queue clear --done --yes

# List persisted scan_results, optionally filtered by library (name or id) and status.
mxlrcgo-svc scan results
mxlrcgo-svc scan results --library Music --status pending
mxlrcgo-svc scan results --library 1 --limit 200

# Delete every scan_results row for a single library. Without --yes, prints what would be deleted.
# The library row itself is left intact.
mxlrcgo-svc scan clear --library Music
mxlrcgo-svc scan clear --library Music --yes
```

### Environment variables

All settings can come from a TOML config file, but for container deployments environment variables are usually easier. Precedence is **CLI flag > environment variable > config file > built-in default**. The table below is the complete env-var surface; the watcher and verification sections above give the operational detail.

| Variable | Default | Purpose |
|----------|---------|---------|
| `MUSIXMATCH_TOKEN` | (required) | Musixmatch API token. `MXLRC_API_TOKEN` is accepted as a lower-precedence alias. |
| `MXLRC_WEBHOOK_API_KEY` | (none) | Comma-separated webhook API key(s) accepted by the server. Generate with `mxlrcgo-svc keys create --scope webhook`. |
| `MXLRC_SERVER_ADDR` | `127.0.0.1:3876` | HTTP listen address for `serve`. Docker images default this to `0.0.0.0:50705`. |
| `MXLRC_OUTPUT_DIR` | XDG / `/music` | Fallback output directory for webhook jobs that resolve via metadata. |
| `MXLRC_DB_PATH` | XDG / `/config/mxlrcgo.db` | SQLite database path. |
| `MXLRC_DOCKER` | `false` | When `true`, storage defaults resolve under `/config`. Set automatically in the images. |
| `MXLRC_API_COOLDOWN` | `15` | Seconds between Musixmatch requests. `MXLRC_COOLDOWN` is a lower-precedence alias. |
| `MXLRC_API_CIRCUIT_OPEN_DURATION` | `1800` | Seconds the worker circuit breaker stays open after a rate-limit signal (floor 300). |
| `MXLRC_SCAN_INTERVAL` | `900` | `serve` library-scan interval in seconds. `0` scans once without repeating. |
| `MXLRC_WORK_INTERVAL` | `0` | Worker poll interval in seconds. `0` falls back to `api.cooldown` (15s floor). |
| `MXLRC_PROVIDER_PRIMARY` | `musixmatch` | Primary lyrics provider. |
| `MXLRC_PROVIDERS_DISABLED` | (none) | Comma-separated providers to disable. |
| `MXLRCGO_WATCH_ENABLED` | `false` | Enable the optional low-latency filesystem watcher (see above). |
| `MXLRCGO_WATCH_DEBOUNCE_MS` | `2000` | Watcher debounce window in milliseconds. |
| `MXLRCGO_WATCH_MAX_DIRS` | `100000` | Watcher safety cap on directories watched. |
| `MXLRC_VERIFICATION_ENABLED` | `false` | Enable Whisper-based lyric verification (requires a sidecar and `ffmpeg`). |
| `MXLRC_VERIFICATION_WHISPER_URL` | (none) | Whisper-compatible transcription endpoint. `MXLRC_WHISPER_URL` is an alias. |
| `MXLRC_VERIFICATION_FFMPEG_PATH` | `ffmpeg` | Path to the `ffmpeg` binary used to extract audio samples. |
| `MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS` | `30` | Audio sample length sent to Whisper. `MXLRC_VERIFICATION_SAMPLE_DURATION` is an alias. |
| `MXLRC_VERIFICATION_MIN_CONFIDENCE` | `0.85` | Below this Musixmatch confidence, verify against Whisper (0-1). |
| `MXLRC_VERIFICATION_MIN_SIMILARITY` | `0.35` | Minimum transcript/lyric overlap to accept (0-1). |
| `PUID` / `PGID` | `99` / `100` | Container-only: user/group the process drops to for file ownership. |

## Docker

The container runs the webhook service on port `50705` and stores its config and SQLite database under `/config`. Mount your media following the [TRaSH Guides](https://trash-guides.info/File-and-Folder-Structure/How-to-set-up/Unraid/) single-mount convention: map your data parent to `/data` and point the app at `/data/media/music`. (The image's built-in default is `/music`, which still works for the simplest single-folder case; just keep `MXLRC_OUTPUT_DIR` at its `/music` default and mount there instead.)

Published GHCR tags:

- `latest` - latest stable `v*.*.*` release
- `<version>` - exact release version, for example `1.0.0`
- `<major>.<minor>` - stable minor line, for example `1.0`
- `beta` - latest prerelease channel tag
- `<version>-<pre>` - exact prerelease version, for example `1.1.0-beta.1` or `1.1.0-rc.1`
- `dev` / `nightly` - latest scheduled build from `main`
- `nightly-YYYYMMDD` - dated nightly build from `main`

```sh
docker run -d \
  --name mxlrcgo-svc \
  -p 50705:50705 \
  -e MUSIXMATCH_TOKEN=YOUR_TOKEN \
  -e MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key \
  -e PUID=99 \
  -e PGID=100 \
  -e MXLRC_OUTPUT_DIR=/data/media/music \
  -v mxlrcgo-svc-config:/config \
  -v /path/to/your/data:/data:rw \
  --restart unless-stopped \
  ghcr.io/sydlexius/mxlrcgo-svc:latest
```

For Compose, copy `docker-compose.example.yml`, set `MUSIXMATCH_TOKEN` and `MXLRC_WEBHOOK_API_KEY`, adjust the music volume, then run:

```sh
docker compose up -d
```

`MXLRC_DOCKER=true` makes default storage paths resolve to `/config/config.toml` and `/config/mxlrcgo.db`.

To inspect or maintain the queue and scan state inside the container, exec the same `mxlrcgo-svc queue` and `mxlrcgo-svc scan results` / `mxlrcgo-svc scan clear` commands documented in the Inspection commands section above (for example `docker exec mxlrcgo-svc mxlrcgo-svc queue failed`).

## Unraid

An Unraid Community Applications template is provided at `unraid/mxlrcgo-svc.xml`. It follows the same template conventions as the `sydlexius/unraid-templates` repository: GHCR image, bridge networking, `/config` appdata, a music library mapping, and advanced `PUID`/`PGID` permission and tuning fields (scan/work intervals and the filesystem watcher).

**Library mounts.** Prefer mapping the parent of your media into the container **once** and adding library roots beneath it, rather than a separate mount per library. This keeps container-visible paths stable and matches the single-mount convention used by the [TRaSH Guides Unraid layout](https://trash-guides.info/File-and-Folder-Structure/How-to-set-up/Unraid/), which maps `/mnt/user/data` to `/data` with media under `/data/media`:

| Host path | Container path |
|-----------|----------------|
| `/mnt/user/data` | `/data` |

Then register the library (or libraries) under it (paths are container-visible):

```sh
docker exec mxlrcgo-svc mxlrcgo-svc library add /data/media/music --name Music
docker exec mxlrcgo-svc mxlrcgo-svc scan
```

(Unlike the *arr apps, mxlrcgo-svc never moves or hardlinks files; it only reads audio and writes a `.lrc`/`.txt` sibling. The single-mount convention is still worth following so paths match the rest of your stack.)

If your music instead lives in several separate top-level shares, map their common parent once, or add one **Path** mapping per share beneath `/data/media` (for example `/mnt/user/<share>` to `/data/media/<share>`) and register each with `library add`. Lyrics are written next to each audio file, so libraries do not need a shared output root; set `MXLRC_OUTPUT_DIR` only for the webhook metadata-fallback case (step 3 under [Path resolution](#path-resolution-dockerunraid)).

## Development

`make help` lists every target. The entrypoint is `cmd/mxlrcgo-svc`, so use `go run ./cmd/mxlrcgo-svc [args]` (not `go run .`).

### Quality gate and git hooks

Wire the tracked git hooks once (sets `core.hooksPath=.githooks`, a relative shared setting, so every worktree -- including any you add later -- inherits them with no extra setup):

```sh
make hooks      # enable the pre-commit + pre-push hooks
make doctor     # verify the hooks are wired and tool-version pins agree
```

`make gate` runs the full pre-push gate (the same chain `.githooks/pre-push` runs): conflict-marker check, gofmt, build, race tests, patch coverage, golangci-lint, actionlint, and govulncheck. The pre-commit hook runs a faster subset on each commit.

Other useful targets:

```sh
make smoke               # lightweight CLI smoke test
make test                # race tests
make test-shuffle        # race tests with randomized order (-shuffle=on)
make test-cover          # coverage profile + HTML report
make coverage-floor      # enforce the per-package coverage floor
make vulncheck           # govulncheck (pinned)
make scan                # build the Docker image and scan it for HIGH+ CVEs (needs Docker + grype)
make sync-tool-versions  # assert the golangci-lint pin matches across CI and pre-commit
```

---

## How to get the Musixmatch Token
Follow steps 1 to 5 from the guide [here](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work) to get a new Musixmatch token.

## Token Configuration

A Musixmatch API token is required. Supply it using any of the following methods (listed in order of precedence):

1. **`--token` CLI flag** — highest priority
  ```sh
  mxlrcgo-svc --token YOUR_TOKEN adele,hello
  ```

2. **`MUSIXMATCH_TOKEN` environment variable**
  ```sh
  export MUSIXMATCH_TOKEN=YOUR_TOKEN
  mxlrcgo-svc adele,hello
  ```

3. **`.env` file** — place in the working directory where you run the command
  ```sh
  MUSIXMATCH_TOKEN=YOUR_TOKEN
  ```

## Credits
* [Spicetify Lyrics Plus](https://github.com/spicetify/spicetify-cli/tree/master/CustomApps/lyrics-plus)
