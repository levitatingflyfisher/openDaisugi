"""Kinematic invariant checks for robotics envelopes — workspace, velocity, joint limits."""

from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    Envelope,
    Invariant,
    JointMoveStep,
    Permission,
    ShellStep,
)
from opendaisugi.verify import verify


def _env(**perms_kw) -> Envelope:
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(**perms_kw),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="stay in workspace"),
            Invariant(type="velocity_bounded", description="stay under velocity limit"),
            Invariant(type="joint_limits_respected", description="stay within joint limits"),
        ],
    )


def _violations_by_invariant(result, invariant_type: str) -> list:
    return [v for v in result.violations if v.detail.get("invariant") == invariant_type]


def test_cartesian_target_in_workspace_passes():
    env = _env(workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0)))
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(0.5, 0.0, 0.3)),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert not _violations_by_invariant(result, "end_effector_in_workspace")


def test_cartesian_target_out_of_workspace_flagged():
    env = _env(workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0)))
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(1.5, 0.0, 0.3)),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert _violations_by_invariant(result, "end_effector_in_workspace")


def test_joint_target_within_limits_passes():
    env = _env(joint_limits={"j1": (-1.0, 1.0), "j2": (-2.0, 2.0)})
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 0.5, "j2": 1.5}),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert not _violations_by_invariant(result, "joint_limits_respected")


def test_joint_target_exceeds_limit_flagged():
    env = _env(joint_limits={"j1": (-1.0, 1.0)})
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 1.5}),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert _violations_by_invariant(result, "joint_limits_respected")


def test_joint_target_missing_from_limits_flagged():
    env = _env(joint_limits={"j1": (-1.0, 1.0)})
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 0.5, "j_unknown": 0.3}),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert _violations_by_invariant(result, "joint_limits_respected")


def test_velocity_within_bound_passes():
    env = _env(velocity_limit=2.0)
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 1.0}, duration_s=1.0),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert not _violations_by_invariant(result, "velocity_bounded")


def test_velocity_exceeds_bound_flagged():
    env = _env(velocity_limit=0.5)
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 2.0}, duration_s=1.0),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert _violations_by_invariant(result, "velocity_bounded")


def test_no_robot_steps_no_robot_violations():
    # Shell plan against a robot envelope — invariants should quietly not apply.
    env = _env(workspace_bounds=((0, -1, 0), (1, 1, 1)))
    env.permissions.shell = True
    env.permissions.shell_allowlist = ["echo"]
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="a", command="echo hi"),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert not _violations_by_invariant(result, "end_effector_in_workspace")


def test_invariant_not_declared_not_checked():
    # Envelope with velocity_limit but no velocity_bounded invariant — shouldn't flag.
    env = _env(velocity_limit=0.01)
    env.invariants = []  # no invariants declared
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="a", joint_targets={"j1": 2.0}, duration_s=1.0),
    ])
    result = verify(plan, env, z3_timeout_ms=500)
    assert not _violations_by_invariant(result, "velocity_bounded")


# --- Task 5: obstacle non-intersection ---------------------------------

def _obstacle_env(workspace_bounds, obstacles):
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(workspace_bounds=workspace_bounds, obstacles=obstacles),
        invariants=[Invariant(type="no_obstacle_penetration", description="no collision")],
    )
    return env


def test_obstacle_avoidance_passes_when_trajectory_clear():
    # Obstacle is offset from rest-pose origin (robot doesn't start inside it).
    env = _obstacle_env(
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[((0.4, 0.4, 0.4), (0.6, 0.6, 0.6))],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(0.2, 0.0, 0.0)),
        CartesianMoveStep(id="b", target_position=(0.2, 0.2, 0.2), depends_on=["a"]),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert not _violations_by_invariant(result, "no_obstacle_penetration")


def test_obstacle_penetration_flagged_when_midpoint_inside_box():
    env = _obstacle_env(
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[((-0.1, -0.1, -0.1), (0.1, 0.1, 0.1))],
    )
    # Segment from (-0.5, 0, 0) to (0.5, 0, 0) passes through origin.
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(-0.5, 0.0, 0.0)),
        CartesianMoveStep(id="b", target_position=(0.5, 0.0, 0.0), depends_on=["a"]),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert _violations_by_invariant(result, "no_obstacle_penetration")


def test_obstacle_endpoint_inside_box_flagged():
    env = _obstacle_env(
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[((-0.1, -0.1, -0.1), (0.1, 0.1, 0.1))],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(0.0, 0.0, 0.0)),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert _violations_by_invariant(result, "no_obstacle_penetration")


def test_multiple_obstacles_all_checked():
    env = _obstacle_env(
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[
            ((0.2, -0.1, -0.1), (0.4, 0.1, 0.1)),
            ((0.6, -0.1, -0.1), (0.8, 0.1, 0.1)),
        ],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(0.3, 0.0, 0.0)),
        CartesianMoveStep(id="b", target_position=(0.7, 0.0, 0.0), depends_on=["a"]),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    # Both endpoints are inside obstacles — expect ≥2 violations.
    flagged = _violations_by_invariant(result, "no_obstacle_penetration")
    assert len(flagged) >= 2


def test_no_obstacles_no_checks():
    env = _obstacle_env(
        workspace_bounds=((-1, -1, -1), (1, 1, 1)),
        obstacles=[],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(id="a", target_position=(0.0, 0.0, 0.0)),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert not _violations_by_invariant(result, "no_obstacle_penetration")


def test_interpolate_positions_is_evenly_spaced():
    from opendaisugi.z3_checks import _interpolate_positions
    pts = _interpolate_positions((0.0, 0.0, 0.0), (1.0, 0.0, 0.0), n=5)
    assert len(pts) == 5
    assert pts[0] == (0.0, 0.0, 0.0)
    assert pts[-1] == (1.0, 0.0, 0.0)
    diffs = [pts[i + 1][0] - pts[i][0] for i in range(4)]
    assert all(abs(d - 0.25) < 1e-9 for d in diffs)
