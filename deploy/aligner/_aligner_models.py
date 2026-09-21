"""Real (non-stub) Demucs/WhisperX model implementations.

A SEPARATE module from app.py, imported only from inside app.py's
`_build_separator`/`_build_aligner` (never at app.py's module scope). Keeps
heavy ML imports out of app.py's import graph so test_app.py can import
app.py and drive the HTTP contract against a stubbed Separator/Aligner with
none of these packages installed.

test_app.py covers only the model-load locking here, with the ML packages
faked in sys.modules; the inference paths run only in a container (see
README.md "Test"; the README lands in slice 4 of #1005).
"""

import contextlib
import logging
import os
import tempfile
import threading

logger = logging.getLogger("canticle.aligner.models")

_EMISSION_WINDOW_SECONDS = 30


class DemucsSeparator:
    """Isolates the vocal stem from a mix using Demucs (htdemucs by default).

    Demucs, not UVR-Karaoke -- see README.md "Why Demucs, not UVR-Karaoke" (README lands in
    slice 4 of #1005) for the rationale (packaging/reproducibility, not a
    claimed quality edge).
    """

    def __init__(self, model_name: str, device: str):
        self._model_name = model_name
        self._device = device
        self._model = None  # loaded lazily on first use, not at construction
        self._load_lock = threading.Lock()

    def _load(self):
        # Locked so two cold requests cannot both load (and both hold) the
        # weights; app.py also serializes the pipeline, this does not rely on it.
        with self._load_lock:
            if self._model is not None:
                return self._model
            from demucs.pretrained import get_model  # noqa: PLC0415

            model = get_model(self._model_name)
            model.to(self._device)
            model.eval()
            self._model = model
            return model

    def separate(self, audio_path: str) -> str:
        # Local import: app.py's public seam (`app.Separator`) never imports
        # this module at collection time, so app is imported lazily too
        # rather than creating a module-load-order dependency.
        import wave  # noqa: PLC0415

        import numpy as np  # noqa: PLC0415
        import torch  # noqa: PLC0415
        from demucs.apply import apply_model  # noqa: PLC0415

        from app import decode_pcm  # noqa: PLC0415

        # Load first: a model-load failure is a server fault (500), and the
        # decode needs the model's sample rate. Decode via the ffmpeg CLI,
        # not torchaudio.load: torchaudio >= 2.9 routes load/save through
        # TorchCodec, and decode_pcm is where input-vs-environment faults are
        # classified (400 vs 500).
        model = self._load()
        pcm = decode_pcm(audio_path, model.samplerate, channels=2)  # demucs expects stereo
        wav = torch.from_numpy(np.frombuffer(pcm, dtype=np.float32).reshape(-1, 2).T.copy())
        del pcm  # wav owns a copy (.copy()); free the up-to-~424 MB decode before inference

        # shifts=0: demucs's default shifts=1 applies a RANDOM time offset,
        # which made identical requests return different timings (#1005).
        with torch.no_grad():
            sources = apply_model(model, wav.unsqueeze(0), device=self._device, shifts=0)[0]

        vocals = sources[model.sources.index("vocals")].mean(dim=0)  # fold to mono
        pcm16 = (vocals.clamp(-1.0, 1.0) * 32767).to(torch.int16).cpu().numpy()

        # The caller (app._run_pipeline) deletes this file once the request is
        # done; it is only removed here if writing it fails.
        fd, out_path = tempfile.mkstemp(suffix=".wav")
        os.close(fd)
        try:
            with wave.open(out_path, "wb") as w:
                w.setnchannels(1)
                w.setsampwidth(2)
                w.setframerate(model.samplerate)
                w.writeframes(pcm16.tobytes())
        except BaseException:
            with contextlib.suppress(OSError):
                os.remove(out_path)
            raise
        return out_path


def vad_safe_globals() -> list:
    """The torch weights_only allowlist for whisperx's BUNDLED pyannote VAD.

    That checkpoint pickles these classes and torch >= 2.6 loads with
    weights_only=True, which refuses them. Allow exactly this set, scoped to
    the one load (never the global TORCH_FORCE_NO_WEIGHTS_ONLY_LOAD, which
    would also unguard the runtime-downloaded Demucs and wav2vec2 weights).
    Each entry was confirmed required by probing the load one refusal at a
    time. The Dockerfile's smoke check loads the VAD with this same list, so
    a dependency bump that needs a new entry fails the build.
    """
    import collections  # noqa: PLC0415
    import typing  # noqa: PLC0415

    import omegaconf  # noqa: PLC0415
    import torch  # noqa: PLC0415
    from pyannote.audio.core.model import Introspection  # noqa: PLC0415
    from pyannote.audio.core.task import Problem, Resolution, Specifications  # noqa: PLC0415

    return [
        omegaconf.listconfig.ListConfig, omegaconf.base.ContainerMetadata, omegaconf.base.Metadata,
        omegaconf.nodes.AnyNode, typing.Any, list, dict, int, collections.defaultdict,
        torch.torch_version.TorchVersion, Introspection, Specifications, Problem, Resolution,
    ]


class WhisperXAligner:
    """Transcribes and forced-aligns a vocal stem using WhisperX."""

    def __init__(self, whisper_model: str, device: str):
        self._whisper_model_name = whisper_model
        self._device = device
        self._compute_type = "float16" if device == "cuda" else "float32"
        self._whisper_model = None
        # (language, model, metadata) as ONE value, replaced atomically, so a
        # reader can never pair one language's model with another's dictionary.
        self._align = None
        self._whisper_lock = threading.Lock()
        self._align_lock = threading.Lock()

    def _load_whisper(self):
        with self._whisper_lock:
            if self._whisper_model is not None:
                return self._whisper_model
            import torch  # noqa: PLC0415
            import whisperx  # noqa: PLC0415

            with torch.serialization.safe_globals(vad_safe_globals()):
                self._whisper_model = whisperx.load_model(
                    self._whisper_model_name, self._device, compute_type=self._compute_type
                )
            return self._whisper_model

    def _load_align_model(self, language: str):
        with self._align_lock:
            cached = self._align
            if cached is not None and cached[0] == language:
                return cached[1], cached[2]
            import whisperx  # noqa: PLC0415

            model, metadata = whisperx.load_align_model(language_code=language, device=self._device)
            self._align = (language, model, metadata)
            return model, metadata

    def transcribe(self, vocal_path: str, language: str) -> str:
        import whisperx  # noqa: PLC0415

        model = self._load_whisper()
        audio = whisperx.load_audio(vocal_path)
        result = model.transcribe(audio, language=language)
        return " ".join(seg.get("text", "").strip() for seg in result.get("segments", [])).strip()

    def align(self, vocal_path: str, lines: list[str], language: str):
        import torch  # noqa: PLC0415
        import whisperx  # noqa: PLC0415
        from torchaudio.functional import forced_align, merge_tokens  # noqa: PLC0415

        from app import _BadAudioError, plan_alignment, words_from_token_spans  # noqa: PLC0415

        align_model, metadata = self._load_align_model(language)
        dictionary = metadata["dictionary"]
        blank_id = dictionary.get("[pad]", dictionary.get("<pad>", 0))
        targets, planned = plan_alignment(lines, dictionary, blank_id)
        if not targets:
            return []

        # ONE CTC forced alignment of the whole lyric over the whole stem, not
        # whisperx.align() per line: per-line segments each searched the full
        # clip (later lines overlapped earlier ones), and whisperx folds the
        # blank frames after a character into it, so each segment's last word
        # stretched to the end of the audio. merge_tokens drops blanks.
        # Emissions are computed in fixed windows and concatenated: one forward
        # pass over a whole song would make wav2vec2's attention memory grow
        # with the square of the track length. The alignment itself still
        # runs once over the concatenated emission, so it stays monotonic.
        audio = whisperx.load_audio(vocal_path)
        window = _EMISSION_WINDOW_SECONDS * whisperx.audio.SAMPLE_RATE
        chunks = []
        with torch.inference_mode():
            for offset in range(0, len(audio), window):
                piece = torch.from_numpy(audio[offset : offset + window]).unsqueeze(0)
                if piece.shape[-1] < 400:  # wav2vec2's minimum input length
                    piece = torch.nn.functional.pad(piece, (0, 400 - piece.shape[-1]))
                piece = piece.to(self._device)
                if metadata["type"] == "torchaudio":
                    logits, _ = align_model(piece)
                else:
                    logits = align_model(piece).logits
                chunks.append(torch.log_softmax(logits, dim=-1).cpu())
        emission = torch.cat(chunks, dim=1)

        frames = emission.shape[1]
        needed = len(targets) + sum(1 for a, b in zip(targets, targets[1:]) if a == b)
        if needed > frames:
            raise _BadAudioError("audio too short for the supplied lyrics")

        path, scores = forced_align(emission, torch.tensor([targets], dtype=torch.int32), blank=blank_id)
        spans = [(s.start, s.end, s.score) for s in merge_tokens(path[0], scores[0].exp(), blank=blank_id)]
        if len(spans) != len(targets):
            raise RuntimeError(f"forced alignment produced {len(spans)} spans for {len(targets)} targets")
        return words_from_token_spans(planned, spans, len(audio) / whisperx.audio.SAMPLE_RATE / frames)
