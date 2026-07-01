"""Swarm communication & delegation — verified authority transfer between robots.

`python run_demo.py` — no physics, no GPU, no network, no model. Pure envelope
algebra, so it runs anywhere.

The idea: in openDaisugi a **message that carries authority IS a delegation.**
"Drone, patrol sector 3" is a *tasking envelope*. Safe swarm communication means
the recipient (or a coordinator) PROVES the transferred authority is contained
*before anyone acts* — `envelope_subsumes(recipient_authority, task)` for delegation,
disjointness (`aabb_disjoint` / `verify_swarm_tasking`) for deconfliction. Every
hand-off, reassignment, and cross-swarm entry is gated on re-proof, fail-closed.

Four onboarding/verification scenarios, each LEADING WITH THE REJECTION (the value
is what the verifier refuses):

  1. Delegation down a hierarchy   (mission → squads → drones; nested containment)
  2. Lateral hand-off              (drone A hands a task to drone B)
  3. Comms-loss reassignment       (a survivor covers a downed peer, gated on re-proof)
  4. Cross-swarm coordination      (two swarms verify disjoint airspace before entry)

HONEST SCOPE (see docs/spec/yellow-paper.md §7): analytic geometry, plan/volume level
— waypoint-in-box is not path-in-box; disjoint boxes are collision-free only if the
margin >= vehicle radius + position uncertainty. This proves *tasking* is safe, not
that flight is.
"""

from opendaisugi import (
    Envelope,
    Invariant,
    Permission,
    aabb_disjoint,
    aabb_intersection,
    envelope_subsumes,
    partition_and_assign,
    verify_swarm_tasking,
)

RULE = "─" * 76
MARGIN = 0.5  # separation gap = vehicle radius + position uncertainty


def authority(name, bounds, *, velocity=3.0):
    """An agent's authority = a physical-stakes envelope over a volume. The
    end_effector_in_workspace invariant is the switch that makes the bound enforced."""
    return Envelope(
        generated_by=name, task=f"{name} operating authority", stakes="physical",
        permissions=Permission(workspace_bounds=bounds, velocity_limit=velocity),
        invariants=[Invariant(type="end_effector_in_workspace",
                              description="agent stays within its granted volume")],
    )


def _ok(holds):
    return "✓ ACCEPTED" if holds else "✗ REJECTED"


# ───────────────────────── Scenario 1: delegation hierarchy ──────────────────────

def scenario_hierarchy():
    print(RULE)
    print(" 1. DELEGATION DOWN A HIERARCHY — mission → squads → drones")
    print(RULE)
    mission = authority("mission", ((0, 0, 0), (40, 20, 10)))
    # The mission commander delegates two squad slices (a tasking-envelope message).
    squads = partition_and_assign(mission, ["squad_alpha", "squad_bravo"], axis=0, margin=MARGIN)
    for sid, env in squads.items():
        holds = envelope_subsumes(mission, env).holds
        print(f"   delegate {sid:12} ⊆ mission?   {_ok(holds)}")
    # Squad Alpha sub-delegates to two drones within ITS slice.
    alpha = squads["squad_alpha"]
    drones = partition_and_assign(alpha, ["a1", "a2"], axis=1, margin=MARGIN)
    for did, env in drones.items():
        holds_sq = envelope_subsumes(alpha, env).holds        # within the squad's grant
        holds_mi = envelope_subsumes(mission, env).holds      # transitively within the mission
        print(f"   sub-delegate {did:8} ⊆ squad_alpha? {_ok(holds_sq)}   ⊆ mission (transitive)? {_ok(holds_mi)}")

    # THE REJECTION: squad Alpha tries to task a drone BEYOND its own delegated slice.
    (lx, ly, lz), (hx, hy, hz) = alpha.permissions.workspace_bounds
    rogue = authority("a_rogue", ((lx, ly, lz), (hx + 15, hy, hz)))  # reaches into Bravo's slice
    r = envelope_subsumes(alpha, rogue)
    print(f"\n   ✗ squad Alpha tasks a drone beyond its grant → {_ok(r.holds)}")
    if not r.holds and r.reasons:
        print(f"       reason: {r.reasons[0][:64]}")
    print("   → you cannot delegate authority you were never given (fail-closed).")


# ───────────────────────── Scenario 2: lateral hand-off ──────────────────────────

def scenario_handoff():
    print("\n" + RULE)
    print(" 2. LATERAL HAND-OFF — drone A hands a tracking task to drone B")
    print(RULE)
    # Drone A patrols x∈[0,10]; drone B patrols x∈[10,20].
    b = authority("drone_B", ((10, 0, 0), (20, 20, 10)))
    # A target crosses from A's sector into B's. A HANDS OFF a task = a tasking
    # envelope. The hand-off is safe iff the task is within B's own authority.
    task_in_b = authority("track_target", ((12, 5, 0), (18, 15, 8)))   # lives in B's sector
    task_spills = authority("track_target", ((12, 5, 0), (24, 15, 8)))  # spills past B's sector
    r_ok = envelope_subsumes(b, task_in_b)
    r_bad = envelope_subsumes(b, task_spills)
    print(f"   A → B: 'track this, it's in your sector'  ⊆ B? {_ok(r_ok.holds)}")
    print(f"   A → B: 'chase it past your boundary'      ⊆ B? {_ok(r_bad.holds)}")
    print("   → a hand-off can only transfer a task the RECIPIENT is authorized to do;")
    print("     B re-proves the message on receipt before accepting the task.")


# ───────────────────── Scenario 3: comms-loss reassignment ───────────────────────

def scenario_comms_loss():
    print("\n" + RULE)
    print(" 3. COMMS-LOSS REASSIGNMENT — a survivor covers a downed peer, gated on re-proof")
    print(RULE)
    mission = authority("mission", ((0, 0, 0), (30, 20, 10)))
    fleet = partition_and_assign(mission, ["west", "mid", "east"], axis=0, margin=MARGIN)
    print("   fleet tasked; mid loses comms and goes dark.")
    (wlo), (whi) = fleet["west"].permissions.workspace_bounds
    (mlo), (mhi) = fleet["mid"].permissions.workspace_bounds

    # SAFE reassignment: west expands to cover west+mid; re-prove vs coordinator AND survivors.
    west_expanded = authority("west", (wlo, mhi))  # union box west→mid
    survivors = {"west": west_expanded, "east": fleet["east"]}
    v_ok = verify_swarm_tasking(mission, survivors, margin=MARGIN)
    print(f"   reassign mid's sector → WEST (expand), re-verify swarm: {_ok(v_ok.ok)} "
          f"(subsumed={v_ok.all_subsumed}, disjoint={v_ok.deconflicted})")

    # UNSAFE reassignment: hand mid's sector to BOTH neighbors → overlap.
    east_grab = authority("east", (mlo, fleet["east"].permissions.workspace_bounds[1]))
    bad = {"west": west_expanded, "east": east_grab}
    v_bad = verify_swarm_tasking(mission, bad, margin=MARGIN)
    print(f"   reassign mid's sector → BOTH neighbors, re-verify swarm: {_ok(v_bad.ok)}")
    if v_bad.conflicts:
        c = v_bad.conflicts[0]
        overlap = aabb_intersection(west_expanded.permissions.workspace_bounds,
                                    east_grab.permissions.workspace_bounds)
        print(f"       conflict: {c.drone_a} vs {c.drone_b}; shared region ≈ {overlap}")
    print("   → the reassignment MESSAGE is refused BEFORE any drone moves (fail-closed).")


# ───────────────────── Scenario 4: cross-swarm coordination ──────────────────────

def scenario_cross_swarm():
    print("\n" + RULE)
    print(" 4. CROSS-SWARM COORDINATION — two swarms verify disjoint airspace before entry")
    print(RULE)
    alpha_volume = ((0, 0, 0), (15, 20, 10))
    bravo_clear = ((16, 0, 0), (30, 20, 10))   # published: clear of Alpha
    bravo_entering = ((12, 0, 0), (30, 20, 10))  # published: overlaps Alpha
    ok = aabb_disjoint(alpha_volume, bravo_clear, margin=MARGIN)
    bad = aabb_disjoint(alpha_volume, bravo_entering, margin=MARGIN)
    print(f"   Bravo publishes a clear volume; Alpha verifies disjoint:    {_ok(ok)}")
    print(f"   Bravo publishes a volume that overlaps Alpha; Alpha checks: {_ok(bad)}")
    if not bad:
        print(f"       shared region ≈ {aabb_intersection(alpha_volume, bravo_entering)}")
    print("   → each swarm publishes its volume; the OTHER proves disjointness before")
    print("     entering shared airspace. No central authority needed — just re-proof.")


def main():
    print("\n SWARM COMMUNICATION & DELEGATION — a message that carries authority is a")
    print(" delegation, and safe communication = the recipient re-proves it, fail-closed.\n")
    scenario_hierarchy()
    scenario_handoff()
    scenario_comms_loss()
    scenario_cross_swarm()
    print("\n" + RULE)
    print(" Every transfer of authority — down a hierarchy, sideways as a hand-off, or")
    print(" across swarms — is a machine-checked containment proof. The swarm coordinates")
    print(" by exchanging tasking envelopes; openDaisugi refuses any that aren't contained.")
    print(RULE)


if __name__ == "__main__":
    main()
