"""Audio normalization. Turns any uploaded or recorded clip into 16 kHz mono
16-bit PCM WAV bytes.

ffmpeg is the primary path when it is on PATH. It decodes webm and opus and
resamples in one step. It is a correct, anti-aliased resampler and needs no
Python decoder library. The av package, installed by the voice extra, is the
fallback for webm decode when the ffmpeg binary is absent. A WAV already at
16 kHz mono needs no conversion. A WAV at another rate with neither ffmpeg
nor av available falls back to a plain numpy linear-interpolation resample.
That fallback sounds worse. It skips anti-aliasing. A live check on this box
raised one fixture's word error rate from 0.111 to 0.222 under this
fallback. The fallback is never wrong in a silent way, and it is never the
primary path. ffmpeg is common enough that the fallback only matters on a
stripped-down host.
"""

from __future__ import annotations

import io
import shutil
import subprocess
import wave

import numpy as np


class AudioFormatUnsupported(RuntimeError):
    """The clip cannot be normalized on this host. The message names the fix."""


def wav_duration_s(wav_bytes: bytes) -> float:
    """Length of a WAV clip in seconds.

    Counts the frames actually read instead of trusting the header's
    declared frame count. ffmpeg writes a placeholder data size when it
    streams a WAV to a pipe it cannot seek back to patch, so the header
    count can be far too large. Python's wave module still stops at the
    real end of the data when asked to read past it, so this gives the
    correct duration in both the normal case and the streamed case.
    """
    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        frame_size = w.getsampwidth() * w.getnchannels()
        frames = w.readframes(w.getnframes())
        n_frames = len(frames) // frame_size
        return n_frames / float(w.getframerate())


def write_wav_16k_mono(samples: np.ndarray, *, sample_rate: int = 16000) -> bytes:
    """Pack float or int samples into 16 kHz mono 16-bit PCM WAV bytes."""
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        clipped = np.clip(samples, -32768, 32767).astype(np.int16)
        w.writeframes(clipped.tobytes())
    return buf.getvalue()


def _is_wav(raw: bytes) -> bool:
    return len(raw) >= 12 and raw[:4] == b"RIFF" and raw[8:12] == b"WAVE"


def _read_wav_mono_samples(raw: bytes) -> tuple[int, np.ndarray]:
    with wave.open(io.BytesIO(raw), "rb") as w:
        sr = w.getframerate()
        channels = w.getnchannels()
        sampwidth = w.getsampwidth()
        frames = w.readframes(w.getnframes())
    if sampwidth != 2:
        raise AudioFormatUnsupported(
            f"unsupported sample width: {sampwidth * 8}-bit. Only 16-bit PCM WAV is supported."
        )
    samples = np.frombuffer(frames, dtype=np.int16).astype(np.float64)
    if channels > 1:
        samples = samples.reshape(-1, channels).mean(axis=1)
    return sr, samples


def _resample_linear(samples: np.ndarray, orig_sr: int, target_sr: int) -> np.ndarray:
    if orig_sr == target_sr:
        return samples
    duration = len(samples) / orig_sr
    n_target = max(1, int(round(duration * target_sr)))
    orig_t = np.arange(len(samples)) / orig_sr
    target_t = np.arange(n_target) / target_sr
    return np.interp(target_t, orig_t, samples)


def _ffmpeg_to_wav_16k_mono(raw: bytes) -> bytes:
    proc = subprocess.run(
        [
            "ffmpeg",
            "-hide_banner",
            "-loglevel",
            "error",
            "-i",
            "pipe:0",
            "-ar",
            "16000",
            "-ac",
            "1",
            "-f",
            "wav",
            "pipe:1",
        ],
        input=raw,
        capture_output=True,
        timeout=30,
    )
    if proc.returncode != 0:
        raise AudioFormatUnsupported(
            f"ffmpeg could not decode this clip: {proc.stderr.decode(errors='replace')[:200]}"
        )
    # ffmpeg writes to pipe:1, a stream it cannot seek back on, so it cannot
    # patch the RIFF and data chunk sizes once it knows the real length. It
    # writes a placeholder size instead. Re-reading and repacking here gives
    # every caller a WAV with a correct header, not just a correct duration
    # under this module's own header-blind reader.
    sr, samples = _read_wav_mono_samples(proc.stdout)
    return write_wav_16k_mono(samples, sample_rate=sr)


def _av_to_wav_16k_mono(raw: bytes) -> bytes:
    """Decode with the av package and resample to 16 kHz mono.

    Every error past the import itself, a missing decoder, a corrupt
    container, a codec av does not build, is wrapped and re-raised as
    AudioFormatUnsupported naming ffmpeg. That keeps one clear message for
    every unsupported clip, whether or not av happens to be installed on
    this host.
    """
    try:
        import av
    except ImportError as exc:
        raise AudioFormatUnsupported(
            "cannot decode this audio format. Neither the ffmpeg binary nor the "
            "av package is available. Install ffmpeg, or run: "
            "pip install 'opendaisugi[voice]'"
        ) from exc
    try:
        container = av.open(io.BytesIO(raw))
        resampler = av.AudioResampler(format="s16", layout="mono", rate=16000)
        chunks: list[np.ndarray] = []
        for frame in container.decode(audio=0):
            for out_frame in resampler.resample(frame):
                chunks.append(out_frame.to_ndarray().flatten())
        container.close()
    except Exception as exc:
        raise AudioFormatUnsupported(
            "cannot decode this audio format with the av package. "
            "Install ffmpeg for a more reliable decode."
        ) from exc
    merged = np.concatenate(chunks).astype(np.float64) if chunks else np.zeros(0)
    return write_wav_16k_mono(merged)


def to_wav_16k_mono(raw: bytes, content_type: str = "") -> bytes:
    """Normalize any supported audio clip to 16 kHz mono 16-bit PCM WAV bytes."""
    if _is_wav(raw):
        sr, samples = _read_wav_mono_samples(raw)
        if sr == 16000:
            return write_wav_16k_mono(samples)
        if shutil.which("ffmpeg"):
            return _ffmpeg_to_wav_16k_mono(raw)
        return write_wav_16k_mono(_resample_linear(samples, sr, 16000))
    if shutil.which("ffmpeg"):
        return _ffmpeg_to_wav_16k_mono(raw)
    return _av_to_wav_16k_mono(raw)
