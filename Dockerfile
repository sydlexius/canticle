# syntax=docker/dockerfile:1

FROM golang:1.26.4-alpine@sha256:7a3e50096189ad57c9f9f865e7e4aa8585ed1585248513dc5cda498e2f41812c AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mxlrcgo-svc ./cmd/mxlrcgo-svc

FROM alpine:3.24.0@sha256:a2d49ea686c2adfe3c992e47dc3b5e7fa6e6b5055609400dc2acaeb241c829f4

# Runtime stage. KEEP IN SYNC with build/docker/Dockerfile.goreleaser (the goreleaser
# release image): identical base digest, apk packages, user, ENV, EXPOSE, VOLUME, entrypoint.
# The two differ only in how the binary arrives (built here vs copied by goreleaser).
LABEL org.opencontainers.image.source="https://github.com/sydlexius/mxlrcgo-svc" \
      org.opencontainers.image.description="Fetch synced lyrics from Musixmatch and save .lrc files" \
      org.opencontainers.image.licenses="GPL-3.0" \
      net.unraid.docker.webui="http://[IP]:[PORT:50705]/"

RUN apk add --no-cache bash ca-certificates ffmpeg su-exec tzdata && \
    apk upgrade --no-cache && \
    { grep -q "^mxlrcgo:" /etc/group || addgroup mxlrcgo; } && \
    { id -u mxlrcgo >/dev/null 2>&1 || adduser -u 99 -G mxlrcgo -s /bin/bash -D mxlrcgo; } && \
    mkdir -p /config /music && \
    chown mxlrcgo:mxlrcgo /config /music

COPY --from=build /out/mxlrcgo-svc /usr/local/bin/mxlrcgo-svc
COPY build/docker/entrypoint.sh /entrypoint.sh
# Make the entrypoint executable and pre-ship bash completion: generate the
# static wrapper and source it from the system bashrc so interactive
# `docker exec -it ... bash` sessions get tab-completion with no manual sourcing.
# Alpine's bash compiles SYS_BASHRC=/etc/bash/bashrc (verified: that file is the
# one an interactive non-login shell reads here). We also write the conventional
# /etc/bash.bashrc for robustness if the base image ever changes; both are guarded.
RUN chmod +x /entrypoint.sh && \
    mxlrcgo-svc completion bash > /etc/bash/mxlrcgo-svc.bash && \
    printf '\n[ -f /etc/bash/mxlrcgo-svc.bash ] && . /etc/bash/mxlrcgo-svc.bash\n' \
      | tee -a /etc/bash/bashrc /etc/bash.bashrc > /dev/null

ENV MXLRC_DOCKER=true \
    MXLRC_SERVER_ADDR=0.0.0.0:50705

WORKDIR /config
EXPOSE 50705
VOLUME ["/config", "/music"]

# USER is intentionally omitted so entrypoint.sh can perform PUID/PGID
# remapping and volume ownership fixes as root before dropping to mxlrcgo.
ENTRYPOINT ["/entrypoint.sh"]
CMD ["mxlrcgo-svc", "serve"]
