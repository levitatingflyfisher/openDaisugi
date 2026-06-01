"""VLA executor scaffolding tests (v0.26+)."""
from __future__ import annotations

import json
from pathlib import Path

import pytest

pytest.importorskip("mujoco")

from opendaisugi.models import (
    ActionPlan, Envelope, Invariant, Permission, VLAStep,
)


def _workspace_invariant():
    return Invariant(
        type="end_effector_in_workspace", description="bounded reach",
        enforce=True, expr=None,
    )
from opendaisugi.vla_executor import MockVLAExecutor, VLAExecutorBase
from opendaisugi.verify import verify

_MJCF = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


def test_vla_step_registers_and_validates():
    """VLAStep must be in the discriminated union and round-trip."""
    s = VLAStep(id="s1", task="approach mug",
                target_pose=(0.5, 0.3, 0.0), max_actions=20)
    s2 = VLAStep.model_validate_json(s.model_dump_json())
    assert s2 == s
    # Round-trip via ActionPlan dispatch
    plan = ActionPlan(source="t", task="t", steps=[s])
    plan2 = ActionPlan.model_validate_json(plan.model_dump_json())
    assert isinstance(plan2.steps[0], VLAStep)
    assert plan2.steps[0].task == "approach mug"


def test_mock_executor_runs_actions_against_mujoco():
    """MockVLAExecutor produces real MuJoCo state in the receipt evidence."""
    exe = MockVLAExecutor(mjcf_path=str(_MJCF), num_actions=8)
    step = VLAStep(id="s1", task="reach", target_pose=(0.5, 0.4, 0.0))
    result = exe.run(step, timeout_s=10, max_output_bytes=1024 * 1024)
    assert result.rc == 0
    evidence = json.loads(result.stdout)
    assert evidence["task"] == "reach"
    assert evidence["actions_executed"] == 8
    assert "qpos_final" in evidence
    assert "end_effector_xyz_final" in evidence
    # The arm should have moved off the origin by step 8
    assert any(abs(v) > 0.01 for v in evidence["qpos_final"][:2])


def test_base_class_refuses_to_run_without_subclassing():
    """Calling the base class directly hits NotImplementedError, surfaces
    as rc=1 with a clear message, doesn't crash the supervisor."""
    base = VLAExecutorBase(mjcf_path=str(_MJCF))
    step = VLAStep(id="s1", task="anything", target_pose=(0.0, 0.0, 0.0))
    result = base.run(step, timeout_s=10, max_output_bytes=1024)
    assert result.rc == 1
    assert "_predict_actions not implemented" in result.stdout


def test_executor_refuses_non_vla_steps():
    """Wrong step type → clean rc=1, no crash."""
    from opendaisugi.models import ShellStep
    exe = MockVLAExecutor(mjcf_path=str(_MJCF))
    result = exe.run(
        ShellStep(id="x", command="echo"),
        timeout_s=1, max_output_bytes=1024,
    )
    assert result.rc == 1
    assert "not a VLAStep" in result.stdout


def test_envelope_workspace_bounds_reject_out_of_bounds_target():
    """v0.26: workspace_bounds invariants apply to VLAStep.target_pose
    the same way they apply to CartesianMoveStep.target_position.
    A VLA can't be asked to drive into a forbidden region."""
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(
            workspace_bounds=((-0.4, -0.4, -0.1), (0.4, 0.4, 0.1)),
        ),
        stakes="physical",
        invariants=[_workspace_invariant()],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        VLAStep(id="s1", task="reach", target_pose=(2.0, 0.0, 0.0)),  # outside
    ])
    vr = verify(plan, env)
    assert vr.ok is False
    assert any("workspace bounds" in v.message for v in vr.violations)


def test_envelope_workspace_bounds_accept_in_bounds_target():
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(
            workspace_bounds=((-0.4, -0.4, -0.1), (0.4, 0.4, 0.1)),
        ),
        stakes="physical",
        invariants=[_workspace_invariant()],
    )
    plan = ActionPlan(source="t", task="t", steps=[
        VLAStep(id="s1", task="reach", target_pose=(0.2, 0.1, 0.0)),
    ])
    vr = verify(plan, env)
    # No workspace violation; other checks may fire on stakes='physical'
    # but bounds-check should not.
    assert not any("workspace bounds" in v.message for v in vr.violations)


def test_physical_stakes_refuses_vla_with_preferred_model():
    """A VLAStep that ALSO carries preferred_model='haiku' under physical
    stakes is rejected: the VLA itself is fine, but agent-authored LLM
    delegation is not. v0.19 _check_delegation_safety still fires."""
    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(),
        stakes="physical",
    )
    plan = ActionPlan(source="t", task="t", steps=[
        VLAStep(id="s1", task="reach", target_pose=(0.2, 0.1, 0.0),
                preferred_model="haiku"),
    ])
    vr = verify(plan, env)
    assert vr.ok is False
    assert any("delegation" in v.message.lower() for v in vr.violations)


def test_transformers_executor_lazy_loads():
    """v0.27: TransformersVLAExecutor must not touch torch/transformers at
    construction time. Loading a 450M-param model in __init__ would OOM
    memory-constrained hosts on a `Daisugi()` import path that never
    actually invokes a VLAStep."""
    from opendaisugi.vla_executor import TransformersVLAExecutor
    exe = TransformersVLAExecutor(
        mjcf_path=str(_MJCF),
        model_id="lerobot/smolvla_base",
    )
    # The model id is stored, but nothing else is allocated.
    assert exe._policy is None
    assert exe._processor is None
    assert exe._torch is None


def test_transformers_executor_with_mocked_predict():
    """The full executor pipeline, with the model lazy-loaded as mocks.
    Validates the unpack-action-tensor logic without touching the real
    model. Real model loading is opt-in via OPENDAISUGI_SMOLVLA_SMOKE=1
    in a separate session — this test keeps CI cheap."""
    import sys
    import types
    from unittest.mock import MagicMock, patch
    import numpy as np

    from opendaisugi.vla_executor import TransformersVLAExecutor

    exe = TransformersVLAExecutor(mjcf_path=str(_MJCF), model_id="fake/model")

    # Fake torch with the surface our executor uses.
    fake_torch = types.SimpleNamespace(
        no_grad=lambda: _NullCtx(),
        tensor=lambda data, dtype=None: _FakeTensor(np.array(data)),
        float32=None,
    )

    # Fake transformers imports — we shim them at module-resolve time.
    fake_proc = MagicMock()
    fake_proc.return_value = {"pixel_values": _FakeTensor(np.zeros((1, 3, 240, 320)))}
    fake_model = MagicMock()
    # Pretend the policy returns a (1, 5, 2) action chunk.
    fake_output = types.SimpleNamespace(
        actions=_FakeTensor(np.array([[
            [0.1, 0.05], [0.2, 0.10], [0.3, 0.15], [0.4, 0.20], [0.5, 0.25],
        ]])),
    )
    fake_model.return_value = fake_output
    fake_model.to.return_value = fake_model
    fake_model.eval.return_value = fake_model

    fake_transformers = types.ModuleType("transformers")
    fake_transformers.AutoProcessor = MagicMock()
    fake_transformers.AutoProcessor.from_pretrained = MagicMock(return_value=fake_proc)
    fake_transformers.AutoModel = MagicMock()
    fake_transformers.AutoModel.from_pretrained = MagicMock(return_value=fake_model)

    with patch.dict(sys.modules, {"torch": fake_torch, "transformers": fake_transformers}):
        step = VLAStep(id="s1", task="reach", target_pose=(0.2, 0.1, 0.0),
                       max_actions=4)
        result = exe.run(step, timeout_s=10, max_output_bytes=1024 * 1024)

    # rc=0 because mock pipeline produced 5 actions; executor capped to 4
    # then rolled them all through MuJoCo successfully.
    assert result.rc == 0
    evidence = json.loads(result.stdout)
    # Executor capped to 4; smolvla returned 5; min(5, 4) = 4
    assert evidence["actions_requested"] == 4
    assert evidence["actions_executed"] == 4


class _NullCtx:
    def __enter__(self): return self
    def __exit__(self, *args): return False


class _FakeTensor:
    """Minimal numpy-backed tensor stand-in for the executor's torch usage."""
    def __init__(self, arr):
        import numpy as _np
        self._arr = _np.asarray(arr)
        self.ndim = self._arr.ndim
        self.shape = self._arr.shape
    def __getitem__(self, i): return _FakeTensor(self._arr[i])
    def cpu(self): return self
    def numpy(self): return self._arr
    def to(self, device): return self
    def unsqueeze(self, axis):
        import numpy as _np
        return _FakeTensor(_np.expand_dims(self._arr, axis))


@pytest.mark.skipif(
    "OPENDAISUGI_SMOLVLA_SMOKE" not in __import__("os").environ,
    reason="real-model smoke test; set OPENDAISUGI_SMOLVLA_SMOKE=1 to enable. "
           "Downloads ~2GB of weights and loads them into memory.",
)
def test_smolvla_real_load_smoke():
    """Opt-in smoke test against the real lerobot/smolvla_base model.

    Deliberately gated behind OPENDAISUGI_SMOLVLA_SMOKE=1 because the
    download is ~2GB and the load takes hundreds of MB of RAM. On
    memory-constrained boxes this WILL push the system into swap and
    could OOM. Run only when you've confirmed you have headroom.
    """
    from opendaisugi.vla_executor import TransformersVLAExecutor
    exe = TransformersVLAExecutor(
        mjcf_path=str(_MJCF),
        model_id="lerobot/smolvla_base",
        device="cpu",
        action_horizon=8,
    )
    step = VLAStep(id="s1", task="reach toward the cup",
                   target_pose=(0.2, 0.1, 0.0), max_actions=4)
    result = exe.run(step, timeout_s=60, max_output_bytes=4 * 1024 * 1024)
    assert result.rc == 0, f"smolvla load+predict failed: {result.stdout[:300]}"
    evidence = json.loads(result.stdout)
    assert evidence["actions_executed"] > 0
    assert "end_effector_xyz_final" in evidence


def test_max_actions_caps_runaway_policy():
    """If a (real) VLA returned 1000 actions, the executor caps by
    step.max_actions (20 here) so a misbehaving policy can't run forever."""
    class FloodingExecutor(VLAExecutorBase):
        def _predict_actions(self, step, observation):
            # Pretend the policy emitted way too many actions.
            return [{"j1": 0.0, "j2": 0.0}] * 1000

    exe = FloodingExecutor(mjcf_path=str(_MJCF), max_actions_global=200)
    step = VLAStep(id="s1", task="anything", target_pose=(0.0, 0.0, 0.0),
                   max_actions=20)
    result = exe.run(step, timeout_s=10, max_output_bytes=1024 * 1024)
    evidence = json.loads(result.stdout)
    # Capped at min(step.max_actions, max_actions_global) = 20
    assert evidence["actions_executed"] == 20
    assert evidence["actions_requested"] == 20
