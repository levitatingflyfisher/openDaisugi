"""Shared render harness for the openDaisugi runtime-assurance gallery.

Every scenario builds a small top-down MuJoCo scene (zones + agents), computes a
trajectory that is gated by a REAL opendaisugi.verify / swarm check, and returns
color-coded, captioned frames. `make_gallery.py` renders each to a GIF and tiles the
best into a grid. Headless CPU rendering (MUJOCO_GL=egl, mesa/llvmpipe).
"""
# ruff: noqa: I001  — MUJOCO_GL must be set before `import mujoco`.
import os
os.environ.setdefault("MUJOCO_GL", "egl")

import mujoco
import numpy as np
from PIL import Image, ImageDraw, ImageFont

W, H = 340, 240                      # per-tile render size
GREEN = (0.32, 0.85, 0.46)
AMBER = (1.0, 0.72, 0.20)
RED = (1.0, 0.34, 0.34)
GREY = (0.42, 0.42, 0.48)
BLUE = (0.36, 0.60, 1.0)
N_ZONES, N_AGENTS = 6, 4

# A fixed template: 6 recolorable/resizable zone tiles + 4 mocap agent spheres.
# Scenarios reveal/place what they need; the rest stay invisible (alpha 0 / parked).
_ZONES = "".join(
    f'<geom name="z{i}" type="box" size="1 1 0.02" pos="0 0 0.02" rgba="0.3 0.3 0.3 0"/>'
    for i in range(N_ZONES))
_AGENTS = "".join(
    f'<body name="a{i}" mocap="true" pos="{-20 - i} 0 2">'
    f'<geom name="g{i}" type="sphere" size="0.62" rgba="0.4 0.8 0.5 0"/></body>'
    for i in range(N_AGENTS))
XML = f"""
<mujoco>
  <visual><global offwidth="{W}" offheight="{H}"/>
    <headlight diffuse="0.6 0.6 0.6" ambient="0.4 0.4 0.4"/></visual>
  <worldbody>
    <light pos="15 6 24" dir="0 0 -1" diffuse="0.7 0.7 0.7"/>
    <geom name="ground" type="plane" size="26 14 0.1" pos="15 6 0" rgba="0.11 0.12 0.15 1"/>
    {_ZONES}{_AGENTS}
  </worldbody>
</mujoco>
"""


def _font(sz):
    try:
        return ImageFont.load_default(size=sz)
    except TypeError:
        return ImageFont.load_default()


class Stage:
    """Wraps the scene: place zones once, then push per-frame agent state."""

    def __init__(self):
        self.m = mujoco.MjModel.from_xml_string(XML)
        self.d = mujoco.MjData(self.m)
        self.zid = [mujoco.mj_name2id(self.m, mujoco.mjtObj.mjOBJ_GEOM, f"z{i}") for i in range(N_ZONES)]
        self.gid = [mujoco.mj_name2id(self.m, mujoco.mjtObj.mjOBJ_GEOM, f"g{i}") for i in range(N_AGENTS)]
        self.cam = mujoco.MjvCamera()
        self.cam.lookat[:], self.cam.distance, self.cam.azimuth, self.cam.elevation = [15, 6, 0.4], 40, 90, -64
        self.r = mujoco.Renderer(self.m, height=H, width=W)
        self.frames = []

    def zone(self, i, cx, cy, sx, sy, rgba):
        z = self.zid[i]
        self.m.geom_size[z][:2] = [sx, sy]
        self.m.geom_pos[z][:2] = [cx, cy]
        self.m.geom_rgba[z] = rgba

    def push(self, agents, title, sub, hold=1):
        """agents: list of (x, y, color) or None to hide. Renders `hold` frames."""
        for i in range(N_AGENTS):
            if i < len(agents) and agents[i] is not None:
                x, y, c = agents[i]
                self.d.mocap_pos[i] = [x, y, 2.0]
                self.m.geom_rgba[self.gid[i]][:3] = c
                self.m.geom_rgba[self.gid[i]][3] = 1.0
            else:
                self.m.geom_rgba[self.gid[i]][3] = 0.0
        mujoco.mj_forward(self.m, self.d)
        self.r.update_scene(self.d, camera=self.cam)
        rgb = self.r.render()
        for _ in range(hold):
            self.frames.append(self._caption(rgb.copy(), title, sub))

    def _caption(self, rgb, title, sub):
        img = Image.fromarray(rgb)
        dr = ImageDraw.Draw(img, "RGBA")
        dr.rectangle([0, 0, W, 22], fill=(10, 12, 16, 205))
        dr.text((7, 4), title, fill=(150, 210, 255), font=_font(13))
        dr.rectangle([0, H - 20, W, H], fill=(10, 12, 16, 205))
        dr.text((7, H - 16), sub, fill=(232, 236, 244), font=_font(12))
        return np.asarray(img)

    def close(self):
        self.r.close()
        return self.frames


def lerp(p0, p1, a):
    return (p0[0] + a * (p1[0] - p0[0]), p0[1] + a * (p1[1] - p0[1]))


def play(stage, title, keys, K=6, hold_last=3):
    """keys: list of (agents, subcaption). Interpolate agent positions between
    consecutive keys; use the destination key's colors + caption during the move."""
    stage.push(keys[0][0], title, keys[0][1])
    for j in range(1, len(keys)):
        a0, a1, sub = keys[j - 1][0], keys[j][0], keys[j][1]
        for s in range(K):
            t = (s + 1) / K
            agents = []
            for i in range(len(a1)):
                if a1[i] is None or i >= len(a0) or a0[i] is None:
                    agents.append(a1[i])
                else:
                    (x0, y0, _), (x1, y1, c1) = a0[i], a1[i]
                    agents.append((x0 + t * (x1 - x0), y0 + t * (y1 - y0), c1))
            stage.push(agents, title, sub)
    stage.push(keys[-1][0], title, keys[-1][1], hold=hold_last)
