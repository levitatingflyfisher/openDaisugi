"""The engine a box with no voice choice gets, by its hardware (ruling VO-17).
choose_engine is pure: the hardware and what is installed go in."""

from __future__ import annotations

from opendaisugi.voice.engines import VoiceHardware, choose_engine, hardware_order

DESKTOP = VoiceHardware(ram_gb=15.5, cpus=4, vram_gb=0.0)
ALL = {"parakeet", "moonshine", "faster-whisper"}


def test_the_tiers_follow_ram_and_cores():
    assert hardware_order(DESKTOP) == ["parakeet", "moonshine", "tiny"]
    assert hardware_order(VoiceHardware(8.0, 4, 0.0)) == ["parakeet", "moonshine", "tiny"]
    assert hardware_order(VoiceHardware(7.9, 8, 0.0)) == ["moonshine", "tiny"]
    assert hardware_order(VoiceHardware(32.0, 2, 0.0)) == ["moonshine", "tiny"]
    assert hardware_order(VoiceHardware(2.0, 1, 0.0)) == ["moonshine", "tiny"]
    assert hardware_order(VoiceHardware(1.9, 8, 0.0)) == ["tiny"]
    assert hardware_order(VoiceHardware(None, 8, 0.0)) == ["tiny"]


def test_a_desktop_gets_parakeet_and_says_why():
    assert choose_engine(DESKTOP, ALL, True) == (
        "parakeet",
        "v2",
        "No voice engine is set, so voice uses Parakeet v2: this box has 15.5 GB of RAM and "
        "4 cores.",
    )


def test_a_gpu_is_named_but_changes_nothing():
    hw = VoiceHardware(16.0, 8, 6.0)
    engine, _, line = choose_engine(hw, ALL, False)
    assert engine == "parakeet"
    assert line.endswith(
        "16 GB of RAM and 8 cores, and a GPU with 6 GB (no GPU engine is built yet)."
    )


def test_the_first_installed_tier_wins_and_the_line_names_what_is_missing():
    engine, model, line = choose_engine(DESKTOP, {"moonshine"}, False)
    assert (engine, model) == ("moonshine", "small")
    assert line == (
        "No voice engine is set, so voice uses Moonshine small: this box has 15.5 GB of RAM "
        "and 4 cores. Parakeet v2 would come first, but parakeet-cli is not on PATH."
    )


def test_the_tiny_tier_is_faster_whisper_where_it_imports_else_moonshine_small():
    small = VoiceHardware(1.0, 1, 0.0)
    assert choose_engine(small, ALL, True)[:2] == ("faster-whisper", "tiny.en")
    assert choose_engine(small, {"moonshine"}, False)[:2] == ("moonshine", "small")
    assert choose_engine(VoiceHardware(None, 1, 0.0), set(), False)[2] == (
        "No voice engine is set, so voice uses Moonshine small: this box has an unknown "
        "amount of RAM and 1 core."
    )


def test_with_nothing_installed_the_first_tier_is_chosen():
    assert choose_engine(DESKTOP, set(), False)[:2] == ("parakeet", "v2")


def test_the_layer_names_agree_with_the_voice_pins():
    from opendaisugi import hardware
    from opendaisugi.voice import pins

    assert hardware.PARAKEET_DEFAULT_MODEL == pins.PARAKEET_DEFAULT_MODEL
    assert hardware.MOONSHINE_DEFAULT_MODEL == pins.MOONSHINE_DEFAULT_MODEL
    assert hardware.PARAKEET_BINARY == pins.PARAKEET_BINARY
    assert hardware.MOONSHINE_BINARY == pins.MOONSHINE_BINARY


def test_parakeet_is_skipped_when_this_machine_cannot_use_it():
    # An arch with no pinned Q4_K result and no file on disk: Parakeet would
    # be refused later, so the pick falls to Moonshine and does not mention it.
    engine, model, line = choose_engine(DESKTOP, ALL, True, parakeet_ok=False)
    assert (engine, model) == ("moonshine", "small")
    assert "Parakeet" not in line
    assert choose_engine(DESKTOP, set(), False, parakeet_ok=False)[:2] == ("moonshine", "small")


def test_parakeet_usable_follows_arch_and_a_pinned_file(tmp_path, monkeypatch):
    from opendaisugi.voice import models, pins

    monkeypatch.setenv("XDG_CACHE_HOME", str(tmp_path))
    assert models.parakeet_usable("v2", arch="x86_64")
    assert not models.parakeet_usable("v2", arch="aarch64")
    m = pins.PARAKEET_MODELS["v2"]
    out = models.parakeet_dir("v2") / m.quantized.name
    out.parent.mkdir(parents=True)
    out.write_bytes(b"not the pinned file")
    assert not models.parakeet_usable("v2", arch="aarch64")
    monkeypatch.setattr(models, "_matches", lambda path, sha: path == out)
    assert models.parakeet_usable("v2", arch="aarch64")
