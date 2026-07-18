"""Canticle instrumental-detection sidecar.

A thin FastAPI wrapper around Google's YAMNet (AudioSet classifier). Canticle
POSTs a 16 kHz mono WAV sample to POST /classify (multipart field "file") and
expects back JSON:

    {"mean": {"<AudioSet class name>": <prob 0-1>, ...},
     "max":  {"<AudioSet class name>": <prob 0-1>, ...}}

YAMNet scores ~1s frames independently; we return BOTH reductions over the
clip's frames:

  - "mean" feeds Canticle's music gate (summed mean of the instrumental classes
    vs min_confidence), as before.
  - "max" feeds Canticle's vocal gate. A sung track has a brief, strong singing
    peak that the frame mean dilutes ~10x; the per-class max-over-frames keeps
    that peak intact, which is what separates vocal tracks from instrumentals
    (see issue #384). np.max is free on the same forward pass as np.mean.
"""

import csv
import io
import logging
from contextlib import asynccontextmanager

import numpy as np
import soundfile as sf
import tensorflow as tf
from fastapi import FastAPI, File, HTTPException, UploadFile
from starlette.concurrency import run_in_threadpool

# Directory holding the unpacked YAMNet SavedModel, baked into the image at build
# time (see Dockerfile). Loaded with tf.saved_model.load rather than
# tensorflow_hub.load: hub.load() only resolves a URL to a local cache and then
# calls tf.saved_model.load itself, and the Dockerfile already does the download
# at build time, so hub contributed nothing at runtime while dragging in a
# pkg_resources dependency that capped setuptools below the CVE-2026-59890 fix
# (issue #491). Same SavedModel, same graph, same weights -- byte-identical
# scores.
#
# Deliberately a constant, not an env override: a SavedModel is executable (its
# graph is deserialized and run at load), so an overridable path would turn env
# control into code execution. The model ships in the image and the path never
# varies, so an override buys nothing.
YAMNET_MODEL_DIR = "/app/yamnet"
TARGET_SR = 16000  # YAMNet requires 16 kHz mono
# Cap the in-memory read. Canticle sends at most a ~60s 16 kHz mono WAV (~2 MB);
# 32 MB is a generous ceiling that rejects oversize/malicious uploads before they
# can exhaust memory.
MAX_UPLOAD_BYTES = 32 * 1024 * 1024

logger = logging.getLogger("canticle.yamnet")

_state: dict = {}


def _load_class_names(csv_path: str) -> list[str]:
    names: list[str] = []
    with tf.io.gfile.GFile(csv_path) as f:
        for row in csv.DictReader(f):
            names.append(row["display_name"])
    return names


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Load once at startup. The model is baked into the image at build time, so
    # this is offline and fast. class_map_path() is a SavedModel asset, so it
    # survives the load without tensorflow_hub.
    model = tf.saved_model.load(YAMNET_MODEL_DIR)
    _state["model"] = model
    _state["classes"] = _load_class_names(model.class_map_path().numpy().decode("utf-8"))
    yield
    _state.clear()


app = FastAPI(title="Canticle YAMNet instrumental classifier", lifespan=lifespan)


@app.get("/health")
def health():
    return {"status": "ok", "classes": len(_state.get("classes", []))}


@app.post("/classify")
async def classify(file: UploadFile = File(...)):
    # Read at most MAX_UPLOAD_BYTES + 1 so an oversize upload is rejected without
    # buffering the whole body into memory.
    raw = await file.read(MAX_UPLOAD_BYTES + 1)
    if len(raw) > MAX_UPLOAD_BYTES:
        raise HTTPException(status_code=413, detail="audio file too large")
    try:
        wav, sr = sf.read(io.BytesIO(raw), dtype="float32")
    except Exception as e:  # noqa: BLE001 - surface a clean 400 to the caller
        logger.warning("classify: cannot read audio: %s", e)
        raise HTTPException(status_code=400, detail=f"cannot read audio: {e}")

    # Fold to mono.
    if wav.ndim > 1:
        wav = wav.mean(axis=1).astype(np.float32)

    # Canticle already sends 16 kHz mono, but resample defensively (linear, to
    # avoid pulling scipy/librosa into the image) if a caller sends another rate.
    if sr != TARGET_SR:
        n_out = int(round(wav.shape[0] * TARGET_SR / float(sr)))
        if n_out <= 0:
            raise HTTPException(status_code=400, detail="empty audio after resample")
        wav = np.interp(
            np.linspace(0.0, wav.shape[0], n_out, endpoint=False),
            np.arange(wav.shape[0]),
            wav,
        ).astype(np.float32)

    if wav.size == 0:
        raise HTTPException(status_code=400, detail="empty audio")

    try:
        # Inference is synchronous and CPU-bound. Calling it directly from this
        # async handler would run it ON the event loop, serializing every
        # concurrent /classify: measured at 4 concurrent requests, that is 2.02s
        # versus 0.51s when the work is offloaded, a ~3.9x difference. Dispatch
        # to the threadpool instead. This parallelizes rather than merely
        # interleaving because TensorFlow releases the GIL during inference.
        #
        # The handler stays `async def` (it must: file.read() above is awaited),
        # so the offload is explicit here rather than implicit via a plain `def`.
        scores, _embeddings, _spectrogram = await run_in_threadpool(_state["model"], wav)
    except Exception as e:  # noqa: BLE001 - inference failure is a 500, but log it
        logger.exception("classify: inference failed")
        raise HTTPException(status_code=500, detail=f"inference failed: {e}")
    arr = scores.numpy()  # frames x num_classes
    mean_scores = np.mean(arr, axis=0)  # per-class mean over frames (music gate)
    max_scores = np.max(arr, axis=0)  # per-class peak over frames (vocal gate)
    names = _state["classes"]
    return {
        "mean": {name: float(s) for name, s in zip(names, mean_scores)},
        "max": {name: float(s) for name, s in zip(names, max_scores)},
    }
