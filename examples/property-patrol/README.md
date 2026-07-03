# Property-security patrol — runtime assurance for a VLA swarm, in motion

```
python run_demo.py
```

No physics engine, no GPU, no network, no real model — pure envelope algebra plus a
deterministic **mock "VLA"** policy, so it runs anywhere (CPU, zero extra deps).

## What it shows

The [swarm-tasking](../swarm-tasking/) example proves the *static* story (sectors are
disjoint, delegations are contained). This one puts it **in motion** — the
[Simplex](../../docs/whitepaper.md#3-the-lineage-runtime-assurance) loop running live:

```
each tick, per drone:
  black-box policy PROPOSES a waypoint          (here: a scripted stand-in for pi0 / SmolVLA)
    → verify(move ⊆ drone's sector envelope)    (Stage: workspace bounds via end_effector_in_workspace)
        ok      → drone advances
        reject  → Simplex fallback to a verified-safe baseline (clamp back into sector)
    → live swarm deconfliction (safety bubble)   (aabb_disjoint between drones)
        too close → HOLD the later drone
```

Two guarantees hold **whatever the policy proposes**, proven each tick *before* motion:
a drone can never leave its sector, and no two drones close within the live safety
bubble. The run prints the refusals as they happen and asserts the invariant at the end.

Sample: a drone "chases an intruder" out of its sector → **refused**, falls back
in-sector; two neighbors both patrol legally to their shared boundary → the live
**safety bubble** (stricter than the static sector gap) **holds** the second one.

## Why this is the point

The policy is a **black box** and it is *not* the star — the assurance layer is.
Swap the `MockVLA` for a real [SmolVLA](https://huggingface.co/blog/smolvla) (~450M,
CPU-feasible) at `.propose()` and the guarantees are **identical**. That is the whole
thesis: openDaisugi bounds the black box; the black box is interchangeable.

## Honest scope

This is **analytic geometry, not a flight-safety certificate** (see
[yellow paper §7](../../docs/spec/yellow-paper.md)): waypoint-in-box ≠ path-in-box,
and disjoint boxes are collision-free only if the margin ≥ vehicle radius + position
uncertainty. It is plan/volume-level assurance, sim-free.

**Next step (documented, not built):** a physics upgrade — MuJoCo on CPU (the right
substrate for a 2–4 agent scene; MJX/GPU only pays off at thousands of parallel
scenes) — with a real async SmolVLA driving `.propose()`. The verification wiring
here does not change; only the policy and the physics do.
