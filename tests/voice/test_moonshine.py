"""Tests for the Moonshine engine: moonshine-cli as an external process,
its pinned models, and the engine a box with no voice config gets."""

from __future__ import annotations

import sys
from pathlib import Path

import pytest

from opendaisugi._model_fetch import FetchVerificationError
from opendaisugi.config import Config
from opendaisugi.voice import engines, models, pins
from opendaisugi.voice.prereq import PrereqResult, check_voice_prereqs


def _fake_moonshine_cli(bin_dir: Path, body: str) -> Path:
    bin_dir.mkdir(parents=True, exist_ok=True)
    script = bin_dir / "moonshine-cli"
    script.write_text("#!/bin/sh\n" + body)
    script.chmod(0o755)
    return script


def _model_dir(tmp_path: Path) -> Path:
    d = tmp_path / "model"
    d.mkdir()
    return d


# --- pins ------------------------------------------------------------------


def test_the_curated_models_are_tiny_small_and_medium_streaming_english():
    assert set(pins.MOONSHINE_MODELS) == {"tiny", "small", "medium"}
    names = [f.name for f in pins.MOONSHINE_MODELS["small"]]
    assert names == [
        "adapter.ort",
        "cross_kv.ort",
        "decoder_kv.ort",
        "encoder.ort",
        "frontend.model.ort",
        "frontend.weights.ort",
        "streaming_config.json",
        "tokenizer.bin",
    ]
    for files in pins.MOONSHINE_MODELS.values():
        assert all(len(f.sha256) == 64 and f.size > 0 for f in files)
    assert pins.MOONSHINE_DEFAULT_MODEL == "small"


def test_model_sizes_round_to_megabytes():
    assert models.moonshine_size_mb("tiny") == 45
    assert models.moonshine_size_mb("small") == 142
    assert models.moonshine_size_mb("medium") == 269


def test_the_cache_follows_xdg_cache_home_when_it_is_absolute(monkeypatch, tmp_path):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "c"))
    assert models.moonshine_dir("small") == (
        tmp_path / "c/opendaisugi/models/moonshine/small-streaming-en" / pins.MOONSHINE_REVISION
    )
    monkeypatch.setenv("XDG_CACHE_HOME", "relative")
    monkeypatch.setenv("HOME", str(tmp_path / "h"))
    assert models.moonshine_dir("tiny").parts[-7:-2] == (
        "h",
        ".cache",
        "opendaisugi",
        "models",
        "moonshine",
    )


def test_the_base_url_can_be_moved_for_tests(monkeypatch):
    monkeypatch.delenv(pins.MOONSHINE_BASE_URL_ENV, raising=False)
    assert models.moonshine_url("small", "encoder.ort") == (
        "https://download.moonshine.ai/model/small-streaming-en/quantized_26_08_21/encoder.ort"
    )
    monkeypatch.setenv(pins.MOONSHINE_BASE_URL_ENV, "http://127.0.0.1:1/m")
    assert models.moonshine_url("tiny", "a").startswith("http://127.0.0.1:1/m/tiny-streaming-en/")


# --- the command line and its output ----------------------------------------


def test_moonshine_args_run_the_program_resident():
    args = engines.moonshine_args("/b/moonshine-cli", Path("/m/small"), "small")
    assert args == ["/b/moonshine-cli", "-m", "/m/small", "-a", "small", "--resident"]


def test_moonshine_engine_is_unavailable_with_no_binary_on_path(monkeypatch, tmp_path):
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    with pytest.raises(engines.EngineUnavailable, match="moonshine-cli is not on PATH"):
        engines.MoonshineEngine(str(_model_dir(tmp_path)))


def test_moonshine_engine_is_unavailable_with_no_model_dir(monkeypatch, tmp_path):
    _fake_moonshine_cli(tmp_path / "bin", "exit 0\n")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    with pytest.raises(engines.EngineUnavailable, match="Moonshine model directory missing"):
        engines.MoonshineEngine(str(tmp_path / "absent"))


# --- the curated models, fetched once ---------------------------------------


@pytest.fixture
def cache(monkeypatch, tmp_path):
    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path / "cache"))
    _fake_moonshine_cli(tmp_path / "bin", "exit 0\n")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    return tmp_path / "cache"


def test_a_curated_model_is_fetched_once_with_one_line(monkeypatch, cache, capsys):
    calls = []

    def fake_fetch(url, sha256, dest, *, size=None, notice=True):
        calls.append((url, notice))
        Path(dest).parent.mkdir(parents=True, exist_ok=True)
        Path(dest).write_bytes(b"x")
        return Path(dest)

    monkeypatch.setattr(models, "fetch", fake_fetch)
    monkeypatch.setattr(models, "_matches", lambda path, sha: path.exists())
    engine = engines.MoonshineEngine("small")
    assert engine.model == models.moonshine_dir("small")
    assert engine.arch == "small"
    assert len(calls) == 8 and not any(n for _, n in calls)
    err = capsys.readouterr().err.splitlines()
    assert err == [
        f"Fetching the Moonshine small model (142 MB) into {models.moonshine_dir('small')}. "
        "This happens once."
    ]
    engines.MoonshineEngine("small")
    assert len(calls) == 8
    assert capsys.readouterr().err == ""


def test_a_failed_fetch_names_the_command_to_retry(monkeypatch, cache):
    def fail(url, sha256, dest, *, size=None, notice=True):
        raise OSError("could not download")

    monkeypatch.setattr(models, "fetch", fail)
    with pytest.raises(engines.EngineUnavailable) as exc_info:
        engines.MoonshineEngine("tiny")
    assert str(exc_info.value) == (
        "The Moonshine tiny model could not be fetched. "
        "Check the network, then run daisugi voice serve again."
    )
    assert "not installed" not in str(exc_info.value)


def test_a_file_that_fails_its_pin_says_so(monkeypatch, cache):
    def bad(url, sha256, dest, *, size=None, notice=True):
        raise FetchVerificationError("mismatch")

    monkeypatch.setattr(models, "fetch", bad)
    with pytest.raises(engines.EngineUnavailable) as exc_info:
        engines.MoonshineEngine("medium")
    assert str(exc_info.value) == (
        "A file of the Moonshine medium model did not match its pinned sha256 and was "
        "deleted. Run daisugi voice serve again to fetch it again."
    )


# --- which engine a config picks ---------------------------------------------


def test_moonshine_with_no_model_set_uses_the_default_model(monkeypatch, cache):
    monkeypatch.setattr(models, "ensure_moonshine", lambda name, say: Path("/m") / name)
    engine = engines.pick_engine(Config(voice_engine="moonshine"))
    assert isinstance(engine, engines.MoonshineEngine)
    assert engine.model == Path("/m/small") and engine.arch == "small"


def test_moonshine_with_a_model_dir(monkeypatch, tmp_path):
    _fake_moonshine_cli(tmp_path / "bin", "exit 0\n")
    monkeypatch.setenv("PATH", str(tmp_path / "bin"))
    model = _model_dir(tmp_path)
    engine = engines.pick_engine(Config(voice_engine="moonshine", voice_model=str(model)))
    assert engine.model == model and engine.arch == pins.MOONSHINE_DIR_ARCH


def test_no_voice_config_without_faster_whisper_says_what_the_hardware_picked(
    monkeypatch, cache, capsys
):
    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    monkeypatch.setattr(models, "ensure_moonshine", lambda name, say: Path("/m") / name)
    engine = engines.pick_engine(Config(), hardware=engines.VoiceHardware(4.0, 2, 0.0))
    assert isinstance(engine, engines.MoonshineEngine)
    assert engine.arch == "small"
    assert capsys.readouterr().err.splitlines() == [
        "No voice engine is set, so voice uses Moonshine small: this box has 4 GB of RAM "
        "and 2 cores."
    ]


def test_a_saved_config_with_the_default_values_counts_as_no_config(monkeypatch, cache, tmp_path):
    # save_config writes every field, so a box that ever saved its config
    # names faster-whisper and tiny.en. Those are the defaults: no choice.
    from opendaisugi.config import load_config, save_config

    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    monkeypatch.setattr(models, "ensure_moonshine", lambda name, say: Path("/m") / name)
    save_config(Config(matcher_model="lexical"), tmp_path / "config.yaml")
    saved = load_config(tmp_path / "config.yaml")
    assert "voice_engine" in saved.model_fields_set
    small = engines.VoiceHardware(4.0, 2, 0.0)
    assert isinstance(engines.pick_engine(saved, hardware=small), engines.MoonshineEngine)
    named = Config(voice_engine="faster-whisper", voice_model="tiny.en")
    assert isinstance(engines.pick_engine(named, hardware=small), engines.MoonshineEngine)


def test_no_voice_config_on_a_small_box_with_faster_whisper_keeps_faster_whisper(monkeypatch):

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            pass

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    tiny_box = engines.VoiceHardware(1.5, 1, 0.0)
    assert isinstance(engines.pick_engine(Config(), hardware=tiny_box), engines.FasterWhisperEngine)


def test_faster_whisper_with_another_model_is_never_replaced(monkeypatch):
    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    with pytest.raises(engines.EngineUnavailable, match="opendaisugi\\[voice\\]"):
        engines.pick_engine(Config(voice_engine="faster-whisper", voice_model="base.en"))
    with pytest.raises(engines.EngineUnavailable, match="opendaisugi\\[voice\\]"):
        engines.pick_engine(Config(voice_model="base.en"))


def test_moonshine_with_the_default_model_value_uses_small(monkeypatch, cache):
    monkeypatch.setattr(models, "ensure_moonshine", lambda name, say: Path("/m") / name)
    engine = engines.pick_engine(Config(voice_engine="moonshine", voice_model="tiny.en"))
    assert engine.arch == "small"


def test_valid_names_include_moonshine():
    with pytest.raises(
        engines.UnknownEngine, match="faster-whisper, moonshine, parakeet, whisper.cpp[.]"
    ):
        engines.pick_engine(Config(voice_engine="nope"))


# --- faster-whisper's own first use ------------------------------------------


def test_faster_whisper_pins_the_default_model_revision(monkeypatch, capsys):
    seen = []

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, *, local_files_only=False, revision=None):
            seen.append((local_files_only, revision))

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en")
    assert seen == [(True, pins.FASTER_WHISPER_REVISIONS["tiny.en"])]
    assert capsys.readouterr().err == ""


def _local_entry_not_found():
    return pytest.importorskip("huggingface_hub.errors").LocalEntryNotFoundError


def test_faster_whisper_first_use_says_so_with_the_size(monkeypatch, capsys):
    LocalEntryNotFoundError = _local_entry_not_found()  # noqa: N806
    seen = []

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, *, local_files_only=False, revision=None):
            seen.append(local_files_only)
            if local_files_only:
                raise LocalEntryNotFoundError("not cached")

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    engines.FasterWhisperEngine("tiny.en")
    assert seen == [True, False]
    assert capsys.readouterr().err.splitlines() == [
        "Fetching the faster-whisper tiny.en model (78 MB). This happens once."
    ]


def test_faster_whisper_failed_fetch_names_the_command_to_retry(monkeypatch):
    LocalEntryNotFoundError = _local_entry_not_found()  # noqa: N806

    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, *, local_files_only=False, revision=None):
            if local_files_only:
                raise LocalEntryNotFoundError("not cached")
            raise OSError("offline")

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    with pytest.raises(engines.EngineUnavailable) as exc_info:
        engines.FasterWhisperEngine("base.en")
    assert str(exc_info.value) == (
        "The faster-whisper base.en model could not be fetched. "
        "Check the network, then run daisugi voice serve again."
    )


def test_faster_whisper_other_load_errors_are_not_a_fetch(monkeypatch, capsys):
    # A load that fails for any reason but a model missing from the cache
    # (an older faster-whisper with no revision keyword, a broken model)
    # is that error, not a fetch.
    class FakeWhisperModel:
        def __init__(self, model, device, compute_type, **kw):
            raise TypeError("unexpected keyword argument 'revision'")

    monkeypatch.setattr("faster_whisper.WhisperModel", FakeWhisperModel, raising=False)
    with pytest.raises(TypeError):
        engines.FasterWhisperEngine("tiny.en")
    assert capsys.readouterr().err == ""


# --- prereq -------------------------------------------------------------------


def test_moonshine_cli_on_path_is_a_speech_engine(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: "/x" if name == "moonshine-cli" else None)
    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    result = check_voice_prereqs()
    assert result.moonshine_available and result.ok
    assert PrereqResult(
        faster_whisper_available=False,
        ffmpeg_available=False,
        espeak_available=False,
        moonshine_available=True,
    ).ok
