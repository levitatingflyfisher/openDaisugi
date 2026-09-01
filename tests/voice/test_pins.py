"""Tests for the voice bridge's pinned upstream facts."""

from __future__ import annotations

from opendaisugi.voice import pins

from .conftest import requires_faster_whisper


def test_faster_whisper_test_model_is_a_recognized_name():
    assert pins.FASTER_WHISPER_TEST_MODEL in pins.FASTER_WHISPER_MODEL_NAMES


@requires_faster_whisper
def test_faster_whisper_model_names_is_a_subset_of_the_real_model_list():
    from faster_whisper.utils import _MODELS

    assert pins.FASTER_WHISPER_MODEL_NAMES <= set(_MODELS)


def test_parakeet_url_matches_the_archive_name():
    assert pins.PARAKEET_MODEL_URL.endswith(pins.PARAKEET_MODEL_ARCHIVE + ".tar.bz2")


def test_fixture_ceilings_cover_every_fixture_and_stay_above_the_recorded_value():
    assert set(pins.FIXTURE_MAX_WER) == set(pins.FIXTURE_TRANSCRIPTS)
    assert set(pins.FIXTURE_OBSERVED_WER) == set(pins.FIXTURE_TRANSCRIPTS)
    for name, ceiling in pins.FIXTURE_MAX_WER.items():
        observed = pins.FIXTURE_OBSERVED_WER[name]
        assert observed <= ceiling, f"{name}: ceiling {ceiling} falls below the observed {observed}"
        assert ceiling <= 0.15, f"{name}: ceiling {ceiling} exceeds the 0.15 cap"
    assert 0.0 < pins.FIXTURE_MEAN_MAX_WER <= 0.15


def test_parakeet_model_type_is_the_nemo_variant():
    # Confirmed live: sherpa_onnx.OfflineRecognizer.from_transducer(model_type=...)
    # needs "nemo_transducer" for a NeMo Parakeet-TDT export. Plain "transducer",
    # the parameter's own default, is for icefall or k2-style exports. It will not
    # decode a Parakeet model's duration head correctly.
    assert pins.PARAKEET_MODEL_TYPE == "nemo_transducer"
