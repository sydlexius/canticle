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
[GitHub Releases](https://github.com/sydlexius/canticle/releases) page.

```sh
# Debian / Ubuntu
sudo apt install ./canticle_*.deb

# RHEL / Fedora / Rocky
sudo dnf install ./canticle_*.rpm

# Alpine
sudo apk add --allow-untrusted canticle_*.apk
```

**What the package does:**

- Installs the binary to `/usr/local/bin/canticle`.
- Creates a `mxlrcgo-svc` system user and group (no login shell).
- Creates `/var/lib/mxlrcgo-svc` (mode `0750`, owned by `mxlrcgo-svc:mxlrcgo-svc`) for the SQLite database and state.
- Installs a systemd unit with hardening (`ProtectSystem=strict`, `PrivateTmp`, `NoNewPrivileges`), or an OpenRC script on Alpine (manages ownership and permissions via `start_pre`).
- Places an example config at `/etc/mxlrcgo-svc/config.example.toml`.
- Does **not** enable or start the service automatically.

> **Upgrading from `mxlrcgo-svc`:** the package was named `mxlrcgo-svc` through
> v1.9.1 and is now `canticle`. Installing the `canticle` package replaces an
> existing `mxlrcgo-svc` install in one step (it declares the appropriate
> deb `Replaces`/rpm `Obsoletes`), removing the old package and any stale
> binary it owned. The service unit, system user, and `/var/lib/mxlrcgo-svc`
> data directory intentionally keep the `mxlrcgo-svc` name so the SQLite
> database and an enabled service survive the rename untouched.

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

The published image is `ghcr.io/sydlexius/canticle`. It runs the server on
port `50705` and stores config and the SQLite database under the `/config`
volume. Mount your media data parent to `/data`:

Export secrets first so they are not inlined in the command (prevents shell history and `ps` exposure):

```sh
export MUSIXMATCH_TOKEN=YOUR_TOKEN
export MXLRC_WEBHOOK_API_KEY=mxlrc_your_webhook_key
```

```sh
docker run -d \
  --name canticle \
  -p 50705:50705 \
  -e MUSIXMATCH_TOKEN \
  -e MXLRC_WEBHOOK_API_KEY \
  -e PUID=99 -e PGID=100 \
  -v canticle-config:/config \
  -v /path/to/your/data:/data:rw \
  --restart unless-stopped \
  ghcr.io/sydlexius/canticle:latest
```

For Docker Compose, copy `docker-compose.example.yml`, fill in the token and
key, adjust the music volume, and run `docker compose up -d`.

See the [User Guide](USER_GUIDE.md#docker) for the full Docker and Unraid
setup.

### GPU host requirements (optional word-sync aligner sidecar)

Only the experimental forced-alignment sidecar (`deploy/aligner`) can use a
GPU; Canticle itself never does. The `aligner-gpu` compose profile (the
`VARIANT=cuda` image) needs:

- A Linux Docker host with an NVIDIA GPU of roughly compute capability 6.0
  (Pascal) through 9.0 (Hopper); a Quadro T600 (7.5) is supported. The CUDA
  build's ctranslate2 ships SASS for sm_53 to sm_86 plus PTX for 8.6, and its
  torch (cu126) ships sm_50 to sm_90 with no PTX. Hopper therefore relies on
  PTX JIT, which needs an R570 or newer driver (the JIT consumes CUDA 12.8
  PTX; the 525.60 floor does not cover it). Blackwell (10.x and
  12.x) is NOT supported by this torch build.
- The NVIDIA driver on the host (525.60 or newer for CUDA 12.6), and the
  [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)
  registered with Docker. The image carries the CUDA libraries itself; the
  toolkit injects the driver library at start.
- About 3.5 GB more to pull (~4.1 GB compressed against ~0.65 GB for CPU) and
  ~9.3 GB more on disk (~12.1 GB unpacked against ~2.8 GB), plus roughly 2 GB
  of GPU memory.

Docker Desktop on macOS has no GPU passthrough: use the CPU `aligner` profile
there. A host without a usable GPU running the CUDA image falls back to CPU
(`/health` reports `"device": "cpu"`). See `deploy/aligner/README.md`.

---

## Homebrew (macOS / Linuxbrew)

```sh
brew install sydlexius/tap/canticle
```

Upgrade with `brew upgrade canticle`. Run as a background service with
`brew services start canticle`. Storage defaults follow XDG base directories.

---

## Tarball / zip

Download the archive for your platform from the
[GitHub Releases](https://github.com/sydlexius/canticle/releases) page,
extract the binary, and place it on your `PATH`. On Windows, the signed `.zip`
extracts `canticle.exe`; see the [Windows](USER_GUIDE.md#windows) section of
the User Guide for NSSM service installation.

---

## Build from source

Requires Go 1.26.4 or later.

```sh
go install github.com/sydlexius/canticle/cmd/mxlrcgo-svc@latest
```

Or clone the repository and use `make`:

```sh
git clone https://github.com/sydlexius/canticle.git
cd canticle
make build
```

See the [Developer Guide](DEVELOPER.md) for the full build and test setup.
