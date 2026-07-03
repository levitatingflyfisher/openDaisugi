"""Property-security patrol — runtime assurance for a VLA swarm, in motion.

`python run_demo.py` — no physics engine, no GPU, no network, no real model.
Pure envelope algebra + a deterministic MOCK "VLA" policy, so it runs anywhere.

The story (this is the architecturally-novel part the docs promise):

  A property is divided into disjoint patrol sectors, one per drone. Each tick a
  black-box policy (here a scripted stand-in for a VLA like pi0 / SmolVLA) PROPOSES
  each drone's next waypoint. openDaisugi does the Simplex move LIVE:

      policy proposes  ─▶  verify(move ⊆ drone's sector envelope)  ─▶  accept
                                    │  reject (out of sector)              (drone advances)
                                    └─▶ fall back to a verified-safe baseline
                       ─▶  swarm deconfliction (no two drones within margin) ─▶ hold the later one

  So a drone can NEVER leave its sector and two drones can NEVER close within the
  safety margin — proven before any motion, whatever the policy proposes.

This is Simplex (advanced policy + verified baseline + monitored switching) applied
to a VLA swarm. The policy is a black box; the assurance is the star. Swap the mock
for a real SmolVLA and the guarantees are identical — that is the whole point.

HONEST SCOPE (see docs/spec/yellow-paper.md §7): this is analytic geometry, not a
flight-safety certificate. Waypoint-in-box is not path-in-box; disjoint boxes are
collision-free only if margin >= vehicle radius + position uncertainty. A physics
upgrade (MuJoCo on CPU) + a real async SmolVLA is the documented next step.
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

RULE = "─" * 74
SECTOR_MARGIN = 0.5   # static gap between assigned sectors (partition + certificate)
SAFETY_BUBBLE = 3.0   # LARGER live per-tick separation openDaisugi enforces in motion
DRONE_RADIUS = 0.5    # half-extent of the box a drone occupies


def _coordinator(bounds, *, velocity=3.0):
    # The end_effector_in_workspace invariant is the enforcement switch: it makes
    # verify() reject any CartesianMoveStep whose target leaves workspace_bounds.
    return Envelope(
        generated_by="patrol-coordinator", task="secure the property", stakes="physical",
        permissions=Permission(workspace_bounds=bounds, velocity_limit=velocity),
        invariants=[Invariant(type="end_effector_in_workspace",
                              description="drone stays within its assigned sector")],
    )


def _move(drone_id, target):
    """A single proposed move as a one-step plan we can hand to verify()."""
    return ActionPlan(source=drone_id, task="next waypoint",
                      steps=[CartesianMoveStep(id=f"{drone_id}_step", target_position=target)])


def _clamp_into(sector_bounds, target):
    """Simplex baseline: pull a proposal back inside the sector (return-to-sector)."""
    (lo_x, lo_y, lo_z), (hi_x, hi_y, hi_z) = sector_bounds
    return (min(max(target[0], lo_x + DRONE_RADIUS), hi_x - DRONE_RADIUS),
            min(max(target[1], lo_y + DRONE_RADIUS), hi_y - DRONE_RADIUS),
            min(max(target[2], lo_z + DRONE_RADIUS), hi_z - DRONE_RADIUS))


def _occupies(pos):
    r = DRONE_RADIUS
    return ((pos[0] - r, pos[1] - r, pos[2] - r), (pos[0] + r, pos[1] + r, pos[2] + r))


class MockVLA:
    """Deterministic stand-in for a real VLA. Emits a patrol sweep, and at scripted
    ticks emits proposals that are individually SECTOR-legal but bring two drones
    within the live safety bubble (or wander out of sector) so you can watch
    openDaisugi refuse them. A real SmolVLA drops in at .propose() unchanged."""

    def __init__(self, sector_bounds):
        (self.lx, self.ly, self.lz), (self.hx, self.hy, self.hz) = sector_bounds
        self.cx = (self.lx + self.hx) / 2
        self.cy = (self.ly + self.hy) / 2
        self.cz = (self.lz + self.hz) / 2

    def propose(self, drone_id, tick, pos):
        # Baseline behavior: sweep in y across the sector at mid-x, mid-z.
        y = self.cy + (4.0 if tick % 2 == 0 else -4.0)
        target = (self.cx, y, self.cz)
        # Scripted black-box "surprises" — the kind an unconstrained policy makes:
        if drone_id == "drone_mid" and tick == 3:
            target = (28.0, self.cy, self.cz)                 # chase intruder INTO the east sector
        # tick 4: two neighbors both patrol to their shared boundary — each move is
        # legal in its own sector, but they'd close inside the live safety bubble.
        if drone_id == "drone_west" and tick == 4:
            target = (self.hx - DRONE_RADIUS, self.cy, self.cz)   # west's east edge (legal)
        if drone_id == "drone_mid" and tick == 4:
            target = (self.lx + DRONE_RADIUS, self.cy, self.cz)   # mid's west edge (legal)
        return target


def main():
    print(RULE)
    print(" PROPERTY-SECURITY PATROL — openDaisugi gating a (mock) VLA swarm, live")
    print(RULE)

    # 1. Partition the property into disjoint patrol sectors + prove deconfliction.
    property_bounds = ((0, 0, 0), (30, 12, 8))
    coordinator = _coordinator(property_bounds)
    fleet = ["drone_west", "drone_mid", "drone_east"]
    sectors = partition_and_assign(coordinator, fleet, axis=0, margin=SECTOR_MARGIN)  # split along x
    verdict = verify_swarm_tasking(coordinator, sectors, margin=SECTOR_MARGIN)
    print(f"\n Sectors assigned; static deconfliction certificate: ok={verdict.ok} "
          f"(subsumed={verdict.all_subsumed}, disjoint={verdict.deconflicted})")
    print(f"   static sector gap = {SECTOR_MARGIN}m; live safety bubble = {SAFETY_BUBBLE}m (stricter, in-motion)")
    for did, env in sectors.items():
        print(f"   {did}: sector = {env.permissions.workspace_bounds}")

    # Each drone starts at its sector center; its mock VLA knows its sector bounds.
    pos, vlas = {}, {}
    for did, env in sectors.items():
        (lx, ly, lz), (hx, hy, hz) = env.permissions.workspace_bounds
        pos[did] = ((lx + hx) / 2, (ly + hy) / 2, (lz + hz) / 2)
        vlas[did] = MockVLA(env.permissions.workspace_bounds)

    print("\n" + RULE)
    print(" PATROL LOOP — each tick: VLA proposes → verify → accept | fall back | hold")
    print(RULE)

    stats = {"accepted": 0, "sector_rejects": 0, "deconflict_holds": 0}
    for tick in range(1, 7):
        print(f"\n tick {tick}")
        committed = dict(pos)  # positions locked in this tick, for live deconfliction
        for did in fleet:
            env = sectors[did]
            proposed = vlas[did].propose(did, tick, pos[did])

            # (a) SECTOR GATE — verify the proposed move against the drone's envelope.
            r = verify(_move(did, proposed), env)
            if r.ok:
                target = proposed
                note = f"proposes {tuple(round(v,1) for v in proposed)} → ✓ accepted"
            else:
                baseline = _clamp_into(env.permissions.workspace_bounds, proposed)
                rb = verify(_move(did, baseline), env)  # the baseline must itself verify
                target = baseline if rb.ok else pos[did]
                stats["sector_rejects"] += 1
                reason = r.violations[0].message[:52] if r.violations else "out of sector"
                note = (f"proposes {tuple(round(v,1) for v in proposed)} → ✗ REJECTED "
                        f"({reason})\n              └─ Simplex fallback → "
                        f"{tuple(round(v,1) for v in target)}")

            # (b) SWARM DECONFLICTION — would this move come within margin of a peer?
            conflict = next((o for o in fleet if o != did
                             and not aabb_disjoint(_occupies(target), _occupies(committed[o]),
                                                   margin=SAFETY_BUBBLE)), None)
            if conflict:
                target = pos[did]  # hold; do not close on a neighbor
                stats["deconflict_holds"] += 1
                note += f"\n              └─ DECONFLICT: too close to {conflict} → HOLD"
            else:
                if r.ok:
                    stats["accepted"] += 1

            committed[did] = target
            pos[did] = target
            print(f"   {did}: {note}")

    print("\n" + RULE)
    print(" SUMMARY")
    print(RULE)
    print(f"   accepted moves:        {stats['accepted']}")
    print(f"   out-of-sector refused: {stats['sector_rejects']}  (→ verified fallback)")
    print(f"   deconfliction holds:   {stats['deconflict_holds']}")
    # Prove the safety property held for the whole run.
    for did, p in pos.items():
        (lx, ly, lz), (hx, hy, hz) = sectors[did].permissions.workspace_bounds
        assert lx <= p[0] <= hx and ly <= p[1] <= hy, f"{did} left its sector!"
    print("   INVARIANT: no drone ever left its sector; no pair closed within margin —")
    print("   proven each tick BEFORE motion, whatever the black-box policy proposed.")


if __name__ == "__main__":
    main()
