"""Render the runtime-assurance gallery + a tiled grid GIF.

    pip install mujoco imageio pillow
    MUJOCO_GL=egl python make_gallery.py

First PROVES every scenario is backed by a real opendaisugi rejection (accept the
safe case, refuse the unsafe one), then renders each to docs/assets/gallery/<name>.gif
and tiles them into docs/assets/gallery-grid.gif.
"""
# ruff: noqa: I001
import os
os.environ.setdefault("MUJOCO_GL", "egl")

import numpy as np
from PIL import Image

from opendaisugi import (
    Envelope, Invariant, Permission, ActionPlan,
    aabb_disjoint, envelope_subsumes, partition_and_assign, verify, verify_swarm_tasking,
)
from opendaisugi.models import CartesianMoveStep
import scenarios as S

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "..", "docs", "assets")


def _mv(t):
    return ActionPlan(source="t", task="x", steps=[CartesianMoveStep(id="s", target_position=(t[0], t[1], 4.0))])


def _path(wps):
    return ActionPlan(source="t", task="x", steps=[
        CartesianMoveStep(id=f"s{i}", target_position=(x, y, 4.0), depends_on=([f"s{i-1}"] if i else []))
        for i, (x, y) in enumerate(wps)])


def check_gates():
    """Independently re-run each scenario's core check → (accept_safe, reject_unsafe)."""
    box = ((0, 0, 0), (30, 12, 8))
    ws = Envelope(generated_by="t", task="x", stakes="physical",
                  permissions=Permission(workspace_bounds=((6, 3, 0), (16, 9, 8))),
                  invariants=[Invariant(type="end_effector_in_workspace", description="in")])
    ob = Envelope(generated_by="t", task="x", stakes="physical",
                  permissions=Permission(workspace_bounds=box, obstacles=[((13, 4, 0), (17, 8, 8))]),
                  invariants=[Invariant(type="end_effector_in_workspace", description="in"),
                              Invariant(type="no_obstacle_penetration", description="avoid")])
    mission = Envelope(generated_by="t", task="x", stakes="physical",
                       permissions=Permission(workspace_bounds=box),
                       invariants=[Invariant(type="end_effector_in_workspace", description="in")])
    fleet = partition_and_assign(mission, ["w", "m", "e"], axis=0, margin=0.5)
    grant = fleet["w"]

    def sub(env2):
        return envelope_subsumes(grant, env2).holds

    def E(b):
        return Envelope(generated_by="t", task="x", stakes="physical",
                        permissions=Permission(workspace_bounds=b),
                        invariants=[Invariant(type="end_effector_in_workspace", description="in")])
    (glo), (ghi) = grant.permissions.workspace_bounds

    def R(p):
        return ((p[0] - 0.6, p[1] - 0.6, 0), (p[0] + 0.6, p[1] + 0.6, 8))
    checks = {
        "keep_in (workspace)":    (verify(_mv((11, 6)), ws).ok, not verify(_mv((24, 6)), ws).ok),
        "no_fly (obstacle)":      (verify(_path([(4, 6), (15, 10.5), (25, 6)]), ob).ok,
                                   not verify(_path([(4, 6), (24, 6)]), ob).ok),
        "deconflict (aabb)":      (aabb_disjoint(R((6, 6)), R((24, 6)), margin=3.5),
                                   not aabb_disjoint(R((13.5, 6)), R((16.5, 6)), margin=3.5)),
        "delegation (subsumes)":  (sub(E(((glo[0] + 1, 3, 0), (ghi[0] - 1, 9, 8)))),
                                   not sub(E(((glo[0] + 1, 3, 0), (ghi[0] + 12, 9, 8))))),
        "formation (per-lane)":   (verify(_mv((10, 6)), E(((7.5, 2, 0), (14, 10, 8)))).ok,
                                   not verify(_mv((16, 6)), E(((7.5, 2, 0), (14, 10, 8)))).ok),
        "human_keepout (dyn obs)":(verify(_mv((10, 5)), E(box)).ok,
                                   not verify(_mv((16, 6)), Envelope(generated_by="t", task="x", stakes="physical",
                                        permissions=Permission(workspace_bounds=box, obstacles=[((14, 4, 0), (18, 8, 8))]),
                                        invariants=[Invariant(type="end_effector_in_workspace", description="in"),
                                                    Invariant(type="no_obstacle_penetration", description="avoid")])).ok),
        "intercept (geofence)":   (verify(_mv((20, 6)), E(((0, 0, 0), (28, 12, 8)))).ok,
                                   not verify(_mv((31, 6)), E(((0, 0, 0), (28, 12, 8)))).ok),
        "reassignment (swarm)":   (verify_swarm_tasking(mission, {"w": E((grant.permissions.workspace_bounds[0], fleet["m"].permissions.workspace_bounds[1])), "e": fleet["e"]}, margin=0.5).ok,
                                   not verify_swarm_tasking(mission, {"w": E((grant.permissions.workspace_bounds[0], fleet["m"].permissions.workspace_bounds[1])), "e": E((fleet["m"].permissions.workspace_bounds[0], fleet["e"].permissions.workspace_bounds[1]))}, margin=0.5).ok),
        "cross_swarm (aabb)":     (aabb_disjoint(((0, 0, 0), (15, 12, 8)), ((16, 0, 0), (30, 12, 8)), margin=0.5),
                                   not aabb_disjoint(((0, 0, 0), (15, 12, 8)), ((11, 0, 0), (30, 12, 8)), margin=0.5)),
    }
    # ── the seven 4x4 additions ──
    squads = partition_and_assign(mission, ["al", "br"], axis=0, margin=0.5)
    adr = partition_and_assign(squads["al"], ["a1", "a2"], axis=1, margin=0.4)
    (salx, _, _), _ = squads["al"].permissions.workspace_bounds
    (sblx, _, _), _ = squads["br"].permissions.workspace_bounds
    b_env = E(((12, 0, 0), (24, 12, 8)))
    slalom_env = Envelope(generated_by="t", task="x", stakes="physical",
        permissions=Permission(workspace_bounds=box, obstacles=[((8, 4, 0), (11, 8, 8)), ((19, 4, 0), (22, 8, 8))]),
        invariants=[Invariant(type="end_effector_in_workspace", description="in"),
                    Invariant(type="no_obstacle_penetration", description="a")])
    rtb_env = Envelope(generated_by="t", task="x", stakes="physical",
        permissions=Permission(workspace_bounds=box, custom_step_allowlist=["return_to_base"]),
        invariants=[Invariant(type="end_effector_in_workspace", description="in"),
                    Invariant(type="must_return_to_base", description="end at base", enforce=True,
                              expr={"op": "exists_step", "pred": {"op": "equals", "path": "type", "value": "return_to_base"}})])
    patrol = [CartesianMoveStep(id="a", target_position=(6, 6, 4))]
    checks.update({
        "swarm_of_swarms (nested)":(envelope_subsumes(mission, adr["a1"]).holds,
                                    not envelope_subsumes(squads["br"], E(((salx, 3, 0), (sblx + 4, 9, 8)))).holds),
        "slalom (multi-obstacle)": (verify(_path([(3, 6), (6, 11), (24, 11), (27, 6)]), slalom_env).ok,
                                    not verify(_path([(3, 6), (27, 6)]), slalom_env).ok),
        "handoff (lateral)":       (envelope_subsumes(b_env, E(((13, 4, 0), (23, 9, 8)))).holds,
                                    not envelope_subsumes(b_env, E(((13, 4, 0), (30, 9, 8)))).holds),
        "leash (tether)":          (verify(_mv((10, 6)), E(((4, 2, 0), (12, 10, 8)))).ok,
                                    not verify(_mv((17, 6)), E(((4, 2, 0), (12, 10, 8)))).ok),
        "restricted (TFR)":        (verify(_path([(4, 6), (15, 10.5), (25, 6)]), ob).ok,
                                    not verify(_path([(9, 6), (24, 6)]), ob).ok),
        "corridor (merge)":        (aabb_disjoint(R((4, 6)), R((26, 6)), margin=3.0),
                                    not aabb_disjoint(R((14, 6)), R((16, 6)), margin=3.0)),
        "return_to_base (invar)":  (verify(ActionPlan(source="t", task="x", steps=[*patrol, S._ReturnToBase(id="c", depends_on=["a"])]), rtb_env).ok,
                                    not verify(ActionPlan(source="t", task="x", steps=patrol), rtb_env).ok),
    })
    print("── gate verification (accept safe / reject unsafe) ──")
    allgood = True
    for name, (a, b) in checks.items():
        ok = a and b
        allgood &= ok
        print(f"  {name:26} accept={a!s:5} reject={b!s:5}  {'REAL ✓' if ok else '✗ FAKE'}")
    assert allgood, "a scenario is not backed by a real rejection"
    print("  all scenarios backed by real opendaisugi rejections ✓\n")


def resample(frames, n):
    return [frames[min(len(frames) - 1, int(k / n * len(frames)))] for k in range(n)]


def main():
    import imageio.v2 as imageio
    check_gates()
    os.makedirs(os.path.join(OUT, "gallery"), exist_ok=True)
    order = ["keep_in", "no_fly", "deconflict", "delegation",
             "formation", "human_keepout", "intercept", "reassignment",
             "cross_swarm", "swarm_of_swarms", "slalom", "handoff",
             "leash", "restricted", "corridor", "return_to_base"]
    clips = {}
    for name in order:
        frames = S.ALL[name]()
        clips[name] = frames
        imageio.mimsave(os.path.join(OUT, "gallery", f"{name}.gif"), frames, duration=110, loop=0)
        print(f"  rendered {name:16} ({len(frames)} frames)")

    # tile into a 4x4 grid, all clips resampled to a common length
    N, L = 4, 24
    tw, th = 182, 128
    grid = []
    res = {k: resample(v, L) for k, v in clips.items()}
    for k in range(L):
        canvas = Image.new("RGB", (tw * N, th * N), (8, 9, 12))
        for idx, name in enumerate(order):
            tile = Image.fromarray(res[name][k]).resize((tw, th))
            canvas.paste(tile, ((idx % N) * tw, (idx // N) * th))
        grid.append(np.asarray(canvas))
    imageio.mimsave(os.path.join(OUT, "gallery-grid.gif"), grid, duration=125, loop=0)
    print(f"\n  wrote docs/assets/gallery-grid.gif  ({tw*N}x{th*N}, {L} frames)")


if __name__ == "__main__":
    main()
