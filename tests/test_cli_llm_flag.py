"""CLI --llm flag tests: verify that the flag sets OPENDAISUGI_LLM_BACKEND."""

from __future__ import annotations

import os

import pytest
from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()


def _err(result) -> str:
    """Error text regardless of Click version: newer Click captures stderr
    separately; older Click folds it into output and raises on `.stderr`.
    """
    try:
        return (result.stderr or "") + result.output
    except ValueError:
        return result.output


@pytest.fixture(autouse=True)
def _isolate_llm_backend_env():
    """The CLI sets os.environ directly; restore between tests to avoid bleed."""
    before = os.environ.get("OPENDAISUGI_LLM_BACKEND")
    yield
    if before is None:
        os.environ.pop("OPENDAISUGI_LLM_BACKEND", None)
    else:
        os.environ["OPENDAISUGI_LLM_BACKEND"] = before


def _make_parse_result():
    from opendaisugi.parsers import ParseResult

    return ParseResult(
        source="claude-code",
        source_file="stub.jsonl",
        parsed_at="2026-04-19T00:00:00Z",
        episodes=[],
    )


def test_journal_parse_llm_flag_sets_env(tmp_path, monkeypatch):
    session = tmp_path / "session.jsonl"
    session.write_text('{"type":"user","message":{"role":"user","content":"hi"}}\n')
    out = tmp_path / "episodes.yaml"

    seen = {"backend": None}

    class _FakeParser:
        def parse(self, path):
            seen["backend"] = os.environ.get("OPENDAISUGI_LLM_BACKEND")
            return _make_parse_result()

    monkeypatch.setattr("opendaisugi.cli.get_parser", lambda *a, **kw: _FakeParser())
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)

    result = runner.invoke(
        app,
        ["journal", "parse", str(session), "-o", str(out), "--llm", "claude-code"],
        catch_exceptions=False,
    )
    assert result.exit_code == 0, result.output
    assert seen["backend"] == "claude-code"


def test_journal_parse_default_llm_leaves_env_unset(tmp_path, monkeypatch):
    session = tmp_path / "session.jsonl"
    session.write_text('{"type":"user","message":{"role":"user","content":"hi"}}\n')
    out = tmp_path / "episodes.yaml"

    seen = {"backend": None}

    class _FakeParser:
        def parse(self, path):
            seen["backend"] = os.environ.get("OPENDAISUGI_LLM_BACKEND")
            return _make_parse_result()

    monkeypatch.setattr("opendaisugi.cli.get_parser", lambda *a, **kw: _FakeParser())
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)

    result = runner.invoke(
        app,
        ["journal", "parse", str(session), "-o", str(out)],
        catch_exceptions=False,
    )
    assert result.exit_code == 0, result.output
    assert seen["backend"] is None


def test_journal_parse_rejects_invalid_llm_value(tmp_path):
    session = tmp_path / "session.jsonl"
    session.write_text('{"type":"user","message":{"role":"user","content":"hi"}}\n')
    out = tmp_path / "episodes.yaml"

    result = runner.invoke(
        app,
        ["journal", "parse", str(session), "-o", str(out), "--llm", "bogus"],
    )
    assert result.exit_code == 2
    assert "Invalid --llm" in _err(result)


def test_generate_envelope_llm_flag_sets_env(monkeypatch):
    seen = {"backend": None}

    async def fake_generate_envelope(**kw):
        seen["backend"] = os.environ.get("OPENDAISUGI_LLM_BACKEND")
        from opendaisugi.models import Envelope, Permission

        return Envelope(
            generated_by="test-stub",
            task=kw.get("task", "t"),
            permissions=Permission(),
        )

    monkeypatch.setattr("opendaisugi.envelope.generate_envelope", fake_generate_envelope)
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)

    result = runner.invoke(
        app,
        ["generate-envelope", "say hi", "--llm", "claude-code"],
        catch_exceptions=False,
    )
    assert result.exit_code == 0, result.output
    assert seen["backend"] == "claude-code"


def test_generate_envelope_rejects_invalid_llm_value():
    result = runner.invoke(
        app,
        ["generate-envelope", "say hi", "--llm", "made-up"],
    )
    assert result.exit_code == 2
    assert "Invalid --llm" in _err(result)


def _fake_orchestrate(monkeypatch):
    from opendaisugi import Daisugi
    from tests.test_cli_orchestrate import _fake_result

    async def _ok(self, prompt, **_kw):
        return _fake_result()

    monkeypatch.setattr(Daisugi, "orchestrate", _ok)


def test_llm_flag_absent_leaves_auto_detection(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    res = runner.invoke(app, ["orchestrate", "t", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "OPENDAISUGI_LLM_BACKEND" not in os.environ


def test_explicit_litellm_is_honoured(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert os.environ.get("OPENDAISUGI_LLM_BACKEND") == "litellm"


def test_orchestrate_echoes_resolved_state(monkeypatch, tmp_path):
    _fake_orchestrate(monkeypatch)
    res = runner.invoke(app, ["orchestrate", "t", "--llm", "litellm", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    assert "backend: litellm" in res.output
    assert "gate: shadow" in res.output
    assert "data:" in res.output
