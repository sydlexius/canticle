# CLI Reference

This page documents every subcommand and flag. For operational guidance (running the server, Docker/Unraid, the watcher), see the [User Guide](USER_GUIDE.md). For every setting, see [Configuration](CONFIGURATION.md).

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

## Version

`mxlrcgo-svc --version` prints the embedded build metadata, for example
`mxlrcgo-svc v1.1.0 (commit 1a2b3c4, built 2026-06-05T00:00:00Z)`. Release
binaries and the published Docker images carry the real tag; a `go build` or
`go install` from source reports `dev` unless you inject the ldflags yourself.

## Fetch

One-shot lyric fetching without the HTTP server or DB queue.

### One song

```sh
mxlrcgo-svc adele,hello
mxlrcgo-svc fetch adele,hello
```

### Multiple songs and a custom output directory

```sh
mxlrcgo-svc adele,hello "the killers,mr. brightside" -o some_directory
```

### With a text file and a custom cooldown time

```sh
mxlrcgo-svc example_input.txt -c 20
```

### Directory mode (recursive)

```sh
mxlrcgo-svc "Dream Theater"
```

> **_This option overrides the `-o/--outdir` argument which means the lyrics will be saved in the same directory as the given input._**
>
> **_The output extension depends on the lyric type: `.lrc` when synced lyrics are found, and `.txt` when only unsynced lyrics or an instrumental marker is written._**
>
> **_The `-d/--depth` argument limits the depth of subdirectories to scan; use `-d 0` or `--depth 0` to only scan the specified directory._**

The `--upgrade` flag re-fetches tracks that previously produced a `.txt` (unsynced) file, to promote them to `.lrc` when synced lyrics later become available. Instrumental tracks are always written as `.txt` and are excluded from upgrade - only `--update` (full re-fetch) overrides them.

In directory mode, when audio tags carry ISRC, MusicBrainz recording ID, or duration, those values are read and passed to Musixmatch to improve match precision - for example, distinguishing two recordings of the same title.

## Serve

Run the HTTP server, worker, and library scheduler. See the [User Guide](USER_GUIDE.md#lidarr-webhook-server) for full operational detail.

```sh
mxlrcgo-svc serve --listen 127.0.0.1:3876
mxlrcgo-svc serve --config path/to/config.toml
```

Relevant serve flags: `--listen` (overrides `MXLRC_SERVER_ADDR`), `--scan-interval`, `--work-interval`, and `--config`.

## Library and key management

```sh
mxlrcgo-svc library add /data/media/music --name Music
mxlrcgo-svc library list
mxlrcgo-svc scan
mxlrcgo-svc keys create --name lidarr --scope webhook
mxlrcgo-svc keys list
mxlrcgo-svc config get db.path
```

## Queue and scan inspection

The `queue` and `scan` subcommands expose the durable work queue and persisted scan results. See [Inspection commands](USER_GUIDE.md#inspection-commands) in the User Guide for the full command set (`queue list`/`failed`/`deferred`/`retry`/`clear`, and `scan results`/`clear`).

## Shell completion

```sh
mxlrcgo-svc completion <bash|zsh|fish>
```

See [Shell completion](USER_GUIDE.md#shell-completion) for installation snippets.
