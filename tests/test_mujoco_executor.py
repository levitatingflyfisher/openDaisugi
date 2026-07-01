"""Tests for MuJoCoExecutor — skipped unless the `robotics` extra is installed."""

from pathlib import Path

import pytest

mujoco = pytest.importorskip("mujoco")

from opendaisugi.executor_mujoco import (
    RC_CONTACT_VIOLATION,
    RC_IK_FAILED,
    RC_TORQUE_VIOLATION,
    MuJoCoExecutor,
)
from opendaisugi.models import CartesianMoveStep, GripperStep, JointMoveStep, SimulationResetStep

FIXTURE = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


def _executor(**kw) -> MuJoCoExecutor:
    return MuJoCoExecutor(str(FIXTURE), **kw)


def test_executor_loads_mjcf():
    ex = _executor()
    # 2 hinge joints + 1 slide (gripper); 3 position actuators.
    assert ex.model.njnt == 3
    assert ex.model.nu == 3


def test_sim_reset_step_runs():
    ex = _executor()
    ex.data.qpos[0] = 0.5
    result = ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    assert result.rc == 0
    assert ex.data.qpos[0] == pytest.approx(0.0, abs=1e-6)


def test_joint_move_reaches_target_within_tolerance():
    ex = _executor()
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    target = 0.7
    result = ex.run(
        JointMoveStep(id="m", joint_targets={"j1": target}, duration_s=0.5),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == 0
    jid = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_JOINT, "j1")
    qpos_adr = ex.model.jnt_qposadr[jid]
    assert ex.data.qpos[qpos_adr] == pytest.approx(target, abs=0.1)


def test_cartesian_step_reaches_target_via_ik():
    ex = _executor()
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    # Reachable on the planar arm: r = |target| in XY ≤ 0.6 (link1+link2 length),
    # z must be 0 because both hinges are around +z.
    target = (0.4, 0.3, 0.0)
    result = ex.run(
        CartesianMoveStep(id="c", target_position=target),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == 0, result.stdout
    ee_id = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_BODY, "end_effector")
    ee = ex.data.xpos[ee_id]
    assert ee[0] == pytest.approx(target[0], abs=0.05)
    assert ee[1] == pytest.approx(target[1], abs=0.05)


def test_cartesian_step_tight_tolerance_is_actually_tight():
    # Regression guard: the IK solver must respect ik_tol exactly. A previous
    # 10×ik_tol post-loop slack meant a caller asking for 1e-4 precision could
    # silently get 1cm — enough to cross a workspace bound the envelope
    # already promised was inside.
    import numpy as np

    tol = 1e-4
    ex = _executor(ik_tol=tol, ik_max_iter=1000)
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    target = (0.4, 0.3, 0.0)
    result = ex.run(
        CartesianMoveStep(id="c", target_position=target),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == 0, result.stdout
    # IK solver convergence is on the scratch data; the reached position is
    # whatever the settle phase arrives at. Re-solve IK at the executor's own
    # data to verify the qpos IK found really is within tol of target.
    ee_id = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_BODY, "end_effector")
    # After settle, ee drifts from the IK solution (P-control overshoot etc.),
    # but the IK solution itself must honor tol. Drive fresh IK:
    q_solved = ex._solve_ik_position(target)
    scratch_data = mujoco.MjData(ex.model)
    scratch_data.qpos[:] = q_solved
    mujoco.mj_forward(ex.model, scratch_data)
    residual = float(np.linalg.norm(np.array(target) - scratch_data.xpos[ee_id]))
    assert residual < tol, f"IK reported success but residual={residual} > tol={tol}"


def test_cartesian_step_unreachable_returns_ik_failed():
    ex = _executor()
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    # Arm reach is 0.6; (2, 0, 0) is far outside it.
    result = ex.run(
        CartesianMoveStep(id="c", target_position=(2.0, 0.0, 0.0)),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == RC_IK_FAILED
    assert "IK failed" in result.stdout


def test_unknown_joint_raises_key_error():
    ex = _executor()
    with pytest.raises(KeyError):
        ex.run(
            JointMoveStep(id="m", joint_targets={"j_missing": 0.0}, duration_s=0.1),
            timeout_s=1, max_output_bytes=1024,
        )


# --- Task 8: gripper -----------------------------------------------------


def test_gripper_open_drives_joint_to_upper_bound():
    ex = _executor()
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(GripperStep(id="g", action="open", hold_s=1.0),
                    timeout_s=1, max_output_bytes=1024)
    assert result.rc == 0
    jid = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_JOINT, "j_grip")
    qpos_adr = ex.model.jnt_qposadr[jid]
    lo, hi = ex.model.jnt_range[jid]
    assert ex.data.qpos[qpos_adr] == pytest.approx(hi, abs=0.01)


def test_gripper_raises_when_target_range_undeclared(tmp_path):
    # An MJCF with an unlimited gripper joint AND no actuator ctrlrange has
    # no declared open/close targets. Previous fallback silently invented
    # (-0.05, 0.05); the honest behavior is to surface the misconfig.
    mjcf = tmp_path / "bad_gripper.xml"
    mjcf.write_text(
        '<mujoco>'
        '<compiler angle="radian"/>'
        '<worldbody>'
        '<body><joint name="j_grip" type="slide" axis="0 1 0"/>'
        '<geom type="box" size="0.01 0.01 0.01" contype="0" conaffinity="0"/>'
        '</body>'
        '</worldbody>'
        '<actuator><position name="a_grip" joint="j_grip" kp="10"/></actuator>'
        '</mujoco>'
    )
    ex = MuJoCoExecutor(str(mjcf))
    with pytest.raises(ValueError, match="neither joint range nor ctrlrange"):
        ex.run(GripperStep(id="g", action="open", hold_s=0.01),
               timeout_s=1, max_output_bytes=1024)


def test_gripper_close_drives_joint_to_lower_bound():
    ex = _executor()
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(GripperStep(id="g", action="close", hold_s=1.0),
                    timeout_s=1, max_output_bytes=1024)
    assert result.rc == 0
    jid = mujoco.mj_name2id(ex.model, mujoco.mjtObj.mjOBJ_JOINT, "j_grip")
    qpos_adr = ex.model.jnt_qposadr[jid]
    lo, hi = ex.model.jnt_range[jid]
    assert ex.data.qpos[qpos_adr] == pytest.approx(lo, abs=0.01)


# --- Task 8: rollout-time torque guard -----------------------------------


def test_torque_limit_triggers_violation_on_aggressive_move():
    # torque_limit=0.1 Nm is far below kp*(error) for a 0.5 rad step — the
    # first physics step's actuator_force will exceed it and abort.
    ex = _executor(torque_limit=0.1)
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(
        JointMoveStep(id="m", joint_targets={"j1": 0.5}, duration_s=0.5),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == RC_TORQUE_VIOLATION
    assert "torque_limit violated" in result.stdout


def test_torque_limit_not_triggered_by_gentle_move():
    # Generous bound; peak force stays below it.
    ex = _executor(torque_limit=100.0)
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(
        JointMoveStep(id="m", joint_targets={"j1": 0.1}, duration_s=0.5),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == 0


# --- rollout-time contact guard (dedicated contact-at-rest fixture) -------

# Why a separate fixture: the two_joint_arm fixture has contype=0 on the arm
# links by default, so only finger_tip vs block_geom can collide — and those
# geometries never actually intersect during a simple j1 sweep (finger arcs
# at radius ~0.6, block sits at radius ~0.4). The earlier test accepted both
# outcomes and never actually exercised the guard. This fixture places an
# obstacle box fully overlapping the arm's tip at qpos=0, so ncon > 0 on the
# first physics step regardless of trajectory.
CONTACT_FIXTURE = Path(__file__).parent / "fixtures" / "mjcf" / "contact_at_rest.xml"


def test_forbid_contacts_fires_deterministically_at_rest():
    ex = MuJoCoExecutor(str(CONTACT_FIXTURE), forbid_contacts=True, settle_steps=10)
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(
        JointMoveStep(id="m", joint_targets={"j1": 0.1}, duration_s=0.1),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == RC_CONTACT_VIOLATION
    assert "contact detected" in result.stdout
    # Contact pairs should reference the named geoms, not opaque ids.
    assert "tip" in result.stdout and "wall_geom" in result.stdout


def test_forbid_contacts_disabled_tolerates_overlapping_geoms():
    ex = MuJoCoExecutor(str(CONTACT_FIXTURE), forbid_contacts=False, settle_steps=10)
    ex.run(SimulationResetStep(id="r"), timeout_s=1, max_output_bytes=1024)
    result = ex.run(
        JointMoveStep(id="m", joint_targets={"j1": 0.1}, duration_s=0.1),
        timeout_s=1, max_output_bytes=1024,
    )
    # ncon > 0 but guard disabled — step should succeed.
    assert result.rc == 0
    assert ex.data.ncon > 0
