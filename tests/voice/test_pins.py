"""Tests for the voice bridge's pinned upstream facts."""

from __future__ import annotations

import pytest

from opendaisugi.voice import pins

from .conftest import requires_faster_whisper


def test_faster_whisper_test_model_is_a_recognized_name():
    assert pins.FASTER_WHISPER_TEST_MODEL in pins.FASTER_WHISPER_MODEL_NAMES


@requires_faster_whisper
def test_faster_whisper_model_names_is_a_subset_of_the_real_model_list():
    from faster_whisper.utils import _MODELS

    assert pins.FASTER_WHISPER_MODEL_NAMES <= set(_MODELS)


def test_no_sherpa_onnx_pins_remain():
    assert not [name for name in vars(pins) if "SHERPA" in name]


def test_the_parakeet_model_pins_an_f16_source_and_its_q4_k_file():
    m = pins.PARAKEET_MODELS[pins.PARAKEET_DEFAULT_MODEL]
    assert m.source.name.endswith(".gguf") and m.quantized.name.endswith("-q4_k.gguf")
    for f in (m.source, m.quantized):
        assert len(f.sha256) == 64 and f.size > 0


def test_fixture_ceilings_cover_every_fixture_and_stay_above_the_recorded_value():
    assert set(pins.FIXTURE_MAX_WER) == set(pins.FIXTURE_TRANSCRIPTS)
    assert set(pins.FIXTURE_OBSERVED_WER) == set(pins.FIXTURE_TRANSCRIPTS)
    for name, ceiling in pins.FIXTURE_MAX_WER.items():
        observed = pins.FIXTURE_OBSERVED_WER[name]
        assert observed <= ceiling, f"{name}: ceiling {ceiling} falls below the observed {observed}"
        assert ceiling <= 0.15, f"{name}: ceiling {ceiling} exceeds the 0.15 cap"
    assert 0.0 < pins.FIXTURE_MEAN_MAX_WER <= 0.15


def test_every_allowlisted_faster_whisper_model_is_fetched_at_a_pinned_commit():
    assert set(pins.FASTER_WHISPER_REVISIONS) == pins.FASTER_WHISPER_MODEL_NAMES
    for name, rev in pins.FASTER_WHISPER_REVISIONS.items():
        assert len(rev) == 40 and int(rev, 16) >= 0, name


def test_the_parakeet_file_is_fetched_at_its_pinned_commit():
    assert len(pins.PARAKEET_REVISION) == 40
    assert pins.PARAKEET_BASE_URL.endswith("/resolve/" + pins.PARAKEET_REVISION)


def test_a_machine_with_no_pinned_q4_k_result_refuses_before_it_fetches(monkeypatch, tmp_path):
    from opendaisugi.voice import models

    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "cache"))
    monkeypatch.setattr(models, "fetch", lambda *a, **k: pytest.fail("fetched"))
    said: list[str] = []
    with pytest.raises(models.QuantizeError) as exc:
        models.ensure_parakeet("v2", tmp_path / "q", said.append, arch="aarch64")
    assert str(exc.value) == models.parakeet_quant_refusal("v2", "aarch64")
    assert "aarch64" in str(exc.value) and said == []
    assert models.parakeet_quant_refusal("v2", "x86_64") is None
