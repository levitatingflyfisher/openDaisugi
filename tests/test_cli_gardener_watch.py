"""Tests for `daisugi gardener watch` — cron-friendly one-shot."""

from __future__ import annotations

import json
import time

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _seed_stale(data_dir) -> None:
    store = PathwayStore(data_dir / "pathways.db")
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    store.put(CompiledPathway(
        id="stale", task_description="T",
        task_embedding=[1.0, 0.0, 0.0],
        envelope=env, plan_template=plan,
        source_trace_ids=[], distilled_at=time.time(),
        hit_count=10,
        last_activation_at=time.time() - 90 * 86_400,
    ))


def test_watch_runs_first_time(tmp_path):
    _seed_stale(tmp_path)
    runner = CliRunner()
    result = runner.invoke(app, ["gardener", "watch", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["skipped"] is False
    assert payload["prune"]["removed"] == 1
    assert (tmp_path / ".gardener-last-run").exists()


def test_watch_skips_within_interval(tmp_path):
    _seed_stale(tmp_path)
    runner = CliRunner()
    # First run records timestamp.
    runner.invoke(app, ["gardener", "watch", "--data-dir", str(tmp_path)])
    # Second run immediately — should skip.
    result = runner.invoke(
        app,
        ["gardener", "watch", "--data-dir", str(tmp_path), "--min-interval", "3600"],
    )
    assert result.exit_code == 0
    payload = json.loads(result.output)
    assert payload["skipped"] is True
    assert payload["reason"] == "min_interval_not_elapsed"


def test_watch_force_bypasses_interval(tmp_path):
    _seed_stale(tmp_path)
    runner = CliRunner()
    runner.invoke(app, ["gardener", "watch", "--data-dir", str(tmp_path)])
    result = runner.invoke(
        app,
        ["gardener", "watch", "--data-dir", str(tmp_path), "--force"],
    )
    assert result.exit_code == 0
    payload = json.loads(result.output)
    assert payload["skipped"] is False


def test_watch_dry_run_does_not_stamp(tmp_path):
    _seed_stale(tmp_path)
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["gardener", "watch", "--data-dir", str(tmp_path), "--dry-run"],
    )
    assert result.exit_code == 0
    payload = json.loads(result.output)
    assert payload["dry_run"] is True
    # No stamp file written under --dry-run, so next run still executes.
    assert not (tmp_path / ".gardener-last-run").exists()


def test_watch_handles_corrupt_stamp(tmp_path):
    _seed_stale(tmp_path)
    (tmp_path / ".gardener-last-run").write_text("not-a-number")
    runner = CliRunner()
    result = runner.invoke(app, ["gardener", "watch", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    payload = json.loads(result.output)
    assert payload["skipped"] is False
