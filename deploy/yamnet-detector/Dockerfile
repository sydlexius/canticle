# Pinned by digest (Scorecard Pinned-Dependencies). The tag is kept for
# readability; Docker resolves by the digest. Update both together -- use the
# multi-arch manifest-list (index) digest, NOT a local arch-specific one, so the
# pin resolves on any build arch:
#   docker buildx imagetools inspect python:3.11-slim --format '{{.Manifest.Digest}}'
FROM python:3.11-slim@sha256:cdbd05fb6f457ca275ff51ce00d93d865ca0b6a25f5ffb08262d94f6835771e5

# libsndfile is the native backend soundfile needs to decode the WAV samples.
# curl fetches the YAMNet SavedModel below; it is removed again in the same layer
# so it is not present in the final image.
RUN apt-get update && apt-get install -y --no-install-recommends \
        libsndfile1 \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt .
# --require-hashes enforces the sha256 hashes in the fully-pinned requirements.txt
# (Scorecard Pinned-Dependencies). requirements.txt is generated from
# requirements.in -- see the header in that file to regenerate.
RUN pip install --no-cache-dir --require-hashes -r requirements.txt

COPY app.py .

# Bake the YAMNet model into the image at build time so the container needs no
# network at runtime and the first request is fast.
#
# Fetched and checksum-verified directly rather than via tensorflow_hub (#491).
# hub.load() downloaded this same archive with no integrity check; pinning the
# sha256 means an unexpected upstream change fails the build instead of silently
# re-classifying tracks. tfhub.dev redirects to a short-lived signed Kaggle URL,
# so the stable front door plus this hash is the pinnable pair -- the redirect
# target is not.
#
# To update: download the URL below, `sha256sum` it, and bump YAMNET_SHA256.
# Bumping it correctly invalidates every stored verdict, since the weights really
# did change. Nothing else needs editing -- in particular do NOT update the hash
# baked into internal/db/migrations/038_detector_version_model_key.sql, which is a
# historical record of the model in use when that migration was written, not a
# current-value reference.
# A model change re-classifies tracks, so the verdict cache must invalidate when
# -- and ONLY when -- this hash moves. That is what /health now reports as
# `model_version` (see app.py): the hash IS the model identity, so it cannot
# drift from the weights it names.
#
# This used to be coupled to the CANTICLE APP VERSION instead, which forced the
# workaround "ship a model change as part of a release" and, worse, invalidated
# every stored verdict on every unrelated app release (#684). Do not reintroduce
# that coupling: a model change is now shippable on its own.
ARG YAMNET_URL=https://tfhub.dev/google/yamnet/1?tf-hub-format=compressed
ARG YAMNET_SHA256=b80da2a1a56926fb0767205051a200dd7b3beaf3ea1ea126c42a53943996e5e0

# Surfaced to the app so /health can report which weights are loaded. Baked at
# build time from the same ARG the download is verified against, so the reported
# version and the model on disk cannot disagree.
ENV YAMNET_MODEL_SHA256=${YAMNET_SHA256}

# Image provenance, readable WITHOUT pulling the image (#682):
#   docker buildx imagetools inspect ghcr.io/sydlexius/canticle-yamnet:latest \
#     --format '{{json .Image.Config.Labels}}'
#
# The problem this solves: the image is tagged with CANTICLE's version, so its
# tag says when it was built, not what changed in it. Two tags can be
# byte-identical in substance with nothing to tell an operator that, and -- the
# case that actually matters -- a tag whose MODEL changed looks exactly the same.
# The detector's three-gate thresholds are calibrated against a specific model,
# so a silent model swap is precisely what must not be silent.
#
# model.sha256 is the same ARG the download is checksum-verified against and the
# same value /health reports, so the label, the running service, and the bytes on
# disk cannot disagree. Comparing this label across two tags answers "did this
# actually change?" without pulling 1.56GB.
#
# class-count is the second half of the model's identity: app.py loads the class
# map from the SavedModel's own assets and the CI smoke test asserts 521, so a
# drifted model that kept the same hash (impossible) or a repacked one with a
# different map would show up here.
LABEL org.opencontainers.image.source="https://github.com/sydlexius/canticle" \
      org.opencontainers.image.description="YAMNet AudioSet classifier sidecar for canticle instrumental detection" \
      org.opencontainers.image.licenses="GPL-3.0" \
      net.sydlexius.canticle.yamnet.model.sha256="${YAMNET_SHA256}" \
      net.sydlexius.canticle.yamnet.model.source="${YAMNET_URL}" \
      net.sydlexius.canticle.yamnet.model.class-count="521"
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends curl ca-certificates; \
    # -f so an HTTP error page fails HERE rather than as a confusing sha256
    # mismatch downstream (tfhub.dev serves HTML on error and curl exits 0).
    curl -fsSL -o /tmp/yamnet.tar.gz "${YAMNET_URL}"; \
    echo "${YAMNET_SHA256}  /tmp/yamnet.tar.gz" | sha256sum -c -; \
    mkdir -p /app/yamnet; \
    tar xzf /tmp/yamnet.tar.gz -C /app/yamnet; \
    rm -f /tmp/yamnet.tar.gz; \
    test -f /app/yamnet/saved_model.pb; \
    test -f /app/yamnet/assets/yamnet_class_map.csv; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*

# Drop privileges (matches the unprivileged posture of the rest of the stack).
RUN useradd -u 1000 -m appuser && chown -R appuser:appuser /app
USER appuser

EXPOSE 8080

# Single worker: YAMNet inference is CPU-bound and Canticle already serializes
# detector calls (one in flight at a time) behind a cooldown, so concurrency
# buys nothing and would multiply the memory footprint.
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1"]
