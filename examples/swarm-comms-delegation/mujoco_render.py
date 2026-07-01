"""Render the comms-loss REASSIGNMENT (a delegation transfer) in MuJoCo → a GIF.

    pip install mujoco imageio pillow
    MUJOCO_GL=egl python mujoco_render.py    # headless CPU render (mesa/llvmpipe)

Delegation, visualized. A drone loses comms; a survivor's AUTHORITY is expanded to
cover the gap — but only after openDaisugi re-proves the new tasking is still
contained AND deconflicted. The unsafe alternative (hand the sector to BOTH
neighbors) is refused: the overlap flashes red, before any drone moves. The verify
calls are real (verify_swarm_tasking); the scene is a kinematic illustration.
"""
# ruff: noqa: I001  — MUJOCO_GL must be set before `import mujoco`.
import os
os.environ.setdefault("MUJOCO_GL", "egl")

import mujoco
import numpy as np
from PIL import Image, ImageDraw, ImageFont

from opendaisugi import (
    Envelope, Invariant, Permission, partition_and_assign, verify_swarm_tasking,
)

HERE = os.path.dirname(os.path.abspath(__file__))
OUT = os.path.join(HERE, "..", "..", "docs", "assets", "comms-delegation.gif")

XML = """
<mujoco>
  <visual><global offwidth="640" offheight="380"/>
    <headlight diffuse="0.55 0.55 0.55" ambient="0.35 0.35 0.35"/></visual>
  <worldbody>
    <light pos="15 6 24" dir="0 0 -1" diffuse="0.7 0.7 0.7"/>
    <geom name="ground" type="plane" size="24 12 0.1" pos="15 6 0" rgba="0.12 0.13 0.16 1"/>
    <geom name="sec_w" type="box" size="4.7 6 0.02" pos="4.9 6 0.02" rgba="0.20 0.45 0.95 0.30"/>
    <geom name="sec_m" type="box" size="4.7 6 0.02" pos="15  6 0.02" rgba="0.20 0.80 0.55 0.30"/>
    <geom name="sec_e" type="box" size="4.7 6 0.02" pos="25.1 6 0.02" rgba="0.95 0.60 0.20 0.30"/>
    <geom name="warn" type="box" size="4.7 6 0.03" pos="15 6 0.04" rgba="1 0.2 0.2 0"/>
    <body name="d0" mocap="true" pos="4.9 6 2.4"><geom name="g0" type="sphere" size="0.7" rgba="0.35 0.6 1 1"/></body>
    <body name="d1" mocap="true" pos="15 6 2.4"><geom name="g1" type="sphere" size="0.7" rgba="0.35 0.9 0.65 1"/></body>
    <body name="d2" mocap="true" pos="25.1 6 2.4"><geom name="g2" type="sphere" size="0.7" rgba="1 0.7 0.35 1"/></body>
  </worldbody>
</mujoco>
"""


def _auth(name, bounds):
    return Envelope(generated_by=name, task="op", stakes="physical",
                    permissions=Permission(workspace_bounds=bounds, velocity_limit=3.0),
                    invariants=[Invariant(type="end_effector_in_workspace", description="in volume")])


def _verify_reassignments():
    """The REAL proofs behind the animation (printed for the record)."""
    mission = _auth("mission", ((0, 0, 0), (30, 12, 8)))
    fleet = partition_and_assign(mission, ["west", "mid", "east"], axis=0, margin=0.5)
    (wlo, _), (mlo, mhi) = (fleet["west"].permissions.workspace_bounds,
                              fleet["mid"].permissions.workspace_bounds)
    west_exp = _auth("west", (wlo, mhi))
    ok = verify_swarm_tasking(mission, {"west": west_exp, "east": fleet["east"]}, margin=0.5).ok
    east_grab = _auth("east", (mlo, fleet["east"].permissions.workspace_bounds[1]))
    bad = verify_swarm_tasking(mission, {"west": west_exp, "east": east_grab}, margin=0.5).ok
    return ok, bad


def caption(rgb, text):
    img = Image.fromarray(rgb)
    dr = ImageDraw.Draw(img, "RGBA")
    w, h = img.size
    dr.rectangle([0, h - 30, w, h], fill=(10, 12, 16, 210))
    try:
        font = ImageFont.load_default(size=15)
    except TypeError:
        font = ImageFont.load_default()
    dr.text((10, h - 24), text, fill=(235, 238, 245), font=font)
    return np.asarray(img)


def main():
    import imageio.v2 as imageio
    ok, bad = _verify_reassignments()
    m = mujoco.MjModel.from_xml_string(XML)
    d = mujoco.MjData(m)
    gid = {n: mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_GEOM, n)
           for n in ("sec_w", "sec_m", "sec_e", "warn", "g0", "g1", "g2")}
    cam = mujoco.MjvCamera()
    cam.lookat[:], cam.distance, cam.azimuth, cam.elevation = [15, 6, 0.5], 41, 90, -52
    r = mujoco.Renderer(m, height=380, width=640)
    out = []

    def snap(text, n=12):
        # Gentle hover-bob so held frames stay distinct (not deduped) and look alive.
        xy = [(float(d.mocap_pos[i][0]), float(d.mocap_pos[i][1])) for i in range(3)]
        for k in range(n):
            for i in range(3):
                d.mocap_pos[i] = [xy[i][0], xy[i][1], 2.4 + 0.15 * float(np.sin(k * 0.7))]
            mujoco.mj_forward(m, d)
            r.update_scene(d, camera=cam)
            out.append(caption(r.render(), text))

    GREY = (0.4, 0.4, 0.45)
    # 1. normal ops
    snap("three drones, one sector each — nominal patrol")
    # 2. mid loses comms
    m.geom_rgba[gid["g1"]][:3] = GREY
    snap("drone_mid: COMMS LOST — its sector is now uncovered")
    # 3. reassign to west: expand west's tile over mid's area; west drone slides in
    for a in np.linspace(0, 1, 9):
        m.geom_size[gid["sec_w"]][0] = 4.7 + a * 5.1          # grow half-width in x
        m.geom_pos[gid["sec_w"]][0] = 4.9 + a * 5.0           # recenter
        m.geom_rgba[gid["sec_m"]][3] = 0.30 * (1 - a) + 0.06 * a
        d.mocap_pos[0] = [4.9 + a * 5.0, 6, 2.4]
        mujoco.mj_forward(m, d)
        r.update_scene(d, camera=cam)
        out.append(caption(r.render(),
                           f"reassign mid's sector -> WEST; verify_swarm_tasking: "
                           f"{'ACCEPTED (subsumed + disjoint)' if ok else '...'}"))
    snap("west now covers the gap — re-proved contained AND deconflicted", n=8)
    # 4. the rejected alternative: hand mid's sector to BOTH → overlap flashes red
    for a in list(np.linspace(0, 0.7, 5)) + list(np.linspace(0.7, 0, 5)):
        m.geom_rgba[gid["warn"]][3] = a
        mujoco.mj_forward(m, d)
        r.update_scene(d, camera=cam)
        out.append(caption(r.render(),
                           f"...but hand it to BOTH neighbors? overlap "
                           f"{'REJECTED before any drone moves' if not bad else '...'}"))
    m.geom_rgba[gid["warn"]][3] = 0
    snap("delegation transfer is a machine-checked containment proof, fail-closed", n=10)

    r.close()
    os.makedirs(os.path.dirname(OUT), exist_ok=True)
    imageio.mimsave(OUT, out, duration=95, loop=0)
    print(f"wrote {OUT}  ({len(out)} frames)  [reassign ok={ok}, both-grab rejected={not bad}]")


if __name__ == "__main__":
    main()
