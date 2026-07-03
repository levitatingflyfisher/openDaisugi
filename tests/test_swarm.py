"""Verified swarm tasking: airspace deconfliction via envelope algebra (v0.33)."""

from __future__ import annotations

from opendaisugi.models import Envelope, Permission
from opendaisugi.swarm import (
    SwarmConflict,
    SwarmVerdict,
    aabb_disjoint,
    aabb_intersection,
    partition_airspace,
    partition_and_assign,
    verify_swarm_tasking,
)

BoxT = tuple[tuple[float, float, float], tuple[float, float, float]]


def _env(bounds: BoxT | None, *, velocity=None) -> Envelope:
    return Envelope(
        generated_by="swarm-test", task="fly", stakes="physical",
        permissions=Permission(workspace_bounds=bounds, velocity_limit=velocity),
    )


# --------------------------- AABB geometry ---------------------------

def test_disjoint_boxes_separated_on_x():
    a = ((0, 0, 0), (1, 1, 1))
    b = ((2, 0, 0), (3, 1, 1))
    assert aabb_disjoint(a, b)
    assert aabb_intersection(a, b) is None


def test_touching_faces_are_disjoint():
    a = ((0, 0, 0), (1, 1, 1))
    b = ((1, 0, 0), (2, 1, 1))  # share the x=1 face, no shared volume
    assert aabb_disjoint(a, b)


def test_overlapping_boxes_report_intersection():
    a = ((0, 0, 0), (2, 2, 2))
    b = ((1, 1, 1), (3, 3, 3))
    assert not aabb_disjoint(a, b)
    assert aabb_intersection(a, b) == ((1, 1, 1), (2, 2, 2))


# --------------------------- partitioning ---------------------------

def test_partition_airspace_yields_n_disjoint_covering_slabs():
    total = ((0, 0, 0), (9, 3, 3))
    parts = partition_airspace(total, 3, axis=0)
    assert len(parts) == 3
    # pairwise disjoint
    for i in range(3):
        for j in range(i + 1, 3):
            assert aabb_disjoint(parts[i], parts[j])
    # cover the whole span on the split axis
    assert parts[0][0][0] == 0 and parts[-1][1][0] == 9


def test_partition_with_margin_leaves_a_gap():
    total = ((0, 0, 0), (10, 3, 3))
    parts = partition_airspace(total, 2, axis=0, margin=1.0)
    # slab 0 ends at 5 - 0.5 = 4.5; slab 1 starts at 5 + 0.5 = 5.5 → gap 1.0
    assert parts[0][1][0] == 4.5
    assert parts[1][0][0] == 5.5
    assert aabb_disjoint(parts[0], parts[1], margin=1.0)
    # outer faces stay on the total
    assert parts[0][0][0] == 0 and parts[1][1][0] == 10


def test_partition_margin_too_large_raises():
    import pytest
    with pytest.raises(ValueError):
        partition_airspace(((0, 0, 0), (10, 3, 3)), 5, axis=0, margin=3.0)  # span 2 < margin 3


def test_partition_and_assign_is_provably_deconflicted():
    total = _env(((0, 0, 0), (30, 10, 10)), velocity=2.0)
    assignments = partition_and_assign(total, ["drone_a", "drone_b", "drone_c"], axis=0)
    assert set(assignments) == {"drone_a", "drone_b", "drone_c"}
    verdict = verify_swarm_tasking(total, assignments)
    assert verdict.ok
    assert verdict.deconflicted
    assert verdict.all_subsumed
    assert verdict.conflicts == []


# --------------------------- swarm verification ---------------------------

def test_overlapping_assignments_are_a_conflict():
    total = _env(((0, 0, 0), (30, 10, 10)))
    assignments = {
        "d1": _env(((0, 0, 0), (20, 10, 10))),
        "d2": _env(((10, 0, 0), (30, 10, 10))),  # overlaps d1 on x in [10,20]
    }
    verdict = verify_swarm_tasking(total, assignments)
    assert not verdict.ok
    assert not verdict.deconflicted
    assert any(isinstance(c, SwarmConflict) and {c.drone_a, c.drone_b} == {"d1", "d2"}
               for c in verdict.conflicts)


def test_drone_exceeding_total_fails_subsumption():
    total = _env(((0, 0, 0), (10, 10, 10)))
    assignments = {"rogue": _env(((0, 0, 0), (99, 10, 10)))}  # exceeds total on x
    verdict = verify_swarm_tasking(total, assignments)
    assert not verdict.ok
    assert not verdict.all_subsumed
    assert "rogue" in verdict.subsumption_failures


def test_undeclared_bounds_cannot_be_deconflicted():
    # Fail-closed: a drone with no workspace_bounds can't be proven disjoint.
    total = _env(((0, 0, 0), (10, 10, 10)))
    assignments = {
        "bounded": _env(((0, 0, 0), (5, 10, 10))),
        "unbounded": _env(None),
    }
    verdict = verify_swarm_tasking(total, assignments)
    assert not verdict.ok


def test_margin_turns_touching_sectors_into_a_conflict():
    total = _env(((0, 0, 0), (20, 10, 10)))
    # Two touching sectors (share the x=10 face) — disjoint with margin 0...
    assignments = {
        "a": _env(((0, 0, 0), (10, 10, 10))),
        "b": _env(((10, 0, 0), (20, 10, 10))),
    }
    assert verify_swarm_tasking(total, assignments, margin=0.0).ok
    # ...but a 1.0 safety margin (vehicle radius + estimator error) rejects them.
    v = verify_swarm_tasking(total, assignments, margin=1.0)
    assert not v.ok
    assert any("margin" in c.reason for c in v.conflicts)


def test_aabb_disjoint_respects_margin():
    a = ((0, 0, 0), (1, 1, 1))
    b = ((1.5, 0, 0), (2.5, 1, 1))  # 0.5 gap on x
    assert aabb_disjoint(a, b, margin=0.4)      # gap 0.5 >= 0.4 → separated
    assert not aabb_disjoint(a, b, margin=0.6)  # gap 0.5 < 0.6 → too close


def test_single_drone_is_trivially_deconflicted():
    total = _env(((0, 0, 0), (10, 10, 10)))
    verdict = verify_swarm_tasking(total, {"solo": _env(((0, 0, 0), (5, 5, 5)))})
    assert verdict.ok
    assert isinstance(verdict, SwarmVerdict)


# --------------------------- fail-open guards (SGCM review) ---------------------------

import pytest as _pytest


def test_negative_margin_is_rejected_not_a_fail_open():
    # A negative margin would SUBTRACT from separation and certify overlapping
    # drones as deconflicted — the worst kind of fail-open for a safety layer.
    a = ((0, 0, 0), (1, 1, 1)); b = ((4, 0, 0), (5, 1, 1))
    with _pytest.raises(ValueError):
        aabb_disjoint(a, b, margin=-5)
    total = _env(((0, 0, 0), (30, 10, 10)))
    overlapping = {"d1": _env(((0, 0, 0), (10, 10, 10))), "d2": _env(((8, 0, 0), (18, 10, 10)))}
    with _pytest.raises(ValueError):
        verify_swarm_tasking(total, overlapping, margin=-5)


def test_inverted_box_is_not_certified_deconflicted():
    # A malformed AABB (min > max on an axis) has undefined semantics; certifying
    # it as in-scope + deconflicted is a fail-open.
    total = _env(((0, 0, 0), (30, 10, 10)))
    assignments = {
        "ok": _env(((0, 0, 0), (10, 10, 10))),
        "inverted": _env(((10, 0, 0), (0, 10, 10))),  # min.x=10 > max.x=0
    }
    verdict = verify_swarm_tasking(total, assignments)
    assert not verdict.ok


def test_non_finite_box_is_not_certified():
    total = _env(((0, 0, 0), (30, 10, 10)))
    assignments = {"nan": _env(((0, 0, 0), (float("nan"), 10, 10)))}
    assert not verify_swarm_tasking(total, assignments).ok


def test_inverted_total_fails_closed():
    total = _env(((30, 0, 0), (0, 10, 10)))  # inverted total
    assignments = {"d1": _env(((0, 0, 0), (10, 10, 10)))}
    assert not verify_swarm_tasking(total, assignments).ok
