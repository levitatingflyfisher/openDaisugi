"""End-to-end v0.3.0 flow:
accumulate successful traces → tend → matching task gets compiled envelope.
"""

import sqlite3

import numpy as np
import pytest

from opendaisugi import Daisugi
from opendaisugi.distiller import Distiller, GeneralizedTemplate
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
)


def _success(journal, task: str):
    """Seed a successful trace.

    ``Journal.log()`` writes the YAML + SQLite row but leaves run_id /
    run_status NULL, so ``list_successful_traces`` (which filters on
    run_status='succeeded') would not find it. We log first so
    ``load_trace()`` works (the Distiller needs the YAML body), then
    update the run columns via raw SQL — same pattern used in
    ``tests/test_distiller_tend.py``.
    """
    env = Envelope(
        generated_by="test", task=task,
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan = ActionPlan(
        source="t", task=task,
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )
    r = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id=env.id, plan_id=plan.id, duration_ms=0.1,
    )
    trace_id = journal.log(task=task, envelope=env, plan=plan, result=r)
    run_id = f"run_{trace_id}"
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (run_id, trace_id),
        )
    return trace_id


@pytest.mark.asyncio
async def test_full_loop_accumulate_tend_consume(tmp_path, mock_llm_client, monkeypatch):
    d = Daisugi(data_dir=tmp_path, cache=False)

    # Seed three successful traces for the same task-shape.
    for i in range(3):
        _success(d.journal, f"find stale tmp files batch {i}")

    # Stub embedder for Distiller AND pathway_store.find — same unit vector.
    # Accessing d.pathway_store here forces the lazy property to construct it
    # so monkeypatch can bind _embed_query to the concrete instance.
    store = d.pathway_store
    unit = np.array([1.0, 0.0, 0.0])
    monkeypatch.setattr(
        Distiller, "_embed_tasks",
        lambda self, tasks: np.tile(unit, (len(tasks), 1)),
    )
    monkeypatch.setattr(
        Distiller, "_embed_plan_structures",
        lambda self, sigs: np.tile(unit, (len(sigs), 1)),
    )
    monkeypatch.setattr(store, "_embed_query", lambda _: unit)

    # Stub LLM generalization (deterministic template).
    env_tmpl = Envelope(
        generated_by="distilled", task="T",
        permissions=Permission(shell=True, shell_allowlist=["find"]),
    )
    plan_tmpl = ActionPlan(
        source="template", task="T",
        steps=[ShellStep(id="s1", command="find /tmp -name '*.tmp'")],
    )

    async def _fake_gen(**kwargs):
        return GeneralizedTemplate(
            task_description="find stale temp files",
            plan_template=plan_tmpl,
        )

    from opendaisugi import distiller as dist_mod
    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_gen)

    # Run tend.
    report = await d.tend(min_traces=3)
    assert report.created == 1

    # Now a new task with the same shape should hit the pathway.
    mock_llm_client.chat.completions.last_call = {}
    env = await d.generate_envelope("find stale tmp files newly arrived")
    assert env.generated_by.startswith("compiled-pathway:")
    # LLM must NOT have been called for the envelope this time.
    assert mock_llm_client.call_count == 0
