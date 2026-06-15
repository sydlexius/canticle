# mxlrcgo-svc

[![CI](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/ci.yml/badge.svg)](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/ci.yml)
[![Release](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/release.yml/badge.svg)](https://github.com/sydlexius/mxlrcgo-svc/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/sydlexius/mxlrcgo-svc/branch/main/graph/badge.svg)](https://codecov.io/gh/sydlexius/mxlrcgo-svc)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/sydlexius/mxlrcgo-svc/badge)](https://securityscorecards.dev/viewer/?uri=github.com/sydlexius/mxlrcgo-svc)

Command line tool and webhook service to fetch synced lyrics from [Musixmatch](https://www.musixmatch.com/) and save them as `.lrc` files.

## Documentation

Full documentation is published at **<https://sydlexius.github.io/mxlrcgo-svc/>**:

- [Getting Started](https://sydlexius.github.io/mxlrcgo-svc/GETTING_STARTED/) - onboarding guide: pick a path (one-shot, directory, or daemon) and get to working lyrics.
- [User Guide](https://sydlexius.github.io/mxlrcgo-svc/USER_GUIDE/) - webhook server, Docker/Unraid, the filesystem watcher, inspection commands.
- [CLI Reference](https://sydlexius.github.io/mxlrcgo-svc/CLI_REFERENCE/) - every subcommand and flag.
- [Configuration](https://sydlexius.github.io/mxlrcgo-svc/CONFIGURATION/) - env vars, TOML keys, token precedence, XDG paths.
- [Developer Guide](https://sydlexius.github.io/mxlrcgo-svc/DEVELOPER/) - build, test, the quality gate, design decisions.

## Install

**macOS / Linuxbrew (Homebrew):**

```sh
brew install sydlexius/tap/mxlrcgo-svc
```

**Linux (.deb / .rpm / .apk):** Download the appropriate package for your distro
from the [GitHub Releases](https://github.com/sydlexius/mxlrcgo-svc/releases)
page and install it with your package manager:

```sh
# Debian / Ubuntu
sudo dpkg -i mxlrcgo-svc_*.deb

# RHEL / Fedora / Rocky
sudo rpm -i mxlrcgo-svc_*.rpm

# Alpine
sudo apk add --allow-untrusted mxlrcgo-svc_*.apk
```

The package installs the binary to `/usr/local/bin/mxlrcgo-svc`, a systemd unit
(or OpenRC script on Alpine), and an example config at
`/etc/mxlrcgo-svc/config.example.toml`. It also creates a `mxlrcgo-svc`
system user and a state directory at `/var/lib/mxlrcgo-svc` (mode `0750`) that
holds the SQLite database. The service does **not** start automatically on
install.

After installing, copy the example config, set your token, and start the service:

```sh
sudo cp /etc/mxlrcgo-svc/config.example.toml /etc/mxlrcgo-svc/config.toml
# edit /etc/mxlrcgo-svc/config.toml and set [api] token = "YOUR_TOKEN"
sudo systemctl enable --now mxlrcgo-svc   # systemd
# or: sudo rc-update add mxlrcgo-svc default && sudo rc-service mxlrcgo-svc start  # Alpine OpenRC
```

The state directory and system user are preserved on package removal so the
database survives upgrades and reinstalls. See the
[User Guide](https://sydlexius.github.io/mxlrcgo-svc/USER_GUIDE/#native-packages)
for service commands, log access, and data paths.

**Tarballs / macOS / Windows:** Versioned archives for all platforms are also
available on the [GitHub Releases](https://github.com/sydlexius/mxlrcgo-svc/releases)
page.

**Build from source** (requires Go 1.26.4+):

```sh
go install github.com/sydlexius/mxlrcgo-svc/cmd/mxlrcgo-svc@latest
```

> This fork starts its release line at `v1.0.0`. The upstream `fashni/mxlrc-go` repository does not publish semver release tags, so `v1.0.0` is reserved as the first `mxlrcgo-svc` version.

## Quickstart

```sh
# One song
mxlrcgo-svc adele,hello

# Multiple songs into a custom output directory
mxlrcgo-svc adele,hello "the killers,mr. brightside" -o some_directory

# Directory mode (recursive): writes each lyric file next to its audio file
mxlrcgo-svc "Dream Theater"

# Lidarr webhook server
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key \
  mxlrcgo-svc serve --listen 127.0.0.1:3876
```

Directory mode overrides `-o/--outdir`; the output extension is `.lrc` for synced lyrics and `.txt` for unsynced lyrics or an instrumental marker. See the [CLI Reference](https://sydlexius.github.io/mxlrcgo-svc/CLI_REFERENCE/) for every flag and the [User Guide](https://sydlexius.github.io/mxlrcgo-svc/USER_GUIDE/) for Docker, Unraid, and webhook deployment.

## Token

A Musixmatch API token is required. Supply it via the `--token` CLI flag, the `MUSIXMATCH_TOKEN` environment variable, or a `.env`/config file, in that order of precedence (CLI > env > file). To get a token, follow steps 1 to 5 from the [Spicetify guide](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work). See [Configuration](https://sydlexius.github.io/mxlrcgo-svc/CONFIGURATION/) for the full env-var and TOML surface.

## Encrypted secrets

The Musixmatch token and the webhook API key can be stored encrypted at rest in the SQLite database (AES-256-GCM) instead of as plaintext in config and environment variables. It is opt-in and backward compatible: the encrypted store is the lowest-precedence source, so existing env/TOML setups are unchanged. Import the current plaintext with `mxlrcgo-svc secrets import`, set one by name from stdin with `mxlrcgo-svc secrets set <name>`, and list stored names (never values) with `mxlrcgo-svc secrets list`. The 32-byte master key is auto-generated as a `0600` key file on first use (the universal zero-setup default on all platforms including Docker). Set `MXLRC_MASTER_KEY` to an optional base64-encoded override for key/data separation (recommended when the threat model includes whole-volume theft). Losing the key makes the encrypted secrets unrecoverable by design; the remedy is to re-import or re-set them with the original plaintext. See the [Encrypted secrets guide](https://sydlexius.github.io/mxlrcgo-svc/USER_GUIDE/#encrypted-secrets).

## Credits

- [Spicetify Lyrics Plus](https://github.com/spicetify/spicetify-cli/tree/master/CustomApps/lyrics-plus)
- Forked from [fashni/mxlrc-go](https://github.com/fashni/mxlrc-go).

## Legal

- [Privacy Policy](https://sydlexius.github.io/mxlrcgo-svc/privacy-policy/) - what data leaves your machine during a lyrics lookup and what does not.
- [Code Signing Policy](https://sydlexius.github.io/mxlrcgo-svc/code-signing-policy/) - SignPath attribution, team roles, and release approval process.

## License

[GPL-3.0](LICENSE). This project is a fork of [fashni/mxlrc-go](https://github.com/fashni/mxlrc-go), which is MIT-licensed; the original MIT copyright and permission notice are retained in [NOTICE](NOTICE).
