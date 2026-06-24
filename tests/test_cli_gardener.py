"""Smoke tests for `daisugi gardener` CLI subcommands."""

from __future__ import annotations

import json
import time

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _seed(data_dir, now: float) -> None:
    store = PathwayStore(data_dir / "pathways.db")
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])

    def _p(
        id_: str,
        embedding: list[float],
        *,
        hit_count: int = 0,
        last_activation_at: float = 0.0,
        failure_count: int = 0,
    ) -> CompiledPathway:
        return CompiledPathway(
            id=id_,
            task_description="T",
            task_embedding=embedding,
            envelope=env,
            plan_template=plan,
            source_trace_ids=[],
            distilled_at=now,
            hit_count=hit_count,
            failure_count=failure_count,
            last_activation_at=last_activation_at,
        )

    store.put(_p("stale", [0.0, 0.0, 1.0], hit_count=10,
                 last_activation_at=now - 60 * 86_400))
    store.put(_p("a", [1.0, 0.0, 0.0], hit_count=5, last_activation_at=now))
    store.put(_p("b", [0.99, 0.01, 0.0], hit_count=2, last_activation_at=now))


def test_gardener_prune_dry_run(tmp_path):
    _seed(tmp_path, time.time())
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["gardener", "prune", "--data-dir", str(tmp_path), "--dry-run"],
    )
    assert result.exit_code == 0, result.output
    assert "stale" in result.output
    # Store is untouched under --dry-run.
    assert len(PathwayStore(tmp_path / "pathways.db").list_all()) == 3


def test_gardener_prune_executes(tmp_path):
    _seed(tmp_path, time.time())
    runner = CliRunner()
    result = runner.invoke(
        app, ["gardener", "prune", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    remaining = {p.id for p in PathwayStore(tmp_path / "pathways.db").list_all()}
    assert "stale" not in remaining


def test_gardener_merge_json(tmp_path):
    _seed(tmp_path, time.time())
    runner = CliRunner()
    result = runner.invoke(
        app,
        ["gardener", "merge", "--data-dir", str(tmp_path), "--similarity", "0.9", "--json"],
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    # a and b are near-duplicates — should merge.
    assert len(payload["merged_pairs"]) == 1


def test_gardener_run_pipeline(tmp_path):
    _seed(tmp_path, time.time())
    runner = CliRunner()
    result = runner.invoke(
        app, ["gardener", "run", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    remaining = {p.id for p in PathwayStore(tmp_path / "pathways.db").list_all()}
    # stale pruned, a absorbs b.
    assert remaining == {"a"}


def test_gardener_status_empty(tmp_path):
    runner = CliRunner()
    result = runner.invoke(
        app, ["gardener", "status", "--data-dir", str(tmp_path)],
    )
    assert result.exit_code == 0, result.output
    assert "count: 0" in result.output


def test_gardener_status_json(tmp_path):
    _seed(tmp_path, time.time())
    runner = CliRunner()
    result = runner.invoke(
        app, ["gardener", "status", "--data-dir", str(tmp_path), "--json"],
    )
    assert result.exit_code == 0, result.output
    payload = json.loads(result.output)
    assert payload["count"] == 3
    assert len(payload["pathways"]) == 3
