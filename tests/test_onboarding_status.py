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
                plan_template=ActionPlan(
                    source="t", task="T", steps=[ShellStep(id="s", command="echo")]
                ),
                source_trace_ids=[],
                distilled_at=time.time(),
            )
        )


def test_gather_status_counts_pathways(tmp_path):
    _put_pathways(tmp_path, 2)
    rep = gather_status(tmp_path)
    assert isinstance(rep, StatusReport)
    assert rep.pathway_count == 2


def _without(monkeypatch, *missing):
    real = importlib.util.find_spec

    def find_spec(name, *a, **k):
        return None if name in missing else real(name, *a, **k)

    monkeypatch.setattr(importlib.util, "find_spec", find_spec)


def _matcher(monkeypatch, key):
    from opendaisugi import _search
    from opendaisugi.config import Config

    monkeypatch.setattr(_search, "_active_config", lambda: Config(matcher_model=key))


def test_status_reports_the_matcher_in_use_not_the_extras(tmp_path, monkeypatch):
    # A lexical matcher needs no extra: pathways are on with no
    # sentence-transformers installed.
    _matcher(monkeypatch, "lexical")
    _without(monkeypatch, "sentence_transformers")
    assert gather_status(tmp_path).search_extra_installed is True


def test_status_counts_the_lexical_fallback_as_a_matcher(tmp_path, monkeypatch):
    # The default matcher with its package absent falls back to lexical
    # (ADR-0019), so pathways still work.
    _matcher(monkeypatch, "all-MiniLM-L6-v2")
    _without(monkeypatch, "sentence_transformers")
    assert gather_status(tmp_path).search_extra_installed is True


def test_status_with_an_unbuilt_matcher_reports_pathways_off(tmp_path, monkeypatch):
    _matcher(monkeypatch, "nope")
    rep = gather_status(tmp_path, threshold=0.3)
    assert rep.search_extra_installed is False
    assert not rep.token_savings_ready


def test_status_text_names_an_unbuilt_matcher(tmp_path, monkeypatch):
    _matcher(monkeypatch, "nope")
    res = runner.invoke(app, ["status", "--data-dir", str(tmp_path), "--threshold", "0.3"])
    assert res.exit_code == 0, res.output
    assert "matcher nope is not a built embedder" in res.output


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


def _old_journal(data_dir):
    """An index.db from before every migration: a bare traces table."""
    import sqlite3

    (data_dir / "journal").mkdir(parents=True)
    con = sqlite3.connect(data_dir / "journal" / "index.db")
    con.execute("CREATE TABLE traces (id TEXT, ok INTEGER, duration_ms REAL)")
    con.executemany(
        "INSERT INTO traces VALUES (?, ?, ?)", [("a", 1, 1.0), ("b", 1, 2.0), ("c", 0, 3.0)]
    )
    con.commit()
    con.close()
    return data_dir / "journal" / "index.db"


def test_gather_status_makes_no_journal(tmp_path):
    # Reading never makes a store: a missing journal reads as empty.
    rep = gather_status(tmp_path)
    assert rep.journal_total == 0
    assert list(tmp_path.iterdir()) == []


def test_gather_status_reads_an_old_journal_and_writes_nothing(tmp_path):
    db = _old_journal(tmp_path)
    before = db.read_bytes()
    rep = gather_status(tmp_path)
    assert (rep.journal_total, rep.journal_passed, rep.journal_failed) == (3, 2, 1)
    assert db.read_bytes() == before
    assert sorted(p.name for p in (tmp_path / "journal").iterdir()) == ["index.db"]


def test_gather_status_reads_an_index_with_no_traces_table_as_empty(tmp_path):
    import sqlite3

    (tmp_path / "journal").mkdir()
    sqlite3.connect(tmp_path / "journal" / "index.db").close()
    rep = gather_status(tmp_path)
    assert rep.journal_total == 0


def test_gather_status_keeps_the_count_when_the_hit_sum_is_inf(tmp_path):
    # int() of an inf hit sum raises; the count read before it is kept
    # and the hits stay 0.
    import sqlite3

    _put_pathways(tmp_path, 2)
    con = sqlite3.connect(tmp_path / "pathways.db")
    con.execute("UPDATE pathways SET hit_count = 1e308")
    con.commit()
    con.close()
    rep = gather_status(tmp_path)
    assert rep.pathway_count == 2
    assert rep.pathway_hits == 0
