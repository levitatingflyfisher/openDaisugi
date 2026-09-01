from __future__ import annotations

import sys
import types
from pathlib import Path

import pytest

from opendaisugi.config import Config
from opendaisugi.voice import engines, pins

from .conftest import make_wav, requires_faster_whisper
from .fixtures import generate_fixtures, has_speech_synthesis


def _word_error_rate(ref: str, hyp: str) -> float:
    def norm(s: str) -> list[str]:
        return "".join(c.lower() if c.isalnum() or c.isspace() else " " for c in s).split()

    r, h = norm(ref), norm(hyp)
    d = [[0] * (len(h) + 1) for _ in range(len(r) + 1)]
    for i in range(len(r) + 1):
        d[i][0] = i
    for j in range(len(h) + 1):
        d[0][j] = j
    for i in range(1, len(r) + 1):
        for j in range(1, len(h) + 1):
            cost = 0 if r[i - 1] == h[j - 1] else 1
            d[i][j] = min(d[i - 1][j] + 1, d[i][j - 1] + 1, d[i - 1][j - 1] + cost)
    return d[len(r)][len(h)] / max(1, len(r))


@requires_faster_whisper
def test_pick_engine_defaults_to_faster_whisper_on_cpu(monkeypatch):
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["model"] = model
            captured["device"] = device
            captured["compute_type"] = compute_type

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engine = engines.pick_engine(Config())
    assert isinstance(engine, engines.FasterWhisperEngine)
    assert captured["device"] == "cpu"
    assert captured["model"] == pins.FASTER_WHISPER_TEST_MODEL
    assert captured["compute_type"] == "int8"


@requires_faster_whisper
def test_pick_engine_forwards_a_non_default_compute_type(monkeypatch):
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["compute_type"] = compute_type

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.pick_engine(Config(voice_compute_type="float16"))
    assert captured["compute_type"] == "float16"


def test_pick_engine_raises_unknown_engine_for_a_bad_name():
    with pytest.raises(engines.UnknownEngine) as exc_info:
        engines.pick_engine(Config(voice_engine="parkeet-typo"))
    message = str(exc_info.value)
    assert "faster-whisper" in message
    assert "parakeet" in message


def test_pick_engine_routes_to_parakeet_and_raises_when_sherpa_onnx_missing(monkeypatch, tmp_path):
    monkeypatch.setitem(sys.modules, "sherpa_onnx", None)
    with pytest.raises(engines.EngineUnavailable, match="pip install"):
        engines.pick_engine(Config(voice_engine="parakeet", voice_model=str(tmp_path)))


@requires_faster_whisper
def test_faster_whisper_engine_falls_back_to_cpu_when_cuda_unavailable(monkeypatch):
    monkeypatch.setattr(engines, "_cuda_available", lambda: False)
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["device"] = device

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en", device="cuda", compute_type="int8")
    assert captured["device"] == "cpu"


@requires_faster_whisper
def test_faster_whisper_engine_keeps_cuda_when_available(monkeypatch):
    monkeypatch.setattr(engines, "_cuda_available", lambda: True)
    captured = {}

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            captured["device"] = device

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en", device="cuda", compute_type="int8")
    assert captured["device"] == "cuda"


@requires_faster_whisper
def test_faster_whisper_transcribe_maps_segments_text_and_duration(monkeypatch):
    class FakeSegment:
        def __init__(self, start, end, text):
            self.start = start
            self.end = end
            self.text = text

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type):
            pass

        def transcribe(self, audio, language=None):
            segments = [
                FakeSegment(0.0, 0.5, " hello"),
                FakeSegment(0.5, 1.0, " world "),
            ]
            return iter(segments), None

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engine = engines.FasterWhisperEngine("tiny.en", device="cpu", compute_type="int8")
    wav_bytes = make_wav(sr=16000, n_samples=16000)

    result = engine.transcribe(wav_bytes)

    assert result.text == "hello world"
    assert result.segments == [
        {"start": 0.0, "end": 0.5, "text": "hello"},
        {"start": 0.5, "end": 1.0, "text": "world"},
    ]
    assert result.duration_s == pytest.approx(1.0)
    assert result.rtf >= 0.0


def test_parakeet_raises_when_sherpa_onnx_not_installed(monkeypatch):
    monkeypatch.setitem(sys.modules, "sherpa_onnx", None)
    with pytest.raises(engines.EngineUnavailable, match="pip install"):
        engines.ParakeetEngine(Path("/nonexistent"))


def test_parakeet_raises_when_model_files_are_missing(monkeypatch, tmp_path):
    monkeypatch.setitem(sys.modules, "sherpa_onnx", types.SimpleNamespace(OfflineRecognizer=None))
    with pytest.raises(engines.EngineUnavailable, match="Download and extract"):
        engines.ParakeetEngine(tmp_path)


@requires_faster_whisper
@pytest.mark.voice_live
def test_faster_whisper_transcribes_fixtures_within_the_recorded_wer_ceiling(tmp_path):
    if not has_speech_synthesis():
        pytest.skip("espeak-ng and/or ffmpeg not on PATH: no real speech fixture to score")
    transcripts = generate_fixtures(tmp_path)
    engine = engines.FasterWhisperEngine(
        pins.FASTER_WHISPER_TEST_MODEL, device="cpu", compute_type="int8"
    )
    total_wer = 0.0
    for name, expected in transcripts.items():
        wav_bytes = (tmp_path / name).read_bytes()
        result = engine.transcribe(wav_bytes)
        wer = _word_error_rate(expected, result.text)
        assert wer <= pins.FIXTURE_MAX_WER[name], f"{name}: wer {wer:.3f}, got {result.text!r}"
        total_wer += wer
    assert total_wer / len(transcripts) <= pins.FIXTURE_MEAN_MAX_WER
