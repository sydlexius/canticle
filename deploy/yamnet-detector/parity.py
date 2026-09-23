"""Old-vs-new score-parity check for the YAMNet sidecar (base image vs head
image, #1041/#1043). Run by .github/workflows/yamnet.yml's `parity` job on
ubuntu-latest (amd64) -- TensorFlow cannot import under amd64 emulation on
Apple Silicon, so this only runs in CI.

Scores synthetic clips (generated here; no real library audio) against two
running sidecars, VALIDATES each raw response (shape, key set, finite
in-range values, every gate class present -- see validate_scores), then
compares EACH of the three detector gates separately (never just the final
instrumental bool) and asserts every raw score stays within TOLERANCE.

Only score_clip()/fetch_health() need a running server (via curl); everything
else needs neither TensorFlow nor a server, so it runs locally with plain
python3 against canned JSON responses -- see `--selftest` below, which
exercises validate_scores() against hand-built good/bad responses without
docker or a network call.

Gate thresholds mirror the defaults in internal/config/config.go
(InstrumentalDetectorConfig) and internal/detector/detector.go
(defaultVocalClasses/Confidence, defaultSpeechClasses/Confidence): a fixed
operating point for comparing base vs head, not a live-config read.

None of the synthetic clips is guaranteed to land on either side of the music
gate -- "tone" and "chord_percussion" are only INTENDED to approach it. Only
a live YAMNet inference (which only runs in CI) says where a clip actually
lands; the printed table's music_sum/vocal_peak/speech_mean columns are the
one place that is knowable from a real run.
"""

from __future__ import annotations

import argparse
import json
import math
import os
import random
import struct
import subprocess
import sys
import wave
from pathlib import Path

SAMPLE_RATE = 16000
CLIP_SECONDS = 2.0
MUSIC_CLIP_SECONDS = 4.0

MUSIC_CLASSES = ["Music", "Musical instrument"]
MUSIC_MIN_CONFIDENCE = 0.90
VOCAL_CLASSES = [
    "Singing", "Vocal music", "Choir", "A capella", "Chant", "Rapping",
    "Child singing", "Synthetic singing", "Yodeling", "Humming",
]
VOCAL_MAX_CONFIDENCE = 0.015
SPEECH_CLASSES = ["Speech"]
SPEECH_MAX_CONFIDENCE = 0.20

# Absolute tolerance on 0..1 scores: the TF/numpy bump can shift float32 op
# ordering slightly on byte-identical weights; 1e-3 clears that noise floor
# while still catching a real regression (usually off by tenths).
DEFAULT_TOLERANCE = 1e-3

# A chord progression (C - F - G - C), each note ramped up/down (linear
# attack/release, not a hard edge) plus a 2nd harmonic, so it reads as
# tonal/musical content rather than a raw sine -- intended to approach the
# music gate (>= 0.90 summed mean of MUSIC_CLASSES), unlike "tone" (a bare
# 440Hz sine) or "noise". Percussion noise bursts are layered on top on a
# steady eighth-note beat.
CHORD_PROGRESSION_HZ = [
    (261.63, 329.63, 392.00),  # C major
    (349.23, 440.00, 523.25),  # F major
    (392.00, 493.88, 587.33),  # G major
    (261.63, 329.63, 392.00),  # C major
]


def _write_wav(path: Path, samples: list[float]) -> None:
    with wave.open(str(path), "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(SAMPLE_RATE)
        clipped = [max(-1.0, min(1.0, s)) for s in samples]
        w.writeframes(struct.pack(f"<{len(clipped)}h", *(int(s * 32767) for s in clipped)))


def _chord_percussion_clip(rng: random.Random) -> list[float]:
    """Harmonic chord progression (ramped per note) plus periodic
    noise-burst percussion hits, 16kHz mono -- the clip designed to approach
    the music gate (see module docstring)."""
    n = int(SAMPLE_RATE * MUSIC_CLIP_SECONDS)
    chord_dur = MUSIC_CLIP_SECONDS / len(CHORD_PROGRESSION_HZ)
    ramp = 0.1  # seconds of linear attack/release at each note boundary
    samples = [0.0] * n
    for i in range(n):
        t = i / SAMPLE_RATE
        chord_idx = min(int(t / chord_dur), len(CHORD_PROGRESSION_HZ) - 1)
        t_rel = t - chord_idx * chord_dur
        env = min(1.0, t_rel / ramp, (chord_dur - t_rel) / ramp)
        val = sum(0.5 * math.sin(2 * math.pi * f * t) + 0.15 * math.sin(2 * math.pi * 2 * f * t)
                  for f in CHORD_PROGRESSION_HZ[chord_idx])
        samples[i] = (val / len(CHORD_PROGRESSION_HZ[chord_idx])) * env * 0.6

    # Percussion: short decaying noise bursts on a steady 120bpm eighth-note
    # beat, added on top of the chord bed.
    beat = 60.0 / 120 / 2
    burst_len = int(0.05 * SAMPLE_RATE)
    t_beat = 0.0
    while t_beat < MUSIC_CLIP_SECONDS:
        start = int(t_beat * SAMPLE_RATE)
        for j in range(min(burst_len, n - start)):
            samples[start + j] += 0.35 * rng.uniform(-1.0, 1.0) * (1.0 - j / burst_len)
        t_beat += beat
    return samples


def generate_clips(outdir: Path) -> dict[str, tuple[Path, str]]:
    """{name: (path, gate-intent note)}. Intent, not a guarantee -- only a
    live YAMNet inference (CI) says which AudioSet classes a clip lands on;
    see the parity report for the measured outcome."""
    outdir.mkdir(parents=True, exist_ok=True)
    n = int(SAMPLE_RATE * CLIP_SECONDS)
    rng = random.Random(1041)  # nosec - deterministic synthetic signal, not a secret
    tone = [0.6 * math.sin(2 * math.pi * 440.0 * i / SAMPLE_RATE) for i in range(n)]
    noise = [rng.uniform(-0.5, 0.5) for _ in range(n)]
    specs: dict[str, tuple[list[float], str]] = {
        "silence": ([0.0] * n, "negative control: music gate expected false"),
        "tone": (tone, "pure 440Hz sine: intended to trip the music gate true"),
        "noise": (noise, "broadband noise: second music-gate-false control"),
        "tone_noise": ([0.5 * t + 0.15 * ns for t, ns in zip(tone, noise)], "tone+noise mix: near the music-gate edge"),
        "chord_percussion": (_chord_percussion_clip(rng), "chord progression + percussion: designed to approach the music gate"),
    }
    out: dict[str, tuple[Path, str]] = {}
    for name, (samples, note) in specs.items():
        path = outdir / f"{name}.wav"
        _write_wav(path, samples)
        out[name] = (path, note)
    return out


def score_clip(base_url: str, wav_path: Path) -> dict:
    result = subprocess.run(
        ["curl", "-fsS", "-X", "POST", f"{base_url}/classify",
         "-F", f"file=@{wav_path};type=audio/wav"],
        capture_output=True, text=True, check=True, timeout=60,
    )
    return json.loads(result.stdout)


def fetch_health(base_url: str) -> dict:
    result = subprocess.run(
        ["curl", "-fsS", f"{base_url}/health"],
        capture_output=True, text=True, check=True, timeout=30,
    )
    return json.loads(result.stdout)


def validate_scores(resp: dict, side: str, clip: str, expected_classes: int) -> list[str]:
    """Validate one /classify response before it feeds a gate decision.

    Mirrors what HTTPDetector.Detect (internal/detector/http.go) requires:
    'mean' and 'max' both present and non-empty (maxAvailable), an identical
    key set between them, the model's full class count (from /health, never
    hard-coded -- see app.py's _load_class_names), every configured
    music/vocal/speech class present (Go's baselineComplete guard: a class
    missing from a non-empty max map forces not-instrumental, so comparing
    over an incomplete set proves nothing), and every value finite and in
    [0, 1] (a NaN never wins max()/sum(), and Go's summed gates would
    silently misbehave the same way).

    Returns human-readable error strings (empty when valid), each naming the
    side (base/head), the clip, and the offending key.
    """
    prefix = f"{side} response for clip '{clip}'"
    if not isinstance(resp, dict):
        return [f"{prefix}: response is not a JSON object"]

    errors: list[str] = []
    for field in ("mean", "max"):
        if field not in resp or not isinstance(resp[field], dict):
            errors.append(f"{prefix}: missing or non-object '{field}' field")
    if errors:
        return errors  # nothing further can be checked without both maps

    mean, max_ = resp["mean"], resp["max"]
    if not mean:
        errors.append(f"{prefix}: 'mean' map is empty")
    if not max_:
        errors.append(f"{prefix}: 'max' map is empty")
    if len(mean) != expected_classes:
        errors.append(f"{prefix}: 'mean' has {len(mean)} classes, expected {expected_classes} (per /health)")
    if len(max_) != expected_classes:
        errors.append(f"{prefix}: 'max' has {len(max_)} classes, expected {expected_classes} (per /health)")

    missing_from_max = set(mean) - set(max_)
    missing_from_mean = set(max_) - set(mean)
    if missing_from_max:
        errors.append(f"{prefix}: keys in 'mean' but missing from 'max': {sorted(missing_from_max)}")
    if missing_from_mean:
        errors.append(f"{prefix}: keys in 'max' but missing from 'mean': {sorted(missing_from_mean)}")

    for cls in MUSIC_CLASSES + SPEECH_CLASSES:
        if cls not in mean:
            errors.append(f"{prefix}: music/speech class '{cls}' missing from 'mean'")
    for cls in VOCAL_CLASSES:
        if cls not in max_:
            errors.append(f"{prefix}: vocal class '{cls}' missing from 'max' (Go's baselineComplete would force not-instrumental)")

    for field_name, m in (("mean", mean), ("max", max_)):
        for k, v in m.items():
            if not isinstance(v, (int, float)) or isinstance(v, bool) or not math.isfinite(v):
                errors.append(f"{prefix}: {field_name}['{k}'] is not a finite number ({v!r})")
            elif not (0.0 <= v <= 1.0):
                errors.append(f"{prefix}: {field_name}['{k}'] = {v} is outside [0, 1]")

    return errors


def gate_scores(resp: dict) -> dict:
    """Reapply detector.Instrumental's three gates to one response,
    individually -- never collapsed to a single bool (a caller that only
    compares the final instrumental value cannot tell a moved music_sum from
    a moved vocal_peak, and all four synthetic clips besides chord_percussion
    are expected to land not-instrumental on every gate, which would make a
    bool-only comparison trivially true)."""
    music_sum = sum(resp["mean"].get(c, 0.0) for c in MUSIC_CLASSES)
    vocal_peak = max((resp["max"].get(c, 0.0) for c in VOCAL_CLASSES), default=0.0)
    speech_mean = sum(resp["mean"].get(c, 0.0) for c in SPEECH_CLASSES)
    return {
        "music_sum": music_sum,
        "vocal_peak": vocal_peak,
        "speech_mean": speech_mean,
        "music_gate": music_sum >= MUSIC_MIN_CONFIDENCE,
        "vocal_gate": vocal_peak < VOCAL_MAX_CONFIDENCE,
        "speech_gate": speech_mean < SPEECH_MAX_CONFIDENCE,
    }


def check_key_parity(base: dict, head: dict, clip: str) -> list[str]:
    """Require base and head to report the IDENTICAL key set in both 'mean'
    and 'max' for a clip. Without this, max_abs_diff's old `.get(k, 0.0)`
    fallback treated a key present on only one side as score 0 on the other,
    so a class the head model dropped (or gained) compared zero drift instead
    of failing -- the exact class-set drift this check exists to catch.
    Returns human-readable error strings (empty when the key sets match)."""
    errors: list[str] = []
    for field in ("mean", "max"):
        base_keys = set(base.get(field, {}))
        head_keys = set(head.get(field, {}))
        only_base = base_keys - head_keys
        only_head = head_keys - base_keys
        if only_base:
            errors.append(f"clip '{clip}': '{field}' keys present in base but missing from head: {sorted(only_base)}")
        if only_head:
            errors.append(f"clip '{clip}': '{field}' keys present in head but missing from base: {sorted(only_head)}")
    return errors


def max_abs_diff(base: dict, head: dict) -> float:
    """Assumes base and head already have identical key sets in both fields
    (enforced by check_key_parity before this is called) -- no `.get(k, 0.0)`
    fallback, so a genuinely missing key raises instead of silently scoring
    as a zero-diff match."""
    worst = 0.0
    for field in ("mean", "max"):
        keys = set(base[field]) | set(head[field])
        worst = max(worst, max((abs(base[field][k] - head[field][k]) for k in keys), default=0.0))
    return worst


def health_class_count_errors(base_classes: int, head_classes: int) -> list[str]:
    """base and head must report the SAME /health class count, or every
    per-clip key-set check downstream is comparing two differently-shaped
    models and every score diff is meaningless. A pure helper so --selftest
    can exercise it without a running server."""
    if base_classes != head_classes:
        return [
            f"class count mismatch: base /health reports {base_classes} classes, head reports {head_classes}"
        ]
    return []


def compare(name: str, note: str, base: dict, head: dict, tolerance: float) -> dict:
    base_g, head_g = gate_scores(base), gate_scores(head)
    diff = max_abs_diff(base, head)
    gate_mismatches = [g for g in ("music_gate", "vocal_gate", "speech_gate") if base_g[g] != head_g[g]]
    base_instrumental = base_g["music_gate"] and base_g["vocal_gate"] and base_g["speech_gate"]
    head_instrumental = head_g["music_gate"] and head_g["vocal_gate"] and head_g["speech_gate"]
    return {
        "clip": name, "note": note, "diff": diff,
        "base": base_g, "head": head_g,
        "gate_mismatches": gate_mismatches,
        "ok": not gate_mismatches and diff <= tolerance,
        "base_instrumental": base_instrumental, "head_instrumental": head_instrumental,
    }


def evaluate_clip(
    name: str, note: str, base_resp: dict, head_resp: dict,
    base_classes: int, head_classes: int, tolerance: float,
) -> tuple[list[str], dict | None]:
    """One clip's full validate -> key-parity -> compare pipeline, shared by
    main()'s per-clip loop and --selftest so both exercise the exact same
    skip-on-invalid logic (finding #3, Copilot 4087340486): a malformed or
    key-set-mismatched response returns its errors and a None row rather than
    being handed to compare()/max_abs_diff(), which would otherwise
    KeyError/TypeError on it instead of producing the intended validation
    failure. Returns (errors, row); row is None whenever errors is non-empty.
    """
    errors = validate_scores(base_resp, "base", name, base_classes)
    errors += validate_scores(head_resp, "head", name, head_classes)
    if not errors:
        errors += check_key_parity(base_resp, head_resp, name)
    if errors:
        return errors, None
    return errors, compare(name, note, base_resp, head_resp, tolerance)


def _margin(value: float, threshold: float, sense: str) -> str:
    """Signed distance of `value` from `threshold`, positive meaning "on the
    instrumental-favoring side of the gate". sense='min' is the music gate
    (value >= threshold passes); sense='max' is vocal/speech (value <
    threshold passes)."""
    return f"{value - threshold:+.4f}" if sense == "min" else f"{threshold - value:+.4f}"


def render_table(rows: list[dict], tolerance: float) -> str:
    header = (
        "| Clip | Gate intent | music_sum base/head (margin@>=0.90) | vocal_peak base/head (margin@<0.015) | "
        "speech_mean base/head (margin@<0.20) | Gates match | Base instr | Head instr | Max \\|diff\\| | Result |"
    )
    lines = [header, "|---|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        b, h = r["base"], r["head"]
        result = "PASS" if r["ok"] else "**FAIL**"
        gates_match = "yes" if not r["gate_mismatches"] else f"**NO** ({', '.join(r['gate_mismatches'])})"
        lines.append(
            f"| {r['clip']} | {r['note']} "
            f"| {b['music_sum']:.4f}/{h['music_sum']:.4f} ({_margin(h['music_sum'], MUSIC_MIN_CONFIDENCE, 'min')}) "
            f"| {b['vocal_peak']:.4f}/{h['vocal_peak']:.4f} ({_margin(h['vocal_peak'], VOCAL_MAX_CONFIDENCE, 'max')}) "
            f"| {b['speech_mean']:.4f}/{h['speech_mean']:.4f} ({_margin(h['speech_mean'], SPEECH_MAX_CONFIDENCE, 'max')}) "
            f"| {gates_match} | {r['base_instrumental']} | {r['head_instrumental']} | {r['diff']:.6f} | {result} |"
        )
    lines.append(
        f"\nTolerance: {tolerance} absolute on 0..1 scores. Margin is the HEAD value vs its threshold: "
        "positive means past the gate toward instrumental (music_sum) or safely clear of it (vocal_peak/speech_mean)."
    )
    return "\n".join(lines)


def run_selftest() -> int:
    """Canned-JSON unit checks, no docker/server/TensorFlow needed --
    `python3 parity.py --selftest`. Covers validate_scores() (one well-formed
    response plus one case per required failure mode: missing key, empty
    maps, NaN, out-of-range, missing max class), check_key_parity() (base and
    head each with their own zero-scored extra key), health_class_count_errors()
    (mismatched and matching /health class counts), and evaluate_clip() on a
    malformed response (missing 'max' entirely), which must produce a
    validation failure and skip compare() rather than raising."""
    # A small closed class set (the three gates' classes plus two fillers),
    # so the expected-class-count check has something to under/overcount
    # against.
    classes = sorted(set(MUSIC_CLASSES + VOCAL_CLASSES + SPEECH_CLASSES + ["Filler A", "Filler B"]))
    n = len(classes)

    def good() -> dict:
        return {"mean": {c: 0.01 for c in classes}, "max": {c: 0.01 for c in classes}}

    missing_key = good()
    del missing_key["mean"]
    nan_value = good()
    nan_value["mean"][MUSIC_CLASSES[0]] = float("nan")
    out_of_range = good()
    out_of_range["max"][VOCAL_CLASSES[0]] = 1.5
    missing_max_class = good()
    del missing_max_class["max"][VOCAL_CLASSES[0]]

    # (description, response, want_errors)
    cases: list[tuple[str, dict, bool]] = [
        ("well-formed response", good(), False),
        ("missing 'mean' key", missing_key, True),
        ("empty mean/max maps", {"mean": {}, "max": {}}, True),
        ("NaN score", nan_value, True),
        ("out-of-range score (1.5)", out_of_range, True),
        ("vocal class missing from 'max'", missing_max_class, True),
    ]

    failures: list[str] = []
    for desc, resp, want_errors in cases:
        errors = validate_scores(resp, "selftest", desc, n)
        if bool(errors) != want_errors:
            failures.append(f"{desc}: expected errors={want_errors}, got {errors!r}")
            continue
        print(f"selftest OK: {desc} -> {'; '.join(errors) if errors else 'no errors'}")

    # check_key_parity: base and head each carry their OWN extra key, both
    # scored 0.0 -- the exact shape the old `.get(k, 0.0)` fallback in
    # max_abs_diff would have scored as zero drift instead of flagging
    # (finding #2, CodeRabbit 4088005953 / Copilot 4087340419).
    base_extra_key = good()
    base_extra_key["mean"]["Only In Base"] = 0.0
    base_extra_key["max"]["Only In Base"] = 0.0
    head_extra_key = good()
    head_extra_key["mean"]["Only In Head"] = 0.0
    head_extra_key["max"]["Only In Head"] = 0.0
    key_parity_errors = check_key_parity(base_extra_key, head_extra_key, "selftest-key-mismatch")
    if not key_parity_errors:
        failures.append("mismatched key sets: expected check_key_parity to report errors, got none")
    else:
        print(f"selftest OK: mismatched key sets -> {'; '.join(key_parity_errors)}")

    # health_class_count_errors: base and head disagree on /health's class
    # count -- must fail loudly rather than silently comparing two
    # differently-shaped models.
    class_count_errors = health_class_count_errors(n, n + 1)
    if not class_count_errors:
        failures.append("mismatched class counts: expected health_class_count_errors to report errors, got none")
    else:
        print(f"selftest OK: mismatched class counts -> {'; '.join(class_count_errors)}")
    no_class_count_errors = health_class_count_errors(n, n)
    if no_class_count_errors:
        failures.append(f"matching class counts: expected no errors, got {no_class_count_errors!r}")
    else:
        print("selftest OK: matching class counts -> no errors")

    # evaluate_clip on a malformed response (missing 'max' entirely) must
    # produce a validation failure and a None row -- never let compare()/
    # max_abs_diff() run on it and raise KeyError/TypeError (finding #3,
    # Copilot 4087340486). The bug this guards against would surface here as
    # an uncaught exception, not a wrong return value, so the try/except is
    # deliberate: catching it as a self-test failure produces a readable
    # selftest FAILED line instead of the traceback the fix exists to avoid.
    malformed = {"mean": good()["mean"]}  # no 'max' field at all
    try:
        clip_errors, row = evaluate_clip(
            "selftest-malformed", "malformed response (missing 'max')",
            malformed, good(), n, n, DEFAULT_TOLERANCE,
        )
    except Exception as exc:  # noqa: BLE001 - this IS the failure mode under test
        failures.append(f"malformed response: evaluate_clip raised {exc!r} instead of returning a validation failure")
    else:
        if not clip_errors or row is not None:
            failures.append(f"malformed response: expected (errors, None), got ({clip_errors!r}, {row!r})")
        else:
            print(f"selftest OK: malformed response -> validation failure, no traceback ({'; '.join(clip_errors)})")

    if failures:
        print("\nselftest FAILED:\n" + "\n".join(f"  - {f}" for f in failures), file=sys.stderr)
        return 1
    print("\nselftest OK: all canned-JSON validate_scores() checks behaved as expected")
    return 0


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--base-url")
    p.add_argument("--head-url")
    p.add_argument("--tolerance", type=float, default=DEFAULT_TOLERANCE)
    p.add_argument("--clips-dir", default="/tmp/yamnet-parity-clips")  # noqa: S108 - CI-only scratch dir
    p.add_argument("--selftest", action="store_true",
                    help="run canned-JSON validate_scores() unit checks and exit; no server/docker/TensorFlow needed")
    args = p.parse_args()

    if args.selftest:
        return run_selftest()
    if not args.base_url or not args.head_url:
        p.error("--base-url and --head-url are required unless --selftest is given")

    base_health = fetch_health(args.base_url)
    head_health = fetch_health(args.head_url)
    base_classes, head_classes = base_health.get("classes"), head_health.get("classes")
    for side, classes in (("base", base_classes), ("head", head_classes)):
        if not isinstance(classes, int) or isinstance(classes, bool) or classes <= 0:
            print(f"{side} /health reports an invalid 'classes' count: {classes!r}", file=sys.stderr)
            return 1
    class_count_errors = health_class_count_errors(base_classes, head_classes)
    if class_count_errors:
        for e in class_count_errors:
            print(e, file=sys.stderr)
        return 1

    clips = generate_clips(Path(args.clips_dir))
    rows: list[dict] = []
    validation_errors: list[str] = []
    for name, (path, note) in clips.items():
        base_resp, head_resp = score_clip(args.base_url, path), score_clip(args.head_url, path)
        clip_errors, row = evaluate_clip(
            name, note, base_resp, head_resp, base_classes, head_classes, args.tolerance,
        )
        validation_errors += clip_errors
        if row is not None:
            rows.append(row)

    table = render_table(rows, args.tolerance)
    print(table)
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary_path:
        with open(summary_path, "a", encoding="utf-8") as f:
            f.write("\n## YAMNet score parity (base vs head)\n\n" + table + "\n")
            if validation_errors:
                f.write("\n### Response validation failures\n\n" + "\n".join(f"- {e}" for e in validation_errors) + "\n")

    if validation_errors:
        print("\nresponse validation FAILED:", file=sys.stderr)
        for e in validation_errors:
            print(f"  - {e}", file=sys.stderr)

    if validation_errors or any(not r["ok"] for r in rows):
        print("\nparity FAILED: a response failed validation, a gate decision changed, or a score moved beyond tolerance", file=sys.stderr)
        return 1
    print("\nparity OK: every response validated, every gate decision matched, every score within tolerance")
    return 0


if __name__ == "__main__":
    sys.exit(main())
