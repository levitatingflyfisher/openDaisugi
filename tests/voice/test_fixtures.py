from __future__ import annotations

import wave

from opendaisugi.voice import pins

from .fixtures import generate_fixtures, has_speech_synthesis


def test_generate_fixtures_writes_three_16k_mono_wavs(tmp_path):
    transcripts = generate_fixtures(tmp_path)
    assert set(transcripts) == set(pins.FIXTURE_TRANSCRIPTS)
    for name in pins.FIXTURE_TRANSCRIPTS:
        path = tmp_path / name
        assert path.exists()
        with wave.open(str(path), "rb") as w:
            assert w.getframerate() == 16000
            assert w.getnchannels() == 1
            duration_s = w.getnframes() / w.getframerate()
            assert 0.5 <= duration_s <= 10.0


def test_generate_fixtures_returns_real_transcripts_when_speech_synthesis_available(tmp_path):
    if not has_speech_synthesis():
        import pytest

        pytest.skip("espeak-ng and/or ffmpeg not on PATH")
    transcripts = generate_fixtures(tmp_path)
    assert transcripts == pins.FIXTURE_TRANSCRIPTS


def test_generate_fixtures_returns_none_transcripts_on_the_tone_fallback(tmp_path, monkeypatch):
    monkeypatch.setattr("tests.voice.fixtures.has_speech_synthesis", lambda: False)
    transcripts = generate_fixtures(tmp_path)
    assert all(v is None for v in transcripts.values())
    assert set(transcripts) == set(pins.FIXTURE_TRANSCRIPTS)
