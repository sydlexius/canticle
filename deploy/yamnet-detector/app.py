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
from contextlib import asynccontextmanager

import numpy as np
import soundfile as sf
import tensorflow as tf
import tensorflow_hub as hub
from fastapi import FastAPI, File, HTTPException, UploadFile

YAMNET_HANDLE = "https://tfhub.dev/google/yamnet/1"
TARGET_SR = 16000  # YAMNet requires 16 kHz mono

_state: dict = {}


def _load_class_names(csv_path: str) -> list[str]:
    names: list[str] = []
    with tf.io.gfile.GFile(csv_path) as f:
        for row in csv.DictReader(f):
            names.append(row["display_name"])
    return names


@asynccontextmanager
async def lifespan(_: FastAPI):
    # Load once at startup. The model is baked into the image at build time
    # (TFHUB_CACHE_DIR), so this is offline and fast.
    model = hub.load(YAMNET_HANDLE)
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
    raw = await file.read()
    try:
        wav, sr = sf.read(io.BytesIO(raw), dtype="float32")
    except Exception as e:  # noqa: BLE001 - surface a clean 400 to the caller
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

    scores, _embeddings, _spectrogram = _state["model"](wav)
    arr = scores.numpy()  # frames x num_classes
    mean_scores = np.mean(arr, axis=0)  # per-class mean over frames (music gate)
    max_scores = np.max(arr, axis=0)  # per-class peak over frames (vocal gate)
    names = _state["classes"]
    return {
        "mean": {name: float(s) for name, s in zip(names, mean_scores)},
        "max": {name: float(s) for name, s in zip(names, max_scores)},
    }
