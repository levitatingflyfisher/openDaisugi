"""Deterministic speech fixtures for the voice engine tests.

espeak-ng is the TTS this repo can rely on for a real-speech fixture without
a network call. It runs formant synthesis offline and needs no model
download. When espeak-ng or espeak, plus ffmpeg, are both on PATH,
generate_fixtures synthesizes three short clips with known transcripts. It
resamples them through opendaisugi.voice.audio.to_wav_16k_mono, the exact
path production code uses. When either tool is missing, it falls back to
three numpy sine-wave tones. A tone is not speech. Callers must treat a
None transcript as a sign that no WER assertion is possible. Skip with a
reason instead. Scoring a WER against a tone would be a meaningless number.
That is worse than skipping.
"""

from __future__ import annotations

import shutil
import subprocess
import wave
from pathlib import Path

import numpy as np

from opendaisugi.voice import pins
from opendaisugi.voice.audio import to_wav_16k_mono


def _espeak_binary() -> str | None:
    return shutil.which("espeak-ng") or shutil.which("espeak")


def has_speech_synthesis() -> bool:
    """True only when a real-speech fixture can be produced.

    That needs a TTS binary and ffmpeg. ffmpeg is required here, not merely
    preferred. See the audio module docstring for why the numpy fallback
    sounds measurably worse. A fixture must never use that fallback.
    """
    return _espeak_binary() is not None and shutil.which("ffmpeg") is not None


def generate_fixtures(out_dir: Path) -> dict[str, str | None]:
    """Write the three pins.FIXTURE_TRANSCRIPTS filenames into out_dir as
    16 kHz mono WAVs.

    Returns a dict of filename to expected transcript, or filename to None
    for every entry when the tone fallback ran.
    """
    out_dir.mkdir(parents=True, exist_ok=True)
    if has_speech_synthesis():
        return _generate_speech_fixtures(out_dir)
    return _generate_tone_fixtures(out_dir)


def _generate_speech_fixtures(out_dir: Path) -> dict[str, str | None]:
    binary = _espeak_binary()
    assert binary is not None  # has_speech_synthesis already checked this
    result: dict[str, str | None] = {}
    for name, text in pins.FIXTURE_TRANSCRIPTS.items():
        raw_path = out_dir / f"_raw_{name}"
        subprocess.run(
            [binary, "-w", str(raw_path), text], check=True, capture_output=True, timeout=10
        )
        wav_bytes = to_wav_16k_mono(raw_path.read_bytes(), "audio/wav")
        (out_dir / name).write_bytes(wav_bytes)
        raw_path.unlink(missing_ok=True)
        result[name] = text
    return result


def _generate_tone_fixtures(out_dir: Path) -> dict[str, str | None]:
    result: dict[str, str | None] = {}
    for i, name in enumerate(pins.FIXTURE_TRANSCRIPTS, start=1):
        freq = 220.0 * i
        sr = 16000
        duration_s = 2.0
        t = np.arange(int(sr * duration_s)) / sr
        samples = (np.sin(2 * np.pi * freq * t) * 10000).astype(np.int16)
        with wave.open(str(out_dir / name), "wb") as w:
            w.setnchannels(1)
            w.setsampwidth(2)
            w.setframerate(sr)
            w.writeframes(samples.tobytes())
        result[name] = None
    return result
