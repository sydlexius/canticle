# Configuration

Every setting can come from a TOML config file or be overridden by an environment variable (and a few by a CLI flag). This page documents the precedence, the complete environment-variable surface, the TOML config keys, and the default storage paths.

A fully commented `config.example.toml` ships in the repository root; copy it and edit the keys you need. The sections below cross-reference it.

## Token precedence

A Musixmatch API token is required. Supply it using any of the following methods, listed in order of precedence (highest first):

1. **`--token` CLI flag** - highest priority.
   ```sh
   canticle --token YOUR_TOKEN adele,hello
   ```
2. **`MUSIXMATCH_TOKEN` environment variable** (`MXLRC_API_TOKEN` is accepted as a lower-precedence alias).
   ```sh
   export MUSIXMATCH_TOKEN=YOUR_TOKEN
   canticle adele,hello
   ```
3. **Config file / `.env` file** - place a `.env` in the working directory where you run the command, or set the token in the TOML config.
   ```sh
   MUSIXMATCH_TOKEN=YOUR_TOKEN
   ```

To obtain a token, follow steps 1 to 5 from the [Spicetify guide](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work).

## General precedence

For all settings, precedence is **CLI flag > environment variable > config file > built-in default**.

## Storage paths

`canticle` resolves its config file and SQLite database using XDG base directories by default, with overrides for Docker and native packages.

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
| `MXLRC_WEBHOOK_API_KEY` | (none) | Comma-separated webhook API key(s) accepted by the server. Generate with `canticle keys create --scope webhook`. |
| `MXLRC_SERVER_ADDR` | `127.0.0.1:3876` | HTTP listen address for `serve`. Docker images default this to `0.0.0.0:50705`. |
| `MXLRC_WEB_UI_ENABLED` | `false` | Enable the browser UI on the serve listener. Env override of `web_ui_enabled` (precedence: env > file). Restart to apply. |
| `MXLRC_TRUSTED_CIDRS` | (none) | Comma-separated CIDRs of trusted client networks (serve mode). Loopback is always trusted. Requests from listed CIDRs may scrape `GET /metrics` and bypass the web UI session requirement. |
| `MXLRC_TRUSTED_PROXIES` | (none) | Comma-separated CIDRs of reverse proxies whose `X-Forwarded-For` is trusted to carry the real client IP (serve mode). Must not overlap `MXLRC_TRUSTED_CIDRS`. |
| `MXLRC_TLS_CERT_FILE` | (none) | PEM certificate path for the serve listener. Set with `MXLRC_TLS_KEY_FILE` to terminate TLS (minimum TLS 1.2). |
| `MXLRC_TLS_KEY_FILE` | (none) | PEM private key path. Required together with `MXLRC_TLS_CERT_FILE`. |
| `MXLRC_TLS_SELF_SIGNED` | `false` | Generate and persist a self-signed certificate on first run under `<dir(db_path)>/tls/`. Mutually exclusive with the cert/key pair. |
| `MXLRC_TLS_REDIRECT_HTTP` | (none) | Plain-HTTP listen address (e.g. `:80`) that 301-redirects to HTTPS. Honored only when TLS is enabled. |
| `MXLRC_TLS_SELF_SIGNED_HOSTS` | (none) | Comma-separated extra SAN hostnames/IPs for the self-signed certificate (on top of `localhost`, `canticle`, `127.0.0.1`, `::1`). Honored only when self-signed. |
| `MXLRC_OUTPUT_DIR` | XDG / `/music` | Output directory for `fetch` mode. **Ignored in `serve` mode** (lyrics are written next to the audio file; the metadata-only webhook fallback uses the internal default). |
| `MXLRC_EMBEDDED_LYRICS` | `off` | Embedded lyrics handling. `off` - ignore (default); `respect` - skip fetching when embedded lyrics exist; `extract` - write embedded lyrics to a sidecar, then skip fetching. A timestamped Vorbis `SYNCEDLYRICS` comment is written as a synced `.lrc` (preferred); unsynced lyrics become `.txt`. ID3 `SYLT` / MP4 atoms are not handled. |
| `MXLRC_BILINGUAL_OUTPUT` | `false` | When `true` and a provider returns a translation track, interleave original and translated lines under shared timestamps in a single `.lrc`. |
| `MXLRC_DB_PATH` | XDG / `/config/mxlrcgo.db` | SQLite database path. |
| `MXLRC_DOCKER` | `false` | When `true`, storage defaults resolve under `/config`. Set automatically in the images. |
| `MXLRC_MASTER_KEY` | (none) | Optional. Base64 of 32 random bytes; overrides the auto-generated key file as the master key for encrypted-at-rest secrets. When set, no key file is read or written. Use for key/data separation (recommended Docker hardening when the threat model includes whole-volume theft). Generate with `openssl rand -base64 32`. See the [Encrypted secrets](USER_GUIDE.md#encrypted-secrets) guide. |
| `MXLRC_SECRETS_KEY_FILE` | XDG / `/config/.mxlrcgo.key` | Override for the auto-generated `0600` key-file location (the native-install default; used only when `MXLRC_MASTER_KEY` is unset). Point it at a separate mount to keep the key off the data volume. |
| `MXLRC_WEBAUTH_ADMIN_USER` | (none) | Bootstrap-only. With `MXLRC_WEBAUTH_ADMIN_PASSWORD`, creates the first web-UI admin on startup when none exists (skipped otherwise). Both must be set; an existing admin is never overwritten. Rotate the password after first run and remove these vars. See [Web UI access](#web-ui-access). |
| `MXLRC_WEBAUTH_ADMIN_PASSWORD` | (none) | Bootstrap-only. The first web-UI admin password (at least 8 characters; a shorter value is a fatal startup error). Never logged. Pairs with `MXLRC_WEBAUTH_ADMIN_USER`. |
| `MXLRC_API_COOLDOWN` | `15` | Seconds between Musixmatch requests. `MXLRC_COOLDOWN` is a lower-precedence alias. |
| `MXLRC_API_CIRCUIT_OPEN_DURATION` | `1800` | Cap (seconds) for the worker circuit-breaker window; the window ramps geometrically up to this ceiling, and a token-renewal signal opens for the full cap (floor 300). |
| `MXLRC_API_CIRCUIT_BACKOFF_BASE` | `60` | Trip-1 circuit-breaker window (seconds); doubles each consecutive throttle up to `MXLRC_API_CIRCUIT_OPEN_DURATION`, resets on a successful fetch or clean miss (floor 15, capped at the open-duration). |
| `MXLRC_MISS_BACKOFF_BASE_HOURS` | `168` | Initial re-check delay in hours for a benign miss (7 days). Doubles on each successive miss up to `MXLRC_MISS_BACKOFF_CAP_HOURS`. Minimum 1. |
| `MXLRC_MISS_BACKOFF_CAP_HOURS` | `672` | Maximum re-check delay in hours for a benign miss (28 days). Clamped to at least `MXLRC_MISS_BACKOFF_BASE_HOURS`. |
| `MXLRC_MAX_MISS_ATTEMPTS` | `15` | Maximum number of miss re-fetches before retiring the queue row. `0` means no cap. |
| `MXLRC_SCAN_INTERVAL` | `900` | `serve` library-scan interval in seconds. `0` scans once without repeating. |
| `MXLRC_WORK_INTERVAL` | `0` | Worker poll interval in seconds. `0` falls back to `api.cooldown` (15s floor). |
| `MXLRC_PROVIDER_PRIMARY` | `musixmatch` | Primary lyrics provider. |
| `MXLRC_PROVIDERS_DISABLED` | (none) | Comma-separated providers to disable. |
| `MXLRC_PROVIDERS_MODE` | `ordered` | Multi-provider dispatch strategy: `ordered` (first suitable result in priority order) or `parallel` (race every lane). |
| `MXLRC_PROVIDERS_RACE_WAIT_SECONDS` | `2` | Parallel-mode synced-upgrade window in seconds. Ignored in `ordered` mode. |
| `MXLRC_PROVIDERS_FALLBACK_ORDER` | (none) | Comma-separated providers consulted, in order, after the primary returns no suitable result. Empty means primary-only. |
| `MXLRC_GUARD_ACCEPTED_SCRIPTS` | (none) | Comma-separated allowlist of Unicode script buckets a lyric body may use (Latin, Han, Kana, Hangul, Other). Empty disables the language/script guard. |
| `MXLRC_GUARD_THRESHOLD` | `0.20` | Maximum tolerated share of foreign-script letters before a result is rejected. Values outside (0, 1] reset to the default. |
| `MXLRC_QUEUE_RANDOMIZE` | `true` | Shuffle worker dequeue order within each priority tier (anti-fingerprint). `false` restores deterministic order. |
| `MXLRC_QUEUE_BATCH_SIZE` | `10` | Shuffled lookahead buffer size: draw N eligible rows at random per batch and serve them in order, so the near-term queue is knowable. `0` disables batching (per-item random). Applies only when `randomize = true`. |
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
| `MXLRC_INSTRUMENTAL_DETECTOR_ENABLED` | `false` | Enable the audio-based instrumental detection sidecar. Requires `MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL`. |
| `MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL` | (none) | Base URL of the AudioSet classifier sidecar, e.g. `http://yamnet:8080`. Required when the detector is enabled. |
| `MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH` | `ffmpeg` | Path to the `ffmpeg` binary used for audio sampling by the instrumental detector. |
| `MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS` | `30` | Length of the audio sample sent to the classifier, clamped to [30, 60]. |
| `MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE` | `0.90` | Minimum summed AudioSet class probability to mark a track instrumental. Values outside (0, 1] reset to `0.90`. |
| `MXLRC_INSTRUMENTAL_DETECTOR_CLASSES` | `Music,Musical instrument` | Comma-separated AudioSet class names whose probabilities are summed and compared against the confidence threshold. |
| `MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS` | `5` | Minimum gap in seconds between consecutive classifier inference calls. `0` disables the cooldown. |
| `MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES` | `Singing,Vocal music,Choir,...` | Comma-separated AudioSet vocal-class names whose peak (max-over-frames) score blocks an instrumental marking (the vocal gate). |
| `MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE` | `0.015` | Maximum vocal-class peak (0-1) tolerated before a track is excluded from instrumental. Values outside (0, 1] reset to `0.015`. |
| `MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES` | `6` | Number of short windows spread across the track and concatenated into one classifier sample. `< 2` disables spreading (single contiguous window). |
| `MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH` | (auto-discover) | Path to `ffprobe` used to read track duration for spread-sample placement. Empty auto-discovers (sibling of ffmpeg, then PATH). Set this when ffmpeg was auto-provisioned (no ffprobe). |
| `MXLRC_ENRICHMENT_ENABLED` | `true` | Global default for recording enrichment (reading ISRC, MusicBrainz ID, and duration from audio tags). Per-library and per-run flags override this. |
| `MXLRC_REALIGN_ENABLED` | `false` | Master switch for serve-mode reactive realign (watcher/webhook/post-scan). The CLI command runs regardless. |
| `MXLRC_REALIGN_ON_SCAN` | `false` | Also run a library-wide realign after each scan (watcher/webhook triggers are governed by `enabled` alone). |
| `MXLRC_REALIGN_REQUIRE_PROVENANCE` | `false` | Only apply exact ISRC/MBID matches; heuristic candidates are reported but skipped. |
| `MXLRC_REALIGN_CROSS_DIRECTORY` | `false` | Allow an exact match to move a sidecar across directories within a library. |
| `MXLRC_REALIGN_IDENTITY_KEYS` | `mbid,isrc` | Ordered provenance identifiers the exact tier matches on (valid: `mbid`, `isrc`). |
| `MXLRC_REALIGN_MIN_CONFIDENCE` | `0.75` | Jaro-Winkler name-guard floor (0-1) for a heuristic rename. |
| `MXLRC_REALIGN_AUTO_APPLY_HEURISTIC` | `false` | Whether serve-mode reactive realign may auto-apply heuristic (name-similarity) matches; off = exact-only. |
| `PUID` / `PGID` | `99` / `100` | Container-only: user/group the process drops to for file ownership. |

## TOML config keys

The TOML config file mirrors the environment variables in named sections. The keys below correspond one-to-one with `config.example.toml`, which carries inline documentation for each.

### `[api]`

```toml
[api]
cooldown = 15
circuit_open_duration = 1800
# circuit_backoff_base_seconds = 60
# miss_backoff_base_hours = 168   # initial re-check delay (hours; 7 days); minimum 1
# miss_backoff_cap_hours = 672    # maximum re-check delay (hours; 28 days)
# max_miss_attempts = 15          # 0 = no cap
```

Request cooldown, the worker circuit-breaker window, and miss re-check backoff (env: `MXLRC_API_COOLDOWN`, `MXLRC_API_CIRCUIT_OPEN_DURATION`, `MXLRC_API_CIRCUIT_BACKOFF_BASE`, `MXLRC_MISS_BACKOFF_BASE_HOURS`, `MXLRC_MISS_BACKOFF_CAP_HOURS`, `MXLRC_MAX_MISS_ATTEMPTS`).

`miss_backoff_base_hours` (default 168, i.e. 7 days) and `miss_backoff_cap_hours` (default 672, i.e. 28 days) govern the escalating re-check cadence for benign misses: the delay doubles each miss (base, 2 x base, 4 x base, ...) up to the cap. `max_miss_attempts` retires the queue row after N miss fetches; `0` means no cap.

### `[output]`

```toml
[output]
dir = "lyrics"
# embedded_lyrics = "off"
# bilingual_output = false
```

Fallback output directory and per-file output controls (env: `MXLRC_OUTPUT_DIR`, `MXLRC_EMBEDDED_LYRICS`, `MXLRC_BILINGUAL_OUTPUT`; CLI: `--embedded-lyrics`).

`embedded_lyrics` controls how lyrics already embedded in the audio file's tags are handled. `off` (default) ignores them and always fetches from providers. `respect` skips fetching for files that already carry embedded lyrics. `extract` writes the embedded lyrics to a sidecar (never overwriting an existing one) and then skips fetching: a Vorbis `SYNCEDLYRICS` comment carrying timestamped LRC text is written as a synced `.lrc` and takes precedence over unsynced lyrics, which are written as `.txt`. ID3 `SYLT` and MP4 synced-lyric atoms are intentionally not handled.

`bilingual_output` (default `false`): when `true` and a provider returns a non-empty translation track, the original and translation lines are interleaved under shared timestamps in a single `.lrc`. See `docs/multilingual-output-policy.md`.

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

### `[server.trusted_networks]`

```toml
[server.trusted_networks]
# cidrs = ["192.168.1.0/24", "10.0.0.0/8"]
# trusted_proxies = ["172.16.0.0/12"]
```

Trusted-network allowlist for serve mode (env: `MXLRC_TRUSTED_CIDRS`, `MXLRC_TRUSTED_PROXIES`). Controls two things: access to `GET /metrics` (see the [User Guide](USER_GUIDE.md#metrics-endpoint)) and the web UI session bypass (requests from a trusted network skip the login prompt).

`cidrs` - CIDRs of trusted client networks. Loopback (`127.x.x.x`, `::1`) is always trusted and does not need to be listed. An empty list (the default) means only loopback is trusted.

`trusted_proxies` - CIDRs of reverse proxies allowed to set `X-Forwarded-For`. Only when a request's immediate peer is within one of these networks is the XFF header consulted (walked right-to-left, skipping proxies) to find the real client IP. Default empty: XFF is never trusted, so a spoofed header cannot forge a trusted source. Entries must not overlap `cidrs`.

An invalid CIDR in either list is a fatal startup error.

#### Web UI access

The first time the UI is enabled there is no admin yet, so every UI page redirects to `/setup`. The onboarding form creates the admin account and can optionally store the Musixmatch token and webhook API key in the encrypted secret store. `/setup` is reachable only from loopback or a configured trusted-network CIDR, so a fresh daemon on a LAN port does not let the first stranger claim the admin account; once an admin exists, `/setup` is closed. The webhook API and health endpoints are unaffected before onboarding - an existing deployment with a webhook key and no admin keeps processing the queue and serving webhooks; only the browsable UI is gated.

For headless/Docker deployments, set both `MXLRC_WEBAUTH_ADMIN_USER` and `MXLRC_WEBAUTH_ADMIN_PASSWORD` to bootstrap the admin at startup instead of using the form. This is idempotent (an existing admin is never overwritten), the password (minimum 8 characters) is never logged, and a too-short password is a fatal startup error. Treat them as bootstrap-only: sign in and rotate the password after first run, then drop the variables.

### `[server.tls]`

```toml
[server.tls]
# cert_file = "/config/tls/cert.pem"
# key_file = "/config/tls/key.pem"
# self_signed = false
# redirect_http = ""
# self_signed_hosts = []
```

Optional transport security for the serve listener. TLS is **off by default** (plain HTTP). Two modes are supported and are mutually exclusive: bring-your-own certificate (`cert_file` + `key_file`, both required together) or a self-signed bootstrap (`self_signed = true`). A contradictory combination is a fatal startup error (env: `MXLRC_TLS_CERT_FILE`, `MXLRC_TLS_KEY_FILE`, `MXLRC_TLS_SELF_SIGNED`, `MXLRC_TLS_REDIRECT_HTTP`, `MXLRC_TLS_SELF_SIGNED_HOSTS`).

| Key | Default | Purpose |
|-----|---------|---------|
| `cert_file` | (none) | PEM-encoded certificate path. With `key_file`, the listener terminates TLS itself (minimum TLS 1.2). |
| `key_file` | (none) | PEM-encoded private key path. Required together with `cert_file`. |
| `self_signed` | `false` | Generate and persist a self-signed certificate on first run (ECDSA P-256, ~365-day validity) under `<dir(db_path)>/tls/`, regenerating when missing or expired. Mutually exclusive with `cert_file`/`key_file`. Browsers show an untrusted-certificate prompt. |
| `redirect_http` | (none) | Optional plain-HTTP listen address (e.g. `:80`) whose every request 301-redirects to the HTTPS address. Empty means no redirect listener. Honored only when TLS is enabled. |
| `self_signed_hosts` | `[]` | Extra hostnames and IP literals to add as Subject Alternative Names in the generated certificate, on top of the built-in SANs (`localhost`, `canticle`, `127.0.0.1`, `::1`). Invalid entries are a startup error. Honored only when `self_signed` is true. |

TLS is considered enabled when either a `cert_file`+`key_file` pair is set or `self_signed` is true; otherwise the listener stays plain HTTP.

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
# mode = "ordered"          # "ordered" (default) or "parallel"
# race_wait_seconds = 2     # parallel-mode synced-upgrade window
# fallback_order = []       # providers consulted after the primary, in order
```

Provider selection and multi-provider dispatch. Musixmatch is the default primary provider (env: `MXLRC_PROVIDER_PRIMARY`, `MXLRC_PROVIDERS_DISABLED`, `MXLRC_PROVIDERS_MODE`, `MXLRC_PROVIDERS_RACE_WAIT_SECONDS`, `MXLRC_PROVIDERS_FALLBACK_ORDER`). See [Multi-provider orchestration](multi-provider-orchestration.md) for the full dispatch model.

| Key | Default | Purpose |
|-----|---------|---------|
| `primary` | `musixmatch` | The primary lyrics provider, always the first lane. |
| `disabled` | `[]` | Provider names to disable. |
| `mode` | `ordered` | Dispatch strategy. `ordered` queries lanes in priority order (primary, then each `fallback_order` entry) and returns the first suitable result. `parallel` dispatches every lane concurrently and races them. Any other value is rejected at load. |
| `race_wait_seconds` | `2` | Parallel-mode only: after a suitable unsynced result arrives, wait up to this many seconds for a synced result (a strict quality upgrade) to preempt it. Non-positive values are reset to the default. Ignored in `ordered` mode. |
| `fallback_order` | `[]` | Provider names consulted, in order, after the primary when it returns no suitable result. Each name must be a known provider; unknown names are rejected at load. Empty means no fallback (only the primary lane runs). |

`parallel` mode makes more upstream calls (every lane is queried per dispatch), so it is not advised against rate-limited providers unless latency matters more than call volume.

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

### `[instrumental_detector]`

```toml
[instrumental_detector]
# enabled = false
# classifier_url = "http://yamnet:8080"
# ffmpeg_path = "ffmpeg"
# sample_duration_seconds = 30
# min_confidence = 0.90
# instrumental_classes = ["Music", "Musical instrument"]
# vocal_classes = ["Singing", "Vocal music", "Choir", "A capella", "Chant", "Rapping", "Child singing", "Synthetic singing", "Yodeling", "Humming"]
# vocal_max_confidence = 0.015
# speech_classes = ["Speech"]
# speech_max_confidence = 0.20
# spread_samples = 6
# ffprobe_path = ""
# cooldown_seconds = 5
```

Optional audio-based instrumental detection sidecar (env: `MXLRC_INSTRUMENTAL_DETECTOR_ENABLED`, `MXLRC_INSTRUMENTAL_DETECTOR_CLASSIFIER_URL`, `MXLRC_INSTRUMENTAL_DETECTOR_FFMPEG_PATH`, `MXLRC_INSTRUMENTAL_DETECTOR_SAMPLE_DURATION_SECONDS`, `MXLRC_INSTRUMENTAL_DETECTOR_MIN_CONFIDENCE`, `MXLRC_INSTRUMENTAL_DETECTOR_CLASSES`, `MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_CLASSES`, `MXLRC_INSTRUMENTAL_DETECTOR_VOCAL_MAX_CONFIDENCE`, `MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_CLASSES`, `MXLRC_INSTRUMENTAL_DETECTOR_SPEECH_MAX_CONFIDENCE`, `MXLRC_INSTRUMENTAL_DETECTOR_SPREAD_SAMPLES`, `MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH`, `MXLRC_INSTRUMENTAL_DETECTOR_COOLDOWN_SECONDS`).

When enabled and a `classifier_url` is set, the detector samples each track's audio with ffmpeg and sends the sample to an external AudioSet classifier (a YAMNet sidecar; vendored at `deploy/yamnet-detector/`). It runs only on provider misses and never overrides provider-supplied data. A track is marked instrumental only when **all three** gates pass: the **music gate** (summed mean probability of the `instrumental_classes` meets `min_confidence`), the **sung-vocal gate** (no `vocal_classes` member peaks at or above `vocal_max_confidence`), and the **speech gate** (#403 -- the frame mean of `speech_classes` stays below `speech_max_confidence`; `Speech` moved out of `vocal_classes` into its own mean-based gate so sustained speech blocks while brief incidental speech does not). The decision is conservative: any doubt resolves to "not instrumental", because a false instrumental suppresses a real lyrics fetch.

To catch vocals that enter after an instrumental intro (arias, jazz, classical), the detector samples `spread_samples` short windows spread across the **whole** track, concatenated into one sample, and gates on the per-class **max-over-frames** peak (the loudest singing moment), which the frame mean dilutes. This requires the sidecar to return `{"mean": {...}, "max": {...}}`; a legacy mean-only sidecar degrades safely to never-instrumental.

`sample_duration_seconds` is clamped to [30, 60]. `min_confidence` and `vocal_max_confidence` values outside (0, 1] reset to `0.90` and `0.015` respectively. `cooldown_seconds` is the minimum gap between consecutive inference calls; `0` disables the cooldown. Track-duration probing needs `ffprobe`; the auto-provisioned ffmpeg ships none, so set `ffprobe_path` (or rely on a PATH ffprobe) or the detector falls back to a single window. **Deploy order:** upgrade Canticle before the sidecar (new Canticle tolerates the old flat-map response; old Canticle cannot parse `{mean,max}`). See **[Instrumental Detection](instrumental-detection.md)** for the full reference (decision model, sidecar setup, and tuning), and the [Instrumental detection](USER_GUIDE.md#instrumental-detection) guide for the per-library and per-run override controls.

ffmpeg resolution is shared with `[verification]`; see [ffmpeg resolution](#ffmpeg-resolution) below. Set `ffmpeg_path` here only to pin a binary separately from the verification path.

### `[enrichment]`

```toml
[enrichment]
enabled = true
```

Global default for recording enrichment (env: `MXLRC_ENRICHMENT_ENABLED`). When enabled, the scanner reads the ISRC, MusicBrainz recording ID, and audio duration from each file's tags and passes them to the matcher to disambiguate results.

Default `true`, preserving the always-on behavior from before per-library control existed. Per-library and per-run flags override this; see [Recording enrichment](USER_GUIDE.md#recording-enrichment) for the full precedence chain.

### `[realign]`

```toml
[realign]
enabled = false
on_scan = false
require_provenance = false
cross_directory = false
identity_keys = ["mbid", "isrc"]
min_confidence = 0.75
```

Governs the `realign` command, which re-attaches orphaned `.lrc` / `.txt` sidecars to renamed audio via a four-tier confidence resolver (see [Realign](CLI_REFERENCE.md#realign)), and serve mode's reactive realign that runs it automatically. Defaults are conservative: the feature is off, provenance is not required, matches are confined to the orphan's directory, and identity is matched MBID-first then ISRC.

When `enabled`, serve mode runs a scoped realign on three triggers: the filesystem watcher's directory-change events, Lidarr rename/import/upgrade webhooks (complementing `contrib/lidarr-rename-sidecars.sh`; the webhook sweeps both the new and the old/previous directories, so a manual import that strands a sidecar self-heals), and -- when `on_scan` is also set -- after each library scan. Reactive realign always writes a JSONL backup (`<db-dir>/realign-serve-backup.jsonl`), never clobbers an existing sidecar, and auto-applies only exact provenance (ISRC/MBID) matches unless `auto_apply_heuristic` is set. Note that relocating a sidecar to audio in a *different* directory additionally requires `cross_directory` and a provenance tag embedded in the sidecar.

| Key | Env | Default | Meaning |
|---|---|---|---|
| `enabled` | `MXLRC_REALIGN_ENABLED` | `false` | Master switch for serve-mode reactive realign (watcher / webhook / post-scan). The `realign` CLI command runs regardless. |
| `on_scan` | `MXLRC_REALIGN_ON_SCAN` | `false` | Also run a library-wide realign after each periodic or manual scan (the watcher and webhook triggers are governed by `enabled` alone). |
| `require_provenance` | `MXLRC_REALIGN_REQUIRE_PROVENANCE` | `false` | Only apply exact (ISRC/MBID) matches; heuristic candidates are reported but skipped. |
| `cross_directory` | `MXLRC_REALIGN_CROSS_DIRECTORY` | `false` | Allow an exact match to move a sidecar to an audio file in a different directory within the same library. |
| `identity_keys` | `MXLRC_REALIGN_IDENTITY_KEYS` | `["mbid","isrc"]` | Ordered provenance identifiers the exact tier matches on, most authoritative first. Valid values: `mbid`, `isrc`. |
| `min_confidence` | `MXLRC_REALIGN_MIN_CONFIDENCE` | `0.75` | Jaro-Winkler name-similarity floor (0-1) for a heuristic rename. When neither side yields an artist/title the check is skipped (positional matching). |
| `auto_apply_heuristic` | `MXLRC_REALIGN_AUTO_APPLY_HEURISTIC` | `false` | Whether serve-mode reactive realign may auto-apply heuristic (name-similarity) matches. Off = unattended realign applies only exact provenance matches; the manual CLI is unaffected. |

The exact tier requires ISRC/MBID-tagged audio; libraries without those tags fall back to the heuristic tier.

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
batch_size = 10
```

The worker shuffles its dequeue order within each priority tier so it stops querying the upstream API in strict alphabetical (library insertion) order, which is a plausible scraping fingerprint. This is **on by default** and affects only the library/serve worker path (`Dequeue`); inspection output (`queue list`) stays deterministic, and the one-shot `fetch` CLI never touches the work queue. Set `randomize = false` (or `MXLRC_QUEUE_RANDOMIZE=false`) to restore the deterministic `created_at`/`id` ordering. The env var overrides the TOML key; an invalid value warns and keeps the current setting.

`batch_size` controls a shuffled lookahead buffer: rather than reshuffling the whole eligible pool on every claim (`ORDER BY RANDOM()`), the worker draws `batch_size` rows at random once, stamps them with an order, and serves them in it before drawing again. This gives the queue a stable, knowable near-term order (so upcoming work can be displayed and starvation is observable) and lets the claim use an index instead of sorting the whole table each time. The randomness moves from per-item to per-batch, so an external observer still sees a randomly composed request stream and the anti-fingerprint property is preserved. Webhook-priority work still preempts within one dequeue. Default `10`; set `batch_size = 0` (or `MXLRC_QUEUE_BATCH_SIZE=0`) to disable batching and restore the per-item random path, a clean rollback. Only applies when `randomize = true`.

### `[watcher]`

```toml
[watcher]
# enabled = false
# debounce_ms = 2000
# max_dirs = 100000
```

Optional low-latency filesystem watcher that layers targeted scans over the periodic scheduler (env: `MXLRCGO_WATCH_ENABLED`, `MXLRCGO_WATCH_DEBOUNCE_MS`, `MXLRCGO_WATCH_MAX_DIRS`). It is **off by default**. See [Filesystem watcher](USER_GUIDE.md#filesystem-watcher-optional-low-latency-scans) in the User Guide for the operational detail.

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | `false` | Master switch for the watcher. |
| `debounce_ms` | `2000` | Quiet period in milliseconds after the last filesystem event before a targeted scan fires; coalesces event storms. A non-positive value is clamped to the default at construction. |
| `max_dirs` | `100000` | Caps how many directories may be watched before startup fails - a safety valve so a misconfigured root fails fast instead of exhausting the kernel inotify watch budget. A non-positive value is clamped to the default. |

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
