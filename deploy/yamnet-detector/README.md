# Canticle YAMNet instrumental-detection sidecar

A thin FastAPI wrapper around Google's [YAMNet](https://tfhub.dev/google/yamnet/1)
(AudioSet classifier). Canticle's optional instrumental detector posts a 16 kHz
mono WAV sample to this service on a provider miss and uses the response to
decide whether to write an instrumental marker.

The SavedModel is fetched and sha256-verified at image build time and loaded
with `tf.saved_model.load` -- `tensorflow-hub` is deliberately not a dependency
(see the Dockerfile and issue #491).

## Contract

- `POST /classify` (multipart field `file`, a 16 kHz mono WAV) returns:

  ```json
  {
    "mean": { "<AudioSet class name>": 0.0, ... },
    "max":  { "<AudioSet class name>": 0.0, ... }
  }
  ```

  Both are full 521-class maps. `mean` is the per-class average over the clip's
  ~1s frames (Canticle's music gate); `max` is the per-class peak over frames
  (Canticle's vocal gate). The peak is what separates vocal tracks from
  instrumentals: a brief singing moment that the mean dilutes ~10x stays intact
  in the max (see issue #384). `np.max` is free on the same forward pass as
  `np.mean`.

- `GET /health` returns `{"status": "ok", "classes": <N>, "model_version": "<sha256>"}`.
  `model_version` is the sha256 of the baked SavedModel archive and is omitted
  entirely when unknown (an image built without the build arg) rather than sent
  empty -- a caller must be able to tell "these are the weights" from "I don't
  know which weights". Canticle keys its instrumental-verdict cache on it, so a
  stored verdict is reused exactly while the weights that produced it are loaded
  (#684).

## Which image am I running?

The image is tagged with **canticle's** version, so the tag records when it was
built rather than what changed in it. The model identity is published as OCI
labels, readable from the registry without pulling ~1.56GB:

```sh
docker buildx imagetools inspect ghcr.io/sydlexius/canticle-yamnet:latest \
  --format '{{json .Image.Config.Labels}}'
```

`net.sydlexius.canticle.yamnet.model.sha256` is the authoritative answer to "did
this tag actually change?" -- compare it across two tags. If it matches, the
classifier is identical no matter what the version numbers say. `model.source`
and `model.class-count` complete the identity. CI verifies both labels against
what the running container reports on `/health`, so a label cannot drift from the
weights it names (#682).

## Deployment contract: pull the published image

**A deployment consumes this sidecar by pulling the published image, not by
building from a hand-copied directory.** CI builds `deploy/yamnet-detector/` and
publishes it to GHCR on every merge to `main` that changes the sidecar, and on
each release tag (see `.github/workflows/yamnet.yml`, issue #498):

```
ghcr.io/sydlexius/canticle-yamnet:<tag>
```

Tags mirror the app image: `nightly` / `dev` (and a dated `nightly-YYYYMMDD`) on
`main`, plus the semver tags (`X.Y.Z`, `X.Y`) on a release. The image is
**amd64-only** -- the `linux/amd64` TensorFlow wheel requires AVX, so there is no
arm64 build and the sidecar cannot run on Apple Silicon (even under emulation the
TF import aborts). Pull a version tag that matches your Canticle version so their
`detector_version` provenance lines up (the recorded `detector_version` is the
Canticle app version -- see the Dockerfile).

Point compose at the published image:

```yaml
services:
  yamnet:
    image: ghcr.io/sydlexius/canticle-yamnet:1.26.0 # match your Canticle version
```

> **Unsupported: a hand-copied `build.context`.** Do not deploy by copying these
> files onto a host and pointing `build.context` at that directory. Nothing syncs
> such a copy to git, nothing validates it, and a merged fix (including a security
> fix) can sit unshipped while a rebuild faithfully reproduces the stale image and
> reports success -- the exact failure that motivated #498 (discovered shipping
> the CVE-2026-59890 fix in #491). Pull the published tag instead.

## Build locally (development only)

```bash
docker build -t canticle-yamnet:local deploy/yamnet-detector
```

This is for local iteration on the sidecar itself; deployments pull the published
image above. (Requires an amd64 builder -- see the AVX note.)

### Resource limits (cap this container)

TensorFlow parallelizes a forward pass across every core it can see, so an
uncapped sidecar will consume the whole host during a library scan and starve
everything else running on it. `docker-compose.example.yml` therefore ships a
`deploy.resources.limits` block with a conservative `cpus: "4"` and `memory: 4G`
(the SavedModel plus the TF runtime sit around 1.5GB resident).

Treat 4 as a floor for good-neighbor behavior, not a performance target.

**THIS IS AN ISOLATION KNOB, NOT A THROUGHPUT KNOB.** An earlier version of this
section said inference was "the dominant cost in the detector path" and told you
to size this against it. That is wrong, and a companion claim of ~40-67s per
track is off by roughly 300x -- it almost certainly timed the whole detector path
(ffmpeg reading audio off the array, plus a cold model load) rather than the
classify call. Measured against a running sidecar, canticle 1.30.1:

| operation | cost |
|---|---|
| `/classify`, 30s sample (typical) | 0.07-0.11s |
| `/classify`, 60s sample (the longest sent) | 0.13-0.24s |
| ffmpeg spread-sample extraction | ~0.10s |

Inference is a fifth of a second, so raising this limit buys almost no
throughput. What it buys is a bound on how much of the host TensorFlow may seize
while it runs. Raise it on a host with cores to spare, lower it on a busy one;
nothing breaks either way.

The real per-track cost is **reading audio off the array**, which is what the
throughput and wattage knobs below actually govern.

### Taming detection (low-wattage recipe)

Detection is deliberately bursty: work that once dripped across days at the
provider's pace now runs in bounded cycles, because a contiguous burst is what
lets the library disks idle afterward. That is better for spindown and strictly
worse for peak draw while a cycle runs, so the bound is worth setting
deliberately. Size against disk reads, not inference.

| knob | where | what it governs |
|---|---|---|
| `instrumental_detector.backfill.batch_size` | canticle config | rows per sweep cycle -- the main wattage/throughput lever (default 100) |
| `instrumental_detector.backfill.interval_minutes` | canticle config | spacing between cycles; longer gaps mean longer contiguous idle windows (default 60) |
| `instrumental_detector.backfill.enabled` | canticle config | turns the automatic sweep off entirely (default true) |
| `instrumental_detector.spread_samples` | canticle config | windows sampled per track; multiplies the ffmpeg work, so it multiplies disk reads |
| `--limit` | `scan reconcile-instrumental` | drains a backlog in bounded manual chunks instead of one sustained run |
| `cpus:` | yamnet compose service | isolation only -- caps TF's forward pass, not throughput |

The defaults (100 rows every 60 minutes) drain roughly 2,400 rows/day without a
sustained burst, so an untended install converges on its own. For a quieter host,
lower `batch_size` or lengthen `interval_minutes` before touching `cpus:` --
those two govern disk reads, which is the cost that matters. Disabling the sweep
leaves the CLI path fully functional, so an operator can still drain a backlog on
their own schedule with `scan reconcile-instrumental --limit N`.

The deployed copy lives on the Unraid host at
`/mnt/vms/dockerappdata/yamnet-detector/`; Canticle reaches it at
`http://yamnet:8080` on the shared compose network.

### Deploy order (important)

When upgrading for the `{mean,max}` contract, **upgrade Canticle first, then this
sidecar.** New Canticle tolerates the old flat-map response (it degrades safely
to "not instrumental"); the *old* Canticle cannot parse `{mean,max}` and would
error on every detection until it is upgraded. So: Canticle, then sidecar.

## Test

A response-shape test that stubs the model (no model download):

```bash
# requirements.txt is hash-pinned (--require-hashes mode), so install pytest
# in a separate step -- pip refuses to mix hashed and unhashed requirements.
pip install --require-hashes -r requirements.txt
pip install pytest
pytest test_app.py -q
```

CI also runs this shape test and a live `/health` smoke check on every change to
`deploy/yamnet-detector/**` (see `.github/workflows/yamnet.yml`), so a change that
breaks the sidecar's build or its `/classify` contract fails CI rather than only
surfacing as "instrumental detection stopped working" in production.
