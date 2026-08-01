"""CLI wiring for `daisugi onboard` — the one-command day-one flow.

The orchestration logic is unit-tested in test_onboarding_orchestrator.py; here we
verify the command discovers real transcripts and runs end-to-end in --dry-run
(no LLM, no writes).
"""

import shutil
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()

_FIXTURE = Path("tests/fixtures/sample_transcript.jsonl")


def test_onboard_help():
    res = runner.invoke(app, ["onboard", "--help"])
    assert res.exit_code == 0
    assert "onboard" in res.output.lower()


def test_onboard_dry_run_discovers_and_writes_no_pathways(tmp_path, monkeypatch):
    # Plant a real claude-code transcript and point discovery at it.
    proj = tmp_path / "projects" / "some-proj"
    proj.mkdir(parents=True)
    shutil.copy(_FIXTURE, proj / "session.jsonl")
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'projects'}")

    res = runner.invoke(
        app,
        [
            "onboard",
            "--dry-run",
            "--data-dir",
            str(tmp_path / "data"),
            "--max-tools",
            "999",  # avoid LLM episode splitting
        ],
    )
    assert res.exit_code == 0, res.output
    # Discovered exactly the one planted transcript.
    assert "1 transcript" in res.output
    # Dry-run: nothing distilled, and it tells the user how to do it for real.
    assert "dry" in res.output.lower()
    # No pathway DB written under the data dir in dry-run.
    assert not (tmp_path / "data" / "pathways.db").exists()


def test_onboard_dry_run_makes_no_llm_split_calls(tmp_path, monkeypatch):
    """--dry-run must make ZERO model calls.

    The parser's LLM episode-splitter fires for any episode over --max-tools, and
    parsing runs before the dry-run gate — so a "discover + report only" run was
    silently billing the user for `claude -p` splitting calls. Dry-run must
    disable the splitter.
    """
    proj = tmp_path / "projects" / "p"
    proj.mkdir(parents=True)
    # One episode with 40 tool calls — over the default --max-tools 30, so the
    # splitter WOULD fire at the default cap.
    tools = ",".join(
        '{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"echo %d"}}' % (i, i)
        for i in range(40)
    )
    (proj / "s.jsonl").write_text(
        '{"role":"user","content":"do a big thing"}\n'
        '{"role":"assistant","content":[' + tools + "]}\n",
        encoding="utf-8",
    )
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'projects'}")

    calls: list[int] = []
    from opendaisugi.parsers.claude_code import ClaudeCodeParser

    def _spy_split(self, *a, **k):
        calls.append(1)
        return []

    monkeypatch.setattr(ClaudeCodeParser, "_llm_split", _spy_split)

    res = runner.invoke(app, ["onboard", "--dry-run", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    assert calls == [], "dry-run invoked the LLM episode-splitter; it must make no model calls"


def _hide_embedder(monkeypatch):
    import importlib.util as _u

    real = _u.find_spec
    monkeypatch.setattr(
        _u,
        "find_spec",
        lambda name, *a, **k: None if name == "sentence_transformers" else real(name, *a, **k),
    )


def test_onboard_refuses_real_run_without_embedder(tmp_path, monkeypatch):
    """Pathway clustering needs sentence-transformers; without it a real onboard
    spends model tokens and distils ZERO token-saving pathways. Fail fast."""
    _hide_embedder(monkeypatch)
    res = runner.invoke(app, ["onboard", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 2, res.output
    assert "opendaisugi[search]" in res.output


def test_onboard_allow_no_embedder_flag_bypasses_guard(tmp_path, monkeypatch):
    # Empty root -> no LLM work; proves --allow-no-embedder gets past the guard.
    _hide_embedder(monkeypatch)
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'empty'}")
    res = runner.invoke(
        app, ["onboard", "--allow-no-embedder", "--data-dir", str(tmp_path / "data")]
    )
    assert res.exit_code == 0, res.output
    assert "no transcripts" in res.output.lower()


def test_onboard_dry_run_notes_missing_embedder_but_proceeds(tmp_path, monkeypatch):
    _hide_embedder(monkeypatch)
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'empty'}")
    res = runner.invoke(app, ["onboard", "--dry-run", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    assert "sentence-transformers" in res.output or "opendaisugi[search]" in res.output


def test_onboard_no_transcripts_is_clean_exit(tmp_path, monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'empty'}")
    res = runner.invoke(app, ["onboard", "--dry-run", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    assert "no transcripts" in res.output.lower()
