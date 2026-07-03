"""Dogfood runner for the PI-VLA kit.

Three scenarios:
1. Clean — three skills inside workspace bounds, VLA rolls out each;
   integrity passes; receipts carry real MuJoCo state.
2. Out-of-bounds target — one skill targets a pose outside workspace_bounds;
   verifier rejects pre-execution via the v0.8 Z3 trajectory check.
3. Delegation attempt under physical stakes — preferred_model='haiku' on
   a VLAStep; _check_delegation_safety rejects pre-execution. (The VLA
   itself is a motor primitive, not LLM delegation; the guard fires on
   agent-authored LLM routing.)
"""
from __future__ import annotations

import asyncio
import json
import os
import sys
from pathlib import Path

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from envelope import build_envelope
from plan import build_plan

from opendaisugi.approval import CallbackStrategy
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, VLAStep
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify
from opendaisugi.vla_executor import MockVLAExecutor

# Repo's two-joint test fixture — same arm exercised by tests/test_mujoco_pickplace.py.
REPO_ROOT = Path(__file__).resolve().parents[2]
MJCF = REPO_ROOT / "tests" / "fixtures" / "mjcf" / "two_joint_arm.xml"


async def run_clean(journal_dir: Path) -> dict:
    env = build_envelope()
    plan = build_plan([
        {"task": "approach the cup", "target_pose": (0.30, 0.20, 0.0)},
        {"task": "pick up the cup", "target_pose": (0.20, 0.30, 0.0)},
        {"task": "place on shelf",  "target_pose": (0.10, 0.40, 0.0)},
    ])
    vr = verify(plan, env)
    if not vr.ok:
        return {"label": "scenario_1_clean", "verify_ok": False,
                "violations": [{"stage": v.stage, "message": v.message[:200]}
                               for v in vr.violations]}
    j = Journal(data_dir=journal_dir)
    exe = MockVLAExecutor(mjcf_path=str(MJCF), num_actions=15)
    sup = Supervisor(
        executors={"vla": exe},
        journal=j,
        approval=CallbackStrategy(lambda step, env: True),
    )
    session = await sup.run(plan, env)
    receipts = j.receipts_for_run(session.id)
    sample_evidence = (
        json.loads(receipts[0].evidence["stdout"]) if receipts else {}
    )
    return {
        "label": "scenario_1_clean",
        "verify_ok": True,
        "run_status": session.status.value,
        "integrity_passed": session.integrity_passed,
        "receipts": len(receipts),
        "expected_steps": len(plan.steps),
        "executor": "MockVLAExecutor",
        "sample_actions_executed": sample_evidence.get("actions_executed"),
        "sample_final_ee_xyz": sample_evidence.get("end_effector_xyz_final"),
    }


def run_out_of_bounds() -> dict:
    """One skill targets a pose far outside workspace_bounds."""
    env = build_envelope()
    plan = build_plan([
        {"task": "reach into the void", "target_pose": (5.0, 0.0, 0.0)},
    ])
    vr = verify(plan, env)
    return {
        "label": "scenario_2_out_of_bounds",
        "verify_ok": vr.ok,
        "violations": [{"stage": v.stage, "message": v.message[:200]}
                       for v in vr.violations],
    }


def run_delegation_attempt() -> dict:
    """preferred_model='haiku' on a VLAStep under physical stakes →
    rejected by v0.19's _check_delegation_safety. The VLA itself isn't
    LLM-routing; the guard fires when an agent-author sets a model hint
    on a step inside a physical-stakes envelope."""
    env = build_envelope()
    bad = VLAStep(
        id="s0", task="reach", target_pose=(0.2, 0.2, 0.0),
        preferred_model="haiku",
    )
    plan = ActionPlan(source="pi-vla-kit", task="bad: delegate motion",
                      steps=[bad])
    vr = verify(plan, env)
    return {
        "label": "scenario_3_motion_delegation_attempt",
        "verify_ok": vr.ok,
        "violations": [{"stage": v.stage, "message": v.message[:200]}
                       for v in vr.violations],
    }


async def main() -> None:
    jdir = Path("/tmp/pi_vla_journal")
    clean = await run_clean(jdir)
    oob = run_out_of_bounds()
    delegated = run_delegation_attempt()
    print(json.dumps({
        "scenario_1_clean": clean,
        "scenario_2_out_of_bounds": oob,
        "scenario_3_motion_delegation_attempt": delegated,
    }, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
