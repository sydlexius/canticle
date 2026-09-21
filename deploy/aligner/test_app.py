"""Unit tests for the aligner sidecar's pure, model-free core.

Covers config resolution, device selection, lyric line splitting, and the
one-sequence CTC alignment planning. None of it needs torch, demucs,
whisperx, or an HTTP client; the HTTP-contract tests arrive with the
endpoints in a later slice of #1005.

Runs locally with only the lightweight test deps:
    pip install --require-hashes -r requirements-test.txt
    pytest test_app.py -q
"""

import pytest

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
