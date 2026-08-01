"""The output layer: TTY detection, NO_COLOR, --plain, -q, and progress notes."""

from __future__ import annotations

import io
import time

import pytest

from opendaisugi import console


@pytest.fixture(autouse=True)
def _restore_console_state():
    """Tests mutate module-global _MODE/_ERR; restore so order doesn't leak."""
    saved_mode = console._MODE
    saved_err = console._ERR
    yield
    console._MODE = saved_mode
    console._ERR = saved_err


class _Stream(io.StringIO):
    def __init__(self, tty: bool) -> None:
        super().__init__()
        self._tty = tty

    def isatty(self) -> bool:
        return self._tty


def test_color_only_on_a_tty():
    assert console.resolve_output(stream=_Stream(True), env={}).color is True
    assert console.resolve_output(stream=_Stream(False), env={}).color is False


def test_no_color_and_dumb_term_disable_color():
    assert console.resolve_output(stream=_Stream(True), env={"NO_COLOR": "1"}).color is False
    assert console.resolve_output(stream=_Stream(True), env={"TERM": "dumb"}).color is False


def test_plain_disables_color_and_boxes():
    mode = console.resolve_output(plain=True, stream=_Stream(True), env={})
    assert mode.color is False and mode.plain is True
    assert console.glyphs(mode) is console.ASCII_BOX


def test_style_is_identity_when_color_off():
    console.set_mode(console.resolve_output(stream=_Stream(False), env={}))
    assert console.style("x", "green") == "x"
    console.set_mode(console.resolve_output(stream=_Stream(True), env={}))
    assert console.style("x", "green") != "x" and "x" in console.style("x", "green")


def test_note_is_silent_under_quiet_but_say_is_not(capsys):
    console.set_mode(console.resolve_output(quiet=True, stream=_Stream(False), env={}))
    console.say("result")
    console.note("progress")
    out = capsys.readouterr()
    assert out.out == "result\n"
    assert out.err == ""


def test_note_goes_to_stderr(capsys):
    console.set_mode(console.resolve_output(stream=_Stream(False), env={}))
    console.note("progress")
    assert capsys.readouterr().err == "progress\n"


def test_step_prints_on_a_tty_and_not_when_piped(capsys):
    tty = _Stream(True)
    console.set_mode(console.resolve_output(stream=tty, env={}), err_stream=tty)
    with console.step("verifying"):
        time.sleep(0.11)
    text = tty.getvalue()
    assert text.startswith("verifying…")
    assert "done (" in text and "s)" in text

    piped = _Stream(False)
    console.set_mode(console.resolve_output(stream=piped, env={}), err_stream=piped)
    with console.step("verifying"):
        pass
    assert piped.getvalue() == ""
