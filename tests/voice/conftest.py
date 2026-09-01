"""Shared fixtures and fakes for the voice test suite.

Later voice tests add their fixtures and fakes here, in one place, so no two
test files carry their own copy that can drift apart.
"""

from __future__ import annotations

import io
import wave
from dataclasses import dataclass

import numpy as np
import pytest

from opendaisugi.voice.prereq import check_voice_prereqs

requires_faster_whisper = pytest.mark.skipif(
    not check_voice_prereqs().faster_whisper_available,
    reason="faster-whisper not installed. Run pip install 'opendaisugi[voice]'.",
)


def make_wav(*, sr: int, n_samples: int, channels: int = 1) -> bytes:
    """Build a 440 Hz tone as 16-bit PCM WAV bytes at the given rate and channel count.

    Every voice test that needs a real WAV file on disk or in memory builds it
    with this one helper, so no two tests carry a copy that can drift apart.
    """
    t = np.arange(n_samples) / sr
    tone = (np.sin(2 * np.pi * 440 * t) * 10000).astype(np.int16)
    if channels == 2:
        tone = np.repeat(tone, 2)
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(channels)
        w.setsampwidth(2)
        w.setframerate(sr)
        w.writeframes(tone.tobytes())
    return buf.getvalue()


@dataclass(frozen=True)
class FakePaneRef:
    """Matches the shape of opendaisugi.floor.backend.PaneRef without importing it."""

    backend: str
    id: str


class FakeBackend:
    """A stand-in pane backend. deliver() only ever forwards this object to its
    injected send callable, so this class carries no behavior of its own.
    name is the one attribute a caller may read directly, to build a pane
    reference when no other backend name was given."""

    name = "fake"


class RecordingSend:
    """A fake send callable. Records every call it receives."""

    def __init__(self) -> None:
        self.calls: list[tuple[object, object, str]] = []

    def __call__(self, backend, pane, text: str) -> str:
        self.calls.append((backend, pane, text))
        return "typed"


class FakeStream:
    """A stand-in microphone stream. Push-to-talk only ever calls start, stop,
    and read on a stream, so this class carries no other behavior."""

    def __init__(self, chunks: list[bytes]) -> None:
        self.chunks = list(chunks)
        self.started = False
        self.stopped = False

    def start(self) -> None:
        self.started = True

    def stop(self) -> None:
        self.stopped = True

    def read(self, frames: int) -> tuple[bytes, bool]:
        if not self.chunks:
            return b"", False
        chunk = self.chunks.pop(0)
        return chunk, bool(self.chunks)


class FakeClient:
    """A stand-in voice server client. Records every transcribe and deliver
    call it receives, and answers a fixed transcript and a fixed preview
    delivery."""

    def __init__(self, text: str = "hello world") -> None:
        self.text = text
        self.transcribe_calls: list[bytes] = []
        self.deliver_calls: list[tuple[str, str, str]] = []

    def transcribe(self, wav_bytes: bytes) -> dict:
        self.transcribe_calls.append(wav_bytes)
        return {"text": self.text}

    def deliver(self, pane: str, text: str, *, mode: str) -> dict:
        self.deliver_calls.append((pane, text, mode))
        return {"delivered": "preview"}
