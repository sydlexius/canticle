"""Unit and HTTP-contract tests for the aligner sidecar.

Covers config resolution, device selection, lyric line splitting, the
one-sequence CTC alignment planning, and the /health + /align contract.

Stubs the Separator/Aligner model layer so the tests need no torch, demucs,
or whisperx installed. TestClient is used WITHOUT a `with` block so lifespan
(which would build the real models) never runs, and stubbed `_state` is what
the handler reads -- same trick as deploy/yamnet-detector/test_app.py.

Runs locally with only the lightweight test deps:
    pip install --require-hashes -r requirements-test.txt
    pytest test_app.py -q
"""

import asyncio
import io
import logging
import os
import sys
import tempfile
import threading
import time
import wave
from types import SimpleNamespace as ns

import pytest
from fastapi.testclient import TestClient

import app as appmod

# --------------------------------------------------------------------------
# device selection (pure function, no ML deps needed)
# --------------------------------------------------------------------------


def test_select_device_override_auto_and_unrecognized_fallback():
    # Explicit override wins even against unavailable hardware.
    assert appmod.select_device("cuda", cuda_available=False, mps_available=False) == "cuda"
    # auto prefers cuda > mps > cpu by availability.
    assert appmod.select_device("auto", cuda_available=True, mps_available=True) == "cuda"
    assert appmod.select_device("auto", cuda_available=False, mps_available=True) == "mps"
    assert appmod.select_device("", cuda_available=False, mps_available=False) == "cpu"
    # An unrecognized value fails to the conservative default (cpu), never to
    # what auto-detection would have picked, even when hardware IS available.
    assert appmod.select_device("quantum", cuda_available=True, mps_available=True) == "cpu"


def test_select_device_override_is_case_insensitive():
    # select_device lowercases its own input (DEVICE_ENV is also lowercased at
    # read time, but select_device must not depend on that). Hardware IS
    # available here, so a case-sensitive compare would read "CUDA" as
    # unrecognized and fail conservatively to cpu.
    assert appmod.select_device("CUDA", cuda_available=True, mps_available=False) == "cuda"
    assert appmod.select_device(" Mps ", cuda_available=True, mps_available=True) == "mps"


# --------------------------------------------------------------------------
# sequencing: all lines aligned as ONE sequence, mapped back to line indices
# --------------------------------------------------------------------------

_DICT = {c: i for i, c in enumerate("-|abcdefghijklmnopqrstuvwxyz'")}  # "-" is blank (id 0)


def _sequential_spans(n_targets, frames_per_token=2):
    return [(i * frames_per_token, i * frames_per_token + 1, 0.5) for i in range(n_targets)]


def test_plan_alignment_one_sequence_with_repeated_chorus():
    lines = ["Na na, na!", "hey", "Na na, na!"]
    targets, planned = appmod.plan_alignment(lines, _DICT, blank_id=0)

    # Word order, texts (punctuation kept) and line indices follow the input,
    # and the repeated chorus line is a distinct LATER stretch of targets.
    assert [(li, text) for li, text, _, _ in planned] == [
        (0, "Na"), (0, "na,"), (0, "na!"), (1, "hey"), (2, "Na"), (2, "na,"), (2, "na!"),
    ]
    starts = [first for _, _, first, _ in planned]
    assert starts == sorted(starts) and len(set(starts)) == len(starts)
    # Punctuation never becomes a CTC target; words are joined by "|".
    assert _DICT["|"] in targets and all(t != 0 for t in targets)
    assert [targets[f:e] for _, _, f, e in planned][0] == [_DICT["n"], _DICT["a"]]


def test_plan_alignment_drops_unalignable_chars_and_blank_symbol():
    targets, planned = appmod.plan_alignment(["--- 42 ok"], _DICT, blank_id=0)
    # "---" maps to the blank id and "42" has no token: both plan an empty range.
    assert [(text, f == e) for _, text, f, e in planned] == [("---", True), ("42", True), ("ok", False)]
    assert 0 not in targets


def test_words_from_token_spans_monotonic_non_overlapping_across_lines():
    lines = ["the quick fox", "hello world", "the quick fox"]
    targets, planned = appmod.plan_alignment(lines, _DICT, blank_id=0)
    words = appmod.words_from_token_spans(planned, _sequential_spans(len(targets)), frame_seconds=0.02)

    assert [w.line_index for w in words] == [0, 0, 0, 1, 1, 2, 2, 2]
    for prev, nxt in zip(words, words[1:]):
        assert prev.end_ms <= nxt.start_ms, (prev, nxt)
    # A word ends where its last character's span ends, not at the next word.
    assert words[0].start_ms == 0 and words[0].end_ms == 100  # "the": 3 tokens -> frames 0..5
    assert all(w.confidence == 0.5 for w in words)


def test_words_from_token_spans_skips_words_without_targets():
    targets, planned = appmod.plan_alignment(["42 go"], _DICT, blank_id=0)
    words = appmod.words_from_token_spans(planned, _sequential_spans(len(targets)), frame_seconds=0.02)
    assert [(w.text, w.line_index) for w in words] == [("go", 0)]


def test_confidence_is_the_mean_of_per_character_scores():
    targets, planned = appmod.plan_alignment(["ab"], _DICT, blank_id=0)
    spans = [(0, 1, 0.2), (2, 3, 0.9)]  # unequal, so a first/last-char shortcut differs
    (word,) = appmod.words_from_token_spans(planned, spans, frame_seconds=0.02)
    assert word.confidence == 0.55


# --------------------------------------------------------------------------
# word separators, apostrophes, Unicode normalization
# --------------------------------------------------------------------------

_SEP = _DICT["|"]


def test_plan_alignment_separator_only_between_words_with_targets():
    targets, _ = appmod.plan_alignment(["go 42 now"], _DICT, blank_id=0)
    assert targets.count(_SEP) == 1, targets  # "go|now", never "go||now"
    targets, _ = appmod.plan_alignment(["go 42"], _DICT, blank_id=0)
    assert targets[-1] != _SEP and _SEP not in targets  # no trailing "go|"
    targets, _ = appmod.plan_alignment(["42 go"], _DICT, blank_id=0)
    assert targets[0] != _SEP  # no leading separator either


def test_plan_alignment_folds_curly_apostrophe_and_normalizes_nfc():
    targets, _ = appmod.plan_alignment(["don’t"], _DICT, blank_id=0)
    assert targets == [_DICT[c] for c in "don't"]
    accented = {**_DICT, "é": 99}
    targets, planned = appmod.plan_alignment(["café"], accented, blank_id=0)  # NFD input
    assert targets == [_DICT["c"], _DICT["a"], _DICT["f"], 99]
    assert planned[0][1] == "café"  # returned word text is the caller's own


# --------------------------------------------------------------------------
# line splitting matches the caller's "\n" count
# --------------------------------------------------------------------------


def test_parse_lines_splits_on_newline_only():
    text = "one\x0cstill one\r\ntwo\x85still two and more\n\nthree\r"
    assert appmod._parse_lines(text) == ["one\x0cstill one", "two\x85still two and more", "three"]


def test_parse_lines_drops_lines_of_only_u001c_to_u001f():
    # str.strip() treats U+001C..U+001F as whitespace (Go's unicode.IsSpace
    # does not); the docstring makes that the contract a Go caller mirrors
    # for line_index, so a narrower blank check must fail here.
    text = "first\n\x1c\n\x1d\x1e\n\x1f  \nsecond"
    assert appmod._parse_lines(text) == ["first", "second"]


# --------------------------------------------------------------------------
# default-language validation
# --------------------------------------------------------------------------


def test_validate_config_rejects_unsupported_default_language(monkeypatch):
    monkeypatch.setattr(appmod, "ALIGN_LANGUAGE", "xx")
    with pytest.raises(RuntimeError, match="ALIGNER_ALIGN_LANGUAGE"):
        appmod.validate_config(appmod.ALIGN_MODELS)
    monkeypatch.setattr(appmod, "ALIGN_LANGUAGE", "en")
    appmod.validate_config(appmod.ALIGN_MODELS)


# --------------------------------------------------------------------------
# env config: int parsing and the lyrics-limit clamp
# --------------------------------------------------------------------------


def test_max_lyrics_bytes_is_clamped_to_the_part_cap(monkeypatch):
    monkeypatch.setenv("ALIGNER_MAX_LYRICS_BYTES", str(4 * 1024 * 1024))
    assert appmod._resolve_max_lyrics_bytes() == appmod.LYRICS_PART_CAP_BYTES


def test_env_int_uses_a_valid_positive_value(monkeypatch):
    monkeypatch.setenv("ALIGNER_TEST_INT", " 42 ")
    assert appmod._env_int("ALIGNER_TEST_INT", 7) == 42


@pytest.mark.parametrize("raw", ["0", "-5"])
def test_env_int_non_positive_falls_back_to_default(monkeypatch, raw):
    monkeypatch.setenv("ALIGNER_TEST_INT", raw)
    assert appmod._env_int("ALIGNER_TEST_INT", 7) == 7


@pytest.mark.parametrize("raw", ["abc", "1.5", "10MB"])
def test_env_int_non_integer_falls_back_to_default(monkeypatch, raw):
    monkeypatch.setenv("ALIGNER_TEST_INT", raw)
    assert appmod._env_int("ALIGNER_TEST_INT", 7) == 7


# --------------------------------------------------------------------------
# HTTP contract: stubbed model layer
# --------------------------------------------------------------------------


class _StubSeparator:
    def __init__(self, vocal_path=os.path.join(tempfile.gettempdir(), f"aligner-test-{os.getpid()}-vocals.wav"), raise_bad_audio=False):
        self._vocal_path = vocal_path
        self._raise_bad_audio = raise_bad_audio

    def separate(self, audio_path):
        if self._raise_bad_audio:
            raise appmod._BadAudioError("cannot decode audio: garbage bytes")
        return self._vocal_path


class _StubAligner:
    def __init__(self, transcript="hello world", words=None, raise_error=False, sleep_seconds=0.0):
        self._transcript = transcript
        self._words = words if words is not None else []
        self._raise_error = raise_error
        self._sleep_seconds = sleep_seconds
        self.align_calls = []

    def transcribe(self, vocal_path, language):
        if self._sleep_seconds:
            time.sleep(self._sleep_seconds)
        if self._raise_error:
            raise RuntimeError("boom")
        return self._transcript

    def align(self, vocal_path, lines, language):
        self.align_calls.append((tuple(lines), language))
        if self._raise_error:
            raise RuntimeError("boom")
        return self._words


def _wav_bytes() -> bytes:
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(16000)
        w.writeframes(b"\x00\x00" * 16000)
    return buf.getvalue()


def _install_stubs(separator=None, aligner=None, device="cpu"):
    appmod._state["separator"] = separator or _StubSeparator()
    appmod._state["aligner"] = aligner or _StubAligner()
    appmod._state["device"] = device


def _post(lyrics=None, files_override=None, **data):
    files = files_override if files_override is not None else {"file": ("track.wav", _wav_bytes(), "audio/wav")}
    body = dict(data)
    if lyrics is not None:
        body["lyrics"] = lyrics
    return TestClient(appmod.app).post("/align", files=files, data=body)


def test_model_builders_fail_loudly_until_the_model_layer_lands(monkeypatch):
    for build in (appmod._build_separator, appmod._build_aligner):
        with pytest.raises(NotImplementedError, match="slice 3"):
            build("cpu")
    monkeypatch.setitem(sys.modules, "torch", ns(cuda=(no := ns(is_available=lambda: False)), backends=ns(mps=no)))
    monkeypatch.setitem(sys.modules, "whisperx", None)  # lifespan's ImportError fallback
    pytest.raises(NotImplementedError, TestClient(appmod.app).__enter__)  # no model layer yet: a real boot fails


# --------------------------------------------------------------------------
# /health
# --------------------------------------------------------------------------


def test_health_is_liveness_and_reports_device_and_models():
    _install_stubs(device="cuda")
    body = TestClient(appmod.app).get("/health").json()
    assert body == {
        "status": "ok",
        "device": "cuda",
        "separation_model": appmod.SEPARATION_MODEL,
        "align_model": "WAV2VEC2_ASR_BASE_960H",
        "whisper_model": appmod.WHISPER_MODEL,
    }


# --------------------------------------------------------------------------
# /align happy path and response shape
# --------------------------------------------------------------------------


def test_align_happy_path_response_shape_and_ordering():
    words = [
        appmod.WordSpan(text="hello", start_ms=1000, end_ms=1200, line_index=0, confidence=0.95),
        appmod.WordSpan(text="world", start_ms=1200, end_ms=1500, line_index=0, confidence=0.9),
        appmod.WordSpan(text="again", start_ms=500, end_ms=700, line_index=1, confidence=0.8),
    ]
    _install_stubs(aligner=_StubAligner(transcript="hello world again", words=words))

    resp = _post(lyrics="hello world\nagain")

    assert resp.status_code == 200, resp.text
    body = resp.json()
    assert set(body.keys()) == {"words", "transcript"}
    assert body["transcript"] == "hello world again"

    # Ordered by start_ms ascending across the whole response, not grouped by
    # line: "again" (start_ms=500) sorts before "hello" (1000) even though
    # its line_index (1) is later in the supplied lyric text.
    starts = [w["start_ms"] for w in body["words"]]
    assert starts == sorted(starts)
    assert body["words"][0] == {
        "text": "again",
        "start_ms": 500,
        "end_ms": 700,
        "line_index": 1,
        "confidence": 0.8,
    }


def test_align_blank_lines_dropped_and_language_default_or_override():
    aligner = _StubAligner()
    _install_stubs(aligner=aligner)

    resp = _post(lyrics="first line\n\n\nsecond line\n")
    assert resp.status_code == 200, resp.text
    # Blank lines never reach the model layer and never occupy a line_index.
    assert aligner.align_calls[0][0] == ("first line", "second line")
    assert aligner.align_calls[0][1] == appmod.ALIGN_LANGUAGE

    resp = _post(lyrics="one line", language="fr")
    assert resp.status_code == 200, resp.text
    assert aligner.align_calls[1][1] == "fr"


def test_align_line_index_contract_with_blank_lines_and_chorus():
    class _PlanningAligner(_StubAligner):
        def align(self, vocal_path, lines, language):
            targets, planned = appmod.plan_alignment(lines, _DICT, blank_id=0)
            return appmod.words_from_token_spans(planned, _sequential_spans(len(targets)), 0.02)

    _install_stubs(aligner=_PlanningAligner())
    resp = _post(lyrics="oh la\n\nverse\n\noh la\n")
    assert resp.status_code == 200, resp.text
    body = resp.json()["words"]
    assert [(w["text"], w["line_index"]) for w in body] == [
        ("oh", 0), ("la", 0), ("verse", 1), ("oh", 2), ("la", 2),
    ]


# --------------------------------------------------------------------------
# Validation, size limits, and error shapes
# --------------------------------------------------------------------------


def test_align_rejects_missing_lyrics_field():
    _install_stubs()
    resp = _post(files_override={"file": ("track.wav", _wav_bytes(), "audio/wav")})
    assert resp.status_code == 422  # FastAPI's own required-field validation


def test_align_rejects_empty_lyrics_and_empty_audio():
    _install_stubs()
    assert _post(lyrics="   \n\n  ").status_code == 400
    assert _post(lyrics="a line", files_override={"file": ("t.wav", b"", "audio/wav")}).status_code == 400


def test_align_rejects_oversized_audio_and_oversized_lyrics(monkeypatch):
    _install_stubs()
    monkeypatch.setattr(appmod, "MAX_AUDIO_BYTES", 10)
    resp = _post(lyrics="a line")
    assert resp.status_code == 413
    assert "too large" in resp.json()["detail"]

    monkeypatch.setattr(appmod, "MAX_AUDIO_BYTES", appmod.DEFAULT_MAX_AUDIO_BYTES)
    monkeypatch.setattr(appmod, "MAX_LYRICS_BYTES", 10)
    resp = _post(lyrics="this lyric text is definitely longer than ten bytes")
    assert resp.status_code == 413
    assert "too large" in resp.json()["detail"]


def test_align_returns_400_on_undecodable_audio():
    _install_stubs(separator=_StubSeparator(raise_bad_audio=True))
    resp = _post(lyrics="a line", files_override={"file": ("t.wav", b"garbage", "audio/wav")})
    assert resp.status_code == 400
    assert resp.json()["detail"] == "cannot read audio"


def test_align_returns_413_when_the_model_layer_reports_audio_too_long():
    class _TooLongSeparator(_StubSeparator):
        def separate(self, audio_path):
            raise appmod._AudioTooLongError("audio longer than 1s")

    _install_stubs(separator=_TooLongSeparator())
    resp = _post(lyrics="a line")
    assert resp.status_code == 413
    assert resp.json()["detail"] == "audio too long"  # stable, never the exception text


def test_align_returns_500_when_model_layer_not_initialized():
    appmod._state.clear()
    resp = _post(lyrics="a line")
    assert resp.status_code == 500
    assert "not initialized" in resp.json()["detail"]


def test_unsupported_language_is_400_not_500():
    aligner = _StubAligner()
    _install_stubs(aligner=aligner)
    resp = _post(lyrics="a line", language="xx")
    assert resp.status_code == 400
    assert "unsupported language" in resp.json()["detail"]
    assert aligner.align_calls == []
    assert _post(lyrics="a line", language="FR").status_code == 200  # case-insensitive


class _LeakyAligner(_StubAligner):
    """Fails with an exception whose MESSAGE quotes the lyric."""

    def align(self, vocal_path, lines, language):
        raise ValueError(f"cannot tokenize line {lines[0]!r}")


def test_align_500_on_pipeline_failure_never_leaks_lyrics_response_or_logs(caplog):
    _install_stubs(aligner=_LeakyAligner())
    secret = "a very private lyric line nobody should see"
    with caplog.at_level(logging.INFO, logger="canticle.aligner"):
        resp = _post(lyrics=secret)
    assert resp.status_code == 500
    assert resp.json()["detail"] == "alignment failed"
    assert secret not in resp.text
    # The FORMATTED record (message + traceback), which is what reaches stderr,
    # not just getMessage(): a logged traceback quotes the exception message.
    formatted = "\n".join(logging.Formatter().format(r) for r in caplog.records)
    assert "pipeline failed: ValueError" in formatted
    assert secret not in formatted


def test_debug_failure_log_has_frames_but_never_the_lyric(caplog):
    _install_stubs(aligner=_LeakyAligner())
    secret = "a private lyric line for the debug log"
    with caplog.at_level(logging.DEBUG, logger="canticle.aligner"):
        assert _post(lyrics=secret).status_code == 500
    assert "in _run_pipeline" in caplog.text  # the frames are logged...
    assert secret not in caplog.text  # ...the exception message is not


def test_align_never_logs_lyric_text_on_success(caplog):
    _install_stubs()
    secret = "another very private lyric line"
    with caplog.at_level(logging.INFO, logger="canticle.aligner"):
        resp = _post(lyrics=secret)
    assert resp.status_code == 200, resp.text
    assert all(secret not in r.getMessage() for r in caplog.records)


# --------------------------------------------------------------------------
# size limits are enforced as the documented 413
# --------------------------------------------------------------------------


def test_oversized_declared_body_rejected_before_multipart_parsing(monkeypatch):
    import starlette.datastructures as sd

    reads = []
    orig = sd.UploadFile.read

    async def spy(self, size=-1):
        reads.append(self.size)
        return await orig(self, size)

    monkeypatch.setattr(sd.UploadFile, "read", spy)
    monkeypatch.setattr(appmod, "MAX_AUDIO_BYTES", 10)
    _install_stubs()
    resp = _post(lyrics="a", files_override={"file": ("t.wav", b"x" * (2 * 1024 * 1024), "audio/wav")})
    assert resp.status_code == 413
    assert reads == [], "the upload was parsed and spooled before the 413"


def test_align_without_content_length_is_411_before_the_handler(monkeypatch):
    import httpx

    seen = []
    monkeypatch.setattr(appmod, "_parse_lines", lambda lyrics: seen.append(lyrics) or ["x"])
    _install_stubs()
    full = httpx.Request("POST", "http://t/align", files=_FILES, data={"lyrics": "a"})
    body = full.read()

    async def chunks():
        yield body

    async def _run():
        async with _async_client() as client:
            req = client.build_request("POST", "/align", content=chunks(), headers={"content-type": full.headers["content-type"]})
            assert "content-length" not in req.headers
            return await client.send(req)

    resp = asyncio.run(_run())
    assert resp.status_code == 411
    assert resp.json()["detail"] == "Content-Length required"
    assert seen == []


def test_lyrics_over_the_multipart_part_cap_is_413(monkeypatch):
    monkeypatch.setattr(appmod, "MAX_LYRICS_BYTES", appmod.LYRICS_PART_CAP_BYTES)
    _install_stubs()
    resp = _post(lyrics="la " * (400 * 1024))  # ~1.2 MB, over Starlette's 1 MiB part cap
    assert resp.status_code == 413, resp.text


# --------------------------------------------------------------------------
# concurrency: /align queues on one lock; /health never waits behind it
# --------------------------------------------------------------------------


class _BlockingAligner(_StubAligner):
    """Tracks how many pipelines run at once; optionally blocks until released."""

    def __init__(self, hold_seconds=0.0, release=None):
        super().__init__()
        self.hold_seconds = hold_seconds
        self.release = release
        self.started = threading.Event()
        self.active = 0
        self.max_active = 0
        self._mu = threading.Lock()

    def transcribe(self, vocal_path, language):
        with self._mu:
            self.active += 1
            self.max_active = max(self.max_active, self.active)
        self.started.set()
        try:
            if self.release is not None:
                self.release.wait(10)
            time.sleep(self.hold_seconds)
        finally:
            with self._mu:
                self.active -= 1
        return self._transcript


def _async_client():
    import httpx

    return httpx.AsyncClient(transport=httpx.ASGITransport(app=appmod.app), base_url="http://t", timeout=60)


_FILES = {"file": ("track.wav", _wav_bytes(), "audio/wav")}


async def _wait_started(aligner, task):
    deadline = time.monotonic() + 2
    while not aligner.started.is_set():
        if task.done():
            raise AssertionError(f"/align finished before the pipeline started: {task.result().status_code}")
        if time.monotonic() > deadline:
            raise AssertionError("pipeline never started within 2s")
        await asyncio.sleep(0.01)


def test_health_answers_while_align_is_running():
    release = threading.Event()
    aligner = _BlockingAligner(release=release)
    _install_stubs(aligner=aligner)

    async def _run():
        async with _async_client() as client:
            task = asyncio.create_task(client.post("/align", files=_FILES, data={"lyrics": "one line"}))
            await _wait_started(aligner, task)
            health = await asyncio.wait_for(client.get("/health"), timeout=2)
            still_running = not task.done()
            release.set()
            return health, still_running, await task

    health, still_running, align = asyncio.run(_run())
    assert health.status_code == 200 and still_running
    assert align.status_code == 200, align.text


def test_concurrent_align_requests_queue_and_never_overlap(monkeypatch):
    monkeypatch.setattr(appmod, "_ADMISSION", threading.BoundedSemaphore(3))
    aligner = _BlockingAligner(hold_seconds=0.1)
    _install_stubs(aligner=aligner)

    async def _run():
        async with _async_client() as client:
            return await asyncio.gather(*(client.post("/align", files=_FILES, data={"lyrics": "l"}) for _ in range(3)))

    responses = asyncio.run(_run())
    # All succeed (queued, never refused with a 5xx the Go breaker would count)...
    assert [r.status_code for r in responses] == [200, 200, 200]
    # ...and never two pipelines at once.
    assert aligner.max_active == 1, f"{aligner.max_active} pipelines ran concurrently"


def test_align_over_the_admission_limit_is_429_and_the_first_completes(monkeypatch):
    monkeypatch.setattr(appmod, "_ADMISSION", threading.BoundedSemaphore(1))
    release = threading.Event()
    aligner = _BlockingAligner(release=release)
    _install_stubs(aligner=aligner)

    async def _run():
        async with _async_client() as client:
            task = asyncio.create_task(client.post("/align", files=_FILES, data={"lyrics": "l"}))
            await _wait_started(aligner, task)
            busy_task = asyncio.create_task(client.post("/align", files=_FILES, data={"lyrics": "l"}))
            refused_promptly = bool((await asyncio.wait({busy_task}, timeout=2))[0])
            health = await asyncio.wait_for(client.get("/health"), timeout=2)
            release.set()
            return refused_promptly, await busy_task, health, await task

    refused_promptly, busy, health, first = asyncio.run(_run())
    assert refused_promptly, "the over-limit request was queued, not refused"
    assert busy.status_code == 429 and busy.json()["detail"] == "aligner busy"
    assert busy.headers["retry-after"]
    assert health.status_code == 200
    assert first.status_code == 200, first.text


def test_admission_slot_is_released_after_success_and_after_a_500(monkeypatch):
    monkeypatch.setattr(appmod, "_ADMISSION", threading.BoundedSemaphore(1))
    _install_stubs(aligner=_StubAligner(raise_error=True))
    assert _post(lyrics="l").status_code == 500
    _install_stubs()
    assert _post(lyrics="l").status_code == 200
    assert _post(lyrics="l").status_code == 200


# --------------------------------------------------------------------------
# upload temp files: fixed name, removed after every request
# --------------------------------------------------------------------------


class _PathRecordingSeparator(_StubSeparator):
    def __init__(self):
        super().__init__()
        self.paths = []

    def separate(self, audio_path):
        self.paths.append(audio_path)
        return super().separate(audio_path)


def test_client_filename_never_reaches_the_filesystem():
    sep = _PathRecordingSeparator()
    _install_stubs(separator=sep)
    assert _post(lyrics="a", files_override={"file": ("x.m3u8", _wav_bytes(), "audio/wav")}).status_code == 200
    assert sep.paths[0].endswith(".upload") and "m3u8" not in sep.paths[0]


class _StemWritingSeparator:
    """Writes a real vocal-stem temp file, as the real separator will."""

    def __init__(self):
        self.paths = []

    def separate(self, audio_path):
        fd, vocal = tempfile.mkstemp(suffix=".wav")
        os.close(fd)
        self.paths += [audio_path, vocal]
        return vocal


@pytest.mark.parametrize("fail", [False, True], ids=["success", "500"])
def test_upload_and_vocal_stem_temp_files_are_removed(monkeypatch, tmp_path, fail):
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    sep = _StemWritingSeparator()
    _install_stubs(separator=sep, aligner=_StubAligner(raise_error=fail))
    resp = _post(lyrics="a line")
    assert resp.status_code == (500 if fail else 200)
    assert len(sep.paths) == 2
    leftover = [p for p in sep.paths if os.path.exists(p)]
    assert leftover == [], f"temp files left behind: {leftover}"
    assert os.listdir(tmp_path) == []


def test_upload_temp_file_is_removed_when_the_write_fails(monkeypatch, tmp_path):
    monkeypatch.setattr(tempfile, "tempdir", str(tmp_path))
    real = tempfile.NamedTemporaryFile

    def failing(*a, **kw):
        f = real(*a, **kw)
        f.write = lambda _data: (_ for _ in ()).throw(OSError("disk full"))
        return f

    monkeypatch.setattr(tempfile, "NamedTemporaryFile", failing)
    _install_stubs()
    assert _post(lyrics="a line").status_code == 500
    assert [p for p in os.listdir(tmp_path) if p.endswith(".upload")] == []
