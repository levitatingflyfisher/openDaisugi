"""Dogfood runner for the dish-wash kit.

Three scenarios:
1. Clean — wash 3 plates; verify passes; supervisor runs through 15 steps;
   integrity check passes; every receipt carries motion-domain evidence.
2. Missing terminal step — drop the final ReturnToDock; envelope invariant
   ``every_plate_wash_ends_with_return_to_dock`` rejects pre-execution.
3. Delegation attempt under physical stakes — author tries to set
   ``preferred_model='haiku'`` on a motion step; ``_check_delegation_safety``
   refuses pre-execution.
"""
from __future__ import annotations

import asyncio
import json
import json as _json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pathlib import Path

import step_types  # noqa: F401  (registers step types via @step_type)
from envelope import build_envelope
from plan import build_plan

from opendaisugi.approval import CallbackStrategy
from opendaisugi.executor import ExecutorResult
from opendaisugi.journal import Journal
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify


class MockRoboticExecutor:
    """Deterministic stand-in for a real robot executor.

    Produces evidence each step's postcondition accepts. In a real
    deployment, the executor would talk to MuJoCo / a ROS topic / a real
    robot driver; here it returns plausible motion telemetry so the
    receipt + integrity machinery can be exercised end-to-end without
    hardware.
    """

    def run(self, step, *, timeout_s, max_output_bytes):
        if step.type == "approach_dish":
            ev = {
                "end_effector_xyz": [0.45, 0.10 * step.dish_index, 0.30],
                "duration_ms": int(step.duration_s * 1000),
            }
        elif step.type == "locate_rim":
            ev = {
                "rim_pose_error_mm": 0.8,
                "rim_normal": [0.0, 0.0, 1.0],
            }
        elif step.type == "begin_scrub":
            ev = {
                "scrub_complete": True,
                "max_force_n": step.contact_force_n,
                "duration_ms": int(step.duration_s * 1000),
            }
        elif step.type == "rinse_with_hose":
            ev = {
                "rinse_volume_ml": int(step.flow_rate_lps * step.duration_s * 1000),
                "duration_ms": int(step.duration_s * 1000),
            }
        elif step.type == "return_to_dock":
            ev = {
                "end_effector_xyz": [0.0, 0.0, 0.50],  # dock pose
                "duration_ms": int(step.duration_s * 1000),
            }
        else:
            ev = {}
        return ExecutorResult(rc=0, stdout=_json.dumps(ev), duration_ms=1.0, timed_out=False)


def _build_executor(use_mujoco: bool):
    """Pick the executor: real MuJoCo simulation when available + opted in,
    deterministic mock otherwise."""
    if use_mujoco:
        from mujoco_executor import DishWashMuJoCoExecutor  # noqa: I001
        # Repo's two-joint test fixture — same model exercised by
        # tests/test_mujoco_pickplace.py.
        repo_root = Path(__file__).resolve().parents[2]
        mjcf = repo_root / "tests" / "fixtures" / "mjcf" / "two_joint_arm.xml"
        return DishWashMuJoCoExecutor(str(mjcf))
    return MockRoboticExecutor()


async def run_clean(journal_dir: Path, *, use_mujoco: bool = False) -> dict:
    env = build_envelope()
    plan = build_plan(num_dishes=3)
    vr = verify(plan, env)
    if not vr.ok:
        return {"label": "scenario_1_clean_three_plates", "verify_ok": False,
                "violations": [{"stage": v.stage, "message": v.message[:200]}
                               for v in vr.violations]}
    j = Journal(data_dir=journal_dir)
    exe = _build_executor(use_mujoco)
    sup = Supervisor(
        executors={
            "approach_dish": exe, "locate_rim": exe, "begin_scrub": exe,
            "rinse_with_hose": exe, "return_to_dock": exe,
        },
        journal=j,
        approval=CallbackStrategy(lambda step, env: True),
    )
    session = await sup.run(plan, env)
    receipts = j.receipts_for_run(session.id)
    return {
        "label": "scenario_1_clean_three_plates",
        "verify_ok": True,
        "run_status": session.status.value,
        "integrity_passed": session.integrity_passed,
        "receipts": len(receipts),
        "expected_steps": len(plan.steps),
        "executor": "DishWashMuJoCoExecutor" if use_mujoco else "MockRoboticExecutor",
        "model_ids": sorted({r.model_id for r in receipts if r.model_id}),
    }


async def run_missing_dock() -> dict:
    """Drop the terminal ReturnToDock — invariant rejects pre-execution."""
    from opendaisugi.models import ActionPlan
    env = build_envelope()
    full = build_plan(num_dishes=2).steps
    # Strip every ReturnToDock to simulate the bug the invariant catches.
    truncated = [s for s in full if s.type != "return_to_dock"]
    plan = ActionPlan(source="dish-wash-kit", task="bad: no dock",
                      steps=truncated)
    vr = verify(plan, env)
    return {
        "label": "scenario_2_missing_return_to_dock",
        "verify_ok": vr.ok,
        "violations": [{"stage": v.stage, "message": v.message[:200]}
                       for v in vr.violations],
    }


def run_delegation_attempt() -> dict:
    """Try to delegate a motion step to Haiku — physical-stakes guard
    refuses pre-execution. No await needed; verify is sync."""
    env = build_envelope()
    full = build_plan(num_dishes=1).steps
    # Stamp preferred_model on a motion step.
    full[0] = full[0].model_copy(update={"preferred_model": "haiku"})
    from opendaisugi.models import ActionPlan
    plan = ActionPlan(source="dish-wash-kit", task="bad: delegate motion",
                      steps=full)
    vr = verify(plan, env)
    return {
        "label": "scenario_3_motion_delegation_attempt",
        "verify_ok": vr.ok,
        "violations": [{"stage": v.stage, "message": v.message[:200]}
                       for v in vr.violations],
    }


async def main() -> None:
    jdir = Path("/tmp/dish_wash_journal")
    # Opt into the MuJoCo path via env var so the kit is runnable on any
    # host but proves itself against real physics when available. CI sets
    # OPENDAISUGI_DISHWASH_MUJOCO=1; local hands run without it.
    import os as _os
    use_mujoco = _os.environ.get("OPENDAISUGI_DISHWASH_MUJOCO") == "1"
    clean = await run_clean(jdir, use_mujoco=use_mujoco)
    missing = await run_missing_dock()
    delegated = run_delegation_attempt()
    print(json.dumps({
        "scenario_1_clean_three_plates": clean,
        "scenario_2_missing_return_to_dock": missing,
        "scenario_3_motion_delegation_attempt": delegated,
    }, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
