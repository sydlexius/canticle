# Install Guide

This page compares the available install methods and helps you pick one. For a quick start after installing, see [Getting Started](GETTING_STARTED.md).

## Comparison

| Method | Best for | Auto-updates | System service | State location |
|--------|----------|:------------:|:--------------:|----------------|
| Native package (`.deb` / `.rpm` / `.apk`) | Linux servers, headless hosts | via package manager | yes (systemd / OpenRC) | `/var/lib/mxlrcgo-svc` |
| Docker / Compose | Containers, Unraid, NAS | via image tag | via compose / container runtime | `/config` volume |
| Homebrew | macOS, Linuxbrew | `brew upgrade` | via `brew services` | XDG data dir |
| Tarball / zip | Any platform, air-gapped | manual | manual | XDG data dir |
| `go install` | Developers, bleeding-edge | manual | manual | XDG data dir |

---

## Native packages (Linux)

Download the `.deb`, `.rpm`, or `.apk` for your distro from the
[GitHub Releases](https://github.com/sydlexius/mxlrcgo-svc/releases) page.

```sh
# Debian / Ubuntu
sudo apt install ./mxlrcgo-svc_*.deb

# RHEL / Fedora / Rocky
sudo dnf install ./mxlrcgo-svc_*.rpm

# Alpine
sudo apk add --allow-untrusted mxlrcgo-svc_*.apk
```

**What the package does:**

- Installs the binary to `/usr/local/bin/mxlrcgo-svc`.
- Creates a `mxlrcgo-svc` system user and group (no login shell).
- Creates `/var/lib/mxlrcgo-svc` (mode `0750`, owned by `mxlrcgo-svc:mxlrcgo-svc`) for the SQLite database and state.
- Installs a systemd unit with hardening (`ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`), or an OpenRC script on Alpine (manages ownership and permissions via `start_pre`).
- Places an example config at `/etc/mxlrcgo-svc/config.example.toml`.
- Does **not** enable or start the service automatically.

**First-time setup:**

```sh
sudo cp /etc/mxlrcgo-svc/config.example.toml /etc/mxlrcgo-svc/config.toml
# Edit config.toml and set [api] token = "YOUR_TOKEN" (and any other settings)
sudo systemctl enable --now mxlrcgo-svc        # systemd
# or on Alpine:
# sudo rc-update add mxlrcgo-svc default
# sudo rc-service mxlrcgo-svc start
```

**Service commands (systemd):**

```sh
sudo systemctl start   mxlrcgo-svc
sudo systemctl stop    mxlrcgo-svc
sudo systemctl restart mxlrcgo-svc
sudo systemctl status  mxlrcgo-svc
sudo journalctl -u mxlrcgo-svc -f
```

**Service commands (OpenRC / Alpine):**

```sh
sudo rc-service mxlrcgo-svc start
sudo rc-service mxlrcgo-svc stop
sudo rc-service mxlrcgo-svc restart
sudo rc-service mxlrcgo-svc status
```

**Uninstall note:** Package removal stops the service but preserves
`/var/lib/mxlrcgo-svc` and the system user so the database survives a
reinstall or upgrade. Remove them manually for a clean uninstall:

```sh
sudo rm -rf /var/lib/mxlrcgo-svc
sudo userdel mxlrcgo-svc
sudo groupdel mxlrcgo-svc
```

See [Native packages](USER_GUIDE.md#native-packages) in the User Guide for the
full operational reference.

---

## Docker

The published image is `ghcr.io/sydlexius/mxlrcgo-svc`. It runs the server on
port `50705` and stores config and the SQLite database under the `/config`
volume. Mount your media data parent to `/data`:

Export secrets first so they are not inlined in the command (prevents shell history and `ps` exposure):

```sh
export MUSIXMATCH_TOKEN=YOUR_TOKEN
export MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key
```

```sh
docker run -d \
  --name mxlrcgo-svc \
  -p 50705:50705 \
  -e MUSIXMATCH_TOKEN \
  -e MXLRC_WEBHOOK_API_KEY \
  -e PUID=99 -e PGID=100 \
  -e MXLRC_OUTPUT_DIR=/data/media/music \
  -v mxlrcgo-svc-config:/config \
  -v /path/to/your/data:/data:rw \
  --restart unless-stopped \
  ghcr.io/sydlexius/mxlrcgo-svc:latest
```

For Docker Compose, copy `docker-compose.example.yml`, fill in the token and
key, adjust the music volume, and run `docker compose up -d`.

See the [User Guide](USER_GUIDE.md#docker) for the full Docker and Unraid
setup.

---

## Homebrew (macOS / Linuxbrew)

```sh
brew install sydlexius/tap/mxlrcgo-svc
```

Upgrade with `brew upgrade mxlrcgo-svc`. Run as a background service with
`brew services start mxlrcgo-svc`. Storage defaults follow XDG base directories.

---

## Tarball / zip

Download the archive for your platform from the
[GitHub Releases](https://github.com/sydlexius/mxlrcgo-svc/releases) page,
extract the binary, and place it on your `PATH`. On Windows, the signed `.zip`
extracts `mxlrcgo-svc.exe`; see the [Windows](USER_GUIDE.md#windows) section of
the User Guide for NSSM service installation.

---

## Build from source

Requires Go 1.26.4 or later.

```sh
go install github.com/sydlexius/mxlrcgo-svc/cmd/mxlrcgo-svc@latest
```

Or clone the repository and use `make`:

```sh
git clone https://github.com/sydlexius/mxlrcgo-svc.git
cd mxlrcgo-svc
make build
```

See the [Developer Guide](DEVELOPER.md) for the full build and test setup.
