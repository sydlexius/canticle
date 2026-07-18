# syntax=docker/dockerfile:1

FROM golang:1.26.5-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build

WORKDIR /src

# bash + curl run scripts/install-tailwind.sh (download + sha256 verify the
# node-free Tailwind standalone CLI); golang:alpine ships neither. libgcc +
# libstdc++ are the C++/unwind runtime the -musl Tailwind binary is dynamically
# linked against (_Unwind_* from libgcc_s, _ZSt*/__cxxabiv* from libstdc++);
# without them it aborts with "Error relocating ... symbol not found" (issue #366).
RUN apk add --no-cache bash curl libgcc libstdc++

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Generate the web UI assets in-image. They are no longer committed (issue #364):
# web/static/embed.go embeds web/static/css/output.css at COMPILE TIME, so templ
# + Tailwind MUST run before `go build`. Alpine is musl-linked, so install the
# -musl Tailwind variant (the glibc linux-x64 binary used in CI will not run
# here). SHA-validated by install-tailwind.sh against the release checksums.
RUN set -eux; \
    case "$(uname -m)" in \
      x86_64)  asset=tailwindcss-linux-x64-musl ;; \
      aarch64) asset=tailwindcss-linux-arm64-musl ;; \
      *) echo "unsupported build arch: $(uname -m)" >&2; exit 1 ;; \
    esac; \
    TAILWIND_ASSET="$asset" scripts/install-tailwind.sh /usr/local/bin/tailwindcss; \
    go tool templ generate; \
    /usr/local/bin/tailwindcss -i web/static/css/input.css -o web/static/css/output.css --minify

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/canticle ./cmd/mxlrcgo-svc

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b

# Runtime stage. KEEP IN SYNC with build/docker/Dockerfile.goreleaser (the goreleaser
# release image): identical base digest, apk packages, user, ENV, EXPOSE, VOLUME, entrypoint.
# The two differ only in how the binary arrives (built here vs copied by goreleaser).
LABEL org.opencontainers.image.source="https://github.com/sydlexius/canticle" \
      org.opencontainers.image.description="Fetch synced lyrics from Musixmatch and save .lrc files" \
      org.opencontainers.image.licenses="GPL-3.0" \
      net.unraid.docker.webui="http://[IP]:[PORT:50705]/"

# ffmpeg floor pinned to remediate CVE-2026-8461 (HIGH, fixed in Alpine 8.1.2-r0).
# The explicit version also cache-busts the GHA BuildKit layer so the image scan
# stops reusing a stale 8.1.1-r0 layer. Keep in sync with Dockerfile.goreleaser;
# raise/drop the floor when the base apk index ships a newer ffmpeg by default.
RUN apk add --no-cache bash ca-certificates "ffmpeg>=8.1.2-r0" su-exec tzdata && \
    apk upgrade --no-cache && \
    { grep -q "^mxlrcgo:" /etc/group || addgroup mxlrcgo; } && \
    { id -u mxlrcgo >/dev/null 2>&1 || adduser -u 99 -G mxlrcgo -s /bin/bash -D mxlrcgo; } && \
    mkdir -p /config /music && \
    chown mxlrcgo:mxlrcgo /config /music

COPY --from=build /out/canticle /usr/local/bin/canticle
COPY build/docker/entrypoint.sh /entrypoint.sh
# Make the entrypoint executable and pre-ship bash completion: generate the
# static wrapper and source it from the system bashrc so interactive
# `docker exec -it ... bash` sessions get tab-completion with no manual sourcing.
# Alpine's bash compiles SYS_BASHRC=/etc/bash/bashrc (verified: that file is the
# one an interactive non-login shell reads here). We also write the conventional
# /etc/bash.bashrc for robustness if the base image ever changes; both are guarded.
RUN chmod +x /entrypoint.sh && \
    canticle completion bash > /etc/bash/canticle.bash && \
    printf '\n[ -f /etc/bash/canticle.bash ] && . /etc/bash/canticle.bash\n' \
      | tee -a /etc/bash/bashrc /etc/bash.bashrc > /dev/null

ENV MXLRC_DOCKER=true \
    MXLRC_SERVER_ADDR=0.0.0.0:50705

WORKDIR /config
EXPOSE 50705
VOLUME ["/config", "/music"]

# USER is intentionally omitted so entrypoint.sh can perform PUID/PGID
# remapping and volume ownership fixes as root before dropping to mxlrcgo.
ENTRYPOINT ["/entrypoint.sh"]
CMD ["canticle", "serve"]
