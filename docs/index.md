# mxlrcgo-svc

`mxlrcgo-svc` is a Go command-line tool and webhook service that fetches synced lyrics from [Musixmatch](https://www.musixmatch.com/) and saves them as `.lrc` files (falling back to `.txt` for unsynced lyrics or instrumental markers). It runs one-shot from the CLI, recursively over a media directory, or as a long-running Lidarr webhook server with a durable work queue, scheduled library scans, and an optional filesystem watcher.

New here? Start with the [Getting Started](GETTING_STARTED.md) guide - it picks a path for you (one-shot, directory, or daemon) and gets you to working lyrics.

## Features at a glance

- One-shot `fetch` for a single song, multiple songs, or a text-file batch.
- Directory mode that walks a music library and writes each lyric file next to its audio file.
- A `serve` mode HTTP server that accepts Lidarr webhooks, with health/readiness endpoints and a durable SQLite work queue.
- Container-friendly path resolution that prefers scanned inventory, so Docker/Unraid mount differences do not need host-to-container path maps.
- Scheduled library scans plus an optional low-latency filesystem watcher.
- TOML config and environment-variable overrides for every setting, with CLI > env > file precedence.
- Shell completion (bash, zsh, fish) and inspection subcommands for the queue and scan results.

## Documentation

- [User Guide](USER_GUIDE.md) - run the webhook server, Docker and Unraid deployment, path resolution, health endpoints, the filesystem watcher, shell completion, and inspection commands.
- [CLI Reference](CLI_REFERENCE.md) - the full usage text, every subcommand and flag, `--version` output, and the library/key-management commands.
- [Configuration](CONFIGURATION.md) - the complete environment-variable table, the TOML config keys, token precedence, and XDG path defaults.
- [Developer Guide](DEVELOPER.md) - development setup, make targets, the quality gate, contributing notes, and design decisions.
