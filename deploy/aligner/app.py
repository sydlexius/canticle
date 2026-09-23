"""Canticle forced-alignment sidecar (issue #1005, epic #482 slice 1).

`POST /align` takes an audio file plus lyric text Canticle already has and
returns word-level timestamps keyed to the supplied lines, plus the raw ASR
transcript so Canticle can run its own content gate
(`verification.Similarity`) without a second sidecar.

NEVER returns a blind transcription as lyrics: `words` always comes from
aligning the caller's own lines to the separated vocal stem; `transcript` is
an independent ASR pass over that stem, a content-gate input only.

Pipeline: Demucs isolates the vocal stem, then the supplied lines are
forced-aligned against it as ONE CTC sequence (a wav2vec2 alignment model
loaded directly via torchaudio/transformers, not the Whisper decoder) while a
faster-whisper ASR pass over the same stem produces the transcript. The model
implementations live in `_aligner_models.py`; this module holds the pure
core, the ffmpeg decode, and the HTTP layer.

Heavy ML imports (torch, torchaudio, demucs, faster_whisper, transformers)
are NEVER imported at module scope -- only inside functions `lifespan()`
calls at real startup, so test_app.py can import this module and drive the
full HTTP contract with the model layer stubbed, with none of those packages
installed.

No lyric text is ever logged: only counts, byte lengths, and line indices.
"""

import logging
import os
import threading
import traceback
import unicodedata
from contextlib import asynccontextmanager
from dataclasses import dataclass
from typing import Optional, Protocol

from fastapi import FastAPI, File, Form, HTTPException, Request, UploadFile
from fastapi.responses import JSONResponse
from starlette.concurrency import run_in_threadpool
from starlette.exceptions import HTTPException as StarletteHTTPException

logger = logging.getLogger("canticle.aligner")

# --------------------------------------------------------------------------
# Config (env vars)
# --------------------------------------------------------------------------

DEFAULT_SEPARATION_MODEL = "htdemucs"
DEFAULT_WHISPER_MODEL = "base"
DEFAULT_ALIGN_LANGUAGE = "en"
DEFAULT_DEVICE = "auto"

# Bounds. Canticle sends one track (a few minutes) plus a lyric sheet (a few
# KB of text); both ceilings are generous multiples of that so a legitimate
# request never trips them, while an oversized/malicious upload is rejected
# before it can exhaust memory. Mirrors yamnet's MAX_UPLOAD_BYTES pattern.
DEFAULT_MAX_AUDIO_BYTES = 100 * 1024 * 1024  # 100 MiB
DEFAULT_MAX_LYRICS_BYTES = 64 * 1024  # 64 KiB
# Starlette's multipart parser rejects any single form field over 1 MiB with
# its own 400 before the /align handler runs, so a lyrics limit above it would
# be unreachable: the configured value is clamped to it.
LYRICS_PART_CAP_BYTES = 1024 * 1024
# Decoded-duration ceiling: bytes alone do not bound work (10 hours of
# silence compresses to a few MB of Opus), and every pipeline stage scales
# with duration. 20 minutes is several times a normal track.
DEFAULT_MAX_AUDIO_SECONDS = 1200
# Wall-clock ceiling on one ffmpeg decode. Decoding is capped at
# MAX_AUDIO_SECONDS of output, which a healthy ffmpeg does in seconds, so
# hitting this means the environment is starved or wedged: a 500, not 413.
DEFAULT_DECODE_TIMEOUT_SECONDS = 300
# Multipart framing + the `language` field, on top of the two content limits,
# for the Content-Length pre-check.
_MULTIPART_OVERHEAD_BYTES = 64 * 1024


def _env_int(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        value = int(raw)
    except ValueError:
        logger.warning("aligner: invalid int for %s=%r, using default %d", name, raw, default)
        return default
    if value <= 0:
        logger.warning("aligner: non-positive value for %s=%r, using default %d", name, raw, default)
        return default
    return value


def _resolve_max_lyrics_bytes() -> int:
    value = _env_int("ALIGNER_MAX_LYRICS_BYTES", DEFAULT_MAX_LYRICS_BYTES)
    if value > LYRICS_PART_CAP_BYTES:
        logger.warning(
            "aligner: ALIGNER_MAX_LYRICS_BYTES=%d exceeds the multipart part cap, clamping to %d",
            value, LYRICS_PART_CAP_BYTES,
        )
        return LYRICS_PART_CAP_BYTES
    return value


MAX_AUDIO_BYTES = _env_int("ALIGNER_MAX_AUDIO_BYTES", DEFAULT_MAX_AUDIO_BYTES)
MAX_LYRICS_BYTES = _resolve_max_lyrics_bytes()
MAX_AUDIO_SECONDS = _env_int("ALIGNER_MAX_AUDIO_SECONDS", DEFAULT_MAX_AUDIO_SECONDS)
DECODE_TIMEOUT_SECONDS = _env_int("ALIGNER_DECODE_TIMEOUT_SECONDS", DEFAULT_DECODE_TIMEOUT_SECONDS)
# /align requests admitted at once (running + queued on _PIPELINE_LOCK). Each
# holds up to MAX_AUDIO_BYTES in memory while it waits, so this bounds queued
# memory; one over it gets 429. Default 2: one running plus one queued.
MAX_PENDING = _env_int("ALIGNER_MAX_PENDING", 2)
SEPARATION_MODEL = os.environ.get("ALIGNER_SEPARATION_MODEL", "").strip() or DEFAULT_SEPARATION_MODEL
WHISPER_MODEL = os.environ.get("ALIGNER_WHISPER_MODEL", "").strip() or DEFAULT_WHISPER_MODEL
ALIGN_LANGUAGE = (os.environ.get("ALIGNER_ALIGN_LANGUAGE", "").strip() or DEFAULT_ALIGN_LANGUAGE).lower()
DEVICE_ENV = os.environ.get("ALIGNER_DEVICE", "").strip().lower() or DEFAULT_DEVICE
LOG_LEVEL = os.environ.get("ALIGNER_LOG_LEVEL", "").strip().upper() or "INFO"

# Languages this sidecar has a DEFAULT align model for, and that model's name:
# a name in torchaudio.pipelines.__all__ (the 5 torch-type languages) is
# loaded via torchaudio; anything else is a Hugging Face repo id loaded via
# transformers. See _aligner_models.FasterWhisperAligner._load_align_model.
#
# THE SOLE SOURCE OF TRUTH (#1016): this used to be a static copy that
# lifespan() overwrote at real boot with whisperx's own
# DEFAULT_ALIGN_MODELS_TORCH | DEFAULT_ALIGN_MODELS_HF tables. Now that
# whisperx is gone there is nothing to sync from, so this table is what both
# request validation AND the real model load consult -- a language added here
# must have a real torchaudio bundle or HF repo id, checked by the
# Dockerfile's build-time smoke check, which imports this module and every
# entry's torchaudio.pipelines.__all__ membership (the SAME check
# _aligner_models.FasterWhisperAligner._load_align_model makes at request
# time) rather than loading any model weights.
# Original source (informational only, nothing copied): whisperx 3.4.3,
# whisperx/alignment.py DEFAULT_ALIGN_MODELS_TORCH | DEFAULT_ALIGN_MODELS_HF.
ALIGN_MODELS = {
    "en": "WAV2VEC2_ASR_BASE_960H",
    "fr": "VOXPOPULI_ASR_BASE_10K_FR",
    "de": "VOXPOPULI_ASR_BASE_10K_DE",
    "es": "VOXPOPULI_ASR_BASE_10K_ES",
    "it": "VOXPOPULI_ASR_BASE_10K_IT",
    "ja": "jonatasgrosman/wav2vec2-large-xlsr-53-japanese",
    "zh": "jonatasgrosman/wav2vec2-large-xlsr-53-chinese-zh-cn",
    "nl": "jonatasgrosman/wav2vec2-large-xlsr-53-dutch",
    "uk": "Yehor/wav2vec2-xls-r-300m-uk-with-small-lm",
    "pt": "jonatasgrosman/wav2vec2-large-xlsr-53-portuguese",
    "ar": "jonatasgrosman/wav2vec2-large-xlsr-53-arabic",
    "cs": "comodoro/wav2vec2-xls-r-300m-cs-250",
    "ru": "jonatasgrosman/wav2vec2-large-xlsr-53-russian",
    "pl": "jonatasgrosman/wav2vec2-large-xlsr-53-polish",
    "hu": "jonatasgrosman/wav2vec2-large-xlsr-53-hungarian",
    "fi": "jonatasgrosman/wav2vec2-large-xlsr-53-finnish",
    "fa": "jonatasgrosman/wav2vec2-large-xlsr-53-persian",
    "el": "jonatasgrosman/wav2vec2-large-xlsr-53-greek",
    "tr": "mpoyraz/wav2vec2-xls-r-300m-cv7-turkish",
    "da": "saattrupdan/wav2vec2-xls-r-300m-ftspeech",
    "he": "imvladikon/wav2vec2-xls-r-300m-hebrew",
    "vi": "nguyenvulebinh/wav2vec2-base-vi",
    "ko": "kresnik/wav2vec2-large-xlsr-korean",
    "ur": "kingabzpro/wav2vec2-large-xls-r-300m-Urdu",
    "te": "anuragshas/wav2vec2-large-xlsr-53-telugu",
    "hi": "theainerd/Wav2Vec2-large-xlsr-hindi",
    "ca": "softcatala/wav2vec2-large-xlsr-catala",
    "ml": "gvs/wav2vec2-large-xlsr-malayalam",
    "no": "NbAiLab/nb-wav2vec2-1b-bokmaal-v2",
    "nn": "NbAiLab/nb-wav2vec2-1b-nynorsk",
    "sk": "comodoro/wav2vec2-xls-r-300m-sk-cv8",
    "sl": "anton-l/wav2vec2-large-xlsr-53-slovenian",
    "hr": "classla/wav2vec2-xls-r-parlaspeech-hr",
    "ro": "gigant/romanian-wav2vec2",
    "eu": "stefan-it/wav2vec2-large-xlsr-53-basque",
    "gl": "ifrz/wav2vec2-large-xlsr-galician",
    "ka": "xsway/wav2vec2-large-xlsr-georgian",
    "lv": "jimregan/wav2vec2-large-xlsr-latvian-cv",
    "tl": "Khalsuu/filipino-wav2vec2-l-xls-r-300m-official",
}


def is_torchaudio_align_model(name: str, torchaudio_module) -> bool:
    """True if `name` is one of torchaudio's own bundled align-model names.

    The ONE membership test shared by the real request-time model load
    (_aligner_models.FasterWhisperAligner._load_align_model) and the
    Dockerfile's build-time smoke check, so the two cannot drift: a name in
    `torchaudio_module.pipelines.__all__` loads via torchaudio, everything
    else is assumed to be a Hugging Face repo id.
    """
    return name in torchaudio_module.pipelines.__all__


def validate_config(align_models: dict) -> None:
    """Fails startup loudly on a default language with no align model.

    Otherwise every request that omits `language` would 400 while /health
    reads ok.
    """
    if ALIGN_LANGUAGE not in align_models:
        raise RuntimeError(
            f"ALIGNER_ALIGN_LANGUAGE={ALIGN_LANGUAGE!r} has no default align model; "
            f"supported: {', '.join(sorted(align_models))}"
        )


_VALID_DEVICES = {"cpu", "cuda", "mps"}


def select_device(env_value: str, cuda_available: bool, mps_available: bool) -> str:
    """Pick the inference device from an explicit override or availability.

    Pure function (availability is passed in, not detected here) so it is
    unit-testable with no ML deps installed; `lifespan()` supplies
    `torch.cuda.is_available()` / `torch.backends.mps.is_available()`.

    An explicit override always wins, even against unavailable hardware
    (fails loudly at model load rather than silently falling back). An
    unrecognized value fails CONSERVATIVELY to "cpu", never to whatever
    auto-detection would have picked (mirrors the repo's "unrecognized input
    never resolves to something less safe" posture for config enums).
    "auto" prefers cuda, then mps, then cpu.
    """
    value = (env_value or "").strip().lower()
    if value in _VALID_DEVICES:
        return value
    if value not in ("", "auto"):
        logger.warning("aligner: unrecognized ALIGNER_DEVICE=%r, falling back to cpu", env_value)
        return "cpu"
    if cuda_available:
        return "cuda"
    if mps_available:
        return "mps"
    return "cpu"


# --------------------------------------------------------------------------
# Alignment planning (pure; the model layer supplies dictionary and spans)
# --------------------------------------------------------------------------


@dataclass(frozen=True)
class WordSpan:
    """One aligned word. Field names match the wire contract exactly."""

    text: str
    start_ms: int
    end_ms: int
    line_index: int
    confidence: float


def plan_alignment(lines: list[str], dictionary: dict, blank_id: int, word_sep: str = "|"):
    """Flattens ALL lines into ONE CTC target sequence (issue #1005).

    Aligning the whole lyric as a single sequence is what makes the output
    monotonic: a CTC path cannot go back in time, so every word of line N+1
    lands after every word of line N, and a repeated line (a chorus) is a
    distinct later stretch of the sequence rather than a second independent
    search over the whole clip. Words are whitespace-delimited tokens of each
    line; characters the align model has no token for (punctuation, digits)
    are dropped from the targets but kept in the returned word text. A char
    that maps to the blank id (the base English model's "-") is dropped too,
    since CTC cannot emit blank as a target. The separator goes only BETWEEN
    two words that both contribute targets, so a dropped word never leaves a
    doubled or trailing separator. Lookup is on the NFC form with U+2019
    folded to an ASCII apostrophe (the token the English model has).

    Returns (targets, planned) where planned is a list of
    (line_index, word_text, first_target, end_target); a word whose range is
    empty has no alignable characters and is later skipped.
    """
    sep = dictionary.get(word_sep)
    use_sep = sep is not None and sep != blank_id
    targets: list[int] = []
    planned: list[tuple[int, str, int, int]] = []
    for line_index, line in enumerate(lines):
        for word in line.split():
            key = unicodedata.normalize("NFC", word).replace("’", "'").lower()
            chars = [t for t in (dictionary.get(ch) for ch in key) if t is not None and t != blank_id]
            if chars and targets and use_sep:
                targets.append(sep)
            first = len(targets)
            targets.extend(chars)
            planned.append((line_index, word, first, len(targets)))
    return targets, planned


def words_from_token_spans(planned, spans, frame_seconds: float) -> list[WordSpan]:
    """Maps per-target CTC token spans back to words and caller line indices.

    `spans[i]` is (start_frame, end_frame_exclusive, score) for targets[i],
    blanks already removed, so a word covers only frames where its own
    characters were emitted and never the silence after it. Because spans are frame-disjoint and in target order, each word's end is
    at or before the next word's start.
    """
    out: list[WordSpan] = []
    for line_index, text, first, end in planned:
        if first == end:
            continue  # no alignable characters; skip rather than fabricate a span
        ws = spans[first:end]
        out.append(
            WordSpan(
                text=text,
                start_ms=int(round(ws[0][0] * frame_seconds * 1000)),
                end_ms=int(round(ws[-1][1] * frame_seconds * 1000)),
                line_index=line_index,
                confidence=round(sum(s[2] for s in ws) / len(ws), 3),
            )
        )
    return out


def _parse_lines(lyrics: str) -> list[str]:
    """Splits supplied lyric text into non-blank lines.

    Splits on "\n" only (a trailing "\r" is stripped), never str.splitlines(),
    which also breaks on form feed, U+0085, U+2028 and friends: the caller
    counts lines by "\n", and a line_index must mean the same line to both.
    Blank lines are dropped and do NOT consume a line_index -- the index
    counts only non-blank lines, 0-based, in supplied order.

    "Blank" means empty after Python `str.strip()` (surviving lines are also
    returned stripped). That whitespace set is WIDER than Go's
    `unicode.IsSpace`: it also includes U+001C..U+001F (the file/group/
    record/unit separators). A Go caller mapping a line_index back to its own
    lines must apply the same rule (e.g. treat a line as blank when it
    contains only `unicode.IsSpace` runes or U+001C..U+001F), or a line made
    only of those separators shifts every later index.
    """
    return [line for line in (raw.strip() for raw in lyrics.split("\n")) if line]


# --------------------------------------------------------------------------
# Model layer seam
# --------------------------------------------------------------------------


class _BadAudioError(Exception):
    """Raised when the CALLER's upload cannot be decoded/used.

    The ONLY exception type the pipeline treats as the caller's fault (400).
    Anything else raised by the model layer is a genuine pipeline failure and
    is left to propagate as a 500. Reserved for the original upload; a
    rejection decoding audio this sidecar generated itself (the separated
    vocal stem) is _InternalAudioError instead -- see decode_pcm's
    `reject_error`.
    """


class _InternalAudioError(Exception):
    """Raised when THIS SIDECAR's own generated audio cannot be decoded.

    Covers a decode rejection on the separated vocal stem (Demucs' own
    output), inside FasterWhisperAligner.transcribe/align: that file was
    never the caller's upload, so a decode failure there is a pipeline bug or
    a broken environment, not the caller's fault, and must propagate as a
    500 like any other unclassified exception -- never read as the
    documented 400 "cannot read audio", which would misclassify a server
    fault as a benign miss the Go client's circuit breaker never counts.
    """


class _AudioTooLongError(Exception):
    """Raised when the decoded audio exceeds MAX_AUDIO_SECONDS (413)."""


FFMPEG = "ffmpeg"


def decode_pcm(
    audio_path: str, sample_rate: int, channels: int, *, reject_error: type[Exception] = _BadAudioError
) -> bytes:
    """Decodes any ffmpeg-readable file to interleaved float32 PCM bytes.

    Classification is the point (the Go client treats 4xx as a benign miss
    that never trips its breaker, so a server fault must NOT read as 400):
    only ffmpeg REJECTING the input (exit code > 0, or no samples) is
    caller-classified, raising `reject_error` (default _BadAudioError, for
    the caller's own upload; callers decoding audio this sidecar generated
    itself pass `reject_error=_InternalAudioError` so the same ffmpeg
    rejection reads as a server fault instead). A missing/unrunnable ffmpeg
    (OSError), ffmpeg killed by a signal (exit code < 0, e.g. OOM), or a
    decode that outlives DECODE_TIMEOUT_SECONDS (subprocess.TimeoutExpired)
    is always an environment fault and propagates as a 500, regardless of
    `reject_error`.

    The file reaches ffmpeg as its stdin (`fd:`), never by name, and
    `-protocol_whitelist fd` forbids every other protocol: format detection
    therefore never sees a file extension, and a playlist-style upload (HLS,
    concat) cannot make ffmpeg open a local file or a URL. `fd:` rather than
    `pipe:` because fd: stays seekable on a regular file, which an MP4 with
    its index (moov) at the end needs.

    Output is capped at MAX_AUDIO_SECONDS + 1 (`-t`), so an over-long input
    costs a bounded decode and is then rejected as _AudioTooLongError (413).
    """
    import subprocess  # noqa: PLC0415

    cmd = [
        FFMPEG, "-nostdin", "-v", "error", "-protocol_whitelist", "fd", "-i", "fd:",
        "-t", str(MAX_AUDIO_SECONDS + 1), "-f", "f32le", "-ac", str(channels), "-ar", str(sample_rate), "-",
    ]
    with open(audio_path, "rb") as src:
        proc = subprocess.run(  # noqa: S603 - fixed argv, no shell
            cmd, stdin=src, stdout=subprocess.PIPE, stderr=subprocess.PIPE, check=False, timeout=DECODE_TIMEOUT_SECONDS
        )
    if proc.returncode != 0 or not proc.stdout:
        # stderr is upload-derived: bounded and stripped of control characters
        # before it reaches the log, and never returned to the client.
        diag = "".join(c if c.isprintable() else " " for c in proc.stderr[-400:].decode("utf-8", "replace")).strip()
        logger.warning("decode: ffmpeg exit=%d stderr=%r", proc.returncode, diag)
    if proc.returncode < 0:
        raise RuntimeError(f"ffmpeg killed by signal {-proc.returncode}")
    if proc.returncode > 0 or not proc.stdout:
        raise reject_error("cannot decode audio")
    if len(proc.stdout) > MAX_AUDIO_SECONDS * sample_rate * channels * 4:
        raise _AudioTooLongError(f"audio longer than {MAX_AUDIO_SECONDS}s")
    return proc.stdout


class Separator(Protocol):
    """Isolates the vocal stem from a mixed-audio file."""

    def separate(self, audio_path: str) -> str:
        """Returns the path to an extracted vocal-only audio file.

        Raises _BadAudioError if the input cannot be decoded.
        """
        ...


class Aligner(Protocol):
    """Transcribes and forced-aligns a vocal stem against supplied lyric lines."""

    def transcribe(self, vocal_path: str, language: str) -> str:
        """Returns the raw ASR transcript of the vocal stem (content-gate input)."""
        ...

    def align(self, vocal_path: str, lines: list[str], language: str) -> list[WordSpan]:
        """Forced-aligns the SUPPLIED lines to the vocal stem; never invents words."""
        ...


def _build_separator(_device: str) -> Separator:
    """Constructs the real Demucs-backed separator; imports demucs lazily.

    Only called from lifespan() at real startup -- never at module import
    time, so importing this module never requires torch/demucs installed.
    """
    from _aligner_models import DemucsSeparator  # noqa: PLC0415 - deliberate lazy import

    return DemucsSeparator(model_name=SEPARATION_MODEL, device=_device)


def _build_aligner(_device: str) -> Aligner:
    """Constructs the real faster-whisper/wav2vec2-backed aligner; imports lazily (see above)."""
    from _aligner_models import FasterWhisperAligner  # noqa: PLC0415 - deliberate lazy import

    return FasterWhisperAligner(whisper_model=WHISPER_MODEL, device=_device)


_state: dict = {}

# One pipeline at a time. Admitted requests (_ADMISSION) QUEUE on this lock
# rather than being refused: a 503 would read as a sidecar fault to the Go client,
# which counts 5xx toward its circuit breaker. Two concurrent Demucs +
# Whisper + wav2vec2 passes would also double peak memory for no throughput
# gain on a CPU-bound box. /health is `async def`, so it runs on the event
# loop and never waits behind a queued /align in the threadpool.
_PIPELINE_LOCK = threading.Lock()
# Admission gate, taken non-blocking before the body is parsed or copied into
# memory; a request over MAX_PENDING gets 429 (a 4xx, not a breaker-counted 5xx).
_ADMISSION = threading.BoundedSemaphore(MAX_PENDING)


def _configure_logging() -> None:
    """Gives the canticle.* loggers a handler so INFO request logs appear.

    uvicorn configures only its own loggers, so without this the aligner's
    INFO lines were dropped (only WARNING+ reached stderr via logging's
    last-resort handler).
    """
    root = logging.getLogger("canticle")
    root.setLevel(LOG_LEVEL)
    if not root.handlers:
        handler = logging.StreamHandler()
        handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s: %(message)s"))
        root.addHandler(handler)


@asynccontextmanager
async def lifespan(_: FastAPI):
    import torch  # noqa: PLC0415 - only the real server boot needs torch present

    _configure_logging()
    validate_config(ALIGN_MODELS)
    device = select_device(DEVICE_ENV, torch.cuda.is_available(), torch.backends.mps.is_available())
    _state["device"] = device
    _state["separator"] = _build_separator(device)
    _state["aligner"] = _build_aligner(device)
    yield
    _state.clear()


app = FastAPI(title="Canticle forced-alignment sidecar", lifespan=lifespan)


@app.middleware("http")
async def reject_oversized_body(request: Request, call_next):
    """Rejects an /align body whose declared size exceeds every limit, BEFORE
    multipart parsing spools it to disk. A POST with no valid Content-Length
    (e.g. chunked) gets 411: Starlette would spool its uncapped file part to
    disk first, and Canticle's client always sends Content-Length. Then
    admits at most MAX_PENDING /align requests (429 beyond), released on
    every exit path; /health is never gated.
    """
    if request.url.path == "/align" and request.method == "POST":
        declared = request.headers.get("content-length", "")
        if not (declared.isascii() and declared.isdigit()):
            return JSONResponse(status_code=411, content={"detail": "Content-Length required"})
        if int(declared) > MAX_AUDIO_BYTES + MAX_LYRICS_BYTES + _MULTIPART_OVERHEAD_BYTES:
            return JSONResponse(status_code=413, content={"detail": "request body too large"})
        if not _ADMISSION.acquire(blocking=False):
            return JSONResponse(status_code=429, content={"detail": "aligner busy"}, headers={"Retry-After": "30"})
        try:
            return await call_next(request)
        finally:
            _ADMISSION.release()
    return await call_next(request)


@app.exception_handler(StarletteHTTPException)
async def part_cap_is_413(request: Request, exc: StarletteHTTPException):
    """Starlette rejects a form field over its 1 MiB part cap with a 400
    ("Part exceeded maximum size ..."); that is an oversized field, so it
    becomes the documented 413. Everything else keeps FastAPI's default.
    """
    from fastapi.exception_handlers import http_exception_handler  # noqa: PLC0415

    if exc.status_code == 400 and str(exc.detail).startswith(("Part exceeded maximum size", "Field exceeded maximum size")):
        return JSONResponse(status_code=413, content={"detail": "form field too large"})
    return await http_exception_handler(request, exc)


@app.get("/health")
async def health():
    """Liveness only: models load lazily on the first /align, not here."""
    return {
        "status": "ok",
        "device": _state.get("device", "unknown"),
        "separation_model": SEPARATION_MODEL,
        "align_model": ALIGN_MODELS.get(ALIGN_LANGUAGE, "unknown"),
        "whisper_model": WHISPER_MODEL,
    }


@app.post("/align")
async def align(
    file: UploadFile = File(...),
    lyrics: str = Form(...),
    language: Optional[str] = Form(None),
):
    # By now Starlette has already spooled the whole upload to a temp file
    # (the Content-Length middleware is what keeps an oversized declared body
    # from getting that far). Reading MAX_AUDIO_BYTES + 1 bounds only how much
    # of it is copied into memory here.
    raw_audio = await file.read(MAX_AUDIO_BYTES + 1)
    if len(raw_audio) > MAX_AUDIO_BYTES:
        raise HTTPException(status_code=413, detail="audio file too large")
    if len(raw_audio) == 0:
        raise HTTPException(status_code=400, detail="empty audio upload")

    lyrics_bytes = lyrics.encode("utf-8", errors="ignore")
    if len(lyrics_bytes) > MAX_LYRICS_BYTES:
        raise HTTPException(status_code=413, detail="lyrics text too large")

    lines = _parse_lines(lyrics)
    if not lines:
        raise HTTPException(status_code=400, detail="lyrics text is empty")

    lang = ((language or "").strip() or ALIGN_LANGUAGE).lower()
    if lang not in ALIGN_MODELS:
        raise HTTPException(status_code=400, detail=f"unsupported language: {lang[:16]!r}")

    separator: Optional[Separator] = _state.get("separator")
    aligner: Optional[Aligner] = _state.get("aligner")
    if separator is None or aligner is None:
        # Never reachable once lifespan() has run; guards a misuse of the app
        # (e.g. calling /align before startup) with a clean 500 instead of an
        # AttributeError.
        raise HTTPException(status_code=500, detail="model layer not initialized")

    logger.info("align: request received, audio_bytes=%d lines=%d language=%s", len(raw_audio), len(lines), lang)

    try:
        transcript, words = await run_in_threadpool(_run_pipeline, separator, aligner, raw_audio, lines, lang)
    except _AudioTooLongError as e:
        logger.warning("align: audio rejected: %s", e)
        raise HTTPException(status_code=413, detail="audio too long") from e
    except _BadAudioError as e:
        logger.warning("align: cannot read audio: %s", e)
        raise HTTPException(status_code=400, detail="cannot read audio") from e
    except Exception as e:  # noqa: BLE001 - surface a clean 500, never the raw traceback
        # Class name only: an exception message can carry lyric text (a
        # tokenizer error quoting the line), so even DEBUG gets only the
        # traceback FRAMES, never str(e).
        logger.error("align: pipeline failed: %s", e.__class__.__name__)
        logger.debug("align: pipeline failure frames:\n%s", "".join(traceback.format_tb(e.__traceback__)))
        raise HTTPException(status_code=500, detail="alignment failed") from e

    logger.info("align: request complete, words=%d transcript_chars=%d", len(words), len(transcript))

    return {
        "words": [
            {
                "text": w.text,
                "start_ms": w.start_ms,
                "end_ms": w.end_ms,
                "line_index": w.line_index,
                "confidence": w.confidence,
            }
            for w in sorted(words, key=lambda w: w.start_ms)
        ],
        "transcript": transcript,
    }


def _run_pipeline(
    separator: Separator,
    aligner: Aligner,
    raw_audio: bytes,
    lines: list[str],
    language: str,
) -> tuple[str, list[WordSpan]]:
    """Runs separation + transcription + alignment. Synchronous, CPU/GPU-bound.

    Dispatched to the threadpool by the caller so the event loop (and
    /health) stays responsive, and serialized on _PIPELINE_LOCK.

    Writes the upload to a temp file with a FIXED suffix (the client's
    filename never reaches the filesystem or ffmpeg) and removes it, plus the
    separator's vocal-stem file, whatever the outcome.
    """
    import contextlib
    import tempfile

    with _PIPELINE_LOCK:
        # Path recorded before the write, so a failed write is still removed.
        tmp = tempfile.NamedTemporaryFile(suffix=".upload", delete=False)
        audio_path = tmp.name
        vocal_path = None
        try:
            with tmp:
                tmp.write(raw_audio)
            # separator.separate raises _BadAudioError itself when the upload
            # cannot be decoded/used (the real Demucs-backed implementation
            # decodes via decode_pcm); any other exception here is a genuine
            # pipeline failure and is left to propagate as a 500, not
            # reclassified as the caller's fault.
            vocal_path = separator.separate(audio_path)
            transcript = aligner.transcribe(vocal_path, language)
            words = aligner.align(vocal_path, lines, language)
            return transcript, words
        finally:
            for path in {audio_path, vocal_path} - {None}:
                with contextlib.suppress(OSError):
                    os.remove(path)
