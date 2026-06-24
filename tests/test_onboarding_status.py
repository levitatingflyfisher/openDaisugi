"""`daisugi status` — the day-one trust/readiness surface.

Shows whether token savings are actually enabled ([search] + pathways present)
and whether the verified journal is populated, so a new adopter can see at a
glance that routing is live and agent actions are being verified.
"""

import importlib.util
import json
import time

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.onboarding import StatusReport, gather_status
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore

runner = CliRunner()


def _put_pathways(data_dir, n):
    from opendaisugi._search import _MODEL_NAME
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    store = PathwayStore(data_dir / "pathways.db")
    for i in range(n):
        store.put(
            CompiledPathway(
                id=f"pathway_{i:08d}",
                task_description=f"task {i}",
                task_embedding=[0.1, 0.2, 0.3],
                embedding_model=_MODEL_NAME,
                embedding_model_version=_EMBEDDING_MODEL_VERSION,
                envelope=Envelope(generated_by="t", task="T", permissions=Permission(shell=True)),
                plan_template=ActionPlan(source="t", task="T", steps=[ShellStep(id="s", command="echo")]),
                source_trace_ids=[],
                distilled_at=time.time(),
            )
        )


def test_gather_status_counts_pathways(tmp_path):
    _put_pathways(tmp_path, 2)
    rep = gather_status(tmp_path)
    assert isinstance(rep, StatusReport)
    assert rep.pathway_count == 2


def test_gather_status_reports_search_extra_flag(tmp_path):
    rep = gather_status(tmp_path)
    expected = importlib.util.find_spec("sentence_transformers") is not None
    assert rep.search_extra_installed == expected


def test_gather_status_empty_data_dir_is_zero(tmp_path):
    rep = gather_status(tmp_path)
    assert rep.pathway_count == 0
    assert rep.journal_total == 0


def test_status_cli_json(tmp_path):
    _put_pathways(tmp_path, 1)
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path), "--json"])
    assert res.exit_code == 0, res.output
    data = json.loads(res.output)
    assert data["pathway_count"] == 1
    assert "search_extra_installed" in data
    assert "journal_total" in data


def test_status_cli_human_mentions_token_savings_and_trust(tmp_path):
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path)])
    assert res.exit_code == 0, res.output
    low = res.output.lower()
    assert "token" in low or "pathway" in low
    assert "verif" in low or "trust" in low or "journal" in low
