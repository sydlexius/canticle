"""Shape test for the /classify response.

Stubs the YAMNet model so the test needs no model download. TestClient is used
WITHOUT a `with` block so the lifespan (which would load the real model) never
runs, and the stubbed _state is what the handler reads.

Run locally (not in CI; the Go suite is the gate):
    pip install -r requirements.txt pytest
    pytest test_app.py -q
"""

import io
import struct
import wave

import numpy as np

import app as appmod


class _Scores:
    """Mirrors the part of a TF tensor app.py uses: scores.numpy()."""

    def __init__(self, arr):
        self._arr = arr

    def numpy(self):
        return self._arr


class _StubModel:
    def __call__(self, wav):
        # 3 frames x 4 classes; the singing-like class (index 2) peaks on frame 2.
        scores = np.array(
            [
                [0.90, 0.10, 0.00, 0.20],
                [0.80, 0.20, 0.70, 0.10],
                [0.85, 0.15, 0.10, 0.05],
            ],
            dtype=np.float32,
        )
        return _Scores(scores), None, None


def _wav_bytes() -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(struct.pack("<16000h", *([0] * 16000)))
    return buf.getvalue()


def test_classify_returns_mean_and_max():
    from fastapi.testclient import TestClient

    appmod._state["model"] = _StubModel()
    appmod._state["classes"] = ["Music", "Musical instrument", "Singing", "Speech"]

    client = TestClient(appmod.app)
    resp = client.post("/classify", files={"file": ("s.wav", _wav_bytes(), "audio/wav")})

    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert set(body.keys()) == {"mean", "max"}
    # Per-class max-over-frames keeps the peak (0.70) the mean would dilute.
    # Compare with a tolerance: the scores are float32, so float(0.7) round-trips
    # as 0.69999998, not exactly 0.7.
    assert abs(body["max"]["Singing"] - 0.7) < 1e-6
    assert abs(body["mean"]["Singing"] - (0.0 + 0.7 + 0.1) / 3) < 1e-6
    assert abs(body["max"]["Music"] - 0.9) < 1e-6


def test_classify_returns_every_class_in_both_maps():
    """Full-map contract: every configured class appears in BOTH mean and max.

    Canticle's vocal gate (#402) fails safe to not-instrumental when a configured
    vocal class is absent from a non-empty max map, treating absence as a partial
    (contract-violating) response. That guard is only correct if this sidecar
    returns the FULL class set - no thresholding or top-N - on every response.
    Assert that contract here so a future change that drops zero-scored classes is
    caught rather than silently turning Canticle's gate into "never instrumental".
    """
    from fastapi.testclient import TestClient

    # Dedicated stub: "Silence" scores 0.0 across ALL frames. _StubModel has no
    # all-zero class, so on its own this test would pass even against an
    # implementation that dropped zero-scored classes. The zero class makes the
    # full-map contract actually testable: a thresholding/top-N change that omits
    # "Silence" now fails the asserts below.
    classes = ["Music", "Musical instrument", "Singing", "Speech", "Silence"]

    class _ZeroClassModel:
        def __call__(self, wav):
            scores = np.array(
                [
                    [0.90, 0.10, 0.00, 0.20, 0.0],
                    [0.80, 0.20, 0.70, 0.10, 0.0],
                    [0.85, 0.15, 0.10, 0.05, 0.0],
                ],
                dtype=np.float32,
            )
            return _Scores(scores), None, None

    appmod._state["model"] = _ZeroClassModel()
    appmod._state["classes"] = classes

    client = TestClient(appmod.app)
    resp = client.post("/classify", files={"file": ("s.wav", _wav_bytes(), "audio/wav")})

    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert set(body["mean"].keys()) == set(classes)
    assert set(body["max"].keys()) == set(classes)
    # The all-zero class must survive in BOTH maps with value 0.0 - never dropped.
    assert body["mean"]["Silence"] == 0.0
    assert body["max"]["Silence"] == 0.0


def test_classify_does_not_serialize_concurrent_requests():
    """Inference must not run on the event loop.

    The handler is `async def` (it awaits file.read()), so calling the
    synchronous, CPU-bound model directly from it would run inference ON the
    event loop and serialize every concurrent /classify. Measured at 4 concurrent
    requests with a 0.5s inference, that is ~2.0s serialized versus ~0.5s
    offloaded.

    This asserts the offload rather than the wall-clock number, which would be
    flaky on a loaded machine: with a blocking call the total is at least
    CONCURRENCY * INFER, so a threshold below that separates the two
    implementations without depending on how fast the host is.
    """
    import asyncio
    import time

    import httpx

    infer_seconds = 0.2
    concurrency = 4

    class _SleepingModel:
        """Blocking and CPU-bound, like the real forward pass."""

        def __call__(self, wav):
            time.sleep(infer_seconds)
            return _Scores(np.zeros((2, 2), dtype=np.float32)), None, None

    appmod._state["model"] = _SleepingModel()
    appmod._state["classes"] = ["Music", "Speech"]

    async def _run():
        transport = httpx.ASGITransport(app=appmod.app)
        async with httpx.AsyncClient(
            transport=transport, base_url="http://t", timeout=60
        ) as client:
            files = {"file": ("s.wav", _wav_bytes(), "audio/wav")}
            start = time.perf_counter()
            responses = await asyncio.gather(
                *(client.post("/classify", files=files) for _ in range(concurrency))
            )
            return time.perf_counter() - start, responses

    elapsed, responses = asyncio.run(_run())

    assert all(r.status_code == 200 for r in responses), [r.status_code for r in responses]
    serialized = infer_seconds * concurrency
    assert elapsed < serialized * 0.6, (
        f"concurrent /classify took {elapsed:.2f}s; serialized would be ~{serialized:.2f}s. "
        "Inference is running on the event loop - it must be dispatched to the threadpool."
    )
