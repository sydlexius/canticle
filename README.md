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
Usage: mxlrcgo-svc [fetch|serve|scan|library|keys|config]

Commands:
  fetch     fetch lyrics once without HTTP server or DB queue
  serve     run HTTP server, worker, and library scheduler
  scan      scan configured libraries and enqueue missing lyrics
  library   manage library roots
  keys      manage API keys
  config    inspect or update configuration

Legacy flag-only invocation is still supported:
  mxlrcgo-svc [--outdir OUTDIR] [--cooldown COOLDOWN] [--depth DEPTH] [--update] [--upgrade] [--bfs] [--serve] [--listen LISTEN] [--token TOKEN] [--config CONFIG] [SONG ...]
```

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
> **_The `-d/--depth` argument limits the depth of subdirectories to scan; use `-d 0` or `--depth 0` to only scan the specified directory._**

### Lidarr webhook server
```sh
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc --serve --listen 127.0.0.1:3876
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key mxlrcgo-svc serve --listen 127.0.0.1:3876
```

The server listens on `MXLRC_SERVER_ADDR` when `--listen` is not provided. Configure one or more webhook keys with `MXLRC_WEBHOOK_API_KEY`, use `mxlrcgo-svc keys create`, or put the server address and webhook keys in a config file and start with `mxlrcgo-svc serve --config path/to/config.toml`.

Webhook events are enqueued at high priority. If a webhook arrives for an artist/title that previously failed and is waiting out a retry backoff, the high-priority enqueue resets its retry timer so it becomes eligible immediately, jumping the queue. Scan-enqueued duplicates keep their existing backoff, so bulk scan traffic stays rate-limit protected. The worker's circuit breaker still pauses dequeuing globally when the upstream API signals rate limiting.

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
mxlrcgo-svc library add /music --name Music
mxlrcgo-svc library list
mxlrcgo-svc scan
mxlrcgo-svc keys create --name lidarr --scope webhook
mxlrcgo-svc keys list
mxlrcgo-svc config get db.path
```

### Inspection commands

The `queue` and `scan` subcommands expose the durable work queue and persisted
scan results so you can debug what the service is doing without opening the
SQLite database by hand.

```sh
# List the next 50 work_queue rows.
mxlrcgo-svc queue list

# Filter by status; failed is also exposed as a convenience subcommand.
mxlrcgo-svc queue list --status pending --limit 100
mxlrcgo-svc queue failed

# Reset a single failed row back to pending. Refused if the row is not failed.
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

## Docker

The container runs the webhook service on port `50705` and stores its config and SQLite database under `/config`. Mount your music library at `/music`.

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
  -v mxlrcgo-svc-config:/config \
  -v /path/to/your/music:/music:rw \
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

An Unraid Community Applications template is provided at `unraid/mxlrcgo-svc.xml`. It follows the same template conventions as the `sydlexius/unraid-templates` repository: GHCR image, bridge networking, `/config` appdata, `/music` library mapping, and advanced `PUID`/`PGID` permission fields.

## Development

Run the lightweight CLI smoke test:

```sh
make smoke
```

Generate a local coverage profile and HTML report:

```sh
make test-cover
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
