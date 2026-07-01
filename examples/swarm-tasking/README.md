# Verified swarm tasking — airspace deconfliction via envelope algebra

`python run_demo.py` — no physics, no LLM, no network. Pure envelope algebra.

openDaisugi already proves **containment**: `envelope_subsumes(outer, inner)` shows a
delegated scope fits inside a parent's authority, fail-closed on undeclared
capabilities. Swarms need the other half — **disjointness**: a proof that no two
robots were handed overlapping airspace. `opendaisugi.swarm` adds that, so a
coordinator can hand each robot a *tasking envelope* and get a **deconfliction
certificate**:

```
every drone's envelope ⊆ the coordinator's envelope      (subsumption — delegation)
AND every pair of drones' volumes are disjoint            (disjointness — deconfliction)
⟹ the swarm tasking is verified: no drone can leave its lane, no two lanes overlap
```

The demo leads with the **rejections** — the value is what the verifier refuses:

| Case | Result |
|---|---|
| in-sector flight plan | `verify()` **accepts** |
| waypoint crossing into a neighbor's sector | `verify()` **rejects** (names the waypoint) |
| a drone tasked beyond the coordinator's volume | `verify_swarm_tasking` **rejects** (fail-closed subsumption) |
| two sectors that overlap | **rejects** (returns the shared region) |
| a drone that declares no bounds | **rejects** (undeclared = can't prove disjoint) |
| comms-loss reassignment that would overlap a survivor | **rejects** *before* any drone moves |

It also shows **swarm-of-swarms**: delegation is *vertical* (nested `envelope_subsumes`,
coordinator → sub-coordinator → drone) and deconfliction is *horizontal* (sibling
disjointness) — composed into one hierarchy.

## Why this is the differentiator (and where it is NOT)

Swarm **task allocation** (CBBA), **path finding** (CBS/ECBS), **reactive avoidance**
(ORCA/RVO), **certified geofencing** (NASA PolyCARP), **detect-and-avoid**
(NASA DAIDALUS), and **pre-flight operational deconfliction** (ASTM F3548-21 /
InterUSS, NASA UTM) are all more mature — and, for NASA, formally certified — than
anything here. openDaisugi does **not** out-verify them. Its distinct niche is the
**composition, not the geometry**: the tasking spec is a first-class, machine-checkable
object with a subsumption/delegation algebra *and* it gates **LLM-authored** plans
before execution (the v0.32 Orchestrator produces the plan; this verifies it). It is a
complementary plan-/spec-level assurance layer.

## What the certificate PROVES

- Each drone's declared **waypoints** stay inside its assigned volume (spec conformance).
- Each delegation is **provably contained** in its parent's authority, **fail-closed** on
  any undeclared capability.
- Sibling sectors are **provably non-overlapping** (interior-disjoint, plus an optional
  separation `margin`), recursively across a swarm-of-swarms hierarchy.
- The whole tasking structure is one **authored, declarative, machine-checkable
  artifact** — subsumption + disjointness + plan-gating in a single spec.

## What it does NOT prove (honest scope)

- **Not the flown path.** Waypoint-in-box ≠ path-in-box: inertia/overshoot/wind can
  carry a vehicle out of its box between waypoints. This is plan/sim-level, not a
  flight-safety certificate.
- **Not spacetime.** `workspace_bounds` is a 3D AABB with no time axis; real UTM
  (ASTM F3548-21) deconflicts 4D volumes and may share a volume across time. This
  partitions **space, not spacetime** — strictly cruder.
- **Not collision freedom.** Disjoint boxes + in-box waypoints still permit a collision
  (paths leave boxes). Set `margin ≥ vehicle-radius + position-uncertainty`; even then
  it bounds the *assigned volumes*, not the trajectories.
- **Analytic geometry, not a solver.** The disjointness/containment checks are decidable
  interval arithmetic (a separating-axis test) — fast and deterministic, not
  "Z3-backed". Own it as a feature.
- Not a substitute for tactical avoidance (DAIDALUS/ORCA) or certified geofencing
  (PolyCARP).

## Sources

- MuJoCo Menagerie drones (Skydio X2, Crazyflie 2): https://github.com/google-deepmind/mujoco_menagerie
- MuJoCo-Drones-Gym (GPU multi-drone, PID/SE(3)): https://arxiv.org/pdf/2606.08039
- CBBA (consensus task allocation): https://acl.mit.edu/projects/consensus-based-bundle-algorithm
- ORCA / reciprocal velocity obstacles: https://github.com/PathPlanning/ORCA-algorithm
- NASA PolyCARP (verified geofencing) / DAIDALUS / ICAROUS: https://shemesh.larc.nasa.gov/fm/ICAROUS/
- ASTM F3548-21 (UTM strategic deconfliction): https://store.astm.org/f3548-21.html
- InterUSS strategic deconfliction: https://interussplatform.org/strategic-conflict-deflection/

**Optional physics stretch (not in this demo):** replay the *accepted* waypoint plans on
MuJoCo-Drones-Gym `FormationAviary` (built-in controller) and check box containment of
the *flown* trajectory — expected result: some flown paths transiently exit the box
`verify()` approved, a live demonstration of the "not the flown path" caveat above.
