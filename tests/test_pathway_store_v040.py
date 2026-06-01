"""Tests for v0.4.0 pathway lifecycle fields and migration."""

from __future__ import annotations

import sqlite3
import time

import pytest

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.pathway import CompiledPathway
from opendaisugi.pathway_store import PathwayStore


def _pathway(id_: str = "pathway_abc12345") -> CompiledPathway:
    env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
    plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
    return CompiledPathway(
        id=id_,
        task_description="generalized task",
        task_embedding=[0.1, 0.2, 0.3],
        envelope=env,
        plan_template=plan,
        source_trace_ids=[],
        distilled_at=time.time(),
    )


def test_new_pathway_defaults_lifecycle_fields(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    loaded = store.list_all()[0]
    assert loaded.last_activation_at == 0.0
    assert loaded.failure_count == 0


def test_increment_hit_stamps_last_activation_at(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    before = time.time()
    store.increment_hit(p.id)
    after = time.time()

    loaded = store.list_all()[0]
    assert loaded.hit_count == 1
    assert before <= loaded.last_activation_at <= after


def test_mark_failure_increments_failure_count(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    p = _pathway()
    store.put(p)
    store.mark_failure(p.id)
    store.mark_failure(p.id)

    loaded = store.list_all()[0]
    assert loaded.failure_count == 2


def test_mark_failure_noop_for_missing_id(tmp_path):
    store = PathwayStore(tmp_path / "p.db")
    store.mark_failure("pathway_nonexistent")


def test_additive_migration_from_v031_schema(tmp_path):
    """A v0.3.1-shaped DB (no lifecycle columns) must migrate and load cleanly."""
    db = tmp_path / "legacy.db"

    # Build the v0.3.1 schema by hand — no last_activation_at or failure_count.
    with sqlite3.connect(db) as con:
        con.executescript(
            """
            CREATE TABLE pathways (
                id TEXT PRIMARY KEY,
                task_description TEXT NOT NULL,
                task_embedding_json TEXT NOT NULL,
                envelope_json TEXT NOT NULL,
                plan_template_json TEXT NOT NULL,
                source_trace_ids_json TEXT NOT NULL,
                pitfalls_json TEXT NOT NULL,
                validation_score REAL NOT NULL,
                version INTEGER NOT NULL DEFAULT 1,
                hit_count INTEGER NOT NULL DEFAULT 0,
                distilled_at REAL NOT NULL,
                embedding_model TEXT NOT NULL DEFAULT '',
                embedding_model_version TEXT NOT NULL DEFAULT ''
            );
            """
        )
        # Insert a legacy row with only v0.3.1 columns present.
        env = Envelope(generated_by="test", task="T", permissions=Permission(shell=True))
        plan = ActionPlan(source="tmpl", task="T", steps=[ShellStep(id="s1", command="echo")])
        con.execute(
            "INSERT INTO pathways (id, task_description, task_embedding_json, "
            "envelope_json, plan_template_json, source_trace_ids_json, "
            "pitfalls_json, validation_score, version, hit_count, distilled_at, "
            "embedding_model, embedding_model_version) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                "legacy_id",
                "legacy task",
                "[0.1, 0.2, 0.3]",
                env.model_dump_json(),
                plan.model_dump_json(),
                "[]",
                "[]",
                0.9,
                1,
                0,
                time.time(),
                "",
                "",
            ),
        )

    # Opening with v0.4.0 PathwayStore must migrate.
    store = PathwayStore(db)

    with sqlite3.connect(db) as con:
        cols = {r[1] for r in con.execute("PRAGMA table_info(pathways)")}
    assert "last_activation_at" in cols
    assert "failure_count" in cols

    # Legacy row must load with default lifecycle values.
    loaded = store.list_all()
    assert len(loaded) == 1
    assert loaded[0].id == "legacy_id"
    assert loaded[0].last_activation_at == 0.0
    assert loaded[0].failure_count == 0


def test_migration_is_idempotent(tmp_path):
    """Opening a v0.4.0 store twice must not fail on ALTER TABLE."""
    db = tmp_path / "p.db"
    store = PathwayStore(db)
    store.put(_pathway())
    # Re-open — should be a no-op migration.
    store2 = PathwayStore(db)
    assert len(store2.list_all()) == 1
