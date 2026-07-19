# Instrumental Detection

Canticle can optionally recognize that a track is **instrumental** (no lyrics to
find) by listening to the audio, so it writes an instrumental marker instead of
re-querying providers forever for lyrics that do not exist. This page is the
single reference for how that works, how to deploy it, and how to tune it.

It is **disabled by default**. Nothing in this page applies unless you set
`enabled = true` and a `classifier_url` under `[instrumental_detector]`.

## What it does and when it runs

When enabled, the detector samples a track's audio with `ffmpeg`, sends the
sample to an external [AudioSet](https://research.google.com/audioset/)
classifier (a YAMNet sidecar, vendored at `deploy/yamnet-detector/`), and decides
whether the track is instrumental. If it is, the worker writes the usual
instrumental marker (`.txt`) for that track.

Two rules bound when it can act:

- **Only on provider misses.** The detector runs *after* every lyrics provider
  has come back empty. If a provider returns lyrics, the track is never
  classified - the audio model never overrides provider-supplied data.
- **Never destructive.** It only writes a marker where there would otherwise be a
  miss. Instrumental markers are excluded from `--upgrade` (re-fetching an
  instrumental just reproduces the marker); force a re-check with `--update`
  (full re-fetch) after a catalog change.

## The decision model (the core)

A track is marked instrumental only when **all three** gates pass. Any gate
failing means "not instrumental", and the track is left as a normal miss.

| Gate | Condition | Default |
|------|-----------|---------|
| **Music gate** | The **mean** over frames of the summed `instrumental_classes` probabilities is at least `min_confidence`. | `min_confidence = 0.90`, `instrumental_classes = ["Music", "Musical instrument"]` |
| **Sung-vocal gate** (#384) | The **peak** (max over frames) of *every* `vocal_classes` score stays **below** `vocal_max_confidence`. | `vocal_max_confidence = 0.015`, `vocal_classes` = the singing/vocal set below |
| **Speech gate** (#403) | The summed frame **mean** of the `speech_classes` stays **below** `speech_max_confidence`. | `speech_max_confidence = 0.20` (provisional), `speech_classes = ["Speech"]` |

The default `vocal_classes` (sung-vocal) set is:

```
Singing, Vocal music, Choir, A capella, Chant, Rapping,
Child singing, Synthetic singing, Yodeling, Humming
```

`Speech` is **no longer** in `vocal_classes`: as of #403 it is governed by its own
**speech gate** on sustained presence. A track with brief incidental speech (a
crowd sample, an announcer, a single line of dialogue over a bed) should still be
markable instrumental, whereas a *sustained* spoken-word track should not. The
peak-based sung-vocal gate could not tell those apart (both produce a high single
peak); the mean-based speech gate can. Any legacy config that still lists
`Speech` in `vocal_classes` is **de-duplicated** at detector construction - the
class is removed from the effective peak set so it is governed solely by the
speech (mean) gate, delivering the fix without requiring you to edit your config.

### Why mean for music, max for sung vocals, and mean for speech

This asymmetry is the heart of the design:

- **Music** is gated on the frame **mean**, because instrumental backing is
  present throughout the track - a sustained, track-wide property.
- **Sung vocals** are gated on the frame **max**, because singing can be *brief*.
  A short sung passage is diluted to near-nothing by the mean but preserved by
  the max. Gating sung vocals on their loudest single moment is what stops an
  otherwise-instrumental aria from slipping through.
- **Speech** is gated on the frame **mean**, because it models *sustained*
  presence. A brief announcer or crowd transient has a high single peak but a
  near-zero mean; a spoken-word track (monologue, narration over a bed) has a
  high mean. Mean is robust to a single loud transient where a raised peak
  threshold would not be, so brief incidental speech no longer wrongly blocks an
  instrumental marking while sustained spoken word still does.

### Spread sampling

Late-entering vocals (an aria after a long instrumental intro, a vocal that
arrives two minutes in) would be missed by sampling only the start of the track.
So the detector builds **one** sample from `spread_samples` short windows
(default **6**) distributed evenly across the **whole** track, concatenated into a
single `sample_duration_seconds` clip, and runs one inference on it. The vocal
peak is then taken across that whole spread.

This requires the sidecar to return both reductions - `{"mean": {...}, "max":
{...}}` - on the one forward pass. A value of `spread_samples < 2` disables
spreading and uses a single contiguous window from the start.

### Conservative by construction

Any doubt resolves to **not instrumental**. A false instrumental is strictly
worse than a missed one: it permanently suppresses a real lyrics fetch, whereas a
missed instrumental just gets retried. Every threshold default is chosen to fail
safe in that direction.

### Calibration evidence

The `vocal_max_confidence = 0.015` default was calibrated on 2026-07-19 against a
296-track labeled sample scored by the live sidecar: 146 tracks that provider
lyrics prove are vocal, and 150 provider-labeled instrumentals. The music gate
was held at `0.90` and the speech gate at `0.20`.

**The two classes overlap; there is no clean separating margin.** An earlier
6-track sample suggested instrumentals topped out near `0.021` while vocals
floored near `0.059`, implying a comfortable 3x gap. The larger sample refutes
that: known-vocal `vocal_peak` ranges from `0.0019` to `0.8861` (median `0.0959`,
p5 `0.0257`), so genuinely-vocal tracks reach well into what looks like
instrumental territory. Any threshold trades one error against the other.

| `vocal_max_confidence` | False instrumental (of 146 known-vocal) | True instrumental recovered (of 150) |
|---|---|---|
| `0.005` | 0.68% | 24.0% |
| `0.010` | 1.37% | 42.7% |
| **`0.015`** (default) | **1.37%** | **54.0%** |
| `0.020` | 2.05% | 58.7% |
| `0.030` (prior default) | 4.79% | 66.0% |

`0.015` is chosen because it recovers the most true instrumentals available at
its error level: it costs no additional false instrumentals over `0.010`, and
the prior `0.030` misclassified 4.79% of known-vocal tracks.

Note that `cmd/vocalcalib -mode sweep` returns `0.007589` at its default 1%
error ceiling. That output is **not** the default, deliberately. It is pinned
exactly to one sample's `vocal_peak` (moving it by `1e-5` doubles the error
rate), and it is dominated by `0.015` - same real error count, 22 points less
recovery. At n=146 a single track is 0.68%, so a 1% ceiling is finer than the
sample can resolve and pushes the sweep into a strictly worse operating point.
Treat the sweep as evidence, not as the decision.

**The music gate, not the vocal gate, is now the binding constraint on
recovery.** 32 of the 150 labeled instrumentals (21%) score `music_sum < 0.90`
and cannot be recovered at any vocal threshold. The speech gate blocks only 2 of
150. Further recovery gains have to come from `min_confidence`, which has not
been calibrated.

### Applying a threshold change to rows already decided

Changing `vocal_max_confidence` only affects future detections. Rows the old
value already decided are never revisited on their own, in either direction, so
a recalibration is finished only once the backlog is re-decided:

```sh
# Thresholds LOOSENED: promote rows the old, tighter gate buried.
canticle scan reconcile-instrumental-recalibrate

# Thresholds TIGHTENED: revert rows the old, looser gate wrongly settled.
canticle scan reconcile-instrumental-recalibrate --reverse
```

Both re-decide from the telemetry already stored on each row
(`music_sum`/`vocal_peak`/`speech_mean`), so neither re-reads audio or calls the
sidecar - they are arithmetic passes, not detection passes. Both are dry-run by
default, print what they would change, and need `--yes` to apply; applying
writes a JSONL backup of every changed row first.

The `--reverse` direction removes marker sidecars, so it is guarded: a marker is
deleted only when its `[source:]` header proves the **detector** wrote it. A
provider-declared instrumental, or a legacy bare marker with no header (which is
indistinguishable from one), is left untouched and counted as
`skipped(provider-owned=N)`. Reverted rows return to the queue at low priority
for a normal provider fetch.

Rows with **no** stored telemetry (written before migration 025) are invisible to
both directions - there are no scores to re-decide from. Those need a real
re-scan via `scan reconcile`.

The speech gate's `speech_max_confidence = 0.20` default (#403) is a different
kind of value: it is a **provisional placeholder**, chosen conservatively low
(biased toward "not instrumental", preserving lyric protection) pending a
#384-style calibration sweep over the audit set to pin the final constant. Because
the key is configurable, that calibration refines the value without a code change.
The acceptance criterion - that incidental-speech instrumentals get re-confirmed -
is satisfied by the **post-calibration** validation gate (re-running the audit
set), not by the placeholder itself.

## Sidecar setup

The classifier is a small YAMNet HTTP service. Canticle does not publish an image
for it; you build it on the host from the vendored source.

- **Source:** `deploy/yamnet-detector/` in this repo (Dockerfile + FastAPI app).
- **Response contract:** `POST /classify` returns
  `{"mean": {<class>: <prob>, ...}, "max": {<class>: <prob>, ...}}` - both the
  mean and the max-over-frames reduction for **every** AudioSet class (a full map,
  no thresholding or top-N). The vocal gate needs the `max` map; a legacy mean-only
  sidecar degrades safely to never-instrumental rather than producing wrong
  markers. The full-map guarantee matters: Canticle records the vocal classes a
  healthy response carries and, on every later decision, treats a configured vocal
  class **missing** from a non-empty `max` map as a partial/contract-violating
  response and fails safe to not-instrumental (see Operations below). A custom
  sidecar that omits zero-scored classes would trip this on normal tracks, so any
  replacement must honor the full-map contract.
- **Wire it up:** point `classifier_url` at the service, e.g.
  `http://yamnet:8080`.

### ffmpeg and ffprobe are both required

The app container needs **both** `ffmpeg` and `ffprobe` on PATH:

- `ffmpeg` extracts and concatenates the audio sample.
- `ffprobe` reads each track's **duration** so spread sampling can place its
  windows.

If `ffmpeg` was auto-provisioned by Canticle it ships **no** `ffprobe`; set
`ffprobe_path` (or install `ffprobe` on PATH) or the detector silently falls back
to a single window from the start and loses late-vocal protection. See
[Configuration -> ffmpeg resolution](CONFIGURATION.md#ffmpeg-resolution) for how
`ffmpeg` is located.

### Upgrade ordering (important)

When upgrading a running deployment, **upgrade Canticle before the sidecar.** The
new Canticle tolerates an old flat-map (mean-only) sidecar response and degrades
safely; an old Canticle **cannot** parse the new `{mean, max}` response. Upgrading
the sidecar first can break classification until the app catches up.

## Configuration reference and tuning

All keys live under `[instrumental_detector]`; each has an
`MXLRC_INSTRUMENTAL_DETECTOR_*` environment equivalent (see the full table in
[Configuration](CONFIGURATION.md)).

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | `false` | Master switch. |
| `classifier_url` | (none) | Sidecar base URL. Required when enabled. |
| `ffmpeg_path` | `ffmpeg` | ffmpeg binary (PATH or auto-provisioned). |
| `ffprobe_path` | (auto-discover) | ffprobe binary for duration probing. Sibling of ffmpeg, then PATH. |
| `sample_duration_seconds` | `30` | Total sample length, clamped to [30, 60]. |
| `spread_samples` | `6` | Windows spread across the track. `< 2` disables spreading. |
| `min_confidence` | `0.90` | Music-gate threshold (mean). Values outside (0, 1] reset to `0.90`. |
| `instrumental_classes` | `["Music", "Musical instrument"]` | Classes summed for the music gate. |
| `vocal_max_confidence` | `0.015` | Sung-vocal-gate threshold (peak). Values outside (0, 1] reset to `0.015`. |
| `vocal_classes` | (the singing/vocal set above) | Sung-vocal classes whose peak blocks an instrumental marking. |
| `speech_max_confidence` | `0.20` (provisional) | Speech-gate threshold (summed mean). Values outside (0, 1] reset to `0.20`. |
| `speech_classes` | `["Speech"]` | Speech classes gated on sustained mean (not peak). |
| `cooldown_seconds` | `5` | Minimum gap between inference calls. `0` disables. |

**The defaults are the calibrated values - do not change the thresholds without a
specific reason.** They were tuned against real audio (#384) to maximize the
margin between instrumentals and vocals.

If you do tune:

- **`vocal_max_confidence`** is the dial that matters. **Raising** it lets more
  tracks pass as instrumental (more false instrumentals - the dangerous
  direction). **Lowering** it marks fewer tracks instrumental (safer, but you may
  re-query genuine instrumentals).
- **`speech_max_confidence`** controls the speech gate the same way, on the
  summed Speech **mean**. Its `0.20` default is provisional (see Calibration
  evidence): raising it tolerates more sustained speech as instrumental; lowering
  it blocks instrumental marking on less speech. Pin it from a calibration sweep
  before relying on the exact value.
- **`min_confidence`** rarely needs changing; lowering it admits non-music audio
  (field recordings, spoken word) as "instrumental".
- **`spread_samples`** trades inference cost for coverage. Fewer windows risks
  missing a late vocal entry; more windows dilutes each one's audio share within
  the fixed `sample_duration_seconds`.

## Operations and troubleshooting

The decision is logged. Look for the detector decision line, which reports the
computed `music_sum`, the `vocal_peak`, the `speech_mean`, and the resulting
verdict, so you can see *why* a track was or was not marked.

- **The borderline band.** A `vocal_peak` of roughly `0.015-0.05` is genuinely
  mixed material (quiet backing vocals, sparse vocal samples). The default
  `0.015` deliberately keeps these on the "not instrumental" side.
- **A false instrumental** (a vocal track wrongly marked) usually means a vocal
  that the sample under-weighted - check that `ffprobe` is present and spread
  sampling is actually running (a single-window fallback is the common culprit).
- **Speech vs sung vocals are distinct failure modes.** A spoken-word track
  wrongly *not* marked, or an incidental-speech instrumental wrongly *blocked*,
  is the **speech gate** (`speech_mean` vs `speech_max_confidence`), not the
  sung-vocal gate (`vocal_peak`). Read `speech_mean` in the decision line: a high
  value is sustained speech (correctly blocked); a near-zero value with a high
  Speech peak is brief incidental speech (correctly allowed through the now-split
  gate). Tune `speech_max_confidence`, not `vocal_max_confidence`, for these.
- **Partial classifier responses.** A non-empty `max` map that omits a vocal class
  the sidecar normally returns is a contract violation (a truncated/corrupt
  response): the absent class would silently contribute 0 to `vocal_peak` and
  weaken the gate, so the detector treats that decision as **not instrumental** and
  logs `detector: vocal classes missing from a non-empty classifier max map` at
  `Error` on **every** occurrence (not once per process). A configured vocal class
  the sidecar *never* returns is instead a permanent config/contract mismatch: it
  is logged once and dropped from the baseline so the gate keeps running on the
  classes the sidecar does emit. Note the deliberate severity split: a *fully*
  absent `max` map is the expected legacy mean-only degradation and stays at
  `Warn`, while a present-but-partial map is the unexpected violation at `Error`.
  This fail-safe is scoped to the partial-response case only - it does not explain
  the conservative `0.015-0.05` borderline band (by design) or other separately
  tracked refinements (cross-version model drift). `Speech` activation on
  non-lyrical audio is now handled by the dedicated sustained-mean speech gate
  (#403). Per-decision telemetry (the scores, the winning vocal class, and the
  detector version) is persisted on the `work_queue` row - see *Decision
  telemetry* below.
- **Re-classifying / clearing stale markers.** After changing thresholds or
  fixing the sidecar, re-validate existing markers with `scan reconcile` (see
  below) rather than a blanket `--update`. Instrumental markers are otherwise
  sticky - `--upgrade` skips them by design.

### Decision telemetry on `work_queue`

When the detector renders a verdict it stamps the decision inputs onto the
`work_queue` row (migration 025), alongside the boolean `instrumental_result`, in
a single atomic update:

| column | meaning |
| --- | --- |
| `music_sum` | summed music-gate score (`Result.Confidence`) |
| `vocal_peak` | peak sung-vocal score (`Result.VocalConfidence`) |
| `speech_mean` | summed speech-gate score (`Result.SpeechConfidence`) |
| `vocal_class` | the configured vocal class that produced `vocal_peak` (empty when none scored) |
| `detector_version` | the Canticle app version (`internal/version`) at decision time |

All five are **nullable**: `NULL` means detection did not run for that row
(detection disabled, no source path, or a row written before migration 025). They
make borderline review and drift detection a query rather than a 40-minute
re-inference pass:

```sql
-- borderline sung-vocal tracks worth a human look
SELECT id, source_path, vocal_peak FROM work_queue
WHERE instrumental_result = 1 AND vocal_peak BETWEEN 0.015 AND 0.05;

-- markers written by an older detector build (model/threshold drift)
SELECT id, source_path, detector_version FROM work_queue
WHERE instrumental_result = 1 AND detector_version <> '<current-version>';
```

> The version is the Canticle app version, which captures Go-side gate/threshold
> drift. If sidecar/model identity is wanted later, derive it from the sidecar's
> pinned model `YAMNET_SHA256` build arg or image tag rather than a new `/health`
> field.

### Reconciling stale markers (`scan reconcile`)

`scan reconcile` re-runs the detector over instrumental-tagged tracks and, for any
the current detector no longer classifies as instrumental, deletes the exact
instrumental `.txt` marker and re-queues the row so the scheduler re-fetches it.

- **Dry-run by default.** It prints what would change; pass `--yes` to apply.
- **Cheap by default.** Using the telemetry above, it re-infers only the
  *candidate* set - borderline `vocal_peak`/`speech_mean`, cross-version, or
  un-scored (pre-telemetry) rows - instead of every tagged track. `--all` forces a
  full re-inference of every `instrumental_result = 1` row.
- **Safe deletes.** A `.txt` is removed only when its content is *exactly* the
  instrumental marker, so genuine unsynced `.txt` lyrics are never touched. Every
  cleared row is written to a JSONL backup first (`--backup <path>`, default
  `<db-dir>/reconcile-backup-<timestamp>.jsonl`).
- **No starvation.** Re-queued rows are deferred at `priority = -100`, so a bulk
  reconcile is dequeued strictly behind foreground work.
- `--library <name|id>` scopes the run; `--limit <n>` caps it.

```sh
mxlrcgo-svc scan reconcile                 # dry run, narrowed candidate set
mxlrcgo-svc scan reconcile --yes           # apply
mxlrcgo-svc scan reconcile --all --yes     # re-infer every tagged track
```
