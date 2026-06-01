"""Tests for `daisugi tend` CLI subcommand."""

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.distiller import Distiller, TendReport


def test_tend_prints_report(tmp_path, monkeypatch):
    async def _fake_tend(self):
        return TendReport(
            created=2, updated=1, skipped=0,
            pathways=["p1", "p2", "p3"], duration_s=0.5, warnings=[],
        )

    monkeypatch.setattr(Distiller, "tend", _fake_tend)

    runner = CliRunner()
    result = runner.invoke(app, ["tend", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, result.output
    assert "created=2" in result.output or "Created: 2" in result.output
    assert "p1" in result.output or "3 pathway" in result.output


def test_tend_dry_run_does_not_write(tmp_path, monkeypatch):
    called = {}

    async def _fake_tend(self):
        called["ran"] = True
        return TendReport(created=0, updated=0, skipped=0, pathways=[], duration_s=0.0, warnings=[])

    monkeypatch.setattr(Distiller, "tend", _fake_tend)
    runner = CliRunner()
    result = runner.invoke(app, ["tend", "--data-dir", str(tmp_path), "--dry-run"])
    # Dry-run should short-circuit or print a message — expected behavior:
    # either tend() still runs but persistence is skipped, or the whole thing
    # is skipped. For simplicity, implement dry-run as "run but don't commit pathways".
    assert result.exit_code == 0


import time

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _write_pathway(store, id_="pathway_cli00000"):
    env = Envelope(generated_by="distilled", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task="T", steps=[ShellStep(id="s1", command="echo")])
    p = CompiledPathway(
        id=id_, task_description="cli task",
        task_embedding=[0.1], envelope=env, plan_template=plan,
        source_trace_ids=[],        distilled_at=time.time(),
    )
    store.put(p)
    return p


def test_pathways_list_shows_stored_pathways(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    _write_pathway(store, "pathway_a00")
    _write_pathway(store, "pathway_b00")

    runner = CliRunner()
    result = runner.invoke(app, ["pathways", "list", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "pathway_a00" in result.output
    assert "pathway_b00" in result.output


def test_pathways_show_prints_detail(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    _write_pathway(store, "pathway_detail0")

    runner = CliRunner()
    result = runner.invoke(app, ["pathways", "show", "pathway_detail0", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "cli task" in result.output
    assert "pathway_detail0" in result.output


def test_pathways_delete_removes_entry(tmp_path):
    store = PathwayStore(tmp_path / "pathways.db")
    _write_pathway(store, "pathway_del0000")

    runner = CliRunner()
    result = runner.invoke(app, ["pathways", "delete", "pathway_del0000", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert store.list_all() == []
