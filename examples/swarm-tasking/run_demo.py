"""Verified swarm tasking — airspace deconfliction via envelope algebra. Runnable.

No physics, no LLM, no network — pure envelope algebra. Demonstrates openDaisugi's
differentiator for drone-swarm delegation:

  1. A coordinator partitions its operational volume into disjoint sectors.
  2. Each drone is delegated a sector — proved CONTAINED in the coordinator
     (subsumption) and proved NON-OVERLAPPING with siblings (deconfliction).
  3. Per-drone flight plans are gated: in-sector waypoints ACCEPT, out-of-sector
     REJECT.
  4. Swarm-of-swarms: nested delegation + top-level deconfliction.
  5. Comms-loss reassignment is gated on a fresh proof before it's allowed.

We LEAD WITH THE REJECTIONS — the value is what the verifier refuses.

Honest scope: this is plan/volume-level, analytic geometry (not Z3, not 4D, not a
flight-safety certificate). See swarm.py and README.md. Run:  python run_demo.py
"""

from opendaisugi import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    aabb_disjoint,
    partition_and_assign,
    verify,
    verify_swarm_tasking,
)
from opendaisugi.models import CartesianMoveStep

RULE = "─" * 70
MARGIN = 0.5  # separation gap = vehicle radius + position-uncertainty (you set this)


def _coordinator(bounds, *, velocity=3.0):
    # The end_effector_in_workspace invariant is the enforcement switch: it makes
    # verify() reject any CartesianMoveStep whose target leaves workspace_bounds.
    # Drones inherit it (partition_and_assign copies the coordinator), so each
    # drone's plan is gated against its own tightened sector.
    return Envelope(
        generated_by="swarm-coordinator", task="survey the field", stakes="physical",
        permissions=Permission(workspace_bounds=bounds, velocity_limit=velocity),
        invariants=[Invariant(type="end_effector_in_workspace",
                              description="drone stays within its assigned sector")],
    )


def _flight_plan(drone_id, waypoints):
    steps = [
        CartesianMoveStep(id=f"{drone_id}_wp{i}", target_position=wp,
                          depends_on=([f"{drone_id}_wp{i-1}"] if i else []))
        for i, wp in enumerate(waypoints)
    ]
    return ActionPlan(source=drone_id, task="fly waypoints", steps=steps)


def main():
    print(RULE)
    print("1. COORDINATOR partitions its operational volume into disjoint sectors")
    print(RULE)
    total = _coordinator(((0, 0, 0), (30, 10, 10)))
    fleet = ["drone_west", "drone_mid", "drone_east"]
    assignments = partition_and_assign(total, fleet, axis=0, margin=MARGIN)  # split along x
    for did, env in assignments.items():
        print(f"  {did}: workspace_bounds = {env.permissions.workspace_bounds}")

    verdict = verify_swarm_tasking(total, assignments, margin=0.5)
    print(f"\n  DECONFLICTION CERTIFICATE: ok={verdict.ok} "
          f"(subsumed={verdict.all_subsumed}, deconflicted={verdict.deconflicted})")
    print("  → every drone provably in-scope AND no two sectors overlap (0.5m margin)")

    print("\n" + RULE)
    print("2. PLAN GATING — the verifier ACCEPTS in-sector, REJECTS out-of-sector")
    print(RULE)
    west = assignments["drone_west"]  # sector x in [0,10]
    ok_plan = _flight_plan("drone_west", [(1, 1, 1), (5, 5, 5), (9, 2, 8)])
    bad_plan = _flight_plan("drone_west", [(1, 1, 1), (18, 5, 5)])  # 18 is in drone_east!
    r_ok = verify(ok_plan, west)
    r_bad = verify(bad_plan, west)
    print(f"  in-sector plan   → verify.ok = {r_ok.ok}   ✓ ACCEPTED")
    print(f"  crosses into east→ verify.ok = {r_bad.ok}  ✗ REJECTED")
    if not r_bad.ok:
        print(f"      reason: {r_bad.violations[0].message}")

    print("\n" + RULE)
    print("3. REJECTIONS — the money: what openDaisugi refuses")
    print(RULE)

    # (a) over-broad delegation: a drone tasked beyond the coordinator's volume
    rogue = {"rogue": _coordinator(((0, 0, 0), (99, 10, 10)))}  # x max 99 > 30
    v = verify_swarm_tasking(total, rogue)
    print(f"  (a) over-broad delegation (drone exceeds coordinator): ok={v.ok}  ✗ REJECTED")
    print(f"      {list(v.subsumption_failures.values())[0][:80]}")

    # (b) overlapping sectors
    overlap = {
        "a": _coordinator(((0, 0, 0), (20, 10, 10))),
        "b": _coordinator(((10, 0, 0), (30, 10, 10))),  # overlaps a on x in [10,20]
    }
    v = verify_swarm_tasking(total, overlap)
    print(f"  (b) overlapping sectors: ok={v.ok}  ✗ REJECTED")
    print(f"      conflict region: {v.conflicts[0].region}")

    # (c) undeclared bounds (fail-closed)
    undeclared = {
        "bounded": _coordinator(((0, 0, 0), (10, 10, 10))),
        "ghost": Envelope(generated_by="x", task="y", stakes="physical",
                          permissions=Permission()),  # no workspace_bounds
    }
    v = verify_swarm_tasking(total, undeclared)
    print(f"  (c) drone with undeclared bounds: ok={v.ok}  ✗ REJECTED (fail-closed)")

    print("\n" + RULE)
    print("4. SWARM-OF-SWARMS — nested delegation + top-level deconfliction")
    print(RULE)
    # Coordinator delegates to two sub-coordinators (west half / east half)...
    west_half, east_half = partition_and_assign(total, ["west_swarm", "east_swarm"], axis=0).values()
    # ...each sub-coordinator partitions its half among its own drones.
    west_drones = partition_and_assign(west_half, ["w1", "w2"], axis=1)   # split along y
    east_drones = partition_and_assign(east_half, ["e1", "e2"], axis=1)
    print(f"  west_swarm ⊆ coordinator: {verify_swarm_tasking(total, {'west_swarm': west_half}).ok}")
    print(f"  w1,w2 ⊆ west_swarm + disjoint: {verify_swarm_tasking(west_half, west_drones).ok}")
    print(f"  e1,e2 ⊆ east_swarm + disjoint: {verify_swarm_tasking(east_half, east_drones).ok}")
    # top-level: the two swarms don't share airspace
    print(f"  west_swarm ∥ east_swarm (disjoint): "
          f"{aabb_disjoint(west_half.permissions.workspace_bounds, east_half.permissions.workspace_bounds)}")

    print("\n" + RULE)
    print("5. COMMS-LOSS REASSIGNMENT — gated on a fresh proof")
    print(RULE)
    # drone_mid drops. Try to give its sector to drone_west by enlarging west.
    survivors = {k: v for k, v in assignments.items() if k != "drone_mid"}
    mid_box = assignments["drone_mid"].permissions.workspace_bounds
    west_box = assignments["drone_west"].permissions.workspace_bounds
    # Enlarge west to absorb mid's x-range (they're adjacent along x).
    enlarged = ((west_box[0][0], west_box[0][1], west_box[0][2]),
                (mid_box[1][0], west_box[1][1], west_box[1][2]))
    proposed = dict(survivors)
    proposed["drone_west"] = total.model_copy(update={
        "permissions": total.permissions.model_copy(update={"workspace_bounds": enlarged})})
    v = verify_swarm_tasking(total, proposed)
    print(f"  reassign drone_mid's sector to drone_west → re-verify: ok={v.ok}")
    if v.ok:
        print("  ✓ reassignment ALLOWED — enlarged sector still ⊆ coordinator and "
              "still disjoint from drone_east")
    print("\n(If the enlargement had overlapped drone_east, the reassignment would be "
          "REJECTED before any drone moved.)")


if __name__ == "__main__":
    main()
