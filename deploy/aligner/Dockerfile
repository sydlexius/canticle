# Pinned by digest (Scorecard Pinned-Dependencies). The tag is kept for
# readability; Docker resolves by the digest. Update both together -- use the
# multi-arch manifest-list (index) digest, NOT a local arch-specific one, so
# the pin resolves on any build arch:
#   docker buildx imagetools inspect python:3.11-slim --format '{{.Manifest.Digest}}'
FROM python:3.11-slim@sha256:cdbd05fb6f457ca275ff51ce00d93d865ca0b6a25f5ffb08262d94f6835771e5

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

COPY requirements.txt .

# NOT --require-hashes here, unlike deploy/yamnet-detector's Dockerfile:
# requirements.txt pins the TOP-LEVEL packages only (transitive deps resolve
# at build time). The full transitive lock with hashes is issue #1017.
#
# torch/torchaudio come from the PyTorch CPU index FIRST, at the versions
# requirements.txt pins (read from it, so the pin has one home). On x86_64,
# PyPI's torch wheel is the CUDA build and drags in every nvidia-*-cu12
# library (~5 GB this CPU-only image never uses); the CPU index's build does
# not. --index-url (not --extra-index-url) so nothing else resolves from that
# index. The second install then sees torch==2.8.0 already satisfied (a
# `2.8.0+cpu` local version matches an unqualified `==2.8.0` pin) and leaves
# it alone. On aarch64 both indexes carry a CPU build, so this is a no-op
# there. The CUDA variant is issue #1013's, not this image's.
RUN grep -E '^(torch|torchaudio)==' requirements.txt > /tmp/torch-cpu.txt \
    && test "$(wc -l < /tmp/torch-cpu.txt)" -eq 2 \
    && pip install --no-cache-dir --index-url https://download.pytorch.org/whl/cpu \
        -r /tmp/torch-cpu.txt \
    && pip install --no-cache-dir -r requirements.txt \
    && rm /tmp/torch-cpu.txt

# whisperx 3.4.3 pins ctranslate2<4.5.0, which resolves 4.4.0. Its x86_64
# wheel bundles libctranslate2-*.so with a PT_GNU_STACK header requesting an
# executable stack (RWE); this base image's glibc (2.41, Debian trixie)
# refuses to dlopen such a library, so `import ctranslate2` -- and with it
# every /align -- fails with "cannot enable executable stack". The library
# does not actually need an executable stack, so clear the flag on exactly
# that file rather than relaxing the loader for the whole process
# (GLIBC_TUNABLES=glibc.rtld.execstack=2 also works, but re-enables the
# exec-stack allowance for every library the process loads). The glob fails
# the build if it matches nothing, so a ctranslate2 upgrade that renames or
# moves the library is caught here rather than shipping unpatched. On aarch64
# the flag is already clear and --clear-execstack is a no-op; the verify loop
# asserts the final state on both arches.
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
import app; \
torch_langs = [lang for lang, name in app.ALIGN_MODELS.items() if hasattr(torchaudio.pipelines, name)]; \
assert torch_langs, 'no ALIGN_MODELS entry resolves to a torchaudio.pipelines bundle'; \
bad = [(lang, name) for lang, name in app.ALIGN_MODELS.items() \
    if not hasattr(torchaudio.pipelines, name) and '/' not in name]; \
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
# Face) on first use and cache them under the running user's home directory.
# Mount a persistent volume at that cache path in production so a container
# restart does not re-download weights on every request. This is a deliberate
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
