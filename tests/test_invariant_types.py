"""v0.27.0 — recognized opaque invariant types are the single source of truth."""
from __future__ import annotations

from opendaisugi._invariant_types import RECOGNIZED_OPAQUE_TYPES
from opendaisugi.models import ActionPlan, Envelope, Invariant, JointMoveStep, Permission
from opendaisugi.verify import verify


def test_recognized_set_matches_z3_robotics_handlers():
    assert RECOGNIZED_OPAQUE_TYPES == frozenset({
        "end_effector_in_workspace", "joint_limits_respected",
        "velocity_bounded", "no_obstacle_penetration",
    })


def test_recognized_robotics_invariant_not_flagged_at_physical_stakes():
    plan = ActionPlan(source="t", task="t",
                      steps=[JointMoveStep(id="s1", joint_targets={"j1": 0.1})])
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(),
                   stakes="physical",
                   invariants=[Invariant(type="joint_limits_respected",
                                         description="stay in range", expr=None)])
    result = verify(plan, env)
    # Must NOT be flagged by the strict-reject path (it's discharged elsewhere).
    assert not any(v.detail.get("reason") == "opaque_unrecognized"
                   for v in result.violations)
