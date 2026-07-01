"""Verified swarm tasking — airspace deconfliction via envelope algebra (v0.33).

openDaisugi already proves *containment*: ``envelope_subsumes(outer, inner)`` shows
a delegated scope fits inside a parent's (a drone stays within the operational
volume it was given). Swarms need the other half — *disjointness*: a proof that no
two drones were handed overlapping airspace. This module adds that, so a coordinator
can hand each robot a tasking envelope and get a **deconfliction certificate**:

    every drone's envelope ⊆ the total operational envelope   (subsumption — reused)
    AND every pair of drones' workspace volumes are disjoint   (deconfliction — new)
    ⟹ the swarm's tasking is verified: no drone can leave its lane, and no two
      lanes overlap.

This is PLAN-LEVEL / volume-level assurance over declared ``workspace_bounds`` AABBs.
Honest scope (what it does and does NOT prove):

- **Analytic geometry, not a solver.** The disjointness/containment checks are
  decidable interval arithmetic (a separating-axis test) — fast and deterministic,
  not "Z3-backed". Z3 only enters for the shell/invariant part of ``envelope_subsumes``.
- **Space, not spacetime.** ``workspace_bounds`` is a 3D AABB with no time axis, so
  this forces *spatial* disjointness for the whole mission. Real UTM (ASTM F3548-21)
  deconflicts 4D volumes and may share a volume across time; this is strictly cruder.
- **Volumes, not flight.** It proves assigned *volumes* don't overlap and that a plan's
  declared waypoints sit inside a volume. It does NOT prove the *flown* path stays in
  the box (inertia/overshoot/wind can carry a vehicle out between waypoints), and
  disjoint boxes + in-box waypoints still permit a collision — hence the ``margin``
  (set it ≥ position-uncertainty + vehicle-radius). Complementary to tactical avoidance
  (DAIDALUS/ORCA) and certified geofencing (NASA PolyCARP), not a replacement.

Fail-closed: a drone that declares no ``workspace_bounds`` cannot be proven disjoint
from anything, so it is denied.
"""

import math
from dataclasses import dataclass, field

from opendaisugi.models import Envelope
from opendaisugi.subsumption import envelope_subsumes

Box = tuple[tuple[float, float, float], tuple[float, float, float]]


def _box_invalid_reason(box: Box) -> str | None:
    """None if ``box`` is a well-formed AABB, else a reason string.

    A valid AABB has finite coordinates and ``min[k] <= max[k]`` on every axis.
    An inverted (min>max) or non-finite box has undefined separating-axis
    semantics — treating it as valid is a fail-open, so callers reject it.
    """
    try:
        (lo, hi) = box
        if len(lo) != 3 or len(hi) != 3:
            return f"box must be two xyz triples, got {box!r}"
    except (TypeError, ValueError):
        return f"box must be ((x,y,z),(x,y,z)), got {box!r}"
    for k in range(3):
        if not (math.isfinite(lo[k]) and math.isfinite(hi[k])):
            return f"non-finite coordinate on axis {k}: {box!r}"
        if lo[k] > hi[k]:
            return f"inverted on axis {k} (min {lo[k]} > max {hi[k]}): {box!r}"
    return None


def aabb_disjoint(a: Box, b: Box, *, margin: float = 0.0) -> bool:
    """True if two axis-aligned boxes are separated by at least ``margin``.

    Separating-axis test: two boxes are disjoint iff, on some axis, one's max is
    below the other's min. With ``margin=0`` (default) boxes that merely touch on
    a face are disjoint — a shared boundary plane has zero volume. Real physical
    separation needs ``margin ≥ position-uncertainty + vehicle-radius`` so two
    vehicles on adjacent boundaries can't occupy the same point; pass it explicitly
    (openDaisugi can't know your vehicle's size or your estimator's error).

    Raises ``ValueError`` on a negative ``margin`` — a negative margin would
    *subtract* from separation and report overlapping boxes as disjoint (fail-open).
    """
    if margin < 0:
        raise ValueError(f"margin must be non-negative, got {margin}")
    (a_min, a_max) = a
    (b_min, b_max) = b
    for k in range(3):
        if a_max[k] + margin <= b_min[k] or b_max[k] + margin <= a_min[k]:
            return True  # separated on axis k by >= margin ⟹ no shared volume
    return False


def aabb_intersection(a: Box, b: Box) -> Box | None:
    """The overlap box of two AABBs, or ``None`` if they are disjoint.

    The overlap region is the concrete counterexample to deconfliction: the
    airspace two drones could both occupy.
    """
    if aabb_disjoint(a, b):
        return None
    (a_min, a_max) = a
    (b_min, b_max) = b
    lo = tuple(max(a_min[k], b_min[k]) for k in range(3))
    hi = tuple(min(a_max[k], b_max[k]) for k in range(3))
    return (lo, hi)


def partition_airspace(total: Box, n: int, *, axis: int = 0, margin: float = 0.0) -> list[Box]:
    """Split an AABB into ``n`` disjoint slabs along ``axis``.

    Returns ``n`` boxes stacked on ``axis`` and pairwise disjoint by construction.
    With ``margin=0`` they tile ``total`` exactly (adjacent faces touch). With
    ``margin>0`` each interior boundary is pulled in by ``margin/2`` so adjacent
    slabs are separated by a ``margin`` gap — pass the same margin to
    ``verify_swarm_tasking`` and the assignment is certified deconflicted at that
    separation. ``axis`` 0/1/2 = x/y/z; z (altitude bands) is common for drones.
    """
    if n < 1:
        raise ValueError("n must be >= 1")
    if axis not in (0, 1, 2):
        raise ValueError("axis must be 0, 1, or 2")
    if margin < 0:
        raise ValueError("margin must be non-negative")
    (lo, hi) = total
    span = (hi[axis] - lo[axis]) / n
    if margin >= span:
        raise ValueError(f"margin {margin} too large for {n} slabs of span {span:.3f}")
    boxes: list[Box] = []
    for i in range(n):
        b_lo = list(lo)
        b_hi = list(hi)
        # Interior faces recede by margin/2; the two outer faces stay on the total.
        b_lo[axis] = lo[axis] + i * span + (margin / 2 if i > 0 else 0.0)
        b_hi[axis] = (lo[axis] + (i + 1) * span - margin / 2) if i < n - 1 else hi[axis]
        boxes.append((tuple(b_lo), tuple(b_hi)))
    return boxes


def _with_workspace_bounds(envelope: Envelope, bounds: Box) -> Envelope:
    """Copy ``envelope`` with its workspace_bounds tightened to ``bounds``."""
    perms = envelope.permissions.model_copy(update={"workspace_bounds": bounds})
    return envelope.model_copy(update={"permissions": perms})


def partition_and_assign(
    total: Envelope, drone_ids: list[str], *, axis: int = 0, margin: float = 0.0
) -> dict[str, Envelope]:
    """Partition ``total``'s airspace into disjoint sub-volumes, one per drone.

    Each returned envelope is a copy of ``total`` with its ``workspace_bounds``
    tightened to one slab (and its enforcement invariants + other capability bounds
    inherited, keeping it subsumed). ``margin`` leaves a separation gap between
    adjacent sectors; ``verify_swarm_tasking(total, result, margin=margin)`` is then
    guaranteed ``ok``.
    """
    if total.permissions.workspace_bounds is None:
        raise ValueError("total envelope must declare workspace_bounds to partition")
    slabs = partition_airspace(
        total.permissions.workspace_bounds, len(drone_ids), axis=axis, margin=margin
    )
    return {did: _with_workspace_bounds(total, slab) for did, slab in zip(drone_ids, slabs, strict=True)}


@dataclass(frozen=True)
class SwarmConflict:
    """Two drones assigned overlapping airspace (the shared region is the proof)."""

    drone_a: str
    drone_b: str
    region: Box | None  # None when a drone declared no bounds (can't localize)
    reason: str


@dataclass
class SwarmVerdict:
    """Result of verifying a swarm tasking assignment."""

    all_subsumed: bool
    deconflicted: bool
    subsumption_failures: dict[str, str] = field(default_factory=dict)
    conflicts: list[SwarmConflict] = field(default_factory=list)

    @property
    def ok(self) -> bool:
        return self.all_subsumed and self.deconflicted


def verify_swarm_tasking(
    total: Envelope,
    assignments: dict[str, Envelope],
    *,
    require_subsumption: bool = True,
    margin: float = 0.0,
    timeout_ms: int = 2000,
) -> SwarmVerdict:
    """Certify a swarm tasking assignment: every drone in-scope, no two overlapping.

    - **Subsumption** (when ``require_subsumption``): each drone's envelope must be
      subsumed by ``total`` — it can't be tasked outside the operational envelope or
      with capabilities the coordinator doesn't hold. Reuses ``envelope_subsumes``
      (incl. its fail-closed robot-capability checks).
    - **Deconfliction**: every pair of drones' ``workspace_bounds`` must be disjoint
      by at least ``margin``. A drone that declares no bounds is denied (undeclared
      = can't prove disjoint).

    Returns a :class:`SwarmVerdict`; ``.ok`` iff both hold. Conflicts carry the
    concrete overlap region so the caller can see exactly where two drones could meet.

    Raises ``ValueError`` on a negative ``margin``. Malformed workspace_bounds
    (inverted or non-finite) fail closed — the affected drone is marked unsubsumed
    rather than silently certified.
    """
    if margin < 0:
        raise ValueError(f"margin must be non-negative, got {margin}")

    subsumption_failures: dict[str, str] = {}

    # A malformed total volume makes every containment claim meaningless — fail closed.
    total_box = total.permissions.workspace_bounds
    total_bad = _box_invalid_reason(total_box) if total_box is not None else None
    if total_bad is not None:
        subsumption_failures["__total__"] = f"total workspace_bounds invalid: {total_bad}"

    # Boxes that are malformed can't participate in a sound separating-axis test —
    # exclude them from deconfliction and mark them unsubsumed (fail closed).
    invalid_boxes: dict[str, str] = {}
    for did, env in assignments.items():
        box = env.permissions.workspace_bounds
        if box is not None:
            bad = _box_invalid_reason(box)
            if bad is not None:
                invalid_boxes[did] = bad
                subsumption_failures[did] = f"invalid workspace_bounds: {bad}"

    if require_subsumption:
        for did, env in assignments.items():
            if did in invalid_boxes:
                continue  # already a failure; envelope_subsumes on a bad box is nonsense
            result = envelope_subsumes(total, env, timeout_ms=timeout_ms)
            if not result.holds:
                if result.counterexample is not None:
                    reason = f"admits {result.counterexample.step!r} outside total"
                elif result.reasons:
                    reason = "; ".join(result.reasons)
                else:
                    reason = "not subsumed by total envelope"
                subsumption_failures[did] = reason

    conflicts: list[SwarmConflict] = []
    ids = list(assignments)
    for i in range(len(ids)):
        for j in range(i + 1, len(ids)):
            a_id, b_id = ids[i], ids[j]
            a_box = assignments[a_id].permissions.workspace_bounds
            b_box = assignments[b_id].permissions.workspace_bounds
            if a_box is None or b_box is None or a_id in invalid_boxes or b_id in invalid_boxes:
                bad_ids = [d for d in (a_id, b_id)
                           if assignments[d].permissions.workspace_bounds is None
                           or d in invalid_boxes]
                conflicts.append(SwarmConflict(
                    drone_a=a_id, drone_b=b_id, region=None,
                    reason=f"drone(s) {bad_ids} have missing or malformed workspace_bounds "
                           f"(cannot prove disjoint → denied)",
                ))
                continue
            if not aabb_disjoint(a_box, b_box, margin=margin):
                region = aabb_intersection(a_box, b_box)
                if region is not None:
                    reason = f"assigned airspace overlaps in region {region}"
                else:
                    reason = f"assigned airspace separated by less than safety margin {margin}"
                conflicts.append(SwarmConflict(
                    drone_a=a_id, drone_b=b_id, region=region, reason=reason,
                ))

    return SwarmVerdict(
        all_subsumed=(not subsumption_failures),
        deconflicted=(not conflicts),
        subsumption_failures=subsumption_failures,
        conflicts=conflicts,
    )


__all__ = [
    "Box",
    "SwarmConflict",
    "SwarmVerdict",
    "aabb_disjoint",
    "aabb_intersection",
    "partition_airspace",
    "partition_and_assign",
    "verify_swarm_tasking",
]
