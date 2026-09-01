"""Speech-to-text engines: a Protocol plus two adapters.

FasterWhisperEngine wraps CTranslate2 through the faster-whisper package. It
is the default engine. It runs on CPU with int8 unless Config.voice_device
is "cuda" and ctranslate2 actually reports a CUDA device. CUDA is opt-in and
verified, never auto-detected. This workshop's own box has a GPU that has
crashed other torch-based inference under CUDA before. See ADR-0019. This
engine's CUDA path is untested on it.

ParakeetEngine wraps sherpa-onnx and the NeMo Parakeet-TDT model. It is
opt-in and needs a pre-extracted model directory. See pins.PARAKEET_MODEL_URL.
It is never auto-downloaded. The archive is about 460 MB, and this box
treats /tmp as RAM.
"""

from __future__ import annotations

import io
import time
import wave
from dataclasses import dataclass
from pathlib import Path
from typing import TYPE_CHECKING, Protocol

from opendaisugi.voice import pins
from opendaisugi.voice.audio import wav_duration_s

if TYPE_CHECKING:
    from opendaisugi.config import Config


@dataclass(frozen=True)
class Transcript:
    text: str
    segments: list[dict]
    duration_s: float
    rtf: float  # wall time divided by duration_s. Above 1 means slower than real time.


class Engine(Protocol):
    name: str

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript: ...


class EngineUnavailable(RuntimeError):
    """The configured engine's package, or its model files, are not available on this host."""


class UnknownEngine(ValueError):
    """config.voice_engine names an engine this module does not build."""


def _cuda_available() -> bool:
    """Best-effort probe for a visible CUDA device. Never raises."""
    try:
        import ctranslate2

        return ctranslate2.get_cuda_device_count() > 0
    except Exception:
        return False


class FasterWhisperEngine:
    name = "faster-whisper"

    def __init__(
        self,
        model: str = pins.FASTER_WHISPER_TEST_MODEL,
        *,
        device: str = "cpu",
        compute_type: str = "int8",
    ) -> None:
        try:
            from faster_whisper import WhisperModel
        except ImportError as exc:
            raise EngineUnavailable(
                "faster-whisper is not installed. Install it with: pip install 'opendaisugi[voice]'"
            ) from exc
        resolved_device = device
        if device == "cuda" and not _cuda_available():
            resolved_device = "cpu"
        self._model = WhisperModel(model, device=resolved_device, compute_type=compute_type)
        self.model_name = model
        self.device = resolved_device

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        duration_s = wav_duration_s(wav_16k_mono)
        start = time.monotonic()
        segments_iter, _info = self._model.transcribe(io.BytesIO(wav_16k_mono), language=language)
        segments = [{"start": s.start, "end": s.end, "text": s.text.strip()} for s in segments_iter]
        elapsed = time.monotonic() - start
        text = " ".join(s["text"] for s in segments).strip()
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


class ParakeetEngine:
    name = "parakeet"

    def __init__(self, model_dir: Path) -> None:
        try:
            import sherpa_onnx
        except ImportError as exc:
            raise EngineUnavailable(
                "sherpa-onnx is not installed. Install it with: "
                "pip install 'opendaisugi[voice-parakeet]'"
            ) from exc
        missing = [f for f in pins.PARAKEET_FILES if not (model_dir / f).exists()]
        if missing:
            raise EngineUnavailable(
                f"Parakeet model files missing in {model_dir}: {', '.join(missing)}. "
                f"Download and extract: {pins.PARAKEET_MODEL_URL}"
            )
        # pins.PARAKEET_FILES is ordered encoder, decoder, joiner, tokens. Reading
        # the paths from that one tuple keeps this call and the missing-file
        # check above naming the same four files, so they cannot drift apart.
        encoder, decoder, joiner, tokens = (str(model_dir / f) for f in pins.PARAKEET_FILES)
        self._recognizer = sherpa_onnx.OfflineRecognizer.from_transducer(
            encoder=encoder,
            decoder=decoder,
            joiner=joiner,
            tokens=tokens,
            model_type=pins.PARAKEET_MODEL_TYPE,
            num_threads=1,
            sample_rate=16000,
            feature_dim=80,
            decoding_method="greedy_search",
            provider="cpu",
        )
        self.model_dir = model_dir

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        import numpy as np

        duration_s = wav_duration_s(wav_16k_mono)
        with wave.open(io.BytesIO(wav_16k_mono), "rb") as w:
            raw = w.readframes(w.getnframes())
        samples = np.frombuffer(raw, dtype=np.int16).astype(np.float32) / 32768.0
        start = time.monotonic()
        stream = self._recognizer.create_stream()
        stream.accept_waveform(16000, samples)
        self._recognizer.decode_stream(stream)
        elapsed = time.monotonic() - start
        text = stream.result.text.strip()
        rtf = elapsed / duration_s if duration_s > 0 else 0.0
        segments = [{"start": 0.0, "end": duration_s, "text": text}]
        return Transcript(text=text, segments=segments, duration_s=duration_s, rtf=rtf)


def pick_engine(config: "Config") -> Engine:
    """Return the engine config.voice_engine names.

    faster-whisper runs on CPU by default. It runs on CUDA only when
    config.voice_device is "cuda" and a CUDA device is actually visible.
    Parakeet runs only when config.voice_engine is "parakeet". It checks
    that the sherpa-onnx package and the model files are both present.
    Any other voice_engine value raises UnknownEngine naming both valid
    names.
    """
    if config.voice_engine == "faster-whisper":
        return FasterWhisperEngine(
            config.voice_model, device=config.voice_device, compute_type=config.voice_compute_type
        )
    if config.voice_engine == "parakeet":
        return ParakeetEngine(Path(config.voice_model))
    raise UnknownEngine(
        f"Unknown voice_engine {config.voice_engine!r}. Valid names: faster-whisper, parakeet."
    )
