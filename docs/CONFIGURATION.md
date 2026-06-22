# Configuration

Every setting can come from a TOML config file or be overridden by an environment variable (and a few by a CLI flag). This page documents the precedence, the complete environment-variable surface, the TOML config keys, and the default storage paths.

A fully commented `config.example.toml` ships in the repository root; copy it and edit the keys you need. The sections below cross-reference it.

## Token precedence

A Musixmatch API token is required. Supply it using any of the following methods, listed in order of precedence (highest first):

1. **`--token` CLI flag** - highest priority.
   ```sh
   mxlrcgo-svc --token YOUR_TOKEN adele,hello
   ```
2. **`MUSIXMATCH_TOKEN` environment variable** (`MXLRC_API_TOKEN` is accepted as a lower-precedence alias).
   ```sh
   export MUSIXMATCH_TOKEN=YOUR_TOKEN
   mxlrcgo-svc adele,hello
   ```
3. **Config file / `.env` file** - place a `.env` in the working directory where you run the command, or set the token in the TOML config.
   ```sh
   MUSIXMATCH_TOKEN=YOUR_TOKEN
   ```

To obtain a token, follow steps 1 to 5 from the [Spicetify guide](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work).

## General precedence

For all settings, precedence is **CLI flag > environment variable > config file > built-in default**.

## Storage paths

`mxlrcgo-svc` resolves its config file and SQLite database using XDG base directories by default, with overrides for Docker and native packages.

| Install method | Config file | Database |
|----------------|-------------|----------|
| XDG (default) | `$XDG_CONFIG_HOME/mxlrcgo-svc/config.toml` | `$XDG_DATA_HOME/mxlrcgo-svc/mxlrcgo.db` |
| Docker (`MXLRC_DOCKER=true`) | `/config/config.toml` | `/config/mxlrcgo.db` |
| Native package (`.deb`/`.rpm`/`.apk`) | `/etc/mxlrcgo-svc/config.toml` | `/var/lib/mxlrcgo-svc/mxlrcgo.db` |

Native packages run the service as the `mxlrcgo-svc` system user; that user owns `/var/lib/mxlrcgo-svc` (mode `0750`). The state directory and system user are preserved on package removal so the database survives an upgrade or reinstall.

Override any path explicitly with `MXLRC_DB_PATH`, `MXLRC_OUTPUT_DIR`, or the `--config` flag.

### Instrumental tracks and `--upgrade`

Instrumental tracks always write a `.txt` marker file. These files are intentionally excluded from `--upgrade` promotion: re-fetching an instrumental would simply produce the same marker. Use `--update` (full re-fetch) if you want to force a re-check of an instrumental marker after a catalog change.

## Environment variables

The table below is the complete env-var surface; the watcher and verification sections of the [User Guide](USER_GUIDE.md) give the operational detail.

| Variable | Default | Purpose |
|----------|---------|---------|
| `MUSIXMATCH_TOKEN` | (required) | Musixmatch API token. `MXLRC_API_TOKEN` is accepted as a lower-precedence alias. |
| `MXLRC_WEBHOOK_API_KEY` | (none) | Comma-separated webhook API key(s) accepted by the server. Generate with `mxlrcgo-svc keys create --scope webhook`. |
| `MXLRC_SERVER_ADDR` | `127.0.0.1:3876` | HTTP listen address for `serve`. Docker images default this to `0.0.0.0:50705`. |
| `MXLRC_WEB_UI_ENABLED` | `false` | Enable the browser UI on the serve listener. Env override of `web_ui_enabled` (precedence: env > file). Restart to apply. |
| `MXLRC_OUTPUT_DIR` | XDG / `/music` | Fallback output directory for webhook jobs that resolve via metadata. |
| `MXLRC_DB_PATH` | XDG / `/config/mxlrcgo.db` | SQLite database path. |
| `MXLRC_DOCKER` | `false` | When `true`, storage defaults resolve under `/config`. Set automatically in the images. |
| `MXLRC_MASTER_KEY` | (none) | Optional. Base64 of 32 random bytes; overrides the auto-generated key file as the master key for encrypted-at-rest secrets. When set, no key file is read or written. Use for key/data separation (recommended Docker hardening when the threat model includes whole-volume theft). Generate with `openssl rand -base64 32`. See the [Encrypted secrets](USER_GUIDE.md#encrypted-secrets) guide. |
| `MXLRC_SECRETS_KEY_FILE` | XDG / `/config/.mxlrcgo.key` | Override for the auto-generated `0600` key-file location (the native-install default; used only when `MXLRC_MASTER_KEY` is unset). Point it at a separate mount to keep the key off the data volume. |
| `MXLRC_WEBAUTH_ADMIN_USER` | (none) | Bootstrap-only. With `MXLRC_WEBAUTH_ADMIN_PASSWORD`, creates the first web-UI admin on startup when none exists (skipped otherwise). Both must be set; an existing admin is never overwritten. Rotate the password after first run and remove these vars. See [Web UI access](#web-ui-access). |
| `MXLRC_WEBAUTH_ADMIN_PASSWORD` | (none) | Bootstrap-only. The first web-UI admin password (at least 8 characters; a shorter value is a fatal startup error). Never logged. Pairs with `MXLRC_WEBAUTH_ADMIN_USER`. |
| `MXLRC_API_COOLDOWN` | `15` | Seconds between Musixmatch requests. `MXLRC_COOLDOWN` is a lower-precedence alias. |
| `MXLRC_API_CIRCUIT_OPEN_DURATION` | `1800` | Cap (seconds) for the worker circuit-breaker window; the window ramps geometrically up to this ceiling, and a token-renewal signal opens for the full cap (floor 300). |
| `MXLRC_API_CIRCUIT_BACKOFF_BASE` | `60` | Trip-1 circuit-breaker window (seconds); doubles each consecutive throttle up to `MXLRC_API_CIRCUIT_OPEN_DURATION`, resets on a successful fetch or clean miss (floor 15, capped at the open-duration). |
| `MXLRC_SCAN_INTERVAL` | `900` | `serve` library-scan interval in seconds. `0` scans once without repeating. |
| `MXLRC_WORK_INTERVAL` | `0` | Worker poll interval in seconds. `0` falls back to `api.cooldown` (15s floor). |
| `MXLRC_PROVIDER_PRIMARY` | `musixmatch` | Primary lyrics provider. |
| `MXLRC_PROVIDERS_DISABLED` | (none) | Comma-separated providers to disable. |
| `MXLRC_GUARD_ACCEPTED_SCRIPTS` | (none) | Comma-separated allowlist of Unicode script buckets a lyric body may use (Latin, Han, Kana, Hangul, Other). Empty disables the language/script guard. |
| `MXLRC_GUARD_THRESHOLD` | `0.20` | Maximum tolerated share of foreign-script letters before a result is rejected. Values outside (0, 1] reset to the default. |
| `MXLRC_QUEUE_RANDOMIZE` | `true` | Shuffle worker dequeue order within each priority tier (anti-fingerprint). `false` restores deterministic order. |
| `MXLRC_LOG_LEVEL` | `info` | Minimum log level: `debug`, `info`, `warn`, `error`. `debug` exposes per-request detail, worker idle-polls, and watcher events. |
| `MXLRC_LOG_FORMAT` | `text` | Log output format: `text` (human-readable) or `json` (structured, for log aggregators). |
| `MXLRC_LOG_FILE` | (none) | Log file path. Empty means console-only (stderr). When set, logs go to both stderr and the file with automatic rotation. |
| `MXLRC_LOG_MAX_SIZE_MB` | `10` | Maximum log file size in megabytes before rotation. |
| `MXLRC_LOG_MAX_FILES` | `5` | Number of rotated log files to retain. |
| `MXLRC_LOG_MAX_AGE_DAYS` | `30` | Maximum age in days of retained rotated log files. |
| `MXLRC_LOG_COMPRESS` | `true` | Compress rotated log files with gzip. |
| `MXLRCGO_WATCH_ENABLED` | `false` | Enable the optional low-latency filesystem watcher (see the User Guide). |
| `MXLRCGO_WATCH_DEBOUNCE_MS` | `2000` | Watcher debounce window in milliseconds. |
| `MXLRCGO_WATCH_MAX_DIRS` | `100000` | Watcher safety cap on directories watched. |
| `MXLRC_VERIFICATION_ENABLED` | `false` | Enable Whisper-based lyric verification (requires a sidecar and `ffmpeg`). |
| `MXLRC_VERIFICATION_WHISPER_URL` | (none) | Whisper-compatible transcription endpoint. `MXLRC_WHISPER_URL` is an alias. |
| `MXLRC_VERIFICATION_FFMPEG_PATH` | `ffmpeg` | Path to the `ffmpeg` binary used to extract audio samples. |
| `MXLRC_VERIFICATION_SAMPLE_DURATION_SECONDS` | `30` | Audio sample length sent to Whisper. `MXLRC_VERIFICATION_SAMPLE_DURATION` is an alias. |
| `MXLRC_VERIFICATION_MIN_CONFIDENCE` | `0.85` | Below this Musixmatch confidence, verify against Whisper (0-1). |
| `MXLRC_VERIFICATION_MIN_SIMILARITY` | `0.35` | Minimum transcript/lyric overlap to accept (0-1). |
| `PUID` / `PGID` | `99` / `100` | Container-only: user/group the process drops to for file ownership. |

## TOML config keys

The TOML config file mirrors the environment variables in named sections. The keys below correspond one-to-one with `config.example.toml`, which carries inline documentation for each.

### `[api]`

```toml
[api]
cooldown = 15
circuit_open_duration = 1800
# circuit_backoff_base = 60
```

Request cooldown and the worker circuit-breaker window (env: `MXLRC_API_COOLDOWN`, `MXLRC_API_CIRCUIT_OPEN_DURATION`, `MXLRC_API_CIRCUIT_BACKOFF_BASE`).

### `[output]`

```toml
[output]
dir = "lyrics"
```

Fallback output directory for webhook jobs that resolve via metadata (env: `MXLRC_OUTPUT_DIR`).

### `[db]`

```toml
[db]
# path = "mxlrcgo.db"
```

SQLite database path (env: `MXLRC_DB_PATH`).

### `[server]`

```toml
[server]
addr = "127.0.0.1:3876"
# webhook_api_keys = ["mxlrc_your-webhook-key"]
# scan_interval_seconds = 900
# work_interval_seconds = 0
# web_ui_enabled = false
```

HTTP listen address, webhook keys, and the scheduler scan/worker poll intervals (env: `MXLRC_SERVER_ADDR`, `MXLRC_WEBHOOK_API_KEY`, `MXLRC_SCAN_INTERVAL`, `MXLRC_WORK_INTERVAL`; CLI: `--listen`, `--scan-interval`, `--work-interval`).

`web_ui_enabled` (default `false`, env: `MXLRC_WEB_UI_ENABLED`, precedence env > file) gates the browser UI on the serve listener. When enabled, the UI pages require a session login (a single admin account, separate from the webhook API key), or a request from a trusted network (the `[server.trusted_networks]` CIDR allowlist). Secret values (API token, webhook keys) are always redacted in the Config view. See [Web UI access](#web-ui-access) for the first-run onboarding flow.

#### Web UI access

The first time the UI is enabled there is no admin yet, so every UI page redirects to `/setup`. The onboarding form creates the admin account and can optionally store the Musixmatch token and webhook API key in the encrypted secret store. `/setup` is reachable only from loopback or a configured trusted-network CIDR, so a fresh daemon on a LAN port does not let the first stranger claim the admin account; once an admin exists, `/setup` is closed. The webhook API and health endpoints are unaffected before onboarding - an existing deployment with a webhook key and no admin keeps processing the queue and serving webhooks; only the browsable UI is gated.

For headless/Docker deployments, set both `MXLRC_WEBAUTH_ADMIN_USER` and `MXLRC_WEBAUTH_ADMIN_PASSWORD` to bootstrap the admin at startup instead of using the form. This is idempotent (an existing admin is never overwritten), the password (minimum 8 characters) is never logged, and a too-short password is a fatal startup error. Treat them as bootstrap-only: sign in and rotate the password after first run, then drop the variables.

### `[secrets]`

```toml
[secrets]
# key_file = ""
```

Key-file location override for encrypted-at-rest secrets (env: `MXLRC_SECRETS_KEY_FILE`). Empty uses the default (the hidden `.mxlrcgo.key` beside the database, auto-generated `0600` on first use). The master key is resolved from `MXLRC_MASTER_KEY` first (when set, the key file is skipped entirely), then from this key file; it is never stored in the config. See the [Encrypted secrets](USER_GUIDE.md#encrypted-secrets) guide for the `secrets import` / `set` / `list` commands and key-loss recovery.

### `[providers]`

```toml
[providers]
primary = "musixmatch"
disabled = []
```

Provider selection. Musixmatch is the default provider; the config exposes selection so future providers can be added without changing the fetch and worker paths (env: `MXLRC_PROVIDER_PRIMARY`, `MXLRC_PROVIDERS_DISABLED`).

### `[verification]`

```toml
[verification]
enabled = false
whisper_url = ""
ffmpeg_path = "ffmpeg"
sample_duration_seconds = 30
min_confidence = 0.85
min_similarity = 0.35
```

Optional Whisper-based speech-to-text verification for low-confidence scanned audio. When enabled, the worker extracts a bounded mono 16 kHz WAV sample using `sample_duration_seconds`, then sends it to a Whisper-compatible `/v1/audio/transcriptions` sidecar for audio whose Musixmatch metadata confidence is below `min_confidence`. The transcript must overlap the candidate lyrics by at least `min_similarity`. Environment variables override the TOML keys (`MXLRC_VERIFICATION_*`); `MXLRC_WHISPER_URL` and `MXLRC_VERIFICATION_SAMPLE_DURATION` remain accepted as legacy aliases.

ffmpeg (used to extract the audio sample) is resolved automatically: see [ffmpeg resolution](#ffmpeg-resolution) below. Set `ffmpeg_path` only to pin a specific binary.

### ffmpeg resolution

Both verification and the instrumental detector need `ffmpeg` to extract an audio sample. You do not have to install or locate it yourself: when verification is enabled or a classifier URL is configured, the service resolves ffmpeg in this order:

1. an explicit configured path (`ffmpeg_path`), if set - a missing configured binary is a hard error, never a silent download (air-gapped installs get a clear failure);
2. a previously auto-provisioned binary in the cache;
3. an `ffmpeg` already on your `PATH`;
4. otherwise, a checksum-pinned static `ffmpeg` build is downloaded over HTTPS, verified against a pinned SHA256, and cached.

The auto-download is available for Linux (amd64/arm64) and Windows (amd64). On macOS there is no published static build, so install ffmpeg yourself (`brew install ffmpeg`) or set `ffmpeg_path`. The cached binary lives next to the database (the same data directory, or `/config` under Docker), under `ffmpeg-<version>/`.

Licensing: we do not bundle ffmpeg. The auto-downloaded build is a GPL static build from [BtbN/FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds), fetched at runtime only when needed. The official Docker images already include Alpine's `ffmpeg` package, so containers resolve it on `PATH` (step 3) and never download.

### `[guard]`

```toml
[guard]
accepted_scripts = []
script_guard_threshold = 0.20
```

Optional language/script guard. It rejects fetched lyrics whose body is dominated by scripts outside the allowlist, so a wrong-language match (for example a Cyrillic or CJK body returned for a Latin-script track) is never written or cached. An empty `accepted_scripts` list (the default) disables the guard. Supported buckets: Latin, Han, Kana, Hangul, Other (env: `MXLRC_GUARD_ACCEPTED_SCRIPTS`, `MXLRC_GUARD_THRESHOLD`).

### `[queue]`

```toml
[queue]
randomize = true
```

The worker shuffles its dequeue order within each priority tier so it stops querying the upstream API in strict alphabetical (library insertion) order, which is a plausible scraping fingerprint. This is **on by default** and affects only the library/serve worker path (`Dequeue`); inspection output (`queue list`) stays deterministic, and the one-shot `fetch` CLI never touches the work queue. Set `randomize = false` (or `MXLRC_QUEUE_RANDOMIZE=false`) to restore the deterministic `created_at`/`id` ordering. The env var overrides the TOML key; an invalid value warns and keeps the current setting.

### `[logging]`

```toml
[logging]
level = "info"
format = "text"
# file = ""
# max_size_mb = 10
# max_files = 5
# max_age_days = 30
# compress = true
```

Log level, format, and the rotating file-log settings (env: `MXLRC_LOG_LEVEL`, `MXLRC_LOG_FORMAT`, `MXLRC_LOG_FILE`, `MXLRC_LOG_MAX_SIZE_MB`, `MXLRC_LOG_MAX_FILES`, `MXLRC_LOG_MAX_AGE_DAYS`, `MXLRC_LOG_COMPRESS`).
