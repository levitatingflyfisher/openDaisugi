"""End-to-end pick-and-place sequencing through MuJoCoExecutor.

Exercises reset → joint_move (home) → cartesian_move (above block) →
gripper close → cartesian_move (lift) → cartesian_move (drop zone) →
gripper open on the two-joint planar fixture. This is a *sequencing*
test — it verifies every step type returns rc=0 when wired together,
not the physical grasping. A free-joint block + friction-based grasp
wants a 6-DOF arm and cone-contact tuning; that arrives with a real
Franka fixture in v0.9.
"""

from pathlib import Path

import pytest

mujoco = pytest.importorskip("mujoco")

from opendaisugi.executor_mujoco import MuJoCoExecutor
from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    GripperStep,
    JointMoveStep,
    SimulationResetStep,
)

FIXTURE = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


def test_pick_and_place_sequence_executes_cleanly():
    plan = ActionPlan(source="test", task="pick and place a block", steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(
            id="home",
            joint_targets={"j1": 0.0, "j2": 0.0},
            duration_s=0.5,
            depends_on=["reset"],
        ),
        CartesianMoveStep(
            id="approach",
            target_position=(0.35, 0.20, 0.0),
            depends_on=["home"],
        ),
        GripperStep(id="grasp", action="close", hold_s=0.3, depends_on=["approach"]),
        CartesianMoveStep(
            id="lift",
            target_position=(0.35, 0.15, 0.0),
            depends_on=["grasp"],
        ),
        CartesianMoveStep(
            id="transport",
            target_position=(0.20, 0.35, 0.0),
            depends_on=["lift"],
        ),
        GripperStep(id="release", action="open", hold_s=0.3, depends_on=["transport"]),
        CartesianMoveStep(
            id="retreat",
            target_position=(0.30, 0.30, 0.0),
            depends_on=["release"],
        ),
    ])
    ex = MuJoCoExecutor(str(FIXTURE))
    for step in plan.steps:
        result = ex.run(step, timeout_s=5, max_output_bytes=4096)
        assert result.rc == 0, f"step {step.id} failed: {result.stdout}"

    # After the full sequence, the ee should be near the retreat target.
    ee_id = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_BODY, "end_effector")
    ee = ex.data.xpos[ee_id]
    assert ee[0] == pytest.approx(0.30, abs=0.06)
    assert ee[1] == pytest.approx(0.30, abs=0.06)

    # Gripper should be in the open state after release.
    jid = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_JOINT, "j_grip")
    qpos_adr = ex.model.jnt_qposadr[jid]
    lo, hi = ex.model.jnt_range[jid]
    assert ex.data.qpos[qpos_adr] == pytest.approx(hi, abs=0.01)


def test_pick_and_place_under_torque_limit_runs_to_completion():
    # A torque budget generous enough to absorb the whole sequence; the
    # guard should not fire at any point.
    ex = MuJoCoExecutor(str(FIXTURE), torque_limit=100.0)
    steps = [
        SimulationResetStep(id="reset"),
        CartesianMoveStep(id="a", target_position=(0.35, 0.20, 0.0), depends_on=["reset"]),
        GripperStep(id="b", action="close", hold_s=0.3, depends_on=["a"]),
        CartesianMoveStep(id="c", target_position=(0.20, 0.30, 0.0), depends_on=["b"]),
        GripperStep(id="d", action="open", hold_s=0.3, depends_on=["c"]),
    ]
    for step in steps:
        result = ex.run(step, timeout_s=5, max_output_bytes=4096)
        assert result.rc == 0, f"{step.id}: {result.stdout}"
