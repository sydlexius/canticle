"""Real (non-stub) Demucs/faster-whisper/wav2vec2 model implementations.

A SEPARATE module from app.py, imported only from inside app.py's
`_build_separator`/`_build_aligner` (never at app.py's module scope). Keeps
heavy ML imports out of app.py's import graph so test_app.py can import
app.py and drive the HTTP contract against a stubbed Separator/Aligner with
none of these packages installed.

test_app.py covers only the model-load locking here, with the ML packages
faked in sys.modules; the inference paths run only in a container (see
README.md "Test").
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

    Demucs, not UVR-Karaoke -- see README.md "Why Demucs, not UVR-Karaoke" for
    the rationale (packaging/reproducibility, not a claimed quality edge).
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


# Sample rate every alignment/transcription call in this module decodes and
# reasons about, matching the wav2vec2/whisper models' expected input rate.
# Was `whisperx.audio.SAMPLE_RATE` (also 16000); now a local constant since
# whisperx is gone (#1016).
_ALIGN_SAMPLE_RATE = 16000


def _decode_audio_f32(path: str):
    """Decodes `path` to a 1-D float32 numpy array at `_ALIGN_SAMPLE_RATE`, mono.

    The sidecar's ONE audio-decode path (ffmpeg via app.decode_pcm), reused
    here instead of a second decoder (torchaudio.load or faster-whisper's own
    PyAV-based file decode) -- see DemucsSeparator.separate's comment on why
    decode_pcm and not torchaudio.load.

    Every caller here passes the SEPARATED VOCAL STEM (Demucs' own output),
    never the caller's original upload, so a decode rejection is this
    sidecar's own fault (a pipeline bug or broken environment), not the
    caller's -- reject_error=_InternalAudioError makes that read as a 500,
    not the documented 400 "cannot read audio" _BadAudioError produces for a
    genuinely bad upload.
    """
    import numpy as np  # noqa: PLC0415

    from app import _InternalAudioError, decode_pcm  # noqa: PLC0415

    pcm = decode_pcm(path, _ALIGN_SAMPLE_RATE, channels=1, reject_error=_InternalAudioError)
    return np.frombuffer(pcm, dtype=np.float32)


class FasterWhisperAligner:
    """Transcribes with faster-whisper and forced-aligns with a direct wav2vec2 load.

    Replaces whisperx (#1016), which only wrapped these two steps: whisperx's
    `load_model`/`transcribe` was a thin layer over `faster_whisper.WhisperModel`
    plus its own bundled pyannote VAD, and `load_align_model` picked one of two
    branches this class now picks itself -- a `torchaudio.pipelines` bundle for
    the torch-type languages, a `transformers` `Wav2Vec2ForCTC`/`Wav2Vec2Processor`
    checkpoint for the rest -- keyed by `app.ALIGN_MODELS`, the static table this
    module used to get overwritten by whisperx's own tables at boot.
    """

    def __init__(self, whisper_model: str, device: str):
        self._whisper_model_name = whisper_model
        self._device = device
        # faster-whisper's CTranslate2 backend rejects "mps" (ValueError:
        # unsupported device), so ASR runs on CPU there; alignment keeps MPS.
        self._whisper_device = "cpu" if device == "mps" else device
        self._compute_type = "float16" if device == "cuda" else "float32"
        self._whisper_model = None
        # (language, model, metadata) as ONE value, replaced atomically, so a
        # reader can never pair one language's model with another's dictionary.
        # ONE slot on purpose: an align model is ~0.36-1.2 GB, a library is
        # overwhelmingly one language, and the sidecar has no memory budget to
        # hold several; a language switch pays one model load instead.
        self._align = None
        self._whisper_lock = threading.Lock()
        self._align_lock = threading.Lock()

    def _load_whisper(self):
        with self._whisper_lock:
            if self._whisper_model is not None:
                return self._whisper_model
            from faster_whisper import WhisperModel  # noqa: PLC0415

            self._whisper_model = WhisperModel(
                self._whisper_model_name, device=self._whisper_device, compute_type=self._compute_type
            )
            return self._whisper_model

    def _load_align_model(self, language: str):
        with self._align_lock:
            cached = self._align
            if cached is not None and cached[0] == language:
                return cached[1], cached[2]
            import torchaudio  # noqa: PLC0415

            from app import ALIGN_MODELS, is_torchaudio_align_model  # noqa: PLC0415

            model_name = ALIGN_MODELS[language]
            if is_torchaudio_align_model(model_name, torchaudio):
                # The 5 languages whisperx served from torchaudio's own
                # bundled wav2vec2 checkpoints (en/fr/de/es/it).
                bundle = getattr(torchaudio.pipelines, model_name)
                model = bundle.get_model().to(self._device)
                dictionary = {c.lower(): i for i, c in enumerate(bundle.get_labels())}
                metadata = {"dictionary": dictionary, "type": "torchaudio"}
            else:
                # Every other language: a Hugging Face wav2vec2-CTC checkpoint,
                # same source whisperx used for its DEFAULT_ALIGN_MODELS_HF entries.
                from transformers import Wav2Vec2ForCTC, Wav2Vec2Processor  # noqa: PLC0415

                processor = Wav2Vec2Processor.from_pretrained(model_name)
                model = Wav2Vec2ForCTC.from_pretrained(model_name).to(self._device)
                dictionary = {c.lower(): i for c, i in processor.tokenizer.get_vocab().items()}
                metadata = {"dictionary": dictionary, "type": "huggingface"}
            model.eval()
            self._align = (language, model, metadata)
            return model, metadata

    def transcribe(self, vocal_path: str, language: str) -> str:
        model = self._load_whisper()
        audio = _decode_audio_f32(vocal_path)
        # vad_filter: faster-whisper's own bundled Silero VAD (a different,
        # lighter VAD than whisperx's bundled pyannote one it replaces).
        # Exact parity is not required -- transcript only feeds the content
        # gate (verification.Similarity), never `words`.
        # condition_on_previous_text=False: whisperx ran with this off, and
        # faster-whisper's own default is True. Conditioning on prior text
        # raises the risk of repetition/hallucination loops on sung material
        # (a Whisper failure mode distinct from plain speech); restoring
        # whisperx's posture costs nothing here since each request is one
        # short vocal stem, not a long multi-segment transcript that would
        # benefit from cross-segment context.
        # without_timestamps=True: whisperx also ran with this on (faster-
        # whisper's own default is False). It only tells the decoder to skip
        # predicting timestamp tokens; segment.text is plain text either way,
        # so it does not change what this method joins -- verified against
        # faster-whisper's WhisperModel.transcribe, which returns Segment
        # objects with a .text field regardless of this flag. transcript is
        # a content-gate input only, never `words` (which comes from the
        # forced-align step below, not from Whisper's own timestamps).
        segments, _info = model.transcribe(
            audio,
            language=language,
            vad_filter=True,
            condition_on_previous_text=False,
            without_timestamps=True,
        )
        return " ".join(seg.text.strip() for seg in segments).strip()

    def align(self, vocal_path: str, lines: list[str], language: str):
        import torch  # noqa: PLC0415
        from torchaudio.functional import forced_align, merge_tokens  # noqa: PLC0415

        from app import _BadAudioError, plan_alignment, words_from_token_spans  # noqa: PLC0415

        align_model, metadata = self._load_align_model(language)
        dictionary = metadata["dictionary"]
        blank_id = dictionary.get("[pad]", dictionary.get("<pad>", 0))
        targets, planned = plan_alignment(lines, dictionary, blank_id)
        if not targets:
            return []

        # ONE CTC forced alignment of the whole lyric over the whole stem, not
        # per line: per-line segments would each search the full clip (later
        # lines overlapping earlier ones), and folding the blank frames after
        # a character into it would stretch each segment's last word to the
        # end of the audio. merge_tokens drops blanks.
        # Emissions are computed in fixed windows and concatenated: one
        # forward pass over a whole song would make wav2vec2's attention
        # memory grow with the square of the track length. The alignment
        # itself still runs once over the concatenated emission, so it stays
        # monotonic.
        audio = _decode_audio_f32(vocal_path)
        window = _EMISSION_WINDOW_SECONDS * _ALIGN_SAMPLE_RATE
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
        return words_from_token_spans(planned, spans, len(audio) / _ALIGN_SAMPLE_RATE / frames)
