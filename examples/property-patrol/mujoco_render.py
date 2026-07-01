"""Render the property-patrol scenario in MuJoCo → an animated GIF for the README.

    pip install mujoco imageio pillow      # not core deps; render-only
    MUJOCO_GL=egl python mujoco_render.py   # headless CPU rendering (mesa/llvmpipe)

This is the SAME openDaisugi verification as run_demo.py — a mock VLA proposes each
drone's waypoint, openDaisugi gates it against the drone's sector envelope + live
swarm deconfliction — but driven onto a MuJoCo scene and recorded. The physics is
kinematic (mocap bodies positioned directly); the point is to *see* the assurance:
drones patrol their sectors, an out-of-sector proposal is refused (drone snaps back,
flashes amber), and a peer-proximity proposal is held (drone freezes, flashes red).

Honest scope unchanged (docs/spec/yellow-paper.md §7): analytic geometry, not a
flight-safety certificate.
"""
# ruff: noqa: I001  — MUJOCO_GL must be set in os.environ BEFORE `import mujoco`,
# which deliberately splits the import block.
import os
os.environ.setdefault("MUJOCO_GL", "egl")

import mujoco
import numpy as np
from PIL import Image, ImageDraw, ImageFont

from opendaisugi import (
    ActionPlan, Envelope, Invariant, Permission,
    aabb_disjoint, partition_and_assign, verify,
)
from opendaisugi.models import CartesianMoveStep

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "..", "docs", "assets", "property-patrol.gif")
SECTOR_MARGIN, SAFETY_BUBBLE, R = 0.5, 3.0, 0.5
GREEN, AMBER, RED = (0.30, 0.85, 0.45), (1.0, 0.70, 0.20), (1.0, 0.32, 0.32)

XML = """
<mujoco>
  <visual>
    <global offwidth="640" offheight="380"/>
    <headlight diffuse="0.55 0.55 0.55" ambient="0.35 0.35 0.35"/>
  </visual>
  <worldbody>
    <light pos="15 6 24" dir="0 0 -1" diffuse="0.7 0.7 0.7"/>
    <geom name="ground" type="plane" size="24 12 0.1" pos="15 6 0" rgba="0.12 0.13 0.16 1"/>
    <geom name="sec_w" type="box" size="4.7 6 0.02" pos="4.9 6 0.02" rgba="0.20 0.45 0.95 0.28"/>
    <geom name="sec_m" type="box" size="4.7 6 0.02" pos="15  6 0.02" rgba="0.20 0.80 0.55 0.28"/>
    <geom name="sec_e" type="box" size="4.7 6 0.02" pos="25.1 6 0.02" rgba="0.95 0.60 0.20 0.28"/>
    <body name="d0" mocap="true" pos="4.9 6 2.4"><geom name="g0" type="sphere" size="0.7" rgba="0.35 0.6 1 1"/></body>
    <body name="d1" mocap="true" pos="15 6 2.4"><geom name="g1" type="sphere" size="0.7" rgba="0.35 0.9 0.65 1"/></body>
    <body name="d2" mocap="true" pos="25.1 6 2.4"><geom name="g2" type="sphere" size="0.7" rgba="1 0.7 0.35 1"/></body>
  </worldbody>
</mujoco>
"""

FLEET = ["drone_west", "drone_mid", "drone_east"]


def _coordinator(bounds, velocity=3.0):
    return Envelope(generated_by="patrol", task="secure property", stakes="physical",
                    permissions=Permission(workspace_bounds=bounds, velocity_limit=velocity),
                    invariants=[Invariant(type="end_effector_in_workspace", description="in sector")])


def _move(did, target):
    return ActionPlan(source=did, task="wp", steps=[CartesianMoveStep(id=f"{did}_s", target_position=target)])


def _clamp(bounds, t):
    (lx, ly, lz), (hx, hy, hz) = bounds
    return (min(max(t[0], lx + R), hx - R), min(max(t[1], ly + R), hy - R), min(max(t[2], lz + R), hz - R))


def _box(p):
    return ((p[0] - R, p[1] - R, p[2] - R), (p[0] + R, p[1] + R, p[2] + R))


def _propose(did, tick, sector):
    (lx, ly, lz), (hx, hy, hz) = sector
    cx, cy, cz = (lx + hx) / 2, (ly + hy) / 2, (lz + hz) / 2
    target = (cx, cy + (4.0 if tick % 2 == 0 else -4.0), cz)          # patrol sweep
    if did == "drone_mid" and tick == 3:
        target = (28.0, cy, cz)                                        # chase intruder → out of sector
    if did == "drone_west" and tick == 4:
        target = (hx - R, cy, cz)                                      # to shared boundary (legal)
    if did == "drone_mid" and tick == 4:
        target = (lx + R, cy, cz)                                      # to shared boundary → deconflict
    return target


def compute_trajectory():
    """Run the real openDaisugi gate; return per-tick (positions, colors, caption)."""
    coord = _coordinator(((0, 0, 0), (30, 12, 8)))
    sectors = partition_and_assign(coord, FLEET, axis=0, margin=SECTOR_MARGIN)
    pos = {}
    for did in FLEET:
        (lx, ly, lz), (hx, hy, hz) = sectors[did].permissions.workspace_bounds
        pos[did] = ((lx + hx) / 2, (ly + hy) / 2, (lz + hz) / 2)
    frames = [(dict(pos), {d: GREEN for d in FLEET}, "openDaisugi · property-security patrol")]
    for tick in range(1, 7):
        committed, colors, events = dict(pos), {}, []
        for did in FLEET:
            env = sectors[did]
            proposed = _propose(did, tick, env.permissions.workspace_bounds)
            if verify(_move(did, proposed), env).ok:
                target, colors[did] = proposed, GREEN
            else:
                target = _clamp(env.permissions.workspace_bounds, proposed)
                colors[did] = AMBER
                events.append(f"{did}: out-of-sector REFUSED -> Simplex fallback")
            peer = next((o for o in FLEET if o != did
                         and not aabb_disjoint(_box(target), _box(committed[o]), margin=SAFETY_BUBBLE)), None)
            if peer:
                target, colors[did] = pos[did], RED
                events.append(f"{did}: too close to {peer} -> HOLD")
            committed[did] = target
            pos[did] = target
        caption = f"tick {tick}   " + ("   ".join(events) if events else "all proposals verified in-sector")
        frames.append((dict(pos), colors, caption))
    return frames


def caption_frame(rgb, text):
    img = Image.fromarray(rgb)
    draw = ImageDraw.Draw(img, "RGBA")
    w, h = img.size
    draw.rectangle([0, h - 30, w, h], fill=(10, 12, 16, 210))
    try:
        font = ImageFont.load_default(size=15)
    except TypeError:
        font = ImageFont.load_default()
    draw.text((10, h - 24), text, fill=(235, 238, 245), font=font)
    return np.asarray(img)


def main():
    import imageio.v2 as imageio
    m = mujoco.MjModel.from_xml_string(XML)
    d = mujoco.MjData(m)
    gids = {i: mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_GEOM, f"g{i}") for i in range(3)}
    cam = mujoco.MjvCamera()
    cam.lookat[:], cam.distance, cam.azimuth, cam.elevation = [15, 6, 0.5], 41, 90, -52
    r = mujoco.Renderer(m, height=380, width=640)

    traj = compute_trajectory()
    SUB = 9
    out = []
    for k in range(1, len(traj)):
        (p0, _, _), (p1, colors, caption) = traj[k - 1], traj[k]
        for s in range(SUB):
            a = (s + 1) / SUB
            for i, did in enumerate(FLEET):
                x0, y0, z0 = p0[did]; x1, y1, z1 = p1[did]
                d.mocap_pos[i] = [x0 + a * (x1 - x0), y0 + a * (y1 - y0), 2.4]
                m.geom_rgba[gids[i]][:3] = colors[did]
            mujoco.mj_forward(m, d)
            r.update_scene(d, camera=cam)
            out.append(caption_frame(r.render(), caption))
        for _ in range(3):  # brief hold on each tick's end state
            out.append(out[-1])
    r.close()
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    imageio.mimsave(OUT, out, duration=90, loop=0)
    print(f"wrote {OUT}  ({len(out)} frames)")


if __name__ == "__main__":
    main()
