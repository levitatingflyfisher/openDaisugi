"""Tests for PathwayStore (v0.3.0)."""

import sqlite3
import time

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(id_: str = "pathway_abc12345", embedding=None) -> CompiledPathway:
    # v0.28.4: default to the current embedding_model / version so existing
    # tests don't get filtered out by the find() compatibility check. Tests
    # that want to exercise stale-embedding behavior override these fields.
    from opendaisugi._search import _MODEL_NAME
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id=id_,
        task_description="generalized task",
        task_embedding=embedding if embedding is not None else [0.1, 0.2, 0.3],
        embedding_model=_MODEL_NAME,
        embedding_model_version=_EMBEDDING_MODEL_VERSION,
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
    )


def test_pathway_store_creates_db_and_schema(tmp_path):
    db = tmp_path / "pathways.db"
    PathwayStore(db)
    assert db.exists()
    with sqlite3.connect(db) as con:
        tables = [r[0] for r in con.execute(
            "SELECT name FROM sqlite_master WHERE type='table'"
        )]
    assert "pathways" in tables


def test_put_then_raw_fetch(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    with sqlite3.connect(tmp_path / "p.db") as con:
        row = con.execute(
            "SELECT id, task_description FROM pathways WHERE id = ?",
            (p.id,),
        ).fetchone()
    assert row[0] == p.id
    assert row[1] == p.task_description


def test_put_replaces_existing(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway())
    store.put(_pathway())  # same id
    with sqlite3.connect(tmp_path / "p.db") as con:
        count = con.execute("SELECT COUNT(*) FROM pathways").fetchone()[0]
    assert count == 1


def test_find_returns_best_match_above_threshold(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    # Two pathways with distinct embeddings.
    p_close = _pathway(id_="pathway_close", embedding=[1.0, 0.0, 0.0])
    p_far = _pathway(id_="pathway_far", embedding=[0.0, 1.0, 0.0])
    store.put(p_close)
    store.put(p_far)

    # Stub the embedder so we get a deterministic query vec.
    store._embed_query = lambda _: __import__("numpy").array([1.0, 0.0, 0.0])  # type: ignore[attr-defined]

    match = store.find("query task", threshold=0.85)
    assert match is not None
    assert match.pathway.id == "pathway_close"
    assert match.similarity > 0.99


def test_find_returns_none_when_below_threshold(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway(embedding=[1.0, 0.0, 0.0])
    store.put(p)
    store._embed_query = lambda _: __import__("numpy").array([0.0, 1.0, 0.0])  # type: ignore[attr-defined]

    match = store.find("orthogonal task", threshold=0.85)
    assert match is None


def test_find_returns_none_on_empty_store(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store._embed_query = lambda _: __import__("numpy").array([1.0, 0.0, 0.0])  # type: ignore[attr-defined]
    match = store.find("anything")
    assert match is None


def test_increment_hit(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    store.increment_hit(p.id)
    store.increment_hit(p.id)
    with sqlite3.connect(tmp_path / "p.db") as con:
        hits = con.execute(
            "SELECT hit_count FROM pathways WHERE id = ?", (p.id,)
        ).fetchone()[0]
    assert hits == 2


def test_increment_hit_noop_for_missing_id(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    # Should not raise even though pathway doesn't exist.
    store.increment_hit("pathway_nonexistent")


def test_list_all_returns_all_pathways(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.put(_pathway(id_="pathway_a"))
    store.put(_pathway(id_="pathway_b"))
    all_p = store.list_all()
    assert len(all_p) == 2
    ids = {p.id for p in all_p}
    assert ids == {"pathway_a", "pathway_b"}


def test_delete_removes_pathway(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    assert store.delete(p.id) is True
    assert store.delete(p.id) is False  # already gone
    assert store.list_all() == []


def test_stats(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p1 = _pathway(id_="a")
    p2 = _pathway(id_="b")
    store.put(p1)
    store.put(p2)
    store.increment_hit("a")
    s = store.stats()
    assert s["count"] == 2
    assert s["total_hits"] == 1


def test_v028_4_find_filters_out_stale_embedding_rows(tmp_path):
    """v0.28.4 — find() now filters rows whose embedding_model /
    embedding_model_version don't match the current distiller. Pre-fix,
    vectors from an older embedder ran cosine-sim against current-model
    queries in incompatible spaces, silently surfacing wrong matches.
    """
    import numpy as np
    from opendaisugi._search import _MODEL_NAME
    from opendaisugi.distiller import _EMBEDDING_MODEL_VERSION

    store = PathwayStore(tmp_path / "p.db")
    # Stale row: same vector, different (model, version).
    p_stale = _pathway(id_="stale", embedding=[1.0, 0.0, 0.0])
    p_stale.embedding_model = "old-model-v0"
    p_stale.embedding_model_version = "1"
    store.put(p_stale)
    # Current row: matches the current distiller constants.
    p_fresh = _pathway(id_="fresh", embedding=[1.0, 0.0, 0.0])
    p_fresh.embedding_model = _MODEL_NAME
    p_fresh.embedding_model_version = _EMBEDDING_MODEL_VERSION
    store.put(p_fresh)

    store._embed_query = lambda _: np.array([1.0, 0.0, 0.0])  # type: ignore[attr-defined]
    match = store.find("query", threshold=0.85)
    assert match is not None
    assert match.pathway.id == "fresh", "stale-embedding row must be excluded"


def test_v028_4_find_returns_none_when_all_rows_stale(tmp_path):
    """v0.28.4 — if every row is stale, find() returns None and the user
    gets a warning telling them to re-tend."""
    import numpy as np

    store = PathwayStore(tmp_path / "p.db")
    p = _pathway(embedding=[1.0, 0.0, 0.0])
    p.embedding_model = "old-model-v0"
    p.embedding_model_version = "1"
    store.put(p)
    store._embed_query = lambda _: np.array([1.0, 0.0, 0.0])  # type: ignore[attr-defined]
    match = store.find("query", threshold=0.0)
    assert match is None
