"""CLI: `daisugi quickstart` — the one-stop coworker orientation (setup → onboard → status)."""

import shutil
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app

runner = CliRunner()
_FIXTURE = Path("tests/fixtures/sample_transcript.jsonl")


def test_quickstart_help():
    res = runner.invoke(app, ["quickstart", "--help"])
    assert res.exit_code == 0
    assert "quickstart" in res.output.lower()


def test_quickstart_orients_to_this_machine(tmp_path, monkeypatch):
    proj = tmp_path / "projects" / "p"
    proj.mkdir(parents=True)
    shutil.copy(_FIXTURE, proj / "s.jsonl")
    monkeypatch.setenv("OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'projects'}")

    res = runner.invoke(app, ["quickstart", "--data-dir", str(tmp_path / "data")])
    assert res.exit_code == 0, res.output
    low = res.output.lower()
    assert "llamafile" in low                 # hardware-appropriate model recommendation
    assert "transcript" in low                # discovered existing transcripts
    assert "daisugi onboard" in res.output    # the concrete next-step command
    # guided only — must NOT have spent LLM calls / written pathways
    assert not (tmp_path / "data" / "pathways.db").exists()
