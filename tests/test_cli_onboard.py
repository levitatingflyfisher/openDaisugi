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
    monkeypatch.setenv(
        "OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'projects'}"
    )

    res = runner.invoke(
        app,
        [
            "onboard",
            "--dry-run",
            "--data-dir", str(tmp_path / "data"),
            "--max-tools", "999",  # avoid LLM episode splitting
        ],
    )
    assert res.exit_code == 0, res.output
    # Discovered exactly the one planted transcript.
    assert "1 transcript" in res.output
    # Dry-run: nothing distilled, and it tells the user how to do it for real.
    assert "dry" in res.output.lower()
    # No pathway DB written under the data dir in dry-run.
    assert not (tmp_path / "data" / "pathways.db").exists()


def test_onboard_no_transcripts_is_clean_exit(tmp_path, monkeypatch):
    monkeypatch.setenv(
        "OPENDAISUGI_TRANSCRIPT_ROOTS", f"claude-code={tmp_path / 'empty'}"
    )
    res = runner.invoke(
        app, ["onboard", "--dry-run", "--data-dir", str(tmp_path / "data")]
    )
    assert res.exit_code == 0, res.output
    assert "no transcripts" in res.output.lower()
