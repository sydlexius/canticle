<p align="center">
  <img src="img/canticle-wordmark.svg" alt="Canticle" width="300"/>
</p>

# Canticle

Canticle is a Go command-line tool and webhook service that fetches synced lyrics from [Musixmatch](https://www.musixmatch.com/) and saves them as `.lrc` files (falling back to `.txt` for unsynced lyrics or instrumental markers). It runs one-shot from the CLI, recursively over a media directory, or as a long-running Lidarr webhook server with a durable work queue, scheduled library scans, and an optional filesystem watcher.

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

- [Install Guide](INSTALL.md) - comparison of native packages, Docker, Homebrew, tarball, and source installs; first-time setup per method.
- [User Guide](USER_GUIDE.md) - run the webhook server, Docker and Unraid deployment, native-package service management, path resolution, health endpoints, the filesystem watcher, shell completion, and inspection commands.
- [CLI Reference](CLI_REFERENCE.md) - the full usage text, every subcommand and flag, `--version` output, and the library/key-management commands.
- [Configuration](CONFIGURATION.md) - the complete environment-variable table, the TOML config keys, token precedence, and XDG/Docker/native-package path defaults.
- [Instrumental Detection](instrumental-detection.md) - the optional audio-based instrumental detector: the two-gate decision model, the YAMNet sidecar setup and `{mean,max}` contract, deploy ordering, and threshold tuning.
- [Developer Guide](DEVELOPER.md) - development setup, make targets, the quality gate, contributing notes, and design decisions.

## Legal

- [Privacy Policy](privacy-policy.md) - what data leaves your machine during a lyrics lookup and what does not.
- [Code Signing Policy](code-signing-policy.md) - SignPath attribution, team roles, and release approval process.
