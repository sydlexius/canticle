# syntax=docker/dockerfile:1

FROM golang:1.26.4-alpine@sha256:f23e8b227fb4493eabe03bede4d5a32d04092da71962f1fb79b5f7d1e6c2a17f AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/mxlrcgo-svc ./cmd/mxlrcgo-svc

FROM alpine:3.23.4@sha256:5b10f432ef3da1b8d4c7eb6c487f2f5a8f096bc91145e68878dd4a5019afde11

# Runtime stage. KEEP IN SYNC with build/docker/Dockerfile.goreleaser (the goreleaser
# release image): identical base digest, apk packages, user, ENV, EXPOSE, VOLUME, entrypoint.
# The two differ only in how the binary arrives (built here vs copied by goreleaser).
LABEL org.opencontainers.image.source="https://github.com/sydlexius/mxlrcgo-svc" \
      org.opencontainers.image.description="Fetch synced lyrics from Musixmatch and save .lrc files" \
      org.opencontainers.image.licenses="GPL-3.0"

RUN apk add --no-cache bash ca-certificates su-exec tzdata && \
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
