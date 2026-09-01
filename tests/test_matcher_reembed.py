"""Switching the embedder orphans every stored pathway until something re-embeds
it. `tend` does that now, including the legacy rows that carry no provenance."""

import json
import sqlite3

from opendaisugi.models import ActionPlan, Envelope, Permission
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(pid: str, task: str, vec: list[float], model: str, version: str) -> CompiledPathway:
    return CompiledPathway(
        id=pid,
        task_description=task,
        task_embedding=vec,
        embedding_model=model,
        embedding_model_version=version,
        envelope=Envelope(generated_by="test", task=task, permissions=Permission()),
        plan_template=ActionPlan(source="test", task=task, steps=[]),
        source_trace_ids=["t1"],
        distilled_at=0.0,
    )


def _rows(store: PathwayStore) -> list[dict]:
    con = sqlite3.connect(store._db_path)
    con.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in con.execute("SELECT * FROM pathways")]
    finally:
        con.close()


def test_reembed_restamps_a_stale_row_and_returns_the_count(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))
    n = store.reembed_stale(
        embed=lambda texts: [[1.0, 0.0] for _ in texts], model="new-model", version="3"
    )
    assert n == 1
    row = _rows(store)[0]
    assert row["embedding_model"] == "new-model"
    assert row["embedding_model_version"] == "3"
    assert json.loads(row["task_embedding_json"]) == [1.0, 0.0]


def test_a_current_row_is_left_alone(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "new-model", "3"))
    assert (
        store.reembed_stale(
            embed=lambda texts: [[9.0, 9.0] for _ in texts], model="new-model", version="3"
        )
        == 0
    )
    assert json.loads(_rows(store)[0]["task_embedding_json"]) == [0.0, 1.0]


def test_a_legacy_row_with_no_provenance_gets_stamped_and_still_matches(tmp_path, monkeypatch):
    """Legacy rows are admitted as wildcards by find. After re-stamping they are
    concretely current, and find must still return them."""
    import opendaisugi._search as search
    import opendaisugi.distiller as distiller

    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [1.0, 0.0], "", ""))
    assert (
        store.reembed_stale(
            embed=lambda texts: [[1.0, 0.0] for _ in texts], model="new-model", version="3"
        )
        == 1
    )
    row = _rows(store)[0]
    assert row["embedding_model"] == "new-model"

    store._embed_query = lambda task: [1.0, 0.0]
    monkeypatch.setattr(search, "active_model_name", lambda config=None: "new-model")
    monkeypatch.setattr(distiller, "_EMBEDDING_MODEL_VERSION", "3")
    match = store.find("add a test", threshold=0.5)
    assert match is not None and match.pathway.id == "a"


def test_reembed_reports_zero_rather_than_crashing_when_the_embedder_is_missing(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))

    def _boom(texts):
        raise ImportError("no embedder here")

    assert store.reembed_stale(embed=_boom, model="new", version="3") == 0
    assert _rows(store)[0]["embedding_model"] == "old-model"


def test_reembed_reports_zero_when_the_configured_matcher_is_not_built(tmp_path, monkeypatch):
    import opendaisugi

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("matcher_model: not-a-matcher\n")
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))
    assert store.reembed_stale() == 0
    assert _rows(store)[0]["embedding_model"] == "old-model"


def test_an_identity_bump_orphans_the_store_until_tend_re_embeds(tmp_path, monkeypatch):
    """The identity string is the provenance stamp. Bumping it here reproduces
    the orphan case end to end: find stops matching, and reembed_stale brings
    the row back."""
    import opendaisugi
    from opendaisugi import _search
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("matcher_model: lexical\n")
    _search.reload()

    store = PathwayStore(tmp_path / "p.db")
    vec = [float(x) for x in _search._get_model().encode(["add a test"])[0]]
    store.put(_pathway("a", "add a test", vec, "lexical-hash-v1", _EMBEDDING_MODEL_VERSION))
    assert store.find("add a test") is not None

    monkeypatch.setattr(_search, "_LEXICAL_IDENTITY", "lexical-hash-v2")
    _search.reload()
    assert store.find("add a test") is None, "a stale row must not match across spaces"

    assert store.reembed_stale() == 1
    assert store.get("a").embedding_model == "lexical-hash-v2"
    assert store.find("add a test") is not None
    _search.reload()


def test_tend_re_embeds_stale_rows_before_clustering(tmp_path, monkeypatch):
    """The store catches up on the next tend, with the distiller's own embedder."""
    import asyncio

    import opendaisugi._search as search
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION, Distiller
    from opendaisugi.journal import Journal

    monkeypatch.setattr(search, "active_model_name", lambda config=None: "new-model")
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway("a", "add a test", [0.0, 1.0], "old-model", "1"))
    distiller = Distiller(journal=Journal(data_dir=tmp_path), pathway_store=store)
    distiller._embed_tasks = lambda tasks: [[1.0, 0.0] for _ in tasks]
    report = asyncio.run(distiller.tend())
    assert any("re-embedded 1" in w for w in report.warnings)
    row = _rows(store)[0]
    assert row["embedding_model"] == "new-model"
    assert row["embedding_model_version"] == _EMBEDDING_MODEL_VERSION
    assert json.loads(row["task_embedding_json"]) == [1.0, 0.0]


def test_reload_drops_the_cached_embedder(tmp_path, monkeypatch):
    import opendaisugi
    from opendaisugi import _search

    monkeypatch.setattr(opendaisugi, "DEFAULT_DATA_DIR", tmp_path)
    (tmp_path / "config.yaml").write_text("matcher_model: lexical\n")
    _search.reload()
    first = _search._get_model()
    assert _search._get_model() is first
    _search.reload()
    assert _search._get_model() is not first
    _search.reload()
