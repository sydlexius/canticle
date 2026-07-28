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
Inference is the dominant cost in the detector path, so this limit trades
detector throughput for isolation: raise it on a host with cores to spare, lower
it on a busy one. Nothing breaks either way -- a slower sidecar just means a
longer per-track detection, and Canticle's own detector cooldown and circuit
breaker already bound how hard it is driven.

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
