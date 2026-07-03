"""MuJoCo-backed step executor for robotics plans.

Lives in its own module so mujoco/numpy stay optional. Import via
``from opendaisugi.executor_mujoco import MuJoCoExecutor`` (or the lazy
``opendaisugi.MuJoCoExecutor`` attribute) — neither import touches mujoco
until ``MuJoCoExecutor(...)`` is actually constructed.
"""

from __future__ import annotations

import time
from dataclasses import dataclass
from typing import TYPE_CHECKING

from opendaisugi.executor import ExecutorResult, StepExecutor
from opendaisugi.models import (
    ActionStep,
    CartesianMoveStep,
    Envelope,
    GripperStep,
    JointMoveStep,
    SimulationResetStep,
)

if TYPE_CHECKING:
    import numpy as np


# Return codes for rollout-time violations. rc=0 is success; values above 0
# flag distinct constraint classes so callers (and journals) can distinguish
# "arm pushed too hard" from "arm touched something it shouldn't".
RC_OK = 0
RC_TORQUE_VIOLATION = 3
RC_CONTACT_VIOLATION = 4
RC_IK_FAILED = 5

_DEFAULT_SETTLE_STEPS = 2000
_DEFAULT_POSITION_TOL = 0.05
_DEFAULT_IK_MAX_ITER = 200
_DEFAULT_IK_TOL = 1e-3
_DEFAULT_IK_DAMPING = 0.1


class _IKError(Exception):
    """Internal signal that damped-LS IK didn't converge."""


@dataclass(frozen=True)
class _RolloutOutcome:
    stdout: str
    rc: int = RC_OK


class MuJoCoExecutor:
    """Executes robot steps against a MuJoCo physics simulation.

    The MJCF path is loaded once in __init__; ``run()`` dispatches on the
    Pydantic discriminator. State (``mj_data``) persists across steps so
    joint_move honors prior state — sim_reset is the way to start over.

    Rollout-time guards (``torque_limit``, ``forbid_contacts``) are checked
    after every physics step. The first violation aborts the rollout and
    returns a non-zero rc — the rest of the envelope's invariants ride on
    that signal.
    """

    def __init__(
        self,
        mjcf_path: str,
        *,
        settle_steps: int = _DEFAULT_SETTLE_STEPS,
        position_tol: float = _DEFAULT_POSITION_TOL,
        torque_limit: float | None = None,
        forbid_contacts: bool = False,
        ee_body: str = "end_effector",
        ik_max_iter: int = _DEFAULT_IK_MAX_ITER,
        ik_tol: float = _DEFAULT_IK_TOL,
        ik_damping: float = _DEFAULT_IK_DAMPING,
    ) -> None:
        import mujoco

        self._mujoco = mujoco
        self.mjcf_path = mjcf_path
        self.settle_steps = settle_steps
        self.position_tol = position_tol
        self.torque_limit = torque_limit
        self.forbid_contacts = forbid_contacts
        self.ee_body = ee_body
        self.ik_max_iter = ik_max_iter
        self.ik_tol = ik_tol
        self.ik_damping = ik_damping
        self.model = mujoco.MjModel.from_xml_path(mjcf_path)
        self.data = mujoco.MjData(self.model)

    def run(
        self,
        step: ActionStep,
        *,
        timeout_s: int,
        max_output_bytes: int,
    ) -> ExecutorResult:
        start = time.monotonic()
        if isinstance(step, SimulationResetStep):
            outcome = self._do_reset(step)
        elif isinstance(step, JointMoveStep):
            outcome = self._do_joint_move(step)
        elif isinstance(step, GripperStep):
            outcome = self._do_gripper(step)
        elif isinstance(step, CartesianMoveStep):
            outcome = self._do_cartesian_move(step)
        else:
            raise TypeError(
                f"MuJoCoExecutor cannot run step of type {type(step).__name__}"
            )
        duration_ms = (time.monotonic() - start) * 1000.0
        return ExecutorResult(
            rc=outcome.rc,
            stdout=outcome.stdout,
            duration_ms=duration_ms,
            timed_out=False,
        )

    def configure_from_envelope(self, envelope: Envelope) -> None:
        """Pull rollout-guard settings from envelope.permissions.

        The Supervisor calls this once per run so an envelope that declares
        ``torque_limit=50`` or a non-empty ``obstacles`` list tightens this
        executor's guards without the caller wiring permission→kwargs by
        hand. Settings the envelope doesn't specify are left untouched so
        explicit constructor kwargs still win.
        """
        perms = envelope.permissions
        if perms.torque_limit is not None:
            self.torque_limit = perms.torque_limit
        if perms.obstacles:
            self.forbid_contacts = True

    def _do_reset(self, step: SimulationResetStep) -> _RolloutOutcome:
        self._mujoco.mj_resetData(self.model, self.data)
        self._mujoco.mj_forward(self.model, self.data)
        return _RolloutOutcome(stdout=f"sim reset (seed={step.seed})")

    def _do_joint_move(self, step: JointMoveStep) -> _RolloutOutcome:
        for joint_name, target in step.joint_targets.items():
            actuator_id = self._actuator_id_for_joint(joint_name)
            self.data.ctrl[actuator_id] = target
        violation = self._step_with_guards(self.settle_steps, where=f"joint_move {step.id}")
        if violation is not None:
            return violation
        reached = self._joint_positions(step.joint_targets.keys())
        return _RolloutOutcome(stdout=f"joint_move complete; positions={reached}")

    def _do_gripper(self, step: GripperStep) -> _RolloutOutcome:
        """Drive gripper actuators — open targets max travel, close targets min.

        Fixture convention: any actuator whose name starts with ``a_grip``
        counts as gripper. ``open`` → ctrl = joint range upper, ``close`` →
        lower. Falls through quietly when a fixture has no such actuator (so
        a two_joint_arm without a gripper still parses the step).
        """
        mj = self._mujoco
        applied: list[str] = []
        for aid in range(self.model.nu):
            name = mj.mj_id2name(self.model, mj.mjtObj.mjOBJ_ACTUATOR, aid) or ""
            if not name.startswith("a_grip"):
                continue
            lo, hi = self._gripper_target_range(aid)
            target = hi if step.action == "open" else lo
            self.data.ctrl[aid] = target
            applied.append(f"{name}={target}")

        hold_steps = max(1, int(step.hold_s / self.model.opt.timestep))
        violation = self._step_with_guards(hold_steps, where=f"gripper {step.id}")
        if violation is not None:
            return violation
        return _RolloutOutcome(
            stdout=f"gripper {step.action}; applied=[{', '.join(applied)}]"
        )

    def _do_cartesian_move(self, step: CartesianMoveStep) -> _RolloutOutcome:
        """Solve IK, drive hinge actuators, settle, report final ee position.

        Only hinge joints are re-tasked — slide joints (the gripper) keep
        their current ctrl so a cartesian_move after a close doesn't snap
        the gripper open. Orientation is ignored in v0.8.0 (position-only
        IK is enough for the planar-arm smoke tests); orientation lands
        when a 6-DOF fixture arrives.
        """
        try:
            q_target = self._solve_ik_position(step.target_position)
        except _IKError as e:
            return _RolloutOutcome(
                rc=RC_IK_FAILED,
                stdout=f"cartesian_move {step.id} IK failed: {e}",
            )
        mj = self._mujoco
        for aid in range(self.model.nu):
            jid = int(self.model.actuator_trnid[aid, 0])
            if self.model.jnt_type[jid] == mj.mjtJoint.mjJNT_HINGE:
                qpos_adr = int(self.model.jnt_qposadr[jid])
                self.data.ctrl[aid] = float(q_target[qpos_adr])
        violation = self._step_with_guards(self.settle_steps, where=f"cartesian_move {step.id}")
        if violation is not None:
            return violation
        ee_id = mj.mj_name2id(self.model, mj.mjtObj.mjOBJ_BODY, self.ee_body)
        ee_pos = tuple(float(x) for x in self.data.xpos[ee_id])
        return _RolloutOutcome(
            stdout=f"cartesian_move reached {ee_pos} (target={step.target_position})"
        )

    def _solve_ik_position(
        self,
        target_pos: tuple[float, float, float],
    ) -> "np.ndarray":
        """Damped-LS iterative IK against the ``ee_body`` position.

        Uses a throwaway MjData so d.qpos isn't disturbed — the solver only
        runs forward kinematics, never physics. Position-only; orientation
        is left free.
        """
        import numpy as np

        mj = self._mujoco
        ee_id = mj.mj_name2id(self.model, mj.mjtObj.mjOBJ_BODY, self.ee_body)
        if ee_id < 0:
            raise _IKError(f"ee_body {self.ee_body!r} not found in MJCF")

        scratch = mj.MjData(self.model)
        scratch.qpos[:] = self.data.qpos
        target = np.asarray(target_pos, dtype=float)
        jacp = np.zeros((3, self.model.nv))
        damping2 = self.ik_damping ** 2

        for _ in range(self.ik_max_iter):
            mj.mj_forward(self.model, scratch)
            err = target - np.array(scratch.xpos[ee_id])
            if np.linalg.norm(err) < self.ik_tol:
                return np.array(scratch.qpos)
            mj.mj_jacBody(self.model, scratch, jacp, None, ee_id)
            JJt = jacp @ jacp.T + damping2 * np.eye(3)
            dq = jacp.T @ np.linalg.solve(JJt, err)
            # qpos layout mirrors qvel for hinge/slide joints (1-to-1); the
            # dq vector from a Jacobian is in qvel space, so this holds for
            # the joint types this executor supports (hinge, slide).
            scratch.qpos[: self.model.nv] += dq
        # The last iteration's dq was applied but never checked — the loop
        # tests the *pre-dq* residual. One final forward+check lets the last
        # update count, so a run that converges on iteration N doesn't need
        # a slack retry to be accepted.
        mj.mj_forward(self.model, scratch)
        residual = float(np.linalg.norm(target - scratch.xpos[ee_id]))
        if residual < self.ik_tol:
            return np.array(scratch.qpos)
        raise _IKError(
            f"did not converge in {self.ik_max_iter} iters "
            f"(residual={residual:.4f}, tol={self.ik_tol})"
        )

    def _step_with_guards(self, n_steps: int, *, where: str) -> _RolloutOutcome | None:
        """Step physics `n_steps` times, checking torque/contact guards after each.

        Returns a non-None outcome *only* when a guard fires — the caller
        treats None as success. Checking after every step keeps the violation
        message tied to the specific substep that caused it, rather than an
        aggregate "something exceeded at some point during the rollout."
        """
        for _ in range(n_steps):
            self._mujoco.mj_step(self.model, self.data)
            if self.torque_limit is not None:
                peak = max(abs(float(f)) for f in self.data.actuator_force) if self.model.nu else 0.0
                if peak > self.torque_limit:
                    return _RolloutOutcome(
                        rc=RC_TORQUE_VIOLATION,
                        stdout=(
                            f"torque_limit violated during {where}: "
                            f"peak |actuator_force|={peak:.3f} > limit={self.torque_limit}"
                        ),
                    )
            if self.forbid_contacts and self.data.ncon > 0:
                pairs = self._contact_pair_names(self.data.ncon)
                return _RolloutOutcome(
                    rc=RC_CONTACT_VIOLATION,
                    stdout=(
                        f"contact detected during {where} (ncon={self.data.ncon}); "
                        f"pairs={pairs}"
                    ),
                )
        return None

    def _contact_pair_names(self, ncon: int) -> list[tuple[str, str]]:
        mj = self._mujoco
        out: list[tuple[str, str]] = []
        for i in range(ncon):
            c = self.data.contact[i]
            g1 = mj.mj_id2name(self.model, mj.mjtObj.mjOBJ_GEOM, int(c.geom1)) or f"geom#{c.geom1}"
            g2 = mj.mj_id2name(self.model, mj.mjtObj.mjOBJ_GEOM, int(c.geom2)) or f"geom#{c.geom2}"
            out.append((g1, g2))
        return out

    def _gripper_target_range(self, actuator_id: int) -> tuple[float, float]:
        """The (lo, hi) open/close drive this gripper actuator to.

        Prefers joint range (physical limit), falls back to actuator
        ctrlrange (control limit), raises if neither is declared — a
        gripper with neither bound can't be sensibly opened or closed.
        """
        mj = self._mujoco
        joint_id = int(self.model.actuator_trnid[actuator_id, 0])
        if self.model.jnt_limited[joint_id]:
            lo, hi = self.model.jnt_range[joint_id]
            return float(lo), float(hi)
        if self.model.actuator_ctrllimited[actuator_id]:
            lo, hi = self.model.actuator_ctrlrange[actuator_id]
            return float(lo), float(hi)
        name = (
            mj.mj_id2name(self.model, mj.mjtObj.mjOBJ_ACTUATOR, actuator_id)
            or f"actuator#{actuator_id}"
        )
        raise ValueError(
            f"Gripper actuator {name!r} has neither joint range nor "
            f"ctrlrange — cannot infer open/close targets"
        )

    def _actuator_id_for_joint(self, joint_name: str) -> int:
        """Resolve actuator id by its controlled joint name.

        The convention in our fixture MJCFs is that each hinge joint has
        exactly one position actuator. We walk the actuator table and match
        by ``trnid`` (the joint id the actuator drives). A KeyError with the
        joint name is clearer than the opaque IndexError mujoco would raise
        for an off-by-one ctrl slot.
        """
        mj = self._mujoco
        joint_id = mj.mj_name2id(self.model, mj.mjtObj.mjOBJ_JOINT, joint_name)
        if joint_id < 0:
            raise KeyError(f"Joint {joint_name!r} not found in MJCF")
        for aid in range(self.model.nu):
            if self.model.actuator_trnid[aid, 0] == joint_id:
                return aid
        raise KeyError(f"No actuator drives joint {joint_name!r}")

    def _joint_positions(self, joint_names) -> dict[str, float]:
        mj = self._mujoco
        out: dict[str, float] = {}
        for name in joint_names:
            jid = mj.mj_name2id(self.model, mj.mjtObj.mjOBJ_JOINT, name)
            qpos_adr = self.model.jnt_qposadr[jid]
            out[name] = float(self.data.qpos[qpos_adr])
        return out


def robotics_executors(mjcf_path: str, **kwargs) -> dict[str, StepExecutor]:
    """Factory: one MuJoCoExecutor wired to every robot step kind.

    All four robot step types share a single executor instance so they
    share one MjData — joint_move after sim_reset picks up the reset
    state, cartesian_move after gripper keeps the finger closed. Extra
    kwargs (torque_limit, forbid_contacts, ee_body, ik_*) forward to
    the executor constructor.
    """
    ex = MuJoCoExecutor(mjcf_path, **kwargs)
    return {
        "sim_reset": ex,
        "joint_move": ex,
        "cartesian_move": ex,
        "gripper": ex,
    }
