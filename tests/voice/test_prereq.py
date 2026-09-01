"""Tests for the voice bridge's prerequisite check."""

from __future__ import annotations

import sys

from opendaisugi.voice.prereq import PrereqResult, check_voice_prereqs


def test_check_voice_prereqs_returns_a_result_with_the_three_flags():
    result = check_voice_prereqs()
    assert isinstance(result, PrereqResult)
    assert isinstance(result.faster_whisper_available, bool)
    assert isinstance(result.ffmpeg_available, bool)
    assert isinstance(result.espeak_available, bool)


def test_ok_is_true_only_when_faster_whisper_is_available():
    assert PrereqResult(
        faster_whisper_available=True, ffmpeg_available=False, espeak_available=False
    ).ok
    assert not PrereqResult(
        faster_whisper_available=False, ffmpeg_available=True, espeak_available=True
    ).ok


def test_faster_whisper_available_is_true_when_the_module_is_real():
    result = check_voice_prereqs()
    assert result.faster_whisper_available


def test_faster_whisper_available_flips_to_false_when_the_module_is_hidden(monkeypatch):
    monkeypatch.setitem(sys.modules, "faster_whisper", None)
    result = check_voice_prereqs()
    assert not result.faster_whisper_available


def test_ffmpeg_available_is_true_when_the_binary_is_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: f"/usr/bin/{name}")
    result = check_voice_prereqs()
    assert result.ffmpeg_available


def test_ffmpeg_available_flips_to_false_when_the_binary_is_not_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    result = check_voice_prereqs()
    assert not result.ffmpeg_available


def test_espeak_available_is_true_when_espeak_ng_is_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: f"/usr/bin/{name}")
    result = check_voice_prereqs()
    assert result.espeak_available


def test_espeak_available_flips_to_false_when_no_binary_is_on_path(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    result = check_voice_prereqs()
    assert not result.espeak_available


def test_espeak_available_is_true_when_only_plain_espeak_is_on_path(monkeypatch):
    def which_only_plain_espeak(name):
        return "/usr/bin/espeak" if name == "espeak" else None

    monkeypatch.setattr("shutil.which", which_only_plain_espeak)
    result = check_voice_prereqs()
    assert result.espeak_available


def test_faster_whisper_available_is_false_when_the_import_raises_an_os_error(monkeypatch):
    def raise_os_error(name):
        raise OSError("broken native install")

    monkeypatch.setattr("importlib.import_module", raise_os_error)
    result = check_voice_prereqs()
    assert not result.faster_whisper_available
