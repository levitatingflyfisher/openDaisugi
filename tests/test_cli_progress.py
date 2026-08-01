"""A command that takes longer than a blink says so on a TTY."""

from __future__ import annotations

import io

import pytest
from typer.testing import CliRunner

from opendaisugi import console
from opendaisugi.cli import app

runner = CliRunner()


@pytest.fixture(autouse=True)
def _restore_console_state():
    """Mirror tests/test_console.py's fixture: don't leak _MODE/_ERR across tests.

    This file pokes console._MODE/_ERR directly (to force a TTY mode past
    _root's own console.set_mode call), so a failing assert mid-test would
    otherwise leave _ERR pointing at a dead stream for whatever runs next.
    """
    saved_mode = console._MODE
    saved_err = console._ERR
    yield
    console._MODE = saved_mode
    console._ERR = saved_err


class _Tty(io.StringIO):
    def isatty(self) -> bool:
        return True


def test_orchestrate_prints_a_progress_note_on_a_tty(monkeypatch, tmp_path):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _slow(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _slow)
    err = _Tty()
    monkeypatch.setattr(console, "set_mode", lambda mode, err_stream=None: None)  # keep our TTY mode
    console._MODE = console.resolve_output(stream=_Tty(), env={})
    console._ERR = err
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    # _echo_resolved (Task 2) writes its own "backend: ..." note to the same
    # stream first, so this checks presence rather than a strict prefix —
    # the plan's literal .startswith() assumed no other stderr note precedes
    # the step's label, which no longer holds once _echo_resolved is wired in.
    assert "orchestrating…" in err.getvalue()


def test_tend_prints_a_progress_note_on_a_tty(monkeypatch, tmp_path):
    from opendaisugi import Daisugi

    class _FakeReport:
        created = 0
        updated = 0
        skipped = 0
        duration_s = 0.0
        pathways: list[str] = []
        warnings: list[str] = []

    async def _fake_tend(self, **_kw):
        return _FakeReport()

    monkeypatch.setattr(Daisugi, "tend", _fake_tend)
    err = _Tty()
    monkeypatch.setattr(console, "set_mode", lambda mode, err_stream=None: None)
    console._MODE = console.resolve_output(stream=_Tty(), env={})
    console._ERR = err
    res = runner.invoke(app, ["tend", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert err.getvalue().startswith("tending the garden…")


def test_onboard_reports_progress_on_stderr_not_stdout(monkeypatch, tmp_path):
    """onboard's own progress callback must land on stderr (console.note), not
    stdout — stdout is results-only (clig.dev), and -q must be able to
    silence it. Route via monkeypatching onboarding.discover_transcripts to
    avoid touching the real filesystem/harness roots.

    onboard_cmd also prints a *separate*, differently-worded final summary
    line (the accumulated report.warnings, via typer.echo) that legitimately
    belongs on stdout — this test pins the live *progress* text distinctly
    from that summary so the two can't be conflated.

    ``onboard()``'s ``discover`` parameter defaults to
    ``discover_transcripts`` bound at *definition* time, so patching that
    name on the module has no effect on the already-bound default. Patch
    ``default_transcript_roots`` instead — ``discover_transcripts`` looks
    that up fresh (a bare module-global call) on every invocation — so
    discovery legitimately finds zero roots and never touches the real
    filesystem or this box's real transcripts.
    """
    monkeypatch.setattr("opendaisugi.onboarding.default_transcript_roots", lambda: {})
    err = _Tty()
    monkeypatch.setattr(console, "set_mode", lambda mode, err_stream=None: None)
    console._MODE = console.resolve_output(stream=_Tty(), env={})
    console._ERR = err
    res = runner.invoke(app, ["onboard", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    # the live progress note (onboarding.py's own _say()) must be on stderr —
    # never mixed into the stdout report the command also writes.
    assert "onboard: no transcripts discovered; nothing to distill" in err.getvalue()
    assert "onboard: no transcripts discovered; nothing to distill" not in res.stdout
