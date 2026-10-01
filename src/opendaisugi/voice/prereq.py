"""Host capability check for the voice bridge.

A speech engine is the only hard requirement: faster-whisper, whisper-cli
from whisper.cpp on PATH, moonshine-cli on PATH, or parakeet-cli on PATH. Ffmpeg and espeak-ng are optional.
They make a feature better instead of being load-bearing. Ffmpeg gives real
anti-aliased resampling. Espeak-ng gives a real-speech test fixture.
check_voice_prereqs never raises. The caller decides what to do with a missing
flag."""

from __future__ import annotations

import importlib
import shutil
from dataclasses import dataclass

from opendaisugi.voice import pins


def _importable(name: str) -> bool:
    """Best-effort import probe. Never raises.

    A missing package raises ImportError. A broken native install can raise
    a different error. A compiled extension that fails to load can raise
    OSError. Every such failure counts as not available."""
    try:
        importlib.import_module(name)
    except Exception:
        return False
    return True


@dataclass(frozen=True)
class PrereqResult:
    faster_whisper_available: bool
    ffmpeg_available: bool
    espeak_available: bool
    whisper_cpp_available: bool = False
    moonshine_available: bool = False
    parakeet_available: bool = False

    @property
    def ok(self) -> bool:
        """True only when the hard requirement is met.

        The hard requirement is a working speech engine."""
        return (
            self.faster_whisper_available
            or self.whisper_cpp_available
            or self.moonshine_available
            or self.parakeet_available
        )


def check_voice_prereqs() -> PrereqResult:
    return PrereqResult(
        faster_whisper_available=_importable("faster_whisper"),
        ffmpeg_available=shutil.which("ffmpeg") is not None,
        espeak_available=(shutil.which("espeak-ng") or shutil.which("espeak")) is not None,
        whisper_cpp_available=shutil.which(pins.WHISPER_CPP_BINARY) is not None,
        moonshine_available=shutil.which(pins.MOONSHINE_BINARY) is not None,
        parakeet_available=shutil.which(pins.PARAKEET_BINARY) is not None,
    )
