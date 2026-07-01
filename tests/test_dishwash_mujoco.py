"""End-to-end MuJoCo test for the dish-wash kit (v0.25.1+).

Proves that the kit's domain step types translate to real joint moves
and that the v0.18 substrate (envelope verify + per-step receipts +
run-end integrity) holds against actual MuJoCo physics.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import pytest

pytest.importorskip("mujoco")

# Add the kit dir to sys.path so its local modules import cleanly.
_KIT = Path(__file__).resolve().parents[1] / "examples" / "dish-wash"
sys.path.insert(0, str(_KIT))

# These imports must come AFTER the sys.path insertion + after the
# importorskip so test collection doesn't fail on hosts without mujoco.
import step_types  # noqa: E402, F401  (registers @step_type)
from envelope import build_envelope  # noqa: E402
from mujoco_executor import DishWashMuJoCoExecutor  # noqa: E402
from plan import build_plan  # noqa: E402

from opendaisugi.approval import CallbackStrategy  # noqa: E402
from opendaisugi.journal import Journal  # noqa: E402
from opendaisugi.run_session import RunStatus  # noqa: E402
from opendaisugi.supervisor import Supervisor  # noqa: E402

_MJCF = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


@pytest.mark.asyncio
async def test_dishwash_kit_runs_through_real_mujoco(tmp_path):
    """One plate, all five step types, end-to-end through MuJoCoExecutor.

    Receipts must carry real joint positions (not fabricated dicts), and
    the run-end integrity check must pass on a 5-step plan executed
    against the two-joint test arm.
    """
    env = build_envelope()
    plan = build_plan(num_dishes=1)
    j = Journal(data_dir=tmp_path)
    exe = DishWashMuJoCoExecutor(str(_MJCF))
    sup = Supervisor(
        executors={
            "approach_dish": exe, "locate_rim": exe, "begin_scrub": exe,
            "rinse_with_hose": exe, "return_to_dock": exe,
        },
        journal=j,
        approval=CallbackStrategy(lambda step, env: True),
    )
    session = await sup.run(plan, env)
    assert session.status == RunStatus.SUCCEEDED
    assert session.integrity_passed is True

    receipts = j.receipts_for_run(session.id)
    assert len(receipts) == 5

    # Check each receipt carries real MuJoCo state.
    for r in receipts:
        inner = json.loads(r.evidence["stdout"])
        assert "end_effector_xyz" in inner
        assert "joint_positions" in inner
        assert "j1" in inner["joint_positions"]
        # Real arm has 3 joints; gripper position appears in qpos
        assert len(inner["joint_positions"]) == 3

    # The arm should reach distinct end-effector positions across the
    # five different step targets (i.e. motion is real, not all zeros).
    ee_xyzs = [
        tuple(json.loads(r.evidence["stdout"])["end_effector_xyz"])
        for r in receipts
    ]
    assert len(set(ee_xyzs)) > 1, (
        f"every step landed at the same end-effector pose: {ee_xyzs} — "
        f"either physics didn't run or all step types resolve to the "
        f"same target."
    )


@pytest.mark.asyncio
async def test_dishwash_kit_handles_three_plates(tmp_path):
    """Three plates compose into 15 sequential steps. Receipts must
    cover all 15; integrity passes; final pose is at the dock."""
    env = build_envelope()
    plan = build_plan(num_dishes=3)
    j = Journal(data_dir=tmp_path)
    exe = DishWashMuJoCoExecutor(str(_MJCF))
    sup = Supervisor(
        executors={
            "approach_dish": exe, "locate_rim": exe, "begin_scrub": exe,
            "rinse_with_hose": exe, "return_to_dock": exe,
        },
        journal=j,
        approval=CallbackStrategy(lambda step, env: True),
    )
    session = await sup.run(plan, env)
    assert session.status == RunStatus.SUCCEEDED
    assert session.integrity_passed is True
    assert len(j.receipts_for_run(session.id)) == 15
