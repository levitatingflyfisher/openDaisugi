"""Host capability check for the voice bridge.

Faster-whisper is the only hard requirement. Ffmpeg and espeak-ng are optional.
They make a feature better instead of being load-bearing. Ffmpeg gives real
anti-aliased resampling. Espeak-ng gives a real-speech test fixture.
check_voice_prereqs never raises. The caller decides what to do with a missing
flag."""

from __future__ import annotations

import importlib
import shutil
from dataclasses import dataclass


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

    @property
    def ok(self) -> bool:
        """True only when the hard requirement is met.

        The hard requirement is a working speech engine."""
        return self.faster_whisper_available


def check_voice_prereqs() -> PrereqResult:
    return PrereqResult(
        faster_whisper_available=_importable("faster_whisper"),
        ffmpeg_available=shutil.which("ffmpeg") is not None,
        espeak_available=(shutil.which("espeak-ng") or shutil.which("espeak")) is not None,
    )
