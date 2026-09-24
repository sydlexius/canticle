# Canticle forced-alignment sidecar

A thin FastAPI service that forced-aligns lyric text Canticle already has to
the user's own audio, using Demucs vocal separation followed by a
faster-whisper transcript and a direct wav2vec2 forced alignment. Slice 1 of
epic [#482](https://github.com/sydlexius/canticle/issues/482) (word-sync
generation); implements issue
[#1005](https://github.com/sydlexius/canticle/issues/1005). The forced-
alignment stack dropped whisperx in favor of calling faster-whisper and the
wav2vec2 models directly: issue
[#1016](https://github.com/sydlexius/canticle/issues/1016).

This sidecar **never returns a blind transcription as lyrics**. The `words`
array always comes from aligning the CALLER-SUPPLIED lyric lines to the
separated vocal stem; the independent ASR transcript it also returns exists
only so Canticle can run its own cheap content gate
(`verification.Similarity`, see epic #482's 2026-07-23 design refinement)
without standing up a second sidecar.

## Contract

### `POST /align`

`multipart/form-data`:

| field | required | type | notes |
|---|---|---|---|
| `file` | yes | binary | audio, any container ffmpeg can decode |
| `lyrics` | yes | text (UTF-8) | one lyric line per newline |
| `language` | no | text | ISO 639-1 code; defaults to `ALIGNER_ALIGN_LANGUAGE`. Must be a language this sidecar has a default align model for (`ALIGN_MODELS` in `app.py`), else `400` |

`lyrics` is split on `\n` only (a trailing `\r` is stripped), so a line
means the same thing to the caller and the sidecar. Blank lines in `lyrics` are dropped and do **not** consume a `line_index` --
the index counts only non-blank lines, 0-based, in the order supplied.

Success (`200`):

```json
{
  "words": [
    {"text": "first", "start_ms": 1234, "end_ms": 1400, "line_index": 0, "confidence": 0.91}
  ],
  "transcript": "first line second line ..."
}
```

- `words`: ordered by `start_ms` ascending. Words follow the supplied line
  order and never overlap: each word's `end_ms` is at or before the next
  word's `start_ms`, so every word of line N+1 starts at or after the last
  word of line N ends, and a repeated line (a chorus) gets its own later
  span. A word ends when its last character does, not at the next word.
- A word with no character the alignment model knows (e.g. `42`, `---`) is
  omitted rather than given a fabricated span. If NO word has one, the
  response is still `200`, with `words: []`.
- `start_ms` / `end_ms`: integer milliseconds from the start of the audio.
- `line_index`: 0-based index into the caller's `lyrics` lines (after
  blank-line filtering), so the caller re-attaches each word to its line.
- `confidence`: float `0.0`-`1.0`, the mean of the word's per-character
  scores from `torchaudio.functional.merge_tokens`, rounded to 3 places.
- `transcript`: the raw ASR transcript of the separated vocal stem --
  **never** the lyric text Canticle sent -- meant only as a content-gate
  input (`verification.Similarity(transcript, lyrics)` on the Canticle side
  answers "is this even the right song?" before trusting `words`).

Errors:

| status | when |
|---|---|
| `400` | empty audio, empty `lyrics` after blank-line filtering, unsupported `language`, audio ffmpeg rejects as undecodable (or decodes to nothing), or audio too short to hold the supplied lyrics |
| `411` | the `/align` POST has no valid `Content-Length` (e.g. a chunked upload): Starlette would otherwise spool the uncapped file part to disk before any size check. Canticle's client always sends one |
| `413` | audio exceeds `ALIGNER_MAX_AUDIO_BYTES`, its decoded duration exceeds `ALIGNER_MAX_AUDIO_SECONDS`, `lyrics` exceeds `ALIGNER_MAX_LYRICS_BYTES`, or the declared `Content-Length` exceeds their sum (rejected before the body is parsed) |
| `422` | FastAPI's own required-field validation (`file` or `lyrics` entirely absent) |
| `429` | `ALIGNER_MAX_PENDING` `/align` requests are already admitted (running or queued); sent with `Retry-After: 30`. Checked before the body is parsed. A 4xx, so it never counts toward the Canticle client's breaker |
| `500` | any server-side fault: model load, separation/transcription/alignment, or a broken environment (e.g. ffmpeg missing, killed, or still decoding after `ALIGNER_DECODE_TIMEOUT_SECONDS`). Never a 400, since the Canticle client treats 4xx as a benign miss that does not trip its breaker. The body is a fixed `detail` (`alignment failed`, `model layer not initialized`), never the exception message or any lyric text |

Requests are processed ONE AT A TIME. Up to `ALIGNER_MAX_PENDING` (default 2:
one running, one queued) are admitted; an admitted request waits for the
pipeline rather than being refused (a 503 would count against the Canticle
client's breaker), and one beyond that gets `429` before its body is read, so
a burst cannot pile uploads up in memory. `/health` is never gated and
answers while an `/align` is running.

A decode timeout is a `500`, not a `413`: the decode is already capped at
`ALIGNER_MAX_AUDIO_SECONDS` of output (`ffmpeg -t`), which a healthy host
decodes in seconds, so running past the timeout says the server is starved
or wedged, not that the caller's file is too big.

The upload reaches ffmpeg on stdin, never by the client's filename, with
every protocol but `fd` disallowed, so a playlist-style upload (HLS, concat)
cannot make ffmpeg open a local file or a URL. `smoke_decode.py` asserts
this against the image's own ffmpeg at build time (see "Image smoke checks").

No lyric text is ever logged -- only counts (audio bytes, line count,
transcript length). A pipeline failure logs only the exception class at
`ERROR`; `DEBUG` (`ALIGNER_LOG_LEVEL=DEBUG`) adds the traceback FRAMES, never
the exception message (which can quote a lyric). A failed decode logs
ffmpeg's exit code and the last 400 bytes of its stderr at `WARNING`,
with control characters stripped, and never returns them to the client. Lyrics and titles are private per Canticle's
data-handling policy.

### `GET /health`

```json
{"status": "ok", "device": "cpu", "separation_model": "htdemucs", "align_model": "WAV2VEC2_ASR_BASE_960H", "whisper_model": "base"}
```

`device` reports what was actually selected at startup, not merely the
configured override. `align_model` is the wav2vec2 model used for the
default `ALIGNER_ALIGN_LANGUAGE`; a request with another `language` uses that
language's model. This is a **liveness** check only: models load lazily
on the first `/align` (downloading weights into `/data` on a cold volume),
so `ok` does not mean a model is loaded, and the first request is slow.

### Determinism

Separation runs with Demucs `shifts=0` (the default applies a random time
offset), so a repeated identical request is expected to return identical
`words` on the same image and hardware. `transcript` comes from Whisper
decoding and is not guaranteed to repeat exactly. Callers and tests should still not
depend on bit-identical timings across hardware or library versions.

## Pipeline

1. **Vocal separation** (Demucs, `htdemucs` by default) isolates the singing
   voice from the full mix.
2. **Transcription** (faster-whisper) runs over the separated vocal stem to
   produce `transcript` (the content-gate input).
3. **Forced alignment** (a wav2vec2-CTC alignment model loaded directly via
   torchaudio/transformers, *not* the Whisper decoder) aligns the
   CALLER-SUPPLIED `lyrics` lines to the same vocal stem, producing `words`.
   All lines are aligned as ONE character sequence
   (`torchaudio.functional.forced_align`) and the words mapped back to their
   line indices, which is what makes the output ordered and non-overlapping.

Both 2 and 3 run against the separated vocal stem, not the raw mix -- forced
alignment against an unseparated mix is far less reliable on
instrumentation-heavy tracks.

### Why Demucs, not UVR-Karaoke

The reference implementation this sidecar is informed by,
[rzru/nightingale](https://github.com/rzru/nightingale) (GPL-3.0-or-later per
its own repository), defaults to UVR-Karaoke for separation. This sidecar
makes the opposite default choice for packaging reasons, not a measured
quality difference: Demucs ships as a proper pip package with one canonical,
actively maintained upstream (Meta/`facebookresearch`) and a stable,
versioned pretrained-model release. UVR-Karaoke models are loose community
ONNX/PyTorch checkpoints with no single canonical packaging or
checksum-pinnable source -- the kind of unpinnable dependency
`deploy/yamnet-detector`'s checksum-bake discipline (issue #491) exists to
avoid. Any isolation-quality edge also matters here only insofar as it
changes alignment accuracy, not playback quality, so the bar is lower than a
karaoke product's. Reversible: the `Separator` seam in `app.py` is a narrow
protocol (`separate(path) -> vocal_stem_path`), so a UVR-Karaoke backend can
be swapped in later behind the same seam.

## Model / device selection

| variable | default | notes |
|---|---|---|
| `ALIGNER_DEVICE` | `auto` | `auto`\|`cpu`\|`cuda`\|`mps`. `auto` prefers CUDA, then Apple MPS, then CPU. An explicit override always wins, even against unavailable hardware (fails loudly at model load rather than silently falling back). An unrecognized value falls back to `cpu`, never to what auto-detection would have picked. |
| `ALIGNER_SEPARATION_MODEL` | `htdemucs` | Demucs model name |
| `ALIGNER_WHISPER_MODEL` | `base` | Whisper model size for the transcript pass. On `mps` the Whisper pass runs on CPU (faster-whisper's CTranslate2 backend has no MPS device); separation and alignment stay on MPS |
| `ALIGNER_ALIGN_LANGUAGE` | `en` | default alignment language when a request omits `language`. Startup FAILS if `app.ALIGN_MODELS` has no entry for it. ONE align model is kept loaded (~0.36-1.2 GB each); a request in another language replaces it, paying one model load per language switch |
| `ALIGNER_MAX_AUDIO_BYTES` | `104857600` (100 MiB) | audio upload size ceiling |
| `ALIGNER_MAX_AUDIO_SECONDS` | `1200` (20 min) | decoded-duration ceiling; longer audio is `413` |
| `ALIGNER_DECODE_TIMEOUT_SECONDS` | `300` | wall-clock limit on one ffmpeg decode; exceeding it is `500` |
| `ALIGNER_MAX_LYRICS_BYTES` | `65536` (64 KiB) | `lyrics` field size ceiling; values above 1 MiB (the multipart parser's per-field cap) are clamped to 1 MiB |
| `ALIGNER_MAX_PENDING` | `2` | `/align` requests admitted at once (one running, the rest queued); one more gets `429` |
| `ALIGNER_LOG_LEVEL` | `INFO` | level for the `canticle.*` loggers (per-request INFO lines go to stderr) |

GPU support: this image is **CPU-only**. The Dockerfile installs from one of
`requirements-linux-amd64.txt` / `requirements-linux-arm64.txt` (picked by
`TARGETARCH`, #1017), whose `torch`/`torchaudio` entries (pinned to 2.8.0,
see `requirements.in`) resolve against the PyTorch CPU index
(`https://download.pytorch.org/whl/cpu`, passed as `--extra-index-url`),
because PyPI's x86_64 `torch` wheel is the CUDA build and would add ~5 GB of
unused `nvidia-*-cu12` libraries. A build-time check fails the build if a
CUDA `torch`, any `nvidia-*` package, or `triton` is present. The CUDA
variant is issue [#1013](https://github.com/sydlexius/canticle/issues/1013)'s,
not this image's. `ALIGNER_DEVICE` and the `select_device` seam are already
GPU-ready; only the installed `torch` wheel and base image need to change.

### amd64: ctranslate2's executable-stack flag (historical, now a no-op guard)

whisperx 3.4.3 pinned `ctranslate2<4.5.0` (resolved 4.4.0), whose x86_64
wheel bundled `libctranslate2-*.so` with an executable-stack (`PT_GNU_STACK`
RWE) header. The base image's glibc (2.41, Debian trixie) refuses to load
such a library, so without intervention `import ctranslate2` failed and
every `/align` returned 500 on amd64 (arm64's build of the library was
unaffected).

Dropping whisperx (#1016) lets faster-whisper resolve its own ctranslate2
unconstrained by whisperx's `<4.5.0` pin; it resolves 4.8.2, whose bundled
library already ships with the executable-stack flag clear on **both**
arches -- verified at build time (see the Dockerfile's `patchelf
--print-execstack` before/after log): `execstack: -` before the patch runs,
on amd64 as well as arm64. The `patchelf --clear-execstack` step is KEPT
rather than removed, now as a guard against a future downgrade or pin
reintroducing the flag: it fails the build loudly (`FATAL: execstack still
set on $lib`) rather than shipping a silently-broken amd64 image, and its
cost when the flag is already clear is a few hundred milliseconds. A build-time
`import ctranslate2, faster_whisper, ...` smoke check then fails the build if
the ML stack cannot load, so this class cannot ship behind a green `/health`.

### Image smoke checks

Two build-time `RUN` steps fail the build rather than ship a broken image:

1. The ML-stack import check above (plus the CPU-only assertion, and that
   every `app.ALIGN_MODELS` entry resolves to a real `torchaudio.pipelines`
   bundle or looks like a Hugging Face repo id).
2. `smoke_decode.py`, the decode hardening against the image's OWN ffmpeg
   (the CI test job uses the runner's apt ffmpeg, a different build). It
   builds a synthetic 1 s tone, wraps it in a concat playlist whose entry is
   a `data:` URL, and runs `decode_pcm`'s exact argv with `-f concat -safe 0`
   forced before `-i`: that must decode 0 bytes. The same forced argv
   without `-protocol_whitelist fd` must decode audio (proving the fixture is
   a live attack), and a plain WAV must decode (positive control).

Rerun the decode check against a built image with
`docker run --rm <image> python smoke_decode.py`.

## Build

```bash
docker build --platform linux/amd64 -t canticle-aligner deploy/aligner
```

Both `linux/amd64` and `linux/arm64` build. No workflow builds or publishes
this image yet: the GHCR publish workflow and the CUDA variant are #1013.

## Opt-in, dark by default

No enable/disable flag lives here -- gating is on the Canticle side
(`[word_sync_generate]` config, slice 2/#1006). This sidecar is never called
unless an operator has explicitly configured a base URL for it.

## Licensing and provenance

Licensed GPL-3.0, matching the rest of Canticle (`NOTICE`/`LICENSE`). Model
choices (Demucs, faster-whisper + a direct wav2vec2 forced alignment) are
informed by
[rzru/nightingale](https://github.com/rzru/nightingale) as a reference --
its repository states **GPL-3.0-or-later** (confirmed by inspection at
authoring time, 2026-09-18), license-compatible with Canticle's GPL-3.0.
This sidecar is an independent, from-scratch implementation and vendors no
Nightingale source -- only the choice of libraries and the general pipeline
shape (separate, then forced-align).

## Volumes

Model weights cache under `/data` (`HF_HOME`/`TORCH_HOME`). The container
runs as uid 1000, so `/data` must be writable by uid 1000. An Unraid appdata
share owned by `99:100` (nobody:users) is not: `chown -R 1000:1000` the host
directory, or run the container with `--user 99:100`.

## Test

The test suite needs only the lightweight test dependencies, no
`torch`/`demucs`/`faster-whisper`/`transformers`:

```bash
# from the repo root, with ffmpeg on PATH
pip install --require-hashes -r deploy/aligner/requirements-test.txt
CI=1 pytest deploy/aligner -q
```

CI's `aligner-test` job runs the same commands after apt-installing ffmpeg.
The tests that drive a real ffmpeg (the playlist-upload security tests) skip
when `ffmpeg` is not on `PATH`, but FAIL when `CI` is set, so CI can never
pass them by skipping. Setting `CI=1` locally gives the same guarantee.

`app.py` never imports `torch`/`torchaudio`/`demucs`/`faster_whisper`/
`transformers` at module scope -- those imports live inside
`_build_separator`/`_build_aligner`/`lifespan()`, which only run when the
real server boots. `test_app.py` imports `app` directly and constructs
`TestClient(appmod.app)` **without** a `with` block (so `lifespan` never
runs) after stubbing `appmod._state["separator"]`/`["aligner"]` -- the same
pattern `deploy/yamnet-detector/test_app.py` uses to stub the YAMNet model.
`_aligner_models.py` (the real Demucs/faster-whisper/wav2vec2
implementations) is exercised by the suite only for its model-load locking
(with the ML packages faked); inference runs only in a container.

## Dependency lock: one file per architecture

Every installed package -- not just the 8 top-level ones in
`requirements.in` -- is pinned by exact version and sha256 hash for BOTH
`linux/amd64` and `linux/arm64` (issue
[#1017](https://github.com/sydlexius/canticle/issues/1017)), the same
`uv pip compile --generate-hashes` approach
`deploy/yamnet-detector/requirements.txt` uses, with one difference: this
sidecar locks **per architecture** rather than one portable file.
`requirements-linux-amd64.txt` and `requirements-linux-arm64.txt` are that
pair; the Dockerfile picks between them with `TARGETARCH` (set automatically
by buildx) and installs with `--require-hashes`.

**Why per-architecture, unlike yamnet's single lock:** `torch`/`torchaudio`
resolve against the PyTorch CPU index
(`https://download.pytorch.org/whl/cpu`) to their arch-specific `+cpu`
wheels, and a hash lock only covers the wheel `uv` actually resolved for the
platform it was compiled against -- not every wheel the same version could
produce elsewhere. Everything else in the closure (`demucs`'s tree,
`faster-whisper`, `transformers`, ...) is pure-Python or ships a manylinux
wheel on both arches, so the two files are identical except for hashes and
the `torch`/`torchaudio` build tag; only a native-extension package can
legitimately differ further. yamnet has no such package, hence its one
portable file.

To regenerate BOTH lock files after editing `requirements.in` (never edit
`requirements-linux-*.txt` by hand -- always regenerate both together, since
a stale lock on only one arch is exactly the drift #1017 closes out).
`--exclude-newer` pins resolution to a fixed point in time so the run is
reproducible (see "Reproducibility" below) -- update the date to today (UTC)
when you actually intend to pick up newer releases, and record the new date
in both files' headers:

```bash
cd deploy/aligner
for arch in amd64 arm64; do
  case "$arch" in
    amd64) platform=x86_64-manylinux_2_28 ;;
    arm64) platform=aarch64-manylinux_2_28 ;;
  esac
  docker run --rm --platform linux/amd64 -v "$PWD":/w -w /w python:3.13-slim \
    bash -c "pip install -q uv==0.9.7 && uv pip compile --generate-hashes \
      --python-version 3.13 --python-platform $platform \
      --extra-index-url https://download.pytorch.org/whl/cpu \
      --index-strategy unsafe-best-match --no-header --no-emit-index-url \
      --exclude-newer 2026-09-23T12:00:00Z \
      requirements.in -o requirements-linux-$arch.txt"
done
```

(`--platform linux/amd64` on `docker run` is the HOST container running the
resolver, not the target -- `uv`'s `--python-platform` cross-resolves
without needing to execute on that architecture, which is also how CI, an
`ubuntu-latest` amd64 runner, can regenerate the arm64 lock.
`--index-strategy unsafe-best-match` is required: without it `uv` refuses to
mix `torch`/`torchaudio` from the CPU index with every other package from
PyPI's default index.) Re-add each file's header block by hand afterward
(`uv --no-header` strips it).

`requirements-test.txt` is hash-pinned the same way, but portable (a single
file, no `--python-platform`): it carries `fastapi` and `python-multipart`
(the HTTP layer the tests drive) at the versions `requirements.in` pins --
`requirements-test.in` lists them unpinned under `-c requirements.in`, so the
runtime file stays their one pin home. A constraint pins only what is
requested, so the test lock never pulls in
`torch`/`demucs`/`faster-whisper`/`transformers`. Regenerate it after
changing either file (same `--exclude-newer` reproducibility note applies):

```bash
cd deploy/aligner && docker run --rm --platform linux/amd64 -v "$PWD":/w -w /w python:3.13-slim \
  bash -c 'pip install -q uv==0.9.7 && uv pip compile --generate-hashes \
    --python-version 3.13 --no-header --no-emit-index-url \
    --exclude-newer 2026-09-23T12:00:00Z \
    requirements-test.in -o requirements-test.txt'
```

### Reproducibility

Every regeneration command above pins `--exclude-newer <RFC3339 timestamp>`
so re-running it resolves the same versions rather than whatever the CPU
index/PyPI happen to serve that day -- without it, a routine re-run for an
unrelated edit can silently pick up an unrequested transitive bump (measured
here: a same-day re-resolution without `--exclude-newer` picked up
`filelock` 4.0.3 over the committed 4.0.1). Each lock file's own header
records the exact date its content was verified to reproduce against; keep
that date in sync with whatever you pass on the command line, and re-verify
(regenerate, then diff against the committed file) before trusting a new
date.

## Known limitations and follow-ups

- **No GPU Docker variant yet** -- CPU-only image; the CUDA variant is
  tracked in #1013.
- **No baked-in model weights.** Unlike YAMNet's checksum-pinned bake, Demucs,
  faster-whisper, and the forced-alignment models pull weights from their
  normal hubs on first use, cached under `/data` (see the Dockerfile's
  `HF_HOME`/`TORCH_HOME`). Mount `/data` as a persistent volume in
  production. No single checksum to pin here, since the align-model set
  varies per requested language.
- **No image workflow.** `deploy/yamnet-detector` has
  `.github/workflows/yamnet.yml`; nothing builds or publishes this image
  yet. That workflow (GHCR, plus the CUDA variant) is #1013. CI's
  `aligner-test` job runs only the stubbed test suite, not the image.
- **Alignment emissions are computed in non-overlapping 30-second windows**,
  so a character sung across a window boundary is scored from two
  context-truncated halves; the alignment itself still runs once over the
  concatenated emissions.
- **Not deployed anywhere yet** -- no published image or Unraid wiring; gated
  on the CI workflow above and on the epic reaching a slice that calls it.
