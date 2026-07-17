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

- `GET /health` returns `{"status": "ok", "classes": <N>}`.

## Build & deploy

```bash
docker build -t canticle-yamnet:local .
```

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

This is a maintainer smoke test; the Go test suite is the CI gate.
