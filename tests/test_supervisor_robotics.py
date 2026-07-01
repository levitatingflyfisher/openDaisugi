"""Supervisor → MuJoCoExecutor integration — envelope kwargs reach the executor."""

from pathlib import Path

import pytest

mujoco = pytest.importorskip("mujoco")

from opendaisugi.approval import CallbackStrategy
from opendaisugi.executor_mujoco import RC_TORQUE_VIOLATION, MuJoCoExecutor, robotics_executors
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    JointMoveStep,
    Permission,
    SimulationResetStep,
)
from opendaisugi.supervisor import Supervisor

APPROVE_ALL = CallbackStrategy(lambda step, env: True)


FIXTURE = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


def _plan_and_env(torque_limit: float | None = None) -> tuple[ActionPlan, Envelope]:
    perms = Permission(
        joint_limits={"j1": (-3.14, 3.14), "j2": (-3.14, 3.14)},
        velocity_limit=5.0,
    )
    if torque_limit is not None:
        perms.torque_limit = torque_limit
    env = Envelope(
        generated_by="t", task="move arm",
        permissions=perms,
        invariants=[
            Invariant(type="joint_limits_respected", description="joints"),
            Invariant(type="velocity_bounded", description="vel"),
        ],
    )
    plan = ActionPlan(source="t", task="move arm", steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(
            id="m",
            joint_targets={"j1": 0.5},
            duration_s=1.0,
            depends_on=["reset"],
        ),
    ])
    return plan, env


async def test_supervisor_threads_torque_limit_into_mujoco_executor():
    plan, env = _plan_and_env(torque_limit=0.1)  # trivially tight
    executors = robotics_executors(str(FIXTURE))
    supervisor = Supervisor(executors=executors, approval=APPROVE_ALL)
    session = await supervisor.run(plan, env)

    # Supervisor should have surfaced torque_limit to the shared executor.
    shared: MuJoCoExecutor = executors["joint_move"]  # type: ignore[assignment]
    assert shared.torque_limit == 0.1

    # JointMoveStep should have failed on torque violation.
    joint_outcome = next(o for o in session.steps if o.step_id == "m")
    assert joint_outcome.rc == RC_TORQUE_VIOLATION


async def test_supervisor_without_torque_limit_leaves_executor_default():
    plan, env = _plan_and_env(torque_limit=None)
    executors = robotics_executors(str(FIXTURE), torque_limit=None)
    supervisor = Supervisor(executors=executors, approval=APPROVE_ALL)
    session = await supervisor.run(plan, env)

    # No torque_limit in envelope, and constructor default is None.
    shared: MuJoCoExecutor = executors["joint_move"]  # type: ignore[assignment]
    assert shared.torque_limit is None

    # Succeeds without torque guard firing.
    assert session.status.name == "SUCCEEDED"


async def test_supervisor_obstacle_envelope_enables_contact_guard():
    # Envelope declares obstacles → executor.forbid_contacts flips True.
    perms = Permission(
        joint_limits={"j1": (-3.14, 3.14), "j2": (-3.14, 3.14)},
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[((10, 10, 10), (11, 11, 11))],  # far away — won't actually hit
    )
    env = Envelope(
        generated_by="t", task="t",
        permissions=perms,
        invariants=[Invariant(type="no_obstacle_penetration", description="no")],
    )
    plan = ActionPlan(source="t", task="t", steps=[SimulationResetStep(id="r")])
    executors = robotics_executors(str(FIXTURE))
    supervisor = Supervisor(executors=executors, approval=APPROVE_ALL)
    await supervisor.run(plan, env)

    shared: MuJoCoExecutor = executors["sim_reset"]  # type: ignore[assignment]
    assert shared.forbid_contacts is True
