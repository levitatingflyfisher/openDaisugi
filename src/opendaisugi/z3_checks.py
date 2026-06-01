"""Z3 constraint satisfiability checks for envelopes and plan-vs-envelope.

Z3's primary v0.0.1 role is *envelope self-consistency* — catching cases where
the LLM generates an envelope that is internally contradictory (e.g., shell=False
but shell_allowlist is non-empty). The secondary role is plan-requirement
consistency (added in a later task).

All checks have a configurable timeout (default 500ms). Z3 `unknown` results
are surfaced as `VerificationTimeout` exceptions — the caller decides whether
to treat them as warnings (default) or failures.
"""

from __future__ import annotations

import z3

from opendaisugi._invariant_types import RECOGNIZED_OPAQUE_TYPES
from opendaisugi.exceptions import VerificationTimeout
from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    Envelope,
    JointMoveStep,
    Violation,
)


def check_envelope_self_consistency(
    envelope: Envelope,
    timeout_ms: int = 500,
) -> list[Violation]:
    """Check that an envelope is not internally contradictory."""
    solver = z3.Solver()
    solver.set("timeout", timeout_ms)

    # Declare Z3 variables for the flags we care about.
    shell = z3.Bool("shell")
    can_write = z3.Bool("can_write")

    solver.add(shell == envelope.permissions.shell)
    solver.add(can_write == (len(envelope.permissions.file_write) > 0))

    # Note: `== True` is intentional — these are Z3 BoolRef values, not
    # Python bools. `if shell:` would short-circuit in Python and never
    # reach the solver. `shell == True` builds a Z3 equality term.

    # Constraint 1: non-empty shell_allowlist requires shell=True.
    if envelope.permissions.shell_allowlist:
        solver.add(shell == True)  # noqa: E712

    # Constraint 2: any file_exists postcondition requires file_write permission.
    for pc in envelope.postconditions:
        if pc.type == "file_exists":
            solver.add(can_write == True)  # noqa: E712

    # Constraint 3: bounds sanity on execution time.
    max_time = z3.Int("max_time")
    solver.add(max_time == envelope.permissions.max_execution_time_s)
    solver.add(max_time > 0)
    solver.add(max_time <= 3600)  # 1 hour hard ceiling

    result = solver.check()
    if result == z3.unknown:
        raise VerificationTimeout(
            f"Z3 self-consistency check exceeded {timeout_ms}ms"
        )
    if result == z3.unsat:
        return [
            Violation(
                stage="z3",
                message="Envelope is internally inconsistent",
                detail={"unsat_core": str(solver.unsat_core())},
            )
        ]
    return []


def check_plan_against_envelope(
    plan: ActionPlan,
    envelope: Envelope,
    timeout_ms: int = 500,
) -> list[Violation]:
    """Check that a plan's declared needs are reachable given envelope permissions.

    Most plan-vs-envelope checking happens in the Permission stage (set-based).
    This Z3 check catches logical implications the set-based check doesn't,
    by encoding plan requirements as Z3 constraints and asking for SAT.
    """
    solver = z3.Solver()
    solver.set("timeout", timeout_ms)

    shell_available = z3.Bool("shell_available")
    write_available = z3.Bool("write_available")
    solver.add(shell_available == envelope.permissions.shell)
    solver.add(write_available == (len(envelope.permissions.file_write) > 0))

    if any(step.type == "shell" for step in plan.steps):
        solver.add(shell_available == True)  # noqa: E712
    if any(step.type == "file_write" for step in plan.steps):
        solver.add(write_available == True)  # noqa: E712

    result = solver.check()
    if result == z3.unknown:
        raise VerificationTimeout(
            f"Z3 plan-vs-envelope check exceeded {timeout_ms}ms"
        )
    if result == z3.unsat:
        return [
            Violation(
                stage="z3",
                message="Plan requirements contradict envelope permissions",
                detail={"unsat_core": str(solver.unsat_core())},
            )
        ]
    return []


def _check_workspace_containment(plan: ActionPlan, envelope: Envelope) -> list[Violation]:
    """Every CartesianMoveStep.target_position is inside workspace_bounds.

    Linear interpolation between consecutive endpoints can't leave an AABB
    if both endpoints are in-bounds (convex combination stays in the box),
    so endpoint-only checks are sufficient here. Obstacle checks (non-convex
    complement) are handled separately in _check_obstacle_avoidance.
    """
    bounds = envelope.permissions.workspace_bounds
    if bounds is None:
        return []
    (xmin, ymin, zmin), (xmax, ymax, zmax) = bounds
    violations: list[Violation] = []
    for step in plan.steps:
        # v0.26: VLAStep.target_pose is checked the same way as
        # CartesianMoveStep.target_position. The verifier doesn't see actions
        # inside a VLA rollout, but it does constrain the rollout's *target*
        # so a learned policy can't be asked to drive into a forbidden region.
        target = None
        if isinstance(step, CartesianMoveStep):
            target = step.target_position
        else:
            from opendaisugi.models import VLAStep as _VLAStep
            if isinstance(step, _VLAStep) and step.target_pose is not None:
                target = step.target_pose
        if target is None:
            continue
        x, y, z = target
        if not (xmin <= x <= xmax and ymin <= y <= ymax and zmin <= z <= zmax):
            violations.append(Violation(
                stage="z3",
                message=(
                    f"Step '{step.id}' target {target} "
                    f"outside workspace bounds {bounds}"
                ),
                detail={
                    "invariant": "end_effector_in_workspace",
                    "step": step.id,
                    "target": list(target),
                    "bounds": [list(bounds[0]), list(bounds[1])],
                },
            ))
    return violations


def _check_joint_limits(plan: ActionPlan, envelope: Envelope) -> list[Violation]:
    limits = envelope.permissions.joint_limits
    if not limits:
        return []
    violations: list[Violation] = []
    for step in plan.steps:
        if not isinstance(step, JointMoveStep):
            continue
        for joint, target in step.joint_targets.items():
            if joint not in limits:
                violations.append(Violation(
                    stage="z3",
                    message=(
                        f"Step '{step.id}' joint {joint!r} not declared in "
                        f"envelope joint_limits {list(limits)}"
                    ),
                    detail={
                        "invariant": "joint_limits_respected",
                        "step": step.id,
                        "joint": joint,
                    },
                ))
                continue
            lo, hi = limits[joint]
            if not (lo <= target <= hi):
                violations.append(Violation(
                    stage="z3",
                    message=(
                        f"Step '{step.id}' joint {joint!r} target {target} "
                        f"outside [{lo}, {hi}]"
                    ),
                    detail={
                        "invariant": "joint_limits_respected",
                        "step": step.id,
                        "joint": joint,
                        "target": target,
                        "range": [lo, hi],
                    },
                ))
    return violations


def _check_velocity_bounds(
    plan: ActionPlan,
    envelope: Envelope,
    prev_joint_state: dict[str, float] | None = None,
) -> list[Violation]:
    """Approximation: |Δjoint| / duration_s * velocity_scale ≤ velocity_limit.

    Uses rest-pose (all zeros) as the prior if no prev_joint_state is provided.
    Sequential JointMoveSteps threading the same joints carry state forward.
    """
    limit = envelope.permissions.velocity_limit
    if limit is None:
        return []
    state = dict(prev_joint_state or {})
    violations: list[Violation] = []
    for step in plan.steps:
        if not isinstance(step, JointMoveStep):
            continue
        for joint, target in step.joint_targets.items():
            prev = state.get(joint, 0.0)
            duration = max(step.duration_s, 1e-6)
            peak = abs(target - prev) / duration * step.velocity_scale
            if peak > limit:
                violations.append(Violation(
                    stage="z3",
                    message=(
                        f"Step '{step.id}' joint {joint!r} peak velocity "
                        f"{peak:.3f} rad/s > limit {limit}"
                    ),
                    detail={
                        "invariant": "velocity_bounded",
                        "step": step.id,
                        "joint": joint,
                        "peak_rad_s": peak,
                        "limit_rad_s": limit,
                    },
                ))
            state[joint] = target
    return violations


OBSTACLE_MIDPOINT_SAMPLES = 8


def _interpolate_positions(
    p0: tuple[float, float, float],
    p1: tuple[float, float, float],
    n: int,
) -> list[tuple[float, float, float]]:
    """n evenly-spaced points from p0 to p1 inclusive. n >= 2."""
    if n < 2:
        raise ValueError("n must be >= 2")
    return [
        (
            p0[0] + (p1[0] - p0[0]) * t,
            p0[1] + (p1[1] - p0[1]) * t,
            p0[2] + (p1[2] - p0[2]) * t,
        )
        for t in (i / (n - 1) for i in range(n))
    ]


def _check_obstacle_avoidance(plan: ActionPlan, envelope: Envelope) -> list[Violation]:
    """Sample points along the Cartesian trajectory; flag any point inside any
    declared obstacle AABB.

    Rest-pose (origin) is the prior for the first Cartesian waypoint. For
    symbolic (parameterized) trajectories a Z3 encoding would earn its keep;
    for concrete numeric targets, pure-Python membership is 6 comparisons
    per (point, obstacle) pair — no solver needed.
    """
    obstacles = envelope.permissions.obstacles
    if not obstacles:
        return []
    cartesian_steps = [s for s in plan.steps if isinstance(s, CartesianMoveStep)]
    if not cartesian_steps:
        return []

    prev = (0.0, 0.0, 0.0)
    sample_points: list[tuple[str, tuple[float, float, float]]] = []
    for step in cartesian_steps:
        for pt in _interpolate_positions(prev, step.target_position, OBSTACLE_MIDPOINT_SAMPLES):
            sample_points.append((step.id, pt))
        prev = step.target_position

    violations: list[Violation] = []
    flagged_steps: set[tuple[str, int]] = set()
    for step_id, (x, y, z) in sample_points:
        for idx, ((xmin, ymin, zmin), (xmax, ymax, zmax)) in enumerate(obstacles):
            if (step_id, idx) in flagged_steps:
                continue
            if xmin <= x <= xmax and ymin <= y <= ymax and zmin <= z <= zmax:
                violations.append(Violation(
                    stage="z3",
                    message=(
                        f"Step '{step_id}' trajectory sample "
                        f"({x:.3f}, {y:.3f}, {z:.3f}) inside obstacle #{idx}"
                    ),
                    detail={
                        "invariant": "no_obstacle_penetration",
                        "step": step_id,
                        "obstacle_index": idx,
                        "sample_point": [x, y, z],
                    },
                ))
                flagged_steps.add((step_id, idx))
    return violations


# Single source of truth: the recognized opaque (expr-less) invariant types are
# exactly those with a dedicated handler here. verify.py / subsumption.py consult
# RECOGNIZED_OPAQUE_TYPES for their strict-mode carve-out; the assertion below
# fails at import if the two ever drift apart.
_INVARIANT_HANDLERS = {
    "end_effector_in_workspace": _check_workspace_containment,
    "joint_limits_respected": _check_joint_limits,
    "velocity_bounded": _check_velocity_bounds,
    "no_obstacle_penetration": _check_obstacle_avoidance,
}
assert set(_INVARIANT_HANDLERS) == RECOGNIZED_OPAQUE_TYPES, (
    "z3_checks handler set and RECOGNIZED_OPAQUE_TYPES diverged: "
    f"{set(_INVARIANT_HANDLERS) ^ RECOGNIZED_OPAQUE_TYPES}"
)


def check_plan_invariants(
    plan: ActionPlan,
    envelope: Envelope,
    timeout_ms: int = 500,  # reserved for future symbolic-trajectory work
) -> list[Violation]:
    """Check plan against each invariant declared on the envelope.

    Runs the dedicated symbolic/numerical handlers for the recognized opaque
    invariant types (see RECOGNIZED_OPAQUE_TYPES in _invariant_types.py).
    Predicate-algebra invariants (those carrying ``expr``) and the strict-mode
    treatment of unrecognized opaque types are handled in verify.py — NOT here.
    """
    declared = {inv.type for inv in envelope.invariants}
    violations: list[Violation] = []
    for inv_type, handler in _INVARIANT_HANDLERS.items():
        if inv_type in declared:
            violations.extend(handler(plan, envelope))
    return violations
