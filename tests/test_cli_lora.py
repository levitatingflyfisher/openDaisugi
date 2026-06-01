"""Tests for `daisugi lora export` CLI (v0.5.0)."""

from __future__ import annotations

import json
import sqlite3

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
)


def _seed(journal: Journal, *, task: str, trace_id: str) -> None:
    env = Envelope(
        id=f"env_{trace_id}",
        generated_by="test",
        task=task,
        permissions=Permission(shell=True),
    )
    plan = ActionPlan(
        id=f"plan_{trace_id}", task=task, source="test",
        steps=[ShellStep(id="s1", command="echo hi")],
    )
    result = VerificationResult(
        plan_id=plan.id, envelope_id=env.id,
        ok=True, violations=[], warnings=[], duration_ms=1.0,
    )
    journal.log(
        trace_id=trace_id, task=task,
        envelope=env, plan=plan, result=result,
    )
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_status = 'succeeded' WHERE id = ?",
            (trace_id,),
        )


def test_lora_export_alpaca(tmp_path):
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")
    _seed(journal, task="Deploy the app", trace_id="t2")

    out = tmp_path / "train.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["lora", "export", str(out), "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["written"] == 2
    assert payload["format"] == "alpaca"
    assert payload["output_path"] == str(out)

    lines = out.read_text().strip().splitlines()
    assert len(lines) == 2
    row = json.loads(lines[0])
    assert "instruction" in row and "output" in row


def test_lora_export_chat(tmp_path):
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")

    out = tmp_path / "chat.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        app,
        [
            "lora", "export", str(out),
            "--data-dir", str(tmp_path),
            "--format", "chat",
            "--system-prompt", "You produce envelopes.",
        ],
    )
    assert result.exit_code == 0, result.output

    row = json.loads(out.read_text().strip())
    assert row["messages"][0]["role"] == "system"
    assert row["messages"][1]["content"] == "Build a widget"


def test_lora_export_rejects_unknown_format(tmp_path):
    runner = CliRunner()
    result = runner.invoke(
        app,
        [
            "lora", "export", str(tmp_path / "x.jsonl"),
            "--data-dir", str(tmp_path),
            "--format", "markdown",
        ],
    )
    assert result.exit_code == 2
    assert "Unknown format" in result.output


def test_lora_export_empty_journal(tmp_path):
    Journal(data_dir=tmp_path)  # create the db file
    out = tmp_path / "empty.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["lora", "export", str(out), "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["total"] == 0
    assert payload["written"] == 0
    assert out.exists()
    assert out.read_text() == ""


def test_lora_export_days_filter(tmp_path):
    """--days N sets `since` timestamp; with a large N everything still exports."""
    journal = Journal(data_dir=tmp_path)
    _seed(journal, task="Build a widget", trace_id="t1")

    out = tmp_path / "train.jsonl"
    runner = CliRunner()
    result = runner.invoke(
        app,
        [
            "lora", "export", str(out),
            "--data-dir", str(tmp_path),
            "--days", "30",
        ],
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["written"] == 1
