from __future__ import annotations

import io
import shutil
import struct
import subprocess
import wave

import numpy as np
import pytest

from opendaisugi.voice.audio import (
    AudioFormatUnsupported,
    to_wav_16k_mono,
    wav_duration_s,
    write_wav_16k_mono,
)

from .conftest import make_wav


def test_wav_duration_s_matches_frame_count_over_sample_rate():
    wav_bytes = make_wav(sr=16000, n_samples=32000)
    assert wav_duration_s(wav_bytes) == pytest.approx(2.0, abs=1e-3)


def _make_wav_with_placeholder_data_size(*, sr: int, n_samples: int) -> bytes:
    """Hand-build a mono 16-bit PCM WAV whose data chunk size field is
    wrong on purpose.

    A real WAV writer that cannot seek back on its output, such as ffmpeg
    writing to a pipe, sometimes writes 0xFFFFFFFF as a placeholder for a
    chunk size it does not know yet. This builds a WAV with that same
    placeholder over a real, correctly sized payload, the way a reader
    that trusts the header would see one from such a writer.
    """
    t = np.arange(n_samples) / sr
    samples = (np.sin(2 * np.pi * 440 * t) * 10000).astype(np.int16)
    payload = samples.tobytes()
    fmt_chunk = struct.pack("<HHIIHH", 1, 1, sr, sr * 2, 2, 16)
    placeholder_size = 0xFFFFFFFF
    return (
        b"RIFF"
        + struct.pack("<I", placeholder_size)
        + b"WAVE"
        + b"fmt "
        + struct.pack("<I", len(fmt_chunk))
        + fmt_chunk
        + b"data"
        + struct.pack("<I", placeholder_size)
        + payload
    )


def test_wav_duration_s_ignores_a_placeholder_data_chunk_size():
    wav_bytes = _make_wav_with_placeholder_data_size(sr=16000, n_samples=32000)
    assert wav_duration_s(wav_bytes) == pytest.approx(2.0, abs=1e-3)


def test_write_wav_16k_mono_roundtrips_through_wav_duration_s():
    samples = np.zeros(16000, dtype=np.float64)
    wav_bytes = write_wav_16k_mono(samples)
    assert wav_bytes[:4] == b"RIFF"
    assert wav_duration_s(wav_bytes) == pytest.approx(1.0, abs=1e-3)


def test_to_wav_16k_mono_passes_through_a_wav_already_at_16k_mono():
    wav_bytes = make_wav(sr=16000, n_samples=16000)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    assert wav_duration_s(out) == pytest.approx(1.0, abs=1e-3)
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_resamples_a_22050hz_wav_via_ffmpeg():
    wav_bytes = make_wav(sr=22050, n_samples=22050 * 2, channels=2)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1
    assert wav_duration_s(out) == pytest.approx(2.0, abs=0.05)


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_ffmpeg_path_returns_a_wav_with_a_correct_header():
    """ffmpeg writes to a pipe it cannot seek back on, so it cannot patch
    the RIFF and data chunk sizes once it knows the real length. It writes
    a placeholder size instead. The bytes to_wav_16k_mono returns must
    carry the real size, not that placeholder, so any reader that trusts
    the header, not only this module's own frame-counting reader, gets a
    correct answer.
    """
    wav_bytes = make_wav(sr=22050, n_samples=22050 * 2, channels=2)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    riff_size = struct.unpack("<I", out[4:8])[0]
    assert riff_size == len(out) - 8
    data_index = out.find(b"data")
    data_size = struct.unpack("<I", out[data_index + 4 : data_index + 8])[0]
    assert data_size == len(out) - data_index - 8


def test_to_wav_16k_mono_falls_back_to_numpy_resample_without_ffmpeg(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.audio.shutil.which", lambda name: None)
    wav_bytes = make_wav(sr=8000, n_samples=8000)
    out = to_wav_16k_mono(wav_bytes, "audio/wav")
    with wave.open(io.BytesIO(out), "rb") as w:
        assert w.getframerate() == 16000
    assert wav_duration_s(out) == pytest.approx(1.0, abs=1e-3)


def test_to_wav_16k_mono_rejects_a_non_16_bit_wav():
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(1)  # 8-bit, unsupported
        w.setframerate(16000)
        w.writeframes(b"\x00" * 16000)
    with pytest.raises(AudioFormatUnsupported):
        to_wav_16k_mono(buf.getvalue(), "audio/wav")


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_decodes_webm_via_ffmpeg():
    wav_bytes = make_wav(sr=16000, n_samples=16000)
    proc = subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-hide_banner",
            "-loglevel",
            "error",
            "-i",
            "pipe:0",
            "-f",
            "webm",
            "pipe:1",
        ],
        input=wav_bytes,
        capture_output=True,
        timeout=15,
    )
    assert proc.returncode == 0, proc.stderr
    out = to_wav_16k_mono(proc.stdout, "audio/webm")
    assert wav_duration_s(out) == pytest.approx(1.0, abs=0.1)


@pytest.mark.skipif(shutil.which("ffmpeg") is None, reason="ffmpeg not on PATH")
def test_to_wav_16k_mono_decodes_webm_via_av_when_ffmpeg_is_absent(monkeypatch):
    pytest.importorskip("av")
    wav_bytes = make_wav(sr=16000, n_samples=16000)
    proc = subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-hide_banner",
            "-loglevel",
            "error",
            "-i",
            "pipe:0",
            "-f",
            "webm",
            "pipe:1",
        ],
        input=wav_bytes,
        capture_output=True,
        timeout=15,
    )
    assert proc.returncode == 0, proc.stderr
    monkeypatch.setattr("opendaisugi.voice.audio.shutil.which", lambda name: None)
    out = to_wav_16k_mono(proc.stdout, "audio/webm")
    assert wav_duration_s(out) == pytest.approx(1.0, abs=0.1)


def test_to_wav_16k_mono_raises_a_teaching_error_for_webm_with_no_decoder(monkeypatch):
    monkeypatch.setattr("opendaisugi.voice.audio.shutil.which", lambda name: None)
    with pytest.raises(AudioFormatUnsupported, match="ffmpeg"):
        to_wav_16k_mono(b"not a real webm file", "audio/webm")
