# Pinned by digest (Scorecard Pinned-Dependencies). The tag is kept for
# readability; Docker resolves by the digest. Update both together:
#   docker pull python:3.11-slim && docker inspect --format '{{index .RepoDigests 0}}' python:3.11-slim
FROM python:3.11-slim@sha256:cdbd05fb6f457ca275ff51ce00d93d865ca0b6a25f5ffb08262d94f6835771e5

# libsndfile is the native backend soundfile needs to decode the WAV samples.
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
ENV TFHUB_CACHE_DIR=/app/tfhub_cache
RUN python -c "import tensorflow_hub as hub; hub.load('https://tfhub.dev/google/yamnet/1')"

# Drop privileges (matches the unprivileged posture of the rest of the stack).
RUN useradd -u 1000 -m appuser && chown -R appuser:appuser /app
USER appuser

EXPOSE 8080

# Single worker: YAMNet inference is CPU-bound and Canticle already serializes
# detector calls (one in flight at a time) behind a cooldown, so concurrency
# buys nothing and would multiply the memory footprint.
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1"]
