"""MuJoCo envelope smoke: load MJCF, roll out plan, verify bounds.

Runs on any machine with ``pip install opendaisugi[robotics]``. The
4080 box is overkill; a laptop with MuJoCo installed handles this in
under a second.

Story:
    1. Build a robotics envelope with declared workspace + joint limits.
    2. Stage 1 verify the plan structurally (no MuJoCo needed).
    3. Load the MJCF and run the plan through MuJoCoExecutor.
    4. After rollout, read back the real ``qpos`` and assert it sits
       inside the envelope's declared joint bounds.

This is the "the envelope said it, MuJoCo confirmed it" loop. If the
executor ever drifts outside the declared bounds (actuator saturation,
IK overshoot, contact push-off), the final asserts catch it.
"""

from __future__ import annotations

import sys
from pathlib import Path

try:
    import mujoco  # noqa: F401
except ImportError:
    print(
        "mujoco not installed. Install with: pip install 'opendaisugi[robotics]'",
        file=sys.stderr,
    )
    raise SystemExit(2)

from opendaisugi.executor_mujoco import MuJoCoExecutor
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    JointMoveStep,
    Permission,
    SimulationResetStep,
)
from opendaisugi.verify import verify


REPO_ROOT = Path(__file__).resolve().parents[3]
MJCF = REPO_ROOT / "tests" / "fixtures" / "mjcf" / "two_joint_arm.xml"


def build_envelope() -> Envelope:
    return Envelope(
        generated_by="mujoco-smoke",
        task="reach a target joint configuration",
        stakes="physical",
        permissions=Permission(
            workspace_bounds=((-0.6, -0.6, -0.1), (0.6, 0.6, 0.1)),
            joint_limits={"j1": (-1.5, 1.5), "j2": (-1.5, 1.5)},
            velocity_limit=2.0,
        ),
        invariants=[
            Invariant(type="joint_limits_respected", description="stay in declared hinge range"),
            Invariant(type="velocity_bounded", description="stay under 2.0 rad/s"),
        ],
    )


def build_plan() -> ActionPlan:
    return ActionPlan(
        source="mujoco-smoke",
        task="reach a target joint configuration",
        steps=[
            SimulationResetStep(id="r"),
            JointMoveStep(id="m1", joint_targets={"j1": 0.7}, duration_s=0.5),
            JointMoveStep(id="m2", joint_targets={"j2": -0.6}, duration_s=0.5, depends_on=["m1"]),
        ],
    )


def main() -> int:
    envelope = build_envelope()
    plan = build_plan()

    # Stage 1 — structural verify, no MuJoCo needed.
    result = verify(plan, envelope)
    if not result.ok:
        print(f"Stage 1 rejected the plan: {result.violations}", file=sys.stderr)
        return 1
    print(f"Stage 1 verify OK ({result.duration_ms:.1f} ms)")

    # Rollout — MuJoCo executes the plan against the real MJCF.
    executor = MuJoCoExecutor(str(MJCF))
    for step in plan.steps:
        r = executor.run(step, timeout_s=2, max_output_bytes=1024)
        if r.rc != 0:
            print(f"Executor failed on {step.id}: rc={r.rc}", file=sys.stderr)
            return 1

    # Read back the actual joint state MuJoCo produced.
    # jnt_qposadr[i] gives the first qpos index for joint i. Scalar read is
    # only correct for hinge/slide (1 DOF); ball/free joints pack 4/7 floats,
    # so assert the joint type before reducing to a scalar angle.
    j1_id = mujoco.mj_name2id(executor.model, mujoco.mjtObj.mjOBJ_JOINT, "j1")
    j2_id = mujoco.mj_name2id(executor.model, mujoco.mjtObj.mjOBJ_JOINT, "j2")
    hinge = mujoco.mjtJoint.mjJNT_HINGE
    assert executor.model.jnt_type[j1_id] == hinge, "j1 must be hinge for scalar qpos read"
    assert executor.model.jnt_type[j2_id] == hinge, "j2 must be hinge for scalar qpos read"
    j1_qpos = float(executor.data.qpos[executor.model.jnt_qposadr[j1_id]])
    j2_qpos = float(executor.data.qpos[executor.model.jnt_qposadr[j2_id]])

    # The envelope's joint_limits are the ground truth MuJoCo must obey.
    low1, high1 = envelope.permissions.joint_limits["j1"]
    low2, high2 = envelope.permissions.joint_limits["j2"]
    assert low1 <= j1_qpos <= high1, f"j1 out of declared bounds: {j1_qpos}"
    assert low2 <= j2_qpos <= high2, f"j2 out of declared bounds: {j2_qpos}"

    print(f"Rollout OK: j1={j1_qpos:+.3f} (bounds [{low1}, {high1}])")
    print(f"            j2={j2_qpos:+.3f} (bounds [{low2}, {high2}])")
    print("Envelope bounds held across rollout. Smoke kit green.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
