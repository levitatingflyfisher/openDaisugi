"""Distiller.tend() aggregates robot traces into a CompiledPathway."""

import sqlite3

import numpy as np
import pytest

from opendaisugi.distiller import Distiller, GeneralizedTemplate
from opendaisugi import distiller as dist_mod
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    Envelope,
    GripperStep,
    Invariant,
    JointMoveStep,
    Permission,
    SimulationResetStep,
    VerificationResult,
)
from opendaisugi.pathway_store import PathwayStore


def _robot_plan_env(task: str) -> tuple[ActionPlan, Envelope]:
    env = Envelope(
        generated_by="test", task=task,
        permissions=Permission(
            workspace_bounds=((0, -0.5, 0), (1, 0.5, 1)),
            velocity_limit=2.0,
            joint_limits={"j1": (-3.0, 3.0)},
        ),
        invariants=[Invariant(type="end_effector_in_workspace", description="in ws")],
    )
    plan = ActionPlan(source="t", task=task, steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(id="home", joint_targets={"j1": 0.0}, duration_s=1.0,
                      depends_on=["reset"]),
        CartesianMoveStep(id="go", target_position=(0.3, 0.2, 0.0),
                          depends_on=["home"]),
        GripperStep(id="grab", action="close", depends_on=["go"]),
    ])
    return plan, env


def _write_robot_success_trace(journal: Journal, task: str) -> str:
    plan, env = _robot_plan_env(task)
    result = VerificationResult(
        ok=True, violations=[], warnings=[],
        envelope_id=env.id, plan_id=plan.id, duration_ms=0.1,
    )
    trace_id = journal.log(task=task, envelope=env, plan=plan, result=result)
    run_id = f"run_{trace_id}"
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "UPDATE traces SET run_id = ?, run_status = 'succeeded' WHERE id = ?",
            (run_id, trace_id),
        )
    return trace_id


@pytest.mark.asyncio
async def test_tend_distills_robot_traces_into_pathway(tmp_path, monkeypatch):
    journal = Journal(data_dir=tmp_path)
    store = PathwayStore(tmp_path / "pathways.db")

    for i in range(3):
        _write_robot_success_trace(journal, f"pick block at shelf {i}")

    distiller = Distiller(
        journal=journal,
        pathway_store=store,
        model="test-model",
        min_traces=3,
    )

    # Deterministic embedder: cluster all three traces together.
    monkeypatch.setattr(distiller, "_embed_tasks",
                        lambda tasks: np.ones((len(tasks), 4)))
    monkeypatch.setattr(distiller, "_embed_plan_structures",
                        lambda sigs: np.ones((len(sigs), 4)))

    plan_tmpl, env_tmpl = _robot_plan_env("pick block")

    async def _fake_generalize_template(*, plan, envelope, pitfalls, model):
        return GeneralizedTemplate(
            task_description="pick a block",
            envelope=env_tmpl,
            plan_template=plan_tmpl,
        )

    async def _fake_improve_envelope(*, envelope, failing_plans, model):
        return envelope

    monkeypatch.setattr(dist_mod, "_generalize_template", _fake_generalize_template)
    monkeypatch.setattr(dist_mod, "_improve_envelope", _fake_improve_envelope)

    report = await distiller.tend()

    assert report.created == 1
    pathways = store.list_all()
    assert len(pathways) == 1
    step_types = [s.type for s in pathways[0].plan_template.steps]
    assert "sim_reset" in step_types
    assert "cartesian_move" in step_types
    assert "gripper" in step_types
