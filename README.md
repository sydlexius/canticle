<p align="center">
  <img src="docs/img/canticle-wordmark.svg" alt="Canticle" width="380"/>
</p>

[![CI](https://github.com/sydlexius/canticle/actions/workflows/ci.yml/badge.svg)](https://github.com/sydlexius/canticle/actions/workflows/ci.yml)
[![Release](https://github.com/sydlexius/canticle/actions/workflows/release.yml/badge.svg)](https://github.com/sydlexius/canticle/actions/workflows/release.yml)
[![codecov](https://codecov.io/gh/sydlexius/canticle/branch/main/graph/badge.svg)](https://codecov.io/gh/sydlexius/canticle)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/sydlexius/canticle/badge)](https://securityscorecards.dev/viewer/?uri=github.com/sydlexius/canticle)

Command line tool and webhook service to fetch synced lyrics from [Musixmatch](https://www.musixmatch.com/) and save them as `.lrc` files.

## Documentation

Full documentation is published at **<https://sydlexius.github.io/canticle/>**:

- [Getting Started](https://sydlexius.github.io/canticle/GETTING_STARTED/) - onboarding guide: pick a path (one-shot, directory, or daemon) and get to working lyrics.
- [User Guide](https://sydlexius.github.io/canticle/USER_GUIDE/) - webhook server, Docker/Unraid, the filesystem watcher, inspection commands.
- [CLI Reference](https://sydlexius.github.io/canticle/CLI_REFERENCE/) - every subcommand and flag.
- [Configuration](https://sydlexius.github.io/canticle/CONFIGURATION/) - env vars, TOML keys, token precedence, XDG paths.
- [Developer Guide](https://sydlexius.github.io/canticle/DEVELOPER/) - build, test, the quality gate, design decisions.

## Install

**macOS / Linuxbrew (Homebrew):**

```sh
brew install sydlexius/tap/canticle
```

**Linux (.deb / .rpm / .apk):** Download the appropriate package for your distro
from the [GitHub Releases](https://github.com/sydlexius/canticle/releases)
page and install it with your package manager:

```sh
# Debian / Ubuntu
sudo apt install ./canticle_*.deb

# RHEL / Fedora / Rocky
sudo dnf install ./canticle_*.rpm

# Alpine
sudo apk add --allow-untrusted canticle_*.apk
```

The package installs the binary to `/usr/local/bin/canticle`, a systemd unit
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
[User Guide](https://sydlexius.github.io/canticle/USER_GUIDE/#native-packages)
for service commands, log access, and data paths.

**Tarballs / macOS / Windows:** Versioned archives for all platforms are also
available on the [GitHub Releases](https://github.com/sydlexius/canticle/releases)
page.

**Build from source** (requires Go 1.26.4+):

```sh
# The Go module path is unchanged by the Canticle rebrand; go install resolves
# it from go.mod, which still declares github.com/sydlexius/mxlrcgo-svc.
go install github.com/sydlexius/mxlrcgo-svc/cmd/mxlrcgo-svc@latest
```

> This fork starts its release line at `v1.0.0`. The upstream `fashni/mxlrc-go` repository does not publish semver release tags, so `v1.0.0` is reserved as the first `canticle` version.

## Quickstart

```sh
# One song
canticle adele,hello

# Multiple songs into a custom output directory
canticle adele,hello "the killers,mr. brightside" -o some_directory

# Directory mode (recursive): writes each lyric file next to its audio file
canticle "Dream Theater"

# Lidarr webhook server
MUSIXMATCH_TOKEN=YOUR_TOKEN MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key \
  canticle serve --listen 127.0.0.1:3876
```

Directory mode overrides `-o/--outdir`; the output extension is `.lrc` for synced lyrics and `.txt` for unsynced lyrics or an instrumental marker. See the [CLI Reference](https://sydlexius.github.io/canticle/CLI_REFERENCE/) for every flag and the [User Guide](https://sydlexius.github.io/canticle/USER_GUIDE/) for Docker, Unraid, and webhook deployment.

## Token

A Musixmatch API token is required. Supply it via the `--token` CLI flag, the `MUSIXMATCH_TOKEN` environment variable, or a `.env`/config file, in that order of precedence (CLI > env > file). To get a token, follow steps 1 to 5 from the [Spicetify guide](https://spicetify.app/docs/faq#sometimes-popup-lyrics-andor-lyrics-plus-seem-to-not-work). See [Configuration](https://sydlexius.github.io/canticle/CONFIGURATION/) for the full env-var and TOML surface.

## Encrypted secrets

The Musixmatch token and the webhook API key can be stored encrypted at rest in the SQLite database (AES-256-GCM) instead of as plaintext in config and environment variables. It is opt-in and backward compatible: the encrypted store is the lowest-precedence source, so existing env/TOML setups are unchanged. Import the current plaintext with `canticle secrets import`, set one by name from stdin with `canticle secrets set <name>`, and list stored names (never values) with `canticle secrets list`. The 32-byte master key is auto-generated as a `0600` key file on first use (the universal zero-setup default on all platforms including Docker). Set `MXLRC_MASTER_KEY` to an optional base64-encoded override for key/data separation (recommended when the threat model includes whole-volume theft). Losing the key makes the encrypted secrets unrecoverable by design; the remedy is to re-import or re-set them with the original plaintext. See the [Encrypted secrets guide](https://sydlexius.github.io/canticle/USER_GUIDE/#encrypted-secrets).

## Web UI access (serve mode)

The serve-mode browser UI is gated by a single admin account (session login, separate from the webhook API key). It is off by default; enable it with `web_ui_enabled = true` under `[server]`.

First run is interactive. With no admin yet, every UI page redirects to `/setup`, an onboarding form that creates the admin account and (optionally) stores the Musixmatch token and webhook API key encrypted at rest. `/setup` is reachable only from loopback or a configured trusted network (`[server.trusted_networks].cidrs`), so a stranger on the network cannot claim the admin account. After the admin exists, `/setup` is closed.

For headless deployments (Docker), you can skip the interactive form by setting both `MXLRC_WEBAUTH_ADMIN_USER` and `MXLRC_WEBAUTH_ADMIN_PASSWORD` in the environment. On startup, if no admin exists yet, the daemon creates one from these values (password must be at least 8 characters). It is idempotent (an existing admin is never overwritten) and the password is never logged. Treat these as bootstrap-only credentials: after first run, sign in and rotate the password, then remove the variables from the environment.

### TLS

TLS for the serve listener is off by default (plain HTTP), so deployments behind a TLS-terminating reverse proxy avoid double-encryption by leaving it disabled. Enable it under `[server.tls]` in one of two ways:

- **Bring-your-own certificate:** set `cert_file` and `key_file` (both required together). The listener terminates TLS itself with a TLS 1.2 minimum. Env: `MXLRC_TLS_CERT_FILE`, `MXLRC_TLS_KEY_FILE`.
- **Self-signed bootstrap:** set `self_signed = true` (mutually exclusive with `cert_file`/`key_file`). An ECDSA P-256 certificate (CN `mxlrcgo-svc`, ~365-day validity) is generated and persisted `0600` under `<dir(db_path)>/tls/`, and regenerated when missing or expired. Browsers show an untrusted-certificate prompt; this is intended for a LAN box, not public exposure. Env: `MXLRC_TLS_SELF_SIGNED`.

When TLS is on, the session cookie's `Secure` flag is set automatically. An optional `redirect_http` listen address (e.g. `":80"`, env `MXLRC_TLS_REDIRECT_HTTP`) runs a plain-HTTP listener that 301-redirects every request to the HTTPS address. A contradictory configuration (`self_signed` combined with a cert/key, or only one of `cert_file`/`key_file`) is a fatal startup error. ACME/Let's Encrypt is a planned follow-up.

## Credits

- [Spicetify Lyrics Plus](https://github.com/spicetify/spicetify-cli/tree/master/CustomApps/lyrics-plus)
- Forked from [fashni/mxlrc-go](https://github.com/fashni/mxlrc-go).

## Legal

- [Privacy Policy](https://sydlexius.github.io/canticle/privacy-policy/) - what data leaves your machine during a lyrics lookup and what does not.
- [Code Signing Policy](https://sydlexius.github.io/canticle/code-signing-policy/) - SignPath attribution, team roles, and release approval process.

## License

[GPL-3.0](LICENSE). This project is a fork of [fashni/mxlrc-go](https://github.com/fashni/mxlrc-go), which is MIT-licensed; the original MIT copyright and permission notice are retained in [NOTICE](NOTICE).
