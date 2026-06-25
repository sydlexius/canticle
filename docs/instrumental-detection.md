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

A track is marked instrumental only when **both** gates pass. Either gate failing
means "not instrumental", and the track is left as a normal miss.

| Gate | Condition | Default |
|------|-----------|---------|
| **Music gate** | The **mean** over frames of the summed `instrumental_classes` probabilities is at least `min_confidence`. | `min_confidence = 0.90`, `instrumental_classes = ["Music", "Musical instrument"]` |
| **Vocal gate** (#384) | The **peak** (max over frames) of *every* `vocal_classes` score stays **below** `vocal_max_confidence`. | `vocal_max_confidence = 0.03`, `vocal_classes` = the singing/vocal set below |

The default `vocal_classes` set is:

```
Singing, Speech, Vocal music, Choir, A capella, Chant, Rapping,
Child singing, Synthetic singing, Yodeling, Humming
```

Note that `Speech` is included on purpose: a spoken-vocal-over-music track (an
intro, a monologue over a bed) should not be marked instrumental.

### Why mean for music but max for vocals

This asymmetry is the heart of the design:

- **Music** is gated on the frame **mean**, because instrumental backing is
  present throughout the track - a sustained, track-wide property.
- **Vocals** are gated on the frame **max**, because singing can be *brief*. A
  short sung passage is diluted to near-nothing by the mean but preserved by the
  max. Gating vocals on their loudest single moment is what stops an
  otherwise-instrumental aria from slipping through.

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

The `vocal_max_confidence = 0.03` default was pinned by a sweep over real library
tracks plus known-vocal controls (issue #384). The separation between
instrumentals and vocals on the production metric (`vocal_peak` = max over vocal
classes of each class's max-over-frames):

| Track type | `vocal_peak` | Verdict |
|------------|-------------|---------|
| Baroque oratorio aria | ~0.39 | vocal (scored ~0.004 at a single 30s intro window - a false instrumental that spread sampling fixes) |
| Jazz vocal | ~0.087 | vocal |
| Country vocal | ~0.059 | vocal |
| Orchestral ballet (instr.) | ~0.021 | instrumental |
| Solo guitar (instr.) | ~0.013 | instrumental |
| Baroque concerto (instr.) | ~0.005 | instrumental |

Instrumentals top out near `0.021` and vocals floor near `0.059` - roughly a 3x
margin - so `0.03` sits comfortably between them.

## Sidecar setup

The classifier is a small YAMNet HTTP service. Canticle does not publish an image
for it; you build it on the host from the vendored source.

- **Source:** `deploy/yamnet-detector/` in this repo (Dockerfile + FastAPI app).
- **Response contract:** `POST /classify` returns
  `{"mean": {<class>: <prob>, ...}, "max": {<class>: <prob>, ...}}` - both the
  mean and the max-over-frames reduction for every AudioSet class. The vocal gate
  needs the `max` map; a legacy mean-only sidecar degrades safely to
  never-instrumental rather than producing wrong markers.
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
| `vocal_max_confidence` | `0.03` | Vocal-gate threshold (peak). Values outside (0, 1] reset to `0.03`. |
| `vocal_classes` | (the singing/vocal set above) | Classes whose peak blocks an instrumental marking. |
| `cooldown_seconds` | `5` | Minimum gap between inference calls. `0` disables. |

**The defaults are the calibrated values - do not change the thresholds without a
specific reason.** They were tuned against real audio (#384) to maximize the
margin between instrumentals and vocals.

If you do tune:

- **`vocal_max_confidence`** is the dial that matters. **Raising** it lets more
  tracks pass as instrumental (more false instrumentals - the dangerous
  direction). **Lowering** it marks fewer tracks instrumental (safer, but you may
  re-query genuine instrumentals).
- **`min_confidence`** rarely needs changing; lowering it admits non-music audio
  (field recordings, spoken word) as "instrumental".
- **`spread_samples`** trades inference cost for coverage. Fewer windows risks
  missing a late vocal entry; more windows dilutes each one's audio share within
  the fixed `sample_duration_seconds`.

## Operations and troubleshooting

The decision is logged. Look for the detector decision line, which reports the
computed `music_sum`, the `vocal_peak`, and the resulting verdict, so you can see
*why* a track was or was not marked.

- **The borderline band.** A `vocal_peak` of roughly `0.03-0.05` is genuinely
  mixed material (quiet backing vocals, sparse vocal samples). The default
  `0.03` deliberately keeps these on the "not instrumental" side.
- **A false instrumental** (a vocal track wrongly marked) usually means a vocal
  that the sample under-weighted - check that `ffprobe` is present and spread
  sampling is actually running (a single-window fallback is the common culprit).
- **Re-classifying / clearing stale markers.** After changing thresholds or
  fixing the sidecar, force a re-check of affected tracks with `--update` (a full
  re-fetch). Instrumental markers are otherwise sticky - `--upgrade` skips them
  by design.
