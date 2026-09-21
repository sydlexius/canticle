"""Canticle forced-alignment sidecar (issue #1005, epic #482 slice 1).

The finished sidecar serves `POST /align`: it takes an audio file plus lyric
text Canticle already has and returns word-level timestamps keyed to the
supplied lines, plus the raw ASR transcript so Canticle can run its own
content gate (`verification.Similarity`) without a second sidecar.

NEVER returns a blind transcription as lyrics: `words` always comes from
aligning the caller's own lines; the transcript is an independent ASR pass,
a content-gate input only.

This module currently holds only the pure, model-free core: configuration,
device selection, lyric line splitting, and the CTC alignment planning that
aligns every supplied line as ONE sequence and maps the result back to the
caller's line indices. The HTTP endpoints, audio decoding, the vocal
separation + ASR model layer, and the container image land in later slices
of #1005.

Nothing here imports a heavy ML package (torch, torchaudio, demucs,
whisperx), so test_app.py runs with only pytest installed.

No lyric text is ever logged: only counts, byte lengths, and line indices.
"""

import logging
import os
import unicodedata
from dataclasses import dataclass

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
# its own 400 before an endpoint sees it (the /align handler lands in a later
# slice), so a lyrics limit above it would be unreachable: the configured
# value is clamped to it.
LYRICS_PART_CAP_BYTES = 1024 * 1024
# Decoded-duration ceiling: bytes alone do not bound work (10 hours of
# silence compresses to a few MB of Opus), and every pipeline stage scales
# with duration. 20 minutes is several times a normal track.
DEFAULT_MAX_AUDIO_SECONDS = 1200
# Wall-clock ceiling on one ffmpeg decode. Decoding is capped at
# MAX_AUDIO_SECONDS of output, which a healthy ffmpeg does in seconds, so
# hitting this means the environment is starved or wedged: a 500, not 413.
DEFAULT_DECODE_TIMEOUT_SECONDS = 300
# Multipart framing + the `language` field, on top of the two content limits.
# Reserved for the request Content-Length pre-check, which lands with the
# /align handler in a later slice.
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
SEPARATION_MODEL = os.environ.get("ALIGNER_SEPARATION_MODEL", "").strip() or DEFAULT_SEPARATION_MODEL
WHISPER_MODEL = os.environ.get("ALIGNER_WHISPER_MODEL", "").strip() or DEFAULT_WHISPER_MODEL
ALIGN_LANGUAGE = (os.environ.get("ALIGNER_ALIGN_LANGUAGE", "").strip() or DEFAULT_ALIGN_LANGUAGE).lower()
DEVICE_ENV = os.environ.get("ALIGNER_DEVICE", "").strip().lower() or DEFAULT_DEVICE
LOG_LEVEL = os.environ.get("ALIGNER_LOG_LEVEL", "").strip().upper() or "INFO"

# Languages whisperx has a DEFAULT align model for, and that model's name.
# Source: whisperx 3.4.3, whisperx/alignment.py
# DEFAULT_ALIGN_MODELS_TORCH | DEFAULT_ALIGN_MODELS_HF.
# A static copy so validation works without whisperx installed (the test
# venv). The boot path (added in a later slice) is meant to replace it with
# whisperx's own tables, so a whisperx bump cannot silently drift from what
# is validated; in this slice the static copy is the only table.
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


def validate_config(align_models: dict) -> None:
    """Fails startup loudly on a default language with no align model.

    Otherwise, once the HTTP endpoints land (a later slice), every request
    that omits `language` would fail while the service still looked healthy.
    """
    if ALIGN_LANGUAGE not in align_models:
        raise RuntimeError(
            f"ALIGNER_ALIGN_LANGUAGE={ALIGN_LANGUAGE!r} has no default whisperx align model; "
            f"supported: {', '.join(sorted(align_models))}"
        )


_VALID_DEVICES = {"cpu", "cuda", "mps"}


def select_device(env_value: str, cuda_available: bool, mps_available: bool) -> str:
    """Pick the inference device from an explicit override or availability.

    Pure function (availability is passed in, not detected here) so it is
    unit-testable with no ML deps installed; the server's startup supplies
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
