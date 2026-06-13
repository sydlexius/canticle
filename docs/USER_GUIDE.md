# User Guide

This guide covers running `mxlrcgo-svc` as a long-running service: the Lidarr webhook server, path resolution, health endpoints, Docker and Unraid deployment, the optional filesystem watcher, shell completion, and the inspection commands. For one-shot fetching and the full flag list, see the [CLI Reference](CLI_REFERENCE.md). For every setting, see [Configuration](CONFIGURATION.md).

## Lidarr webhook server

```sh
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc --serve --listen 127.0.0.1:3876
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc serve --listen 127.0.0.1:3876
```

The server listens on `MXLRC_SERVER_ADDR` when `--listen` is not provided. Configure one or more webhook keys with `MXLRC_WEBHOOK_API_KEY`, use `mxlrcgo-svc keys create`, or put the server address and webhook keys in a config file and start with `mxlrcgo-svc serve --config path/to/config.toml`.

Webhook events are enqueued at high priority. If a webhook arrives for an artist/title that previously failed and is waiting out a retry backoff, the high-priority enqueue resets its retry timer so it becomes eligible immediately, jumping the queue. Scan-enqueued duplicates keep their existing backoff, so bulk scan traffic stays rate-limit protected. The worker's circuit breaker still pauses dequeuing globally when the upstream API signals rate limiting.

### Path resolution (Docker/Unraid)

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

To inspect or maintain the queue and scan state inside the container, exec the same `mxlrcgo-svc queue` and `mxlrcgo-svc scan results` / `mxlrcgo-svc scan clear` commands documented in the [Inspection commands](#inspection-commands) section below (for example `docker exec mxlrcgo-svc mxlrcgo-svc queue failed`).

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

## Windows

Download the signed `.zip` archive for `windows/amd64` from the [GitHub releases page](https://github.com/sydlexius/mxlrcgo-svc/releases). Extract `mxlrcgo-svc.exe` to one of:

- **`%LOCALAPPDATA%\mxlrcgo-svc\`** - user-mode install; no administrator rights required.
- **`C:\Program Files\mxlrcgo-svc\`** - system-wide install; requires administrator rights.

Add the chosen directory to your `PATH` so `mxlrcgo-svc` is reachable from any shell.

**Manual run.** To start the server from a terminal (useful for initial testing):

```cmd
set MUSIXMATCH_TOKEN=YOUR_TOKEN
set MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key
mxlrcgo-svc serve --listen 127.0.0.1:3876
```

Or use a config file to keep credentials out of the shell environment:

```cmd
mxlrcgo-svc serve --config C:\path\to\config.toml
```

### NSSM service installation

[NSSM (the Non-Sucking Service Manager)](https://nssm.cc) wraps any executable as a Windows service with automatic restart, reliable start/stop, and log capture. Download a release build from [nssm.cc](https://nssm.cc/download) and place `nssm.exe` somewhere on your `PATH`.

**Install the service.** Run the following from an elevated (Administrator) Command Prompt:

```cmd
nssm install mxlrcgo-svc
```

NSSM opens a GUI. Fill in the tabs:

- **Application tab:**
  - *Path*: full path to `mxlrcgo-svc.exe`, for example `C:\Program Files\mxlrcgo-svc\mxlrcgo-svc.exe`
  - *Arguments*: `serve --listen 0.0.0.0:3876` (or `serve --config C:\path\to\config.toml` if you use a config file)
  - *Startup directory*: the directory containing `mxlrcgo-svc.exe`

- **Environment tab.** Add one variable per line:

  ```
  MUSIXMATCH_TOKEN=YOUR_TOKEN
  MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key
  MXLRC_DB_PATH=C:\ProgramData\mxlrcgo-svc\mxlrcgo.db
  ```

  Set `MXLRC_DB_PATH` explicitly so the database lives in a known, writable location rather than depending on the service account's XDG defaults (see [Data location](#data-location) below).

- **I/O tab.** Set *Stdout* and *Stderr* to a log file, for example `C:\ProgramData\mxlrcgo-svc\logs\mxlrcgo-svc.log`. NSSM rotates these automatically.

Click *Install service*, then start it:

```cmd
nssm start mxlrcgo-svc
```

**Manage the service:**

```cmd
nssm start mxlrcgo-svc
nssm stop mxlrcgo-svc
nssm restart mxlrcgo-svc
nssm status mxlrcgo-svc
nssm remove mxlrcgo-svc confirm   # uninstall
```

To update the configuration after installation, run `nssm edit mxlrcgo-svc` from an elevated prompt.

### Data location

Without an explicit `MXLRC_DB_PATH`, `mxlrcgo-svc` resolves storage paths via XDG base directories. On Windows these defaults resolve to:

| Item | Default path |
|------|-------------|
| Config file | `C:\Users\<user>\.config\mxlrcgo-svc\config.toml` |
| Database | `C:\Users\<user>\.local\share\mxlrcgo-svc\mxlrcgo.db` |

A service account resolves these under its own profile, which may not be immediately obvious. Set `MXLRC_DB_PATH` (and `--config`) explicitly in the NSSM environment tab to put data in a known, persistent location (for example `C:\ProgramData\mxlrcgo-svc\`).

Removing or uninstalling the NSSM service does **not** delete the database or config file. Remove them manually for a clean uninstall.

### SmartScreen note

Even signed binaries can trigger the "Windows protected your PC" prompt on first launch when the executable's download reputation is too low. If you see this:

1. Click **More info**.
2. Click **Run anyway**.

This prompt should not appear again after the first run. See [issue #183](https://github.com/sydlexius/mxlrcgo-svc/issues/183) for background on code signing.

## Recording enrichment

Recording enrichment reads the ISRC, MusicBrainz recording ID, and duration from each file's audio tags and passes them to the matcher to disambiguate results (for example, telling two same-titled recordings apart). It is on by default.

You can control it at three levels, resolved with the precedence **CLI flag > per-library setting > global default**:

- **Global default** (`config.toml`): `[enrichment] enabled = true` (env `MXLRC_ENRICHMENT_ENABLED`). Default `true`. Set `false` to skip enrichment everywhere unless a library or run opts back in.
- **Per library**: `mxlrcgo-svc library add/update --enrich` (force on) or `--enrich=false` (force off). Omit the flag to inherit the global default.
- **Per run**: `mxlrcgo-svc scan --enrich` or `--no-enrich` overrides both for that single scan (the two flags are mutually exclusive). The serve-mode scheduler has no per-run flag; it resolves per library against the global default.

When enrichment is off for a track, the scanner skips ISRC, MBID, and duration extraction as a unit, and the track keeps the `duration_bucket = 0` cache fallback (no behavior regression). A per-library or global change only affects scans run after the change; it does not restamp already-scanned rows.

## Filesystem watcher (optional, low-latency scans)

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

### Watcher-primary mode

Because the periodic scheduler remains the source of truth, you can run the watcher as the primary trigger and demote the periodic scan to a long reconcile backstop. Enable the watcher and raise the interval, e.g.:

```sh
MXLRCGO_WATCH_ENABLED=1
MXLRC_SCAN_INTERVAL=21600   # 6h reconcile backstop (seconds)
```

The startup scan always runs regardless of the interval, so initial reconciliation is guaranteed. Do **not** set the interval to `0` (scan-once) unless you have verified the watcher actually delivers events on your filesystem, because then nothing reconciles missed events.

### Verifying watcher events

The watcher emits `INFO "watcher started"` at boot (with library and directory counts). To confirm it is actually receiving events, enable debug logging (`MXLRC_LOG_LEVEL=debug`) and `touch` a file under a library root, then watch for `DEBUG "watcher: event received"` and a follow-up scan. If nothing appears, your filesystem is not delivering inotify events to the container and you must keep the periodic scan as the source of truth. Common offenders: **Unraid `/mnt/user` (FUSE/shfs) bind mounts**, NFS without NFSv4.1 delegations, SMB/CIFS, and Docker Desktop's virtualized mounts.

## Shell completion

`mxlrcgo-svc completion <bash|zsh|fish>` prints a sourceable completion script that completes subcommands, flags, and configured library names (the last queried live from the database, degrading gracefully when it is absent):

```bash
source <(mxlrcgo-svc completion bash)                 # bash (e.g. in ~/.bashrc)
source <(mxlrcgo-svc completion zsh)                  # zsh  (e.g. in ~/.zshrc)
mxlrcgo-svc completion fish > ~/.config/fish/completions/mxlrcgo-svc.fish
```

The scripts call a hidden `__complete` handler; library-name completion never creates the database.

## Inspection commands

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
