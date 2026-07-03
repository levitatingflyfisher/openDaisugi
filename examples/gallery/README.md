# Runtime-assurance gallery — sixteen kinds of refusal, one library

![Sixteen runtime-assurance scenarios, each a real openDaisugi rejection](../../docs/assets/gallery-grid.gif)

```
pip install mujoco imageio pillow
MUJOCO_GL=egl python make_gallery.py
```

Sixteen short MuJoCo scenarios, each showing openDaisugi **refusing a different kind of
unsafe action** — and every refusal is a *real* `verify()` / swarm check, not a
scripted animation. `make_gallery.py` **proves that first**: it independently re-runs
each scenario's core check and asserts it accepts the safe case and rejects the unsafe
one, before rendering a single frame (this caught two *faked* scenarios during
authoring — a straight line that passed just below an obstacle, and an inverted
delegation box — both fixed). It writes each clip to `docs/assets/gallery/` and tiles
them into the 4×4 grid above.

Color code: **green** accepted · **amber** out-of-bounds refused, pulled back · **red**
hard hold/refusal.

| # | Scenario | What it refuses | The check |
|---|---|---|---|
| 1 | **Keep-in workspace** | reaching outside the assigned volume | `workspace_bounds` |
| 2 | **No-fly / keep-out** | a *path* clipping a keep-out zone (trajectory-sampled) | `obstacles` |
| 3 | **Deconfliction** | two drones closing inside the safety bubble | `aabb_disjoint` |
| 4 | **Delegation** | tasking a drone beyond its granted authority | `envelope_subsumes` |
| 5 | **Formation** | a drone drifting out of its lane | per-lane `workspace_bounds` |
| 6 | **Dynamic keep-out** | entering a *moving* person's exclusion zone | moving `obstacles` |
| 7 | **Geofence / intercept** | chasing an intruder off-property | `workspace_bounds` |
| 8 | **Comms-loss reassignment** | a reassignment that overlaps a survivor | `verify_swarm_tasking` |
| 9 | **Cross-swarm** | one swarm entering another's airspace | `aabb_disjoint` |
| 10 | **Swarm-of-swarms** | a sub-unit tasking beyond its nested grant | chained `envelope_subsumes` |
| 11 | **Slalom** | a path clipping either of two keep-out boxes | multi-`obstacles` |
| 12 | **Lateral hand-off** | handing a peer a task it isn't authorized for | `envelope_subsumes` |
| 13 | **Leash / tether** | straying beyond tether of a moving anchor | anchored `workspace_bounds` |
| 14 | **Restricted airspace** | a TFR closing across the planned path | appearing `obstacle` |
| 15 | **Corridor merge** | two drones entering a shared corridor at once | `aabb_disjoint` |
| 16 | **Must-return-to-base** | a plan that omits the required return step | predicate `exists_step` invariant |

Note #16 is not geometry at all — it's the **predicate algebra** refusing a plan for a
structural reason (`exists_step(return_to_base)` unsatisfied). Same lesson every tile:
the policy is a black box; openDaisugi bounds what it's *allowed* to do, and refuses the
rest — proven before motion.

## Honest scope

Analytic geometry, plan/volume level (see [yellow paper §7](../../docs/spec/yellow-paper.md)):
waypoint-in-box ≠ path-in-box, and disjoint boxes are collision-free only with margin
≥ vehicle radius + position uncertainty. These prove *tasking/plan* safety, not flight.
The scenes are kinematic illustrations; the `verify` calls in them are real.
