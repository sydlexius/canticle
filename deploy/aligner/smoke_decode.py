"""Image smoke check: the I1 decode hardening holds against THIS image's ffmpeg.

test_app.py proves the hardening against whatever ffmpeg the test host has
(apt ffmpeg in CI). This runs the same check against the ffmpeg the image
ships, since that is the decoder production uses. The Dockerfile runs it at
build time, so the build fails if any assertion fails; rerun it against a
built image with `docker run --rm <image> python smoke_decode.py`.

The attack: a concat playlist whose entry is a `data:` URL. Forcing the concat
demuxer (`-f concat -safe 0` before `-i`) onto decode_pcm's exact argv must
decode NOTHING, because `-protocol_whitelist fd` refuses the nested `data:`
open. The same forced argv WITHOUT the whitelist must decode audio, which
proves the playlist is a live attack on this ffmpeg (so the 0 bytes above is
the whitelist working, not a broken fixture). A plain WAV through the
unmodified decode_pcm is the positive control.

All fixtures are synthetic: a 1 s sine tone from ffmpeg's lavfi source.
"""

import base64
import os
import subprocess
import tempfile

import app

SR = 16000


def _tone(path: str) -> None:
    subprocess.run(  # noqa: S603 - fixed argv, no shell
        [app.FFMPEG, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=d=1", "-y", path], check=True
    )


def _forced_concat_bytes(playlist: str, whitelist: bool) -> int:
    """Runs decode_pcm's argv with the concat demuxer forced; returns bytes decoded."""
    real_run = subprocess.run
    seen = []

    def spy(cmd, **kwargs):
        if not whitelist and "-protocol_whitelist" in cmd:
            i = cmd.index("-protocol_whitelist")
            cmd = cmd[:i] + cmd[i + 2 :]
        i = cmd.index("-i")
        cmd = cmd[:i] + ["-f", "concat", "-safe", "0"] + cmd[i:]
        proc = real_run(cmd, **kwargs)
        seen.append(len(proc.stdout))
        return proc

    subprocess.run = spy
    try:
        app.decode_pcm(playlist, SR, 1)
    except app._BadAudioError:
        pass
    finally:
        subprocess.run = real_run
    assert len(seen) == 1, f"decode_pcm ran ffmpeg {len(seen)} times"
    return seen[0]


def main() -> None:
    with tempfile.TemporaryDirectory() as d:
        ts, wav, playlist = (os.path.join(d, n) for n in ("tone.ts", "tone.wav", "list.txt"))
        _tone(ts)
        _tone(wav)
        with open(ts, "rb") as f:
            url = "data:video/mp2t;base64," + base64.b64encode(f.read()).decode("ascii")
        with open(playlist, "w", encoding="ascii") as f:
            f.write(f"ffconcat version 1.0\nfile '{url}'\n")

        hardened = _forced_concat_bytes(playlist, whitelist=True)
        unhardened = _forced_concat_bytes(playlist, whitelist=False)
        control = len(app.decode_pcm(wav, SR, 1))
        print(f"decode smoke: forced concat+data: {hardened} B (whitelist), {unhardened} B (no whitelist), wav {control} B")
        assert hardened == 0, f"forced concat+data: decoded {hardened} B; -protocol_whitelist fd is not holding"
        assert unhardened > 0, "fixture is not a live attack: forced concat+data: decoded nothing without the whitelist"
        assert control > 0, "positive control: a plain WAV decoded to nothing"
    print("decode smoke ok")


if __name__ == "__main__":
    main()
