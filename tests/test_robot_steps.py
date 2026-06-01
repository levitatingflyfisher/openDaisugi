"""Robotics step types — round-trip + validation."""

import pytest
import yaml
from pydantic import ValidationError

from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    GripperStep,
    JointMoveStep,
    SimulationResetStep,
)


def _roundtrip(plan: ActionPlan) -> ActionPlan:
    payload = plan.model_dump(mode="json")
    yaml_text = yaml.safe_dump(payload, sort_keys=False)
    return ActionPlan(**yaml.safe_load(yaml_text))


def test_joint_move_roundtrip():
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="s1", joint_targets={"j1": 0.5, "j2": -0.2}, duration_s=2.0),
    ])
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], JointMoveStep)
    assert rt.steps[0].joint_targets == {"j1": 0.5, "j2": -0.2}
    assert rt.steps[0].duration_s == 2.0
    assert rt.steps[0].velocity_scale == 1.0


def test_cartesian_move_roundtrip():
    plan = ActionPlan(source="t", task="t", steps=[
        CartesianMoveStep(
            id="s1",
            target_position=(0.5, 0.0, 0.3),
            target_orientation=(1.0, 0.0, 0.0, 0.0),
        ),
    ])
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], CartesianMoveStep)
    assert rt.steps[0].target_position == (0.5, 0.0, 0.3)
    assert rt.steps[0].target_orientation == (1.0, 0.0, 0.0, 0.0)


def test_cartesian_move_orientation_optional():
    s = CartesianMoveStep(id="s", target_position=(0.1, 0.2, 0.3))
    assert s.target_orientation is None


def test_gripper_step_roundtrip():
    plan = ActionPlan(source="t", task="t", steps=[
        GripperStep(id="s1", action="close", hold_s=0.5),
    ])
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], GripperStep)
    assert rt.steps[0].action == "close"


def test_gripper_action_rejects_unknown():
    with pytest.raises(ValidationError):
        GripperStep(id="s", action="half")


def test_sim_reset_roundtrip():
    plan = ActionPlan(source="t", task="t", steps=[
        SimulationResetStep(id="s1", seed=123),
    ])
    rt = _roundtrip(plan)
    assert isinstance(rt.steps[0], SimulationResetStep)
    assert rt.steps[0].seed == 123


def test_mixed_robotics_plan_roundtrip():
    plan = ActionPlan(source="t", task="t", steps=[
        SimulationResetStep(id="r"),
        JointMoveStep(id="a", joint_targets={"j1": 0.1}, depends_on=["r"]),
        CartesianMoveStep(id="b", target_position=(0.4, 0.0, 0.2), depends_on=["a"]),
        GripperStep(id="c", action="close", depends_on=["b"]),
    ])
    rt = _roundtrip(plan)
    assert [type(s).__name__ for s in rt.steps] == [
        "SimulationResetStep", "JointMoveStep", "CartesianMoveStep", "GripperStep",
    ]


def test_joint_move_rejects_non_float_targets():
    with pytest.raises(ValidationError):
        JointMoveStep(id="s", joint_targets={"j1": "not-a-number"})


def test_cartesian_move_rejects_wrong_position_arity():
    with pytest.raises(ValidationError):
        CartesianMoveStep(id="s", target_position=(0.1, 0.2))


def test_velocity_scale_bounds():
    with pytest.raises(ValidationError):
        JointMoveStep(id="s", joint_targets={"j1": 0.1}, velocity_scale=1.5)
    with pytest.raises(ValidationError):
        JointMoveStep(id="s", joint_targets={"j1": 0.1}, velocity_scale=-0.1)
