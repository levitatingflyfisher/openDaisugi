"""End-to-end verify() on robot plans — all-invariants green path + integration."""

from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    Envelope,
    GripperStep,
    Invariant,
    JointMoveStep,
    Permission,
    SimulationResetStep,
)
from opendaisugi.verify import verify


def test_verify_robot_plan_passes_when_all_invariants_hold():
    env = Envelope(
        generated_by="t", task="pick",
        permissions=Permission(
            workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0)),
            obstacles=[((0.4, 0.4, 0.4), (0.6, 0.6, 0.6))],
            velocity_limit=2.0,
            joint_limits={"j1": (-2.9, 2.9)},
        ),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="arm in workspace"),
            Invariant(type="no_obstacle_penetration", description="no collision"),
            Invariant(type="velocity_bounded", description="under velocity limit"),
            Invariant(type="joint_limits_respected", description="within joint limits"),
        ],
    )
    plan = ActionPlan(source="t", task="pick", steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(id="home", joint_targets={"j1": 0.0}, duration_s=1.0, depends_on=["reset"]),
        CartesianMoveStep(id="a", target_position=(0.2, 0.0, 0.3), depends_on=["home"]),
        CartesianMoveStep(id="b", target_position=(0.2, 0.2, 0.3), depends_on=["a"]),
        GripperStep(id="close", action="close", depends_on=["b"]),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert result.ok, result.violations


def test_verify_robot_plan_fails_on_workspace_violation():
    env = Envelope(
        generated_by="t", task="pick",
        permissions=Permission(workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0))),
        invariants=[Invariant(type="end_effector_in_workspace", description="in workspace")],
    )
    plan = ActionPlan(source="t", task="pick", steps=[
        CartesianMoveStep(id="a", target_position=(2.0, 0.0, 0.5)),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert not result.ok
    assert any(v.detail.get("invariant") == "end_effector_in_workspace"
               for v in result.violations)


def test_verify_robot_plan_fails_on_multiple_invariants():
    env = Envelope(
        generated_by="t", task="pick",
        permissions=Permission(
            workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0)),
            velocity_limit=0.1,  # very tight
            joint_limits={"j1": (-1.0, 1.0)},
        ),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="in workspace"),
            Invariant(type="velocity_bounded", description="velocity"),
            Invariant(type="joint_limits_respected", description="joints"),
        ],
    )
    plan = ActionPlan(source="t", task="pick", steps=[
        CartesianMoveStep(id="a", target_position=(2.0, 0.0, 0.5)),  # workspace
        JointMoveStep(id="b", joint_targets={"j1": 2.0}, duration_s=0.1, depends_on=["a"]),
    ])
    result = verify(plan, env, z3_timeout_ms=2000)
    assert not result.ok
    flagged = {v.detail.get("invariant") for v in result.violations}
    assert "end_effector_in_workspace" in flagged
    # joint_limits + velocity should both flag on step b
    assert "joint_limits_respected" in flagged
    assert "velocity_bounded" in flagged


def test_robotics_invariant_without_backing_bounds_is_rejected():
    # end_effector_in_workspace enforced but workspace_bounds=None → the z3 handler
    # no-ops, so the invariant was silently vacuous (fail-open) even at physical
    # stakes. Must reject: the operator believes the workspace is guarded.
    from opendaisugi.models import ActionPlan, CartesianMoveStep, Envelope, Invariant, Permission
    from opendaisugi.verify import verify
    env = Envelope(
        generated_by="t", task="x", stakes="physical",
        permissions=Permission(workspace_bounds=None),
        invariants=[Invariant(type="end_effector_in_workspace", description="guarded")],
    )
    plan = ActionPlan(source="t", task="x",
                      steps=[CartesianMoveStep(id="m", target_position=(999.0, 999.0, 999.0))])
    r = verify(plan, env)
    assert not r.ok
    assert any("backing" in v.message.lower() or "workspace_bounds" in v.message for v in r.violations)


def test_robotics_invariant_with_bounds_still_works():
    from opendaisugi.models import ActionPlan, CartesianMoveStep, Envelope, Invariant, Permission
    from opendaisugi.verify import verify
    env = Envelope(
        generated_by="t", task="x", stakes="physical",
        permissions=Permission(workspace_bounds=((0.0, 0.0, 0.0), (1.0, 1.0, 1.0))),
        invariants=[Invariant(type="end_effector_in_workspace", description="guarded")],
    )
    inside = ActionPlan(source="t", task="x",
                        steps=[CartesianMoveStep(id="m", target_position=(0.5, 0.5, 0.5))])
    assert verify(inside, env).ok
    outside = ActionPlan(source="t", task="x",
                         steps=[CartesianMoveStep(id="m", target_position=(9.0, 9.0, 9.0))])
    assert not verify(outside, env).ok
