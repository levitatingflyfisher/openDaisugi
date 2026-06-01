"""Vision-Language-Action (VLA) executor scaffolding (v0.26+).

A VLA — Physical Intelligence's π0/π0.5, an LeRobot policy, an
RT-2-style stack, or any visuomotor controller — fits into the
opendaisugi substrate as one ``VLAStep`` per skill. The verifier
treats the VLA as opaque: the *envelope* around the rollout is
checked, the per-action stream inside the rollout isn't.

This module ships:

- :class:`VLAExecutorBase`: the abstract scaffolding (MuJoCo loading,
  rollout loop, evidence packaging). Subclasses only have to
  implement ``_predict_actions``.
- :class:`MockVLAExecutor`: a deterministic stand-in for tests. Linearly
  interpolates from the current pose to ``step.target_pose`` over a
  fixed number of actions.

Real ``LeRobotPI0Executor`` / ``RT2Executor`` subclasses live in user
code (the integration recipe lives at ``docs/pi-vla-integration.md``).
We don't ship them in v0.26 because (a) the model weights are GPU-sized
and not a fit for the package, and (b) the model card / processor
signatures shift and pinning a version in core would rot.
"""
from __future__ import annotations

import json
import logging
import time
from typing import Any

from opendaisugi.executor import ExecutorResult
from opendaisugi.models import StepBase, VLAStep

_log = logging.getLogger("opendaisugi.vla_executor")


class VLAExecutorBase:
    """StepExecutor scaffolding for any VLA. Subclasses implement
    ``_predict_actions``; the base class handles MuJoCo orchestration.
    """

    def __init__(
        self,
        *,
        mjcf_path: str | None = None,
        max_actions_global: int = 200,
    ) -> None:
        self.mjcf_path = mjcf_path
        self.max_actions_global = max_actions_global
        self._mujoco = None
        self._model = None
        self._data = None
        self._joint_names: list[str] = []
        if mjcf_path:
            self._load_mujoco()

    def _load_mujoco(self) -> None:
        import mujoco
        self._mujoco = mujoco
        self._model = mujoco.MjModel.from_xml_path(self.mjcf_path)
        self._data = mujoco.MjData(self._model)
        # Discover joint names so MockVLAExecutor + observation packagers
        # know what to drive.
        self._joint_names = [
            mujoco.mj_id2name(self._model, mujoco.mjtObj.mjOBJ_JOINT, jid)
            for jid in range(self._model.njnt)
        ]
        self._joint_names = [n for n in self._joint_names if n]

    def _current_observation(self) -> dict[str, Any]:
        """Build the observation dict the policy will consume.

        Default: proprioceptive only — joint positions, velocities, and the
        end-effector body's Cartesian pose if the MJCF declares one. Real
        LeRobot/PI subclasses override to add image data.
        """
        if self._data is None:
            return {}
        m = self._mujoco
        obs: dict[str, Any] = {
            "qpos": [float(v) for v in self._data.qpos],
            "qvel": [float(v) for v in self._data.qvel],
        }
        try:
            ee_id = m.mj_name2id(self._model, m.mjtObj.mjOBJ_BODY, "end_effector")
            if ee_id >= 0:
                obs["end_effector_xyz"] = [float(v) for v in self._data.xpos[ee_id]]
        except Exception:
            pass
        return obs

    def _predict_actions(
        self, step: VLAStep, observation: dict[str, Any],
    ) -> list[dict[str, Any]]:
        """Return a list of action dicts to roll out. Each action is
        ``{joint_name: target}`` — the executor sets the actuator targets and
        steps the simulator forward.

        Subclasses MUST override. Base raises ``NotImplementedError``.
        """
        raise NotImplementedError(
            "VLAExecutorBase._predict_actions is abstract; subclass and "
            "implement (see docs/pi-vla-integration.md for examples)."
        )

    def _apply_action(self, action: dict[str, Any]) -> None:
        """Set actuator targets from one action dict and step the sim."""
        if self._data is None or self._model is None:
            return
        m = self._mujoco
        for joint_name, target in action.items():
            joint_id = m.mj_name2id(self._model, m.mjtObj.mjOBJ_JOINT, joint_name)
            if joint_id < 0:
                continue
            for aid in range(self._model.nu):
                if self._model.actuator_trnid[aid, 0] == joint_id:
                    self._data.ctrl[aid] = float(target)
                    break
        m.mj_step(self._model, self._data)

    def _final_state(self) -> dict[str, Any]:
        if self._data is None:
            return {}
        m = self._mujoco
        out: dict[str, Any] = {
            "qpos_final": [float(v) for v in self._data.qpos],
        }
        try:
            ee_id = m.mj_name2id(self._model, m.mjtObj.mjOBJ_BODY, "end_effector")
            if ee_id >= 0:
                out["end_effector_xyz_final"] = [float(v) for v in self._data.xpos[ee_id]]
        except Exception:
            pass
        out["contact_count"] = int(self._data.ncon)
        return out

    def configure_from_envelope(self, envelope) -> None:
        """Pull rollout-guard settings from the envelope, like
        ``MuJoCoExecutor`` does. Forward to subclasses that care."""
        # No-op default; subclasses can override to pick up
        # workspace_bounds / forbid_contacts / etc.

    def run(
        self,
        step: StepBase,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        if not isinstance(step, VLAStep):
            return ExecutorResult(
                rc=1, stdout=f"VLAExecutorBase: not a VLAStep ({type(step).__name__})",
                duration_ms=0.0, timed_out=False,
            )
        started = time.time()
        if self._data is None:
            return ExecutorResult(
                rc=1, stdout="VLAExecutorBase: no MJCF loaded; pass mjcf_path",
                duration_ms=0.0, timed_out=False,
            )
        observation = self._current_observation()
        try:
            actions = self._predict_actions(step, observation)
        except NotImplementedError:
            return ExecutorResult(
                rc=1, stdout="VLAExecutorBase._predict_actions not implemented",
                duration_ms=0.0, timed_out=False,
            )
        except Exception as exc:
            return ExecutorResult(
                rc=1, stdout=f"VLA inference error: {type(exc).__name__}: {exc}",
                duration_ms=0.0, timed_out=False,
            )
        # Cap by both the step's max_actions AND the executor's global cap
        # so a misbehaving policy can't run forever.
        cap = min(step.max_actions, self.max_actions_global)
        actions = actions[:cap]
        deadline = started + min(step.timeout_s, float(timeout_s))
        executed = 0
        for action in actions:
            if time.time() > deadline:
                break
            self._apply_action(action)
            executed += 1
        evidence: dict[str, Any] = {
            "task": step.task,
            "actions_requested": len(actions),
            "actions_executed": executed,
            "max_actions": step.max_actions,
            "observation_initial": observation,
            **self._final_state(),
        }
        timed_out = executed < len(actions)
        return ExecutorResult(
            rc=0,
            stdout=json.dumps(evidence),
            duration_ms=(time.time() - started) * 1000.0,
            timed_out=timed_out,
        )


class TransformersVLAExecutor(VLAExecutorBase):
    """Generic VLA executor for any HuggingFace transformers-compatible
    visuomotor policy. Designed for ``lerobot/smolvla_base`` and similar
    small VLA checkpoints, but model-id-pluggable.

    The model is **lazy-loaded** on the first ``_predict_actions`` call,
    not at construction. Instantiating this class does not allocate any
    weights, so a ``Daisugi(...)`` import path that never invokes a
    VLAStep stays cheap. This matters on memory-constrained hosts where
    a 450M-param policy load could push the system into swap.

    Default model id targets ``lerobot/smolvla_base`` — a deliberately
    tiny VLA designed to run on consumer CPUs. Real PI π0 (3.3B params)
    needs a GPU; swap the ``model_id`` once VRAM is provisioned.

    See ``docs/pi-vla-integration.md`` for the bigger picture, and the
    docstring of :meth:`_predict_actions` for what the prediction
    output is expected to look like.
    """

    def __init__(
        self,
        *,
        mjcf_path: str,
        model_id: str = "lerobot/smolvla_base",
        device: str = "cpu",
        action_horizon: int = 16,
        cache_dir: str | None = None,
    ) -> None:
        super().__init__(mjcf_path=mjcf_path)
        self.model_id = model_id
        self.device = device
        self.action_horizon = action_horizon
        self.cache_dir = cache_dir
        self._policy = None  # lazy
        self._processor = None  # lazy
        self._torch = None  # lazy
        self._joint_action_keys: list[str] = []

    def _ensure_loaded(self) -> None:
        """Materialize the model on first use. Errors here propagate to
        the executor's exception handler in ``VLAExecutorBase.run`` and
        surface as an rc=1 result — the supervisor's integrity check then
        treats the step as failed, not silently skipped."""
        if self._policy is not None:
            return
        import torch
        from transformers import AutoModel, AutoProcessor
        self._torch = torch
        _log.info(
            "TransformersVLAExecutor.load",
            extra={"model_id": self.model_id, "device": self.device},
        )
        self._processor = AutoProcessor.from_pretrained(
            self.model_id, cache_dir=self.cache_dir, trust_remote_code=True,
        )
        self._policy = AutoModel.from_pretrained(
            self.model_id, cache_dir=self.cache_dir, trust_remote_code=True,
        ).to(self.device)
        self._policy.eval()
        # Joint order convention: derive from the loaded MJCF unless the
        # subclass overrides. The 2-DOF test fixture uses j1, j2, j_grip.
        self._joint_action_keys = [
            n for n in self._joint_names if n
        ][: self.action_horizon]

    def _capture_image(self):
        """Render the current MuJoCo scene to an RGB array. Subclasses can
        override to use a configured camera; the default is a fixed
        viewpoint suitable for the test fixture."""
        if self._mujoco is None:
            return None
        try:
            renderer = self._mujoco.Renderer(self._model, height=240, width=320)
            renderer.update_scene(self._data)
            return renderer.render()
        except Exception:
            # Some test MJCFs lack any geom contype/conaffinity meaningful
            # for the renderer; fall back to None.
            return None

    def _predict_actions(self, step, observation):
        """Call the loaded policy and convert its output to action dicts.

        Output shape contract: the policy may return either
        - a tensor of shape ``(1, T, action_dim)`` — chunked actions
        - a tensor of shape ``(1, action_dim)`` — single action
        - a dict with key ``actions`` of either shape above

        We unpack to a list of ``{joint_name: target}`` dicts, capped at
        ``step.max_actions``. The joint order is taken from the loaded
        MJCF; if the model's action_dim doesn't match, we truncate to the
        smaller of the two so the executor doesn't crash on dim mismatch.
        Subclasses with stricter model contracts override this method.
        """
        self._ensure_loaded()
        image = self._capture_image()
        proprio = observation.get("qpos", [])
        # Build inputs in a model-agnostic shape; the processor decides
        # what to drop. Real-PI subclasses build the inputs the way
        # their model expects.
        torch = self._torch
        inputs = self._processor(
            images=image, text=step.task, return_tensors="pt",
        ) if image is not None else {}
        if proprio:
            inputs["state"] = torch.tensor(
                [proprio], dtype=torch.float32,
            ).to(self.device)
        inputs = {k: (v.to(self.device) if hasattr(v, "to") else v)
                  for k, v in inputs.items()}
        with torch.no_grad():
            output = self._policy(**inputs)
        # Unpack output. Modern VLAs return a dataclass or dict; handle both.
        if hasattr(output, "actions"):
            action_tensor = output.actions
        elif isinstance(output, dict) and "actions" in output:
            action_tensor = output["actions"]
        else:
            action_tensor = output
        # Ensure shape (T, action_dim)
        if action_tensor.ndim == 3:
            action_tensor = action_tensor[0]
        elif action_tensor.ndim == 1:
            action_tensor = action_tensor.unsqueeze(0)
        action_tensor = action_tensor.cpu().numpy()
        # Translate to {joint: target} dicts
        if not self._joint_action_keys:
            self._joint_action_keys = [
                n for n in self._joint_names if n
            ][: action_tensor.shape[1]]
        keys = self._joint_action_keys[: action_tensor.shape[1]]
        actions: list[dict[str, Any]] = []
        for t in range(min(action_tensor.shape[0], step.max_actions)):
            actions.append({
                k: float(action_tensor[t, i]) for i, k in enumerate(keys)
            })
        return actions


class MockVLAExecutor(VLAExecutorBase):
    """Deterministic linear-interpolation policy for tests and CI.

    Generates ``num_actions`` actions that linearly walk the joints from
    their current positions toward small targets derived from
    ``step.target_pose``. No vision, no model load, no GPU. Produces
    real MuJoCo state in the receipt evidence so the substrate is
    exercised end-to-end without leaning on a learned policy.
    """

    def __init__(
        self, *, mjcf_path: str, num_actions: int = 10,
    ) -> None:
        super().__init__(mjcf_path=mjcf_path)
        self.num_actions = num_actions

    def _predict_actions(self, step, observation):
        if not self._joint_names:
            return []
        # Map target_pose's first two components to j1/j2 (planar arm
        # convention); leave gripper at neutral. Real VLA subclasses
        # consume the full image+language input.
        if step.target_pose is not None:
            j1_target = float(step.target_pose[0])
            j2_target = float(step.target_pose[1])
        else:
            j1_target = 0.0
            j2_target = 0.0
        controllable = [n for n in self._joint_names if not n.startswith("j_grip")]
        if not controllable:
            return []
        actions: list[dict[str, Any]] = []
        # Start positions from the current observation.
        start = observation.get("qpos", [0.0] * len(self._joint_names))
        j1_now = float(start[0]) if len(start) > 0 else 0.0
        j2_now = float(start[1]) if len(start) > 1 else 0.0
        for i in range(1, self.num_actions + 1):
            t = i / self.num_actions
            action = {
                controllable[0]: j1_now + t * (j1_target - j1_now),
            }
            if len(controllable) >= 2:
                action[controllable[1]] = j2_now + t * (j2_target - j2_now)
            actions.append(action)
        return actions
