"""Permission extensions for robot envelopes."""

import pytest
from pydantic import ValidationError

from opendaisugi.models import Envelope, Permission


def test_permission_defaults_preserve_non_robot_envelope():
    p = Permission()
    assert p.workspace_bounds is None
    assert p.obstacles == []
    assert p.velocity_limit is None
    assert p.joint_limits == {}
    assert p.torque_limit is None


def test_permission_accepts_workspace_bounds():
    p = Permission(workspace_bounds=((0.2, -0.4, 0.0), (0.8, 0.4, 0.7)))
    assert p.workspace_bounds[0] == (0.2, -0.4, 0.0)


def test_permission_accepts_obstacle_list():
    p = Permission(obstacles=[
        ((0.0, -0.5, 0.0), (0.2, 0.5, 0.7)),
        ((0.5, 0.2, 0.0), (0.6, 0.3, 0.1)),
    ])
    assert len(p.obstacles) == 2


def test_permission_rejects_malformed_workspace():
    with pytest.raises(ValidationError):
        Permission(workspace_bounds=((0.2, -0.4), (0.8, 0.4, 0.7)))


def test_permission_velocity_and_torque():
    p = Permission(velocity_limit=2.0, torque_limit=87.0)
    assert p.velocity_limit == 2.0
    assert p.torque_limit == 87.0


def test_permission_joint_limits_map():
    p = Permission(joint_limits={"j1": (-2.9, 2.9), "j2": (-1.8, 1.8)})
    assert p.joint_limits["j1"] == (-2.9, 2.9)


def test_envelope_roundtrip_with_robot_permissions():
    env = Envelope(
        generated_by="test", task="pick",
        permissions=Permission(
            workspace_bounds=((0.2, -0.4, 0.0), (0.8, 0.4, 0.6)),
            obstacles=[((0.0, -0.5, 0.0), (0.2, 0.5, 0.7))],
            velocity_limit=2.0,
            joint_limits={"j1": (-2.9, 2.9)},
            torque_limit=87.0,
        ),
    )
    payload = env.model_dump(mode="json")
    rt = Envelope(**payload)
    assert rt.permissions.workspace_bounds[1] == (0.8, 0.4, 0.6)
    assert rt.permissions.joint_limits["j1"] == (-2.9, 2.9)
    assert rt.permissions.torque_limit == 87.0
