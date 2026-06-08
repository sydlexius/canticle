# Getting Started

`mxlrcgo-svc` fetches synced lyrics from [Musixmatch](https://www.musixmatch.com/) and saves them as `.lrc` files (falling back to `.txt` for unsynced lyrics or instrumental markers). This guide gets you from nothing to working lyrics, then points you at the reference pages for detail.

## Which path is for you?

- **Just trying it out?** Fetch one song from the command line. Start at [First fetch](#first-fetch-one-shot).
- **Have a music library on disk?** Point the tool at a folder and it writes a lyric file next to each track. See [Directory mode](#directory-mode).
- **Use Lidarr, or run containers?** Run it as a long-lived service that accepts Lidarr webhooks and scans your libraries on a schedule. See [Daemon / serve](#daemon-serve). If you do not know what Lidarr is, you probably do not need it.

Every path needs a Musixmatch token first.

## Get a Musixmatch token

A Musixmatch API token is required. Without one, every fetch returns a `401` and no lyrics are written. The token is a long opaque string (it is not your Musixmatch account password).

To obtain a token, follow steps 1 to 5 of the [Spicetify FAQ](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work). In our words: install the Spicetify lyrics setup it describes, open your browser developer tools on the network tab while lyrics load, find the Musixmatch request, and copy the `usertoken` value out of its query string. That copied string is your token.

The quickest way to provision it for a shell session:

```sh
export MUSIXMATCH_TOKEN=YOUR_TOKEN
```

The token can come from several places. Precedence, highest first:

1. `--token` CLI flag
2. `MUSIXMATCH_TOKEN` environment variable
3. `MXLRC_API_TOKEN` environment variable (lower-precedence alias)
4. `[api] token` in the TOML config file

See [Configuration](CONFIGURATION.md#token-precedence) for the full token and config-precedence detail.

## First fetch (one-shot)

With the token exported, fetch a single song. The query is `artist,title` - a comma, no spaces:

```sh
mxlrcgo-svc adele,hello
```

On success, a lyric file is written to the current directory (or to the directory you pass with `-o/--outdir`). What you get depends on what Musixmatch has:

- **`.lrc`** - synced lyrics, with per-line `[MM:SS.cc]` timestamps. This is the goal.
- **`.txt`** - unsynced (plain) lyrics, or an instrumental marker (`♪ Instrumental ♪`) when the track has no words.

A `.txt` result is not a failure. It means synced lyrics were not available, so the best available content was written instead. If synced lyrics appear later, you can promote the file (see `--upgrade` below).

For multiple songs, a text-file batch, and every flag, see the [CLI Reference](CLI_REFERENCE.md#fetch).

## Directory mode

Point the tool at a folder and it walks the tree, writing a lyric file next to each audio file:

```sh
mxlrcgo-svc /path/to/music
```

Notes:

- The lyric file is written **next to each audio file**, so `-o/--outdir` is ignored in directory mode.
- `-d/--depth` limits recursion depth (default `100`); `-d 0` scans only the given directory.
- `--upgrade` re-fetches tracks that previously produced a `.txt` (unsynced) file, to promote them to `.lrc` once synced lyrics become available.

A bare argument that matches an existing directory triggers a recursive scan. That means `mxlrcgo-svc "Dream Theater"` scans a folder named `Dream Theater`; it is not interpreted as a song query. Use the `artist,title` form for one-shot fetches.

Test on a single album first to confirm the result before scanning a whole library. See the [CLI Reference](CLI_REFERENCE.md#directory-mode-recursive) for the full flag list.

## Daemon / serve

For Lidarr or always-on container use, run the HTTP server. It accepts Lidarr webhooks, scans your registered libraries on a schedule, and processes work through a durable queue.

Register a library, then start the server:

```sh
mxlrcgo-svc library add /path/to/music --name Music
mxlrcgo-svc serve
```

`serve` listens on `MXLRC_SERVER_ADDR` (default `127.0.0.1:3876`) unless you pass `--listen`. Registering and scanning your libraries first is what lets webhooks reuse the exact file paths a scan discovered, so they work even when Lidarr and `mxlrcgo-svc` see the media through different mount paths.

### Lidarr webhook

Lidarr posts to:

```text
POST /api/v1/webhooks/lidarr
```

Create a webhook-scoped key for it:

```sh
mxlrcgo-svc keys create --name lidarr --scope webhook
```

The endpoint authenticates the key in one of two ways (the server accepts either):

- An `Authorization: Bearer <key>` header.
- An `apikey=<key>` query parameter on the request URL (checked first).

Configure whichever your client supports. See the [User Guide](USER_GUIDE.md#lidarr-webhook-server) for path resolution and full webhook behavior.

### Docker

The published image is `ghcr.io/sydlexius/mxlrcgo-svc`. It runs the server on container port `50705`, sets `MXLRC_DOCKER=true` automatically (so storage defaults resolve under `/config`), and honors `PUID`/`PGID` for file ownership. Mount your media data parent once (for example to `/data`):

```sh
docker run -d \
  --name mxlrcgo-svc \
  -p 50705:50705 \
  -e MUSIXMATCH_TOKEN=YOUR_TOKEN \
  -e MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key \
  -v mxlrcgo-svc-config:/config \
  -v /path/to/your/data:/data:rw \
  ghcr.io/sydlexius/mxlrcgo-svc:latest
```

See the [User Guide](USER_GUIDE.md#docker) for the full Docker, Compose, and Unraid setup.

## Verify

Confirm the build and that work is flowing.

```sh
mxlrcgo-svc --version
```

For one-shot and directory runs, check that the expected `.lrc`/`.txt` files were written.

For `serve`, the HTTP endpoints report health and status:

- `GET /healthz` - liveness; returns `200` whenever the server is accepting requests (unauthenticated).
- `GET /readyz` - readiness; returns `200` when the database is reachable, `503` when it is not (unauthenticated).
- `GET /api/v1/status` - a queue summary grouped by status; requires an `admin`-scoped API key.

The inspection subcommands show queue and scan state:

```sh
mxlrcgo-svc queue list
mxlrcgo-svc queue list --status pending --limit 100
mxlrcgo-svc scan results
mxlrcgo-svc scan results --library Music --status pending
```

See the [User Guide](USER_GUIDE.md#inspection-commands) for the full inspection command set.

## Troubleshooting

- **`401` / token rejected.** The token is missing, wrong, or expired. Re-check the [precedence](#get-a-musixmatch-token): a `--token` flag or a stale environment variable can silently override the one you think you are using. Confirm with `--token` explicitly, then re-provision a fresh token from the Spicetify FAQ.
- **Rate limiting / circuit breaker.** When Musixmatch signals throttling, the worker opens a circuit breaker and pauses dequeuing globally to back off. If you hit this often, raise the request cooldown with `MXLRC_API_COOLDOWN` (seconds between requests). See [Configuration](CONFIGURATION.md#environment-variables) for the cooldown and circuit-breaker variables.
- **Benign miss / `deferred`.** A track Musixmatch has no lyrics for yet lands in `queue deferred`, not `queue failed`. This is not a failure; the row waits out a cooldown and re-checks itself later.
- **Unraid `/mnt/user` watcher caveat.** The optional filesystem watcher relies on inotify events, which Unraid `/mnt/user` (FUSE/shfs) mounts often do not deliver into the container. Keep the periodic scan as the source of truth there; do not set the scan interval to `0`. Note the watcher switch is `MXLRCGO_WATCH_ENABLED` (the `MXLRCGO_` prefix, not `MXLRC_`).

## Next steps

- [CLI Reference](CLI_REFERENCE.md) - every subcommand and flag.
- [User Guide](USER_GUIDE.md) - the webhook server, Docker/Unraid, the filesystem watcher, and inspection commands.
- [Configuration](CONFIGURATION.md) - the full environment-variable table, TOML keys, token precedence, and XDG paths.
- [Developer Guide](DEVELOPER.md) - building from source, the make targets, and design decisions.

Advanced (all shipped in v1.4.0; see [Configuration](CONFIGURATION.md) to enable): the language/script guard that rejects wrong-language matches, the `petitlyrics` provider as an alternative to Musixmatch, and `bilingual_output` for songs with a translation.
