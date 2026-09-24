# Pinned by digest (Scorecard Pinned-Dependencies). The tag is kept for
# readability; Docker resolves by the digest. Update both together -- use the
# multi-arch manifest-list (index) digest, NOT a local arch-specific one, so
# the pin resolves on any build arch:
#   docker buildx imagetools inspect python:3.13-slim --format '{{.Manifest.Digest}}'
#
# 3.13, the latest mainline CPython every package in this closure ships a
# linux/amd64 + linux/arm64 wheel for (#1017; torch 2.8.0, torchaudio 2.8.0,
# and ctranslate2 4.8.2 all publish cp313 manylinux wheels on both arches).
# 3.14 is NOT possible yet: torch 2.8.0 has no cp314 wheel on any platform.
# Both 3.11-slim (the prior base) and 3.13-slim resolve to Debian trixie
# (glibc 2.41), so the amd64 execstack analysis below is unaffected by this
# bump.
FROM python:3.13-slim@sha256:8d9d0b8bcf6506481eae4907c18f5e3e7902e629f5f6d684f9e7c32e85e3ddf0

# ffmpeg decodes every upload (app.decode_pcm is the sidecar's ONE audio
# decode path, #1016), so any container Canticle's library holds is accepted;
# libsndfile backs the soundfile dependency pulled in by the ML stack.
# patchelf clears the executable-stack flag on ctranslate2's bundled library
# (see below).
RUN apt-get update && apt-get install -y --no-install-recommends \
        libsndfile1 \
        ffmpeg \
        patchelf \
    && rm -rf /var/lib/apt/lists/*

# Image provenance (mirrors deploy/yamnet-detector's LABEL block). Model
# choices (Demucs for separation, faster-whisper + a direct wav2vec2 load for
# forced alignment, #1016) are informed by rzru/nightingale (GPL-3.0-or-later
# per its own repository, confirmed at authoring time) as a reference
# implementation -- this sidecar is an independent, from-scratch FastAPI
# service and vendors none of Nightingale's code, only the choice of
# libraries. See README.md "Licensing and provenance".
LABEL org.opencontainers.image.source="https://github.com/sydlexius/canticle" \
      org.opencontainers.image.description="Forced-alignment sidecar (Demucs + faster-whisper + wav2vec2) for canticle word-sync generation" \
      org.opencontainers.image.licenses="GPL-3.0" \
      net.sydlexius.canticle.aligner.reference="rzru/nightingale (GPL-3.0-or-later)"

WORKDIR /app

# One full transitive lock PER ARCHITECTURE (#1017), not requirements.txt's
# former top-level-only pins: torch/torchaudio resolve to arch-specific
# `+cpu` wheels from the PyTorch CPU index
# (https://download.pytorch.org/whl/cpu), so amd64 and arm64 need separate
# hash sets even where the package/version string agrees. Both files are
# COPYed in (the image is tiny either way) and TARGETARCH -- set
# automatically by buildx, not hand-passed -- selects which one this build
# actually installs.
ARG TARGETARCH
COPY requirements-linux-amd64.txt requirements-linux-arm64.txt ./

# --require-hashes (Scorecard Pinned-Dependencies), matching
# deploy/yamnet-detector's Dockerfile. --extra-index-url, not --index-url:
# the CPU index mirrors more than torch/torchaudio (numpy, jinja2,
# markupsafe, filelock, fsspec, networkx, pillow, sympy, typing-extensions,
# and others also resolve from it), so it must ADD to PyPI rather than
# replace it -- --index-url would drop everything else in the lock that only
# PyPI carries. This is still CUDA-safe regardless of which index actually
# serves a given package: --require-hashes checks EVERY pin in the lock
# (not just torch/torchaudio) against the exact sha256 recorded when the
# lock was generated against the CPU index resolution, so a same-name,
# same-version artifact from the wrong index (or a future PyPI release)
# fails the hash check rather than installing silently. torch/torchaudio
# specifically pin `torch==2.8.0+cpu` (or, on aarch64, `torchaudio==2.8.0`
# where the CPU index and PyPI happen to publish the identical wheel) -- an
# exact version string pip cannot satisfy from PyPI's `2.8.0` CUDA build, so
# it is forced to the CPU index regardless of search order. The CUDA variant
# is issue #1013's, not this image's.
# Quoted with a `:-` default (not bare "$TARGETARCH"): this script runs
# under `set -u`, and an unset TARGETARCH -- buildx always sets it, but a
# plain `docker build` without buildx, or `docker build .` on an older
# Docker default builder, may not -- would otherwise make the shell itself
# fail on the unbound-variable reference ("parameter not set") before the
# case statement's own `*)` branch ever runs, losing the descriptive FATAL
# line below. Verified: a bare `$TARGETARCH` under `set -u` with
# DOCKER_BUILDKIT=0 dies with the shell's own "parameter not set" (exit 2);
# `${TARGETARCH:-}` reaches `*)` and prints the FATAL message (exit 1).
RUN set -eu; \
    case "${TARGETARCH:-}" in \
        amd64) lock=requirements-linux-amd64.txt ;; \
        arm64) lock=requirements-linux-arm64.txt ;; \
        *) echo "FATAL: no hash lock for TARGETARCH=${TARGETARCH:-<unset>} (only amd64/arm64 are locked)" >&2; exit 1 ;; \
    esac; \
    pip install --no-cache-dir --require-hashes \
        --extra-index-url https://download.pytorch.org/whl/cpu \
        -r "$lock"

# HISTORICAL (#1016 dropped whisperx): whisperx 3.4.3 pinned
# ctranslate2<4.5.0, which resolved 4.4.0, whose x86_64 wheel bundled
# libctranslate2-*.so with a PT_GNU_STACK header requesting an executable
# stack (RWE); this base image's glibc (2.41, Debian trixie) refused to
# dlopen such a library, so `import ctranslate2` -- and with it every
# /align -- failed with "cannot enable executable stack". The library does
# not actually need an executable stack, so this clears the flag on exactly
# that file rather than relaxing the loader for the whole process
# (GLIBC_TUNABLES=glibc.rtld.execstack=2 also works, but re-enables the
# exec-stack allowance for every library the process loads). Without
# whisperx's pin, faster-whisper now resolves ctranslate2 4.8.2, already
# clear on both arches (README.md "amd64: ctranslate2's executable-stack
# flag" has the measured before/after) -- this step is KEPT as a guard
# against a future downgrade reintroducing the flag. The glob fails the
# build if it matches nothing, so a ctranslate2 upgrade that renames or
# moves the library is caught here rather than shipping unpatched. On
# aarch64 the flag is already clear and --clear-execstack is a no-op; the
# verify loop asserts the final state on both arches.
RUN set -eu; \
    site="$(python -c 'import sysconfig; print(sysconfig.get_paths()["purelib"])')"; \
    set -- "$site"/ctranslate2.libs/libctranslate2-*.so*; \
    if [ ! -e "$1" ]; then \
        echo "FATAL: no libctranslate2 under $site/ctranslate2.libs" >&2; exit 1; \
    fi; \
    for lib in "$@"; do \
        echo "before: $lib $(patchelf --print-execstack "$lib")"; \
        patchelf --clear-execstack "$lib"; \
        state="$(patchelf --print-execstack "$lib")"; \
        echo "after:  $lib $state"; \
        [ "$state" = "execstack: -" ] || { echo "FATAL: execstack still set on $lib" >&2; exit 1; }; \
    done

COPY app.py _aligner_models.py smoke_decode.py .

# Build-time smoke check: fail the BUILD if the ML stack cannot load, since
# /health is liveness only and models load lazily, so a broken native import
# would otherwise surface only as a 500 on the first /align in production
# (exactly how the ctranslate2 exec-stack failure shipped). Imports every
# module the real /align path uses (#1016: faster-whisper for the transcript,
# torchaudio + transformers for the two forced-alignment model kinds). Also
# asserts the image stayed CPU-only (a CUDA torch build or any nvidia-* /
# triton distribution means the CPU index was bypassed) and that every
# app.ALIGN_MODELS entry resolves to a real torchaudio.pipelines bundle or
# looks like a Hugging Face repo id -- catching a typo/rename in the language
# table here rather than at request time. Model WEIGHTS are not downloaded
# here (no network, no baked weights -- see README "Known limitations"), only
# the torchaudio.pipelines bundle NAME is checked to exist.
RUN python -c "\
import ctranslate2, faster_whisper, torchaudio, torch, transformers; \
import demucs.pretrained, demucs.apply; \
from torchaudio.functional import forced_align, merge_tokens; \
from transformers import Wav2Vec2ForCTC, Wav2Vec2Processor; \
from importlib.metadata import distributions; \
gpu = sorted(d.metadata['Name'] for d in distributions() \
    if d.metadata['Name'].lower().startswith(('nvidia-', 'triton'))); \
assert torch.version.cuda is None, 'CUDA torch build installed: %s' % torch.__version__; \
assert not gpu, 'GPU distributions installed: %s' % gpu; \
import app, _aligner_models; \
torch_langs = [lang for lang, name in app.ALIGN_MODELS.items() \
    if app.is_torchaudio_align_model(name, torchaudio)]; \
assert torch_langs, 'no ALIGN_MODELS entry resolves to a torchaudio.pipelines bundle'; \
bad = [(lang, name) for lang, name in app.ALIGN_MODELS.items() \
    if not app.is_torchaudio_align_model(name, torchaudio) and '/' not in name]; \
assert not bad, 'ALIGN_MODELS entries neither a torchaudio bundle nor a HF repo id: %r' % bad; \
print('smoke ok: torch', torch.__version__, 'ctranslate2', ctranslate2.__version__, \
    'faster_whisper', faster_whisper.__version__, 'torch-type langs', sorted(torch_langs))"

# Build-time decode smoke check against THIS image's ffmpeg (the CI test job
# uses the runner's apt ffmpeg, a different build): a synthetic concat
# playlist with a data: URL entry, forced through decode_pcm's exact argv,
# must decode 0 bytes, and a plain WAV must decode. See smoke_decode.py.
RUN python smoke_decode.py

# Model weights are NOT baked into this image at build time, unlike YAMNet's
# checksum-pinned SavedModel bake. Demucs, faster-whisper, and the forced-
# alignment models (torchaudio.pipelines / Hugging Face wav2vec2) each pull
# their own pretrained weights from their normal hubs (torch.hub / Hugging
# Face) on first use and cache them under HF_HOME / TORCH_HOME, both set
# below to paths under /data. Mount a persistent volume at /data in
# production so a container restart does not re-download weights. This is a deliberate
# deviation from yamnet's build-time bake (documented in README.md) -- there
# is no single canonical checksum to pin for an align-model set that varies
# per requested language.
ENV HF_HOME=/data/hf-cache \
    TORCH_HOME=/data/torch-cache

# Drop privileges (matches the unprivileged posture of the rest of the stack).
RUN useradd -u 1000 -m appuser \
    && mkdir -p /data/hf-cache /data/torch-cache \
    && chown -R appuser:appuser /app /data
USER appuser

EXPOSE 8080

# Single worker: this sidecar is already heavy per-request (separation +
# transcription + alignment); a second worker would double the resident
# model-weight footprint (and, with a GPU, contend for the same device) for
# no throughput gain on a workload that is deliberately budgeted/serialized
# by the canticle caller, not fanned out (see epic #482's "budgeted candidate
# sweep" design -- one track in flight at a time is the intended shape).
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080", "--workers", "1"]
