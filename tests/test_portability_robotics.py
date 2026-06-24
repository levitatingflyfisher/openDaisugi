"""Portability: robot pathways (CartesianMove/JointMove/Gripper/SimReset) survive round-trip."""

import json

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
from opendaisugi.pathway import CompiledPathway
from opendaisugi.portability import export as export_pathway, parse_bundle


def _robot_pathway() -> CompiledPathway:
    env = Envelope(
        generated_by="test", task="pick and place",
        permissions=Permission(
            workspace_bounds=((0.0, -0.5, 0.0), (1.0, 0.5, 1.0)),
            obstacles=[((0.4, 0.4, 0.4), (0.6, 0.6, 0.6))],
            velocity_limit=2.0,
            joint_limits={"j1": (-3.0, 3.0), "j2": (-3.0, 3.0)},
            torque_limit=50.0,
        ),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="in workspace"),
            Invariant(type="velocity_bounded", description="under limit"),
            Invariant(type="joint_limits_respected", description="within limits"),
        ],
    )
    plan = ActionPlan(source="test", task="pick and place", steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(id="home", joint_targets={"j1": 0.0, "j2": 0.0},
                      duration_s=1.0, depends_on=["reset"]),
        CartesianMoveStep(id="approach", target_position=(0.3, 0.2, 0.0),
                          depends_on=["home"]),
        GripperStep(id="grasp", action="close", hold_s=0.3, depends_on=["approach"]),
        CartesianMoveStep(id="lift", target_position=(0.3, 0.3, 0.0),
                          depends_on=["grasp"]),
        GripperStep(id="release", action="open", hold_s=0.3, depends_on=["lift"]),
    ])
    return CompiledPathway(
        id="path_robot_test",
        task_description="planar pick-and-place",
        task_embedding=[0.1, 0.2, 0.3],
        embedding_model="test-model",
        plan_template=plan,
        envelope=env,
        source_trace_ids=["trace_1", "trace_2"],
        distilled_at=1_700_000_000.0,
    )


def test_robot_pathway_json_roundtrip_preserves_step_types():
    path = _robot_pathway()
    text = export_pathway(path, "json")
    # Sanity: bundle is valid JSON with a pathway key.
    parsed_bundle = json.loads(text)
    assert "pathway" in parsed_bundle

    restored = parse_bundle(text)
    step_types = [s.type for s in restored.plan_template.steps]
    assert step_types == ["sim_reset", "joint_move", "cartesian_move",
                          "gripper", "cartesian_move", "gripper"]


def test_robot_pathway_preserves_permission_robot_fields():
    path = _robot_pathway()
    text = export_pathway(path, "json")
    restored = parse_bundle(text)
    perms = restored.envelope.permissions
    assert perms.workspace_bounds == ((0.0, -0.5, 0.0), (1.0, 0.5, 1.0))
    assert perms.obstacles == [((0.4, 0.4, 0.4), (0.6, 0.6, 0.6))]
    assert perms.velocity_limit == 2.0
    assert perms.joint_limits == {"j1": (-3.0, 3.0), "j2": (-3.0, 3.0)}
    assert perms.torque_limit == 50.0


def test_robot_pathway_preserves_step_payload():
    path = _robot_pathway()
    text = export_pathway(path, "json")
    restored = parse_bundle(text)
    steps_by_id = {s.id: s for s in restored.plan_template.steps}
    assert steps_by_id["home"].joint_targets == {"j1": 0.0, "j2": 0.0}
    assert steps_by_id["approach"].target_position == (0.3, 0.2, 0.0)
    assert steps_by_id["grasp"].action == "close"
    assert steps_by_id["grasp"].hold_s == 0.3
    assert steps_by_id["release"].action == "open"


def test_robot_pathway_skill_format_roundtrip():
    path = _robot_pathway()
    text = export_pathway(path, "skill")
    assert text.startswith("---\n")
    restored = parse_bundle(text)
    step_types = [s.type for s in restored.plan_template.steps]
    assert "cartesian_move" in step_types
    assert "gripper" in step_types
