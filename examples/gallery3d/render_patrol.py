"""Scene 2 picture: a quadrotor patrols a property, drawn from patrol.with_warden().

This script draws; it decides nothing. ``patrol.prove()`` runs first, before any
renderer exists. After that every frame is what ``patrol.without_warden()`` and
``patrol.with_warden()`` returned: where the drone and the person are, whether the
drone is inside the no-fly box, and the fan of plans with the verifier's answer on each.
A plan is coloured and faded from its ``Judged.ok``, never from its name.

The same flight is shown twice, one after the other: take A without the warden (the
plan is flown as it is), take B with the warden (the plan is refused, the fan is
judged, the first accepted plan is flown).

Who flies: a scripted planner. The person's position comes from the simulator.
The quadrotor is no real product: a body, four arms and four rotors, made here from
plain shapes. No third-party model or file is loaded.

Run:  MUJOCO_GL=egl python examples/gallery3d/render_patrol.py [--out DIR]
"""

# ruff: noqa: I001  -- MUJOCO_GL must be set before `import mujoco`.
from __future__ import annotations

import argparse
import math
import os
from dataclasses import dataclass
from pathlib import Path

os.environ.setdefault("MUJOCO_GL", "egl")

import mujoco
import numpy as np
from PIL import Image, ImageDraw

import patrol

# Scene 1's renderer is the house style: same picture size, fonts, pill labels,
# colours, plan style, WebP writer. They are imported, not copied.
from render_delivery import (  # noqa: E402
    AMBER, FADE_FRAMES, FRAME_MS, H, LAST_HOLD_MS, RED, SS, W,
    WEBP_LIMIT, _paste_on_page, _pill, font, plan_style, world_to_pixel,
)  # fmt: skip

DEFAULT_OUT = Path(__file__).resolve().parent / "out"

# The quadrotor, in metres at its true size: a generic machine of plain shapes and no real
# product. The rotor hubs sit at (+-ARM, +-ARM); the farthest point of the machine is a
# rotor's rim, and it lies inside the radius the checks use (patrol.DRONE_RADIUS).
ARM = 0.25
ROTOR_RADIUS = 0.14
HALF_SPAN = math.hypot(ARM, ARM) + ROTOR_RADIUS  # 0.494 m

# The drone is drawn larger than life so it can be seen in a 40 m scene. The checks use the
# true radius; the drawing does not change what is proven. How large is bounded by the
# data: the smallest clearance of the guarded drone from the no-fly box and the person's
# box over all frames of with_warden() is 4.0 m. The drawn half-span must be at most three
# quarters of that clearance, 3.0 m, so the drawn body never reaches a box and a gap of a
# metre shows: DRAW_SCALE <= 3.0 / 0.494 = 6.08. 6 gives 2.96 m. test_render_patrol
# measures the half-span from the compiled model and the clearance from the data.
DRAW_SCALE = 6.0
TILT_DEG = 12.0  # nose-down lean while the drone is moving

# --- the words in the picture -----------------------------------------------
TAKE_NAME = {"A": "without the warden", "B": "with the warden"}
TAKE_COLOR = {"A": (178, 44, 44), "B": (30, 132, 74)}
LABEL_NO_FLY = "no-fly box"
LABEL_PERSON = f"keeps {patrol.PERSON_KEEP_AWAY:.0f} m away: proven"
LABEL_PERSON_UNCHECKED = f"keep-out: {patrol.PERSON_KEEP_AWAY:.0f} m around the person"


def person_label(take: str) -> str:
    """The words on the person's box. Only the take with the warden proves
    anything, so only that take says proven."""
    return LABEL_PERSON if take == "B" else LABEL_PERSON_UNCHECKED


FOOTER = f"pilot: {patrol.DRIVER} · the person's position comes from the simulator"
STATUS = {
    "unchecked": "the plan is flown as it is",
    "breach": "inside the no-fly box",
    "refused": "plan refused",
    "accepted": "accepted",
    "hold": "holding: the rest of the route is refused",
}
REPLAN = "the rest of the route"  # the name patrol.with_warden() gives to each re-check

# --- the camera: one fixed steep oblique view from the south, a little west ----
CAM_LOOK = (20.0, 15.0, 3.0)
CAM_ELEV_DEG = 58.0  # steep enough that the route north of the house is seen beyond its box
CAM_AZIM_DEG = 248.0  # south, turned 22 degrees to the west of the look point
CAM_DIST = 72.0  # chosen so the fence of air fills the picture without leaving it
_HORIZ = CAM_DIST * math.cos(math.radians(CAM_ELEV_DEG))
CAM_POS = (
    CAM_LOOK[0] + _HORIZ * math.cos(math.radians(CAM_AZIM_DEG)),
    CAM_LOOK[1] + _HORIZ * math.sin(math.radians(CAM_AZIM_DEG)),
    CAM_LOOK[2] + CAM_DIST * math.sin(math.radians(CAM_ELEV_DEG)),
)
CAM_FOVY = 40.0
EXTENT = 80.0

LINE_FENCE = (235, 240, 250)


DRONE_BODY = "0.13 0.14 0.17 1"
DRONE_ARM = "0.24 0.25 0.29 1"
DRONE_NOSE = "0.96 0.55 0.10 1"  # an orange nose, so the direction of flight shows
DRONE_ROTOR = "0.97 0.98 1.0 0.82"  # a spinning rotor reads as a pale disc


def _drone(scale: float = DRAW_SCALE) -> str:
    """The quadrotor as one moved body of plain shapes: a body, a nose, four arms, four
    motors and four rotor discs, two skids. The nose points along +x."""

    def n(*values: float) -> str:
        return " ".join(f"{v * scale:.4f}" for v in values)

    g = ['<body name="drone" mocap="true" pos="0 0 0">']
    g.append(f'<geom name="drone_body" type="ellipsoid" size="{n(0.17, 0.09, 0.05)}" '
             f'rgba="{DRONE_BODY}"/>')  # fmt: skip
    g.append(f'<geom name="drone_nose" type="box" size="{n(0.03, 0.055, 0.03)}" '
             f'pos="{n(0.16, 0, -0.005)}" rgba="{DRONE_NOSE}"/>')  # fmt: skip
    for i, (sx, sy) in enumerate(((1, 1), (1, -1), (-1, 1), (-1, -1))):
        x, y = sx * ARM, sy * ARM
        g.append(f'<geom name="drone_arm{i}" type="capsule" size="{n(0.02)}" '
                 f'fromto="0 0 0 {n(x, y, 0)}" rgba="{DRONE_ARM}"/>')  # fmt: skip
        g.append(f'<geom name="drone_motor{i}" type="cylinder" size="{n(0.035, 0.03)}" '
                 f'pos="{n(x, y, 0.01)}" rgba="{DRONE_BODY}"/>')  # fmt: skip
        g.append(f'<geom name="drone_rotor{i}" type="cylinder" size="{n(ROTOR_RADIUS, 0.006)}" '
                 f'pos="{n(x, y, 0.046)}" rgba="{DRONE_ROTOR}"/>')  # fmt: skip
    for i, y in enumerate((0.08, -0.08)):
        ends = f"{n(-0.13, y, -0.085)} {n(0.13, y, -0.085)}"
        g.append(f'<geom name="drone_skid{i}" type="capsule" size="{n(0.012)}" '
                 f'fromto="{ends}" rgba="{DRONE_ARM}"/>')  # fmt: skip
    g.append("</body>")
    return "\n".join(g)


# --- the scene (hand-built MJCF) ----------------------------------------------


def _camera_axes() -> str:
    """xyaxes of the camera: its x to the right of the picture, its y up."""
    pos, look = np.array(CAM_POS), np.array(CAM_LOOK)
    fwd = look - pos
    fwd /= np.linalg.norm(fwd)
    right = np.cross(fwd, [0.0, 0.0, 1.0])
    right /= np.linalg.norm(right)
    up = np.cross(right, fwd)
    return " ".join(f"{v:.5f}" for v in (*right, *up))


def _geom(kind: str, size: str, pos: str, rgba: str, extra: str = "") -> str:
    return f'<geom type="{kind}" size="{size}" pos="{pos}" rgba="{rgba}" {extra}/>'


def _box(lo, hi, rgba: str, extra: str = "") -> str:
    """A box geom from its two corners."""
    c = " ".join(f"{(a + b) / 2:.4f}" for a, b in zip(lo, hi, strict=True))
    s = " ".join(f"{(b - a) / 2:.4f}" for a, b in zip(lo, hi, strict=True))
    return _geom("box", s, c, rgba, extra)


def _ground() -> str:
    """Lawn over the property, a darker surround, a path along the south side."""
    (x0, y0, _), (x1, y1, _) = patrol.FENCE
    g = [
        _box((-300, -300, -0.4), (340, 330, -0.03), "0.22 0.30 0.20 1"),  # surround
        _box((x0, y0, -0.4), (x1, y1, 0.0), "0.42 0.63 0.32 1"),  # lawn
        _box((x0, patrol.PERSON_Y - 1.0, 0.0), (x1, patrol.PERSON_Y + 1.0, 0.03),
             "0.72 0.69 0.62 1"),  # the path the person walks
    ]  # fmt: skip
    # A low fence on the property's edge: posts every 4 m and two rails.
    post = 0.12
    for x in np.arange(x0, x1 + 0.01, 4.0):
        for y in (y0, y1):
            g.append(_box((x - post, y - post, 0), (x + post, y + post, 1.3), "0.93 0.93 0.90 1"))
    for y in np.arange(y0 + 4.0, y1 - 0.01, 4.0):
        for x in (x0, x1):
            g.append(_box((x - post, y - post, 0), (x + post, y + post, 1.3), "0.93 0.93 0.90 1"))
    r = 0.05
    for z in (0.5, 1.0):
        g.append(_box((x0, y0 - r, z - r), (x1, y0 + r, z + r), "0.93 0.93 0.90 1"))
        g.append(_box((x0, y1 - r, z - r), (x1, y1 + r, z + r), "0.93 0.93 0.90 1"))
        g.append(_box((x0 - r, y0, z - r), (x0 + r, y1, z + r), "0.93 0.93 0.90 1"))
        g.append(_box((x1 - r, y0, z - r), (x1 + r, y1, z + r), "0.93 0.93 0.90 1"))
    return "\n".join(g)


def _house() -> str:
    """Walls and gables as one convex prism, a pitched roof of two slabs, a door and two
    windows (cheap coloured boxes, so it reads as a house at small size)."""
    x0, y0, x1, y1 = patrol.HOUSE
    ym = (y0 + y1) / 2
    eave, ridge = 4.4, patrol.HOUSE_HEIGHT - 0.15  # the roof slabs add 0.15 on top
    run = ym - y0 + 0.5  # half the depth, plus an overhang
    rise = ridge - eave
    ang = math.degrees(math.atan2(rise, ym - y0))
    half = math.hypot(run, rise * run / (ym - y0)) / 2
    mid_z = (eave + ridge) / 2 - 0.1
    xh = (x1 - x0) / 2 + 0.5
    cx = (x0 + x1) / 2
    south = _geom("box", f"{xh} {half} 0.15", f"{cx} {y0 + (ym - y0) / 2 - 0.1} {mid_z}",
                  "0.52 0.22 0.18 1", f'euler="{ang:.3f} 0 0"')  # fmt: skip
    north = _geom("box", f"{xh} {half} 0.15", f"{cx} {ym + (ym - y0) / 2 + 0.1} {mid_z}",
                  "0.52 0.22 0.18 1", f'euler="{-ang:.3f} 0 0"')  # fmt: skip
    return "\n".join(
        [
            '<geom type="mesh" mesh="house" rgba="0.93 0.88 0.76 1"/>',
            south,
            north,
            _geom("box", "0.6 0.06 1.0", f"{cx} {y0 - 0.03} 1.0", "0.40 0.24 0.12 1"),  # door
            _geom("box", "0.9 0.06 0.6", f"{x0 + 2.2} {y0 - 0.03} 2.4", "0.55 0.75 0.92 1"),
            _geom("box", "0.9 0.06 0.6", f"{x1 - 2.2} {y0 - 0.03} 2.4", "0.55 0.75 0.92 1"),
            _geom(
                "box", "0.06 0.9 0.6", f"{x0 - 0.03} {ym} 2.4", "0.55 0.75 0.92 1"
            ),  # west window
        ]
    )


def _house_mesh() -> str:
    x0, y0, x1, y1 = patrol.HOUSE
    ym = (y0 + y1) / 2
    eave, ridge = 4.4, patrol.HOUSE_HEIGHT - 0.15
    verts = [
        (x0, y0, 0), (x1, y0, 0), (x0, y1, 0), (x1, y1, 0),
        (x0, y0, eave), (x1, y0, eave), (x0, y1, eave), (x1, y1, eave),
        (x0, ym, ridge), (x1, ym, ridge),
    ]  # fmt: skip
    return '<mesh name="house" vertex="' + " ".join(f"{a} {b} {c}" for a, b, c in verts) + '"/>'


def _volumes() -> str:
    """The no-fly box as a translucent red volume (its edges are drawn in 2D)."""
    lo, hi = patrol.NO_FLY
    return _box(lo, hi, "0.93 0.20 0.20 0.20")


# The person: a root body (torso, head) and four limbs, all mocap bodies; the limbs
# swing about their joint by setting a quaternion. Dark clothes, so it reads as an adult.
JACKET = "0.12 0.15 0.26 1"
TROUSERS = "0.16 0.16 0.18 1"
SKIN = "0.86 0.67 0.52 1"
LIMBS = {  # name -> (offset from the root, fromto, rgba, radius)
    "legN": ((0.0, 0.11, 0.92), "0 0 0 0 0 -0.90", TROUSERS, 0.085),
    "legS": ((0.0, -0.11, 0.92), "0 0 0 0 0 -0.90", TROUSERS, 0.085),
    "armN": ((0.0, 0.27, 1.45), "0 0 0 0 0 -0.62", JACKET, 0.065),
    "armS": ((0.0, -0.27, 1.45), "0 0 0 0 0 -0.62", JACKET, 0.065),
}


def _person() -> str:
    p = ['<body name="person" mocap="true" pos="0 0 0">']
    p.append(f'<geom type="capsule" size="0.19" fromto="0 0 0.98 0 0 1.38" rgba="{JACKET}"/>')
    p.append(_geom("sphere", "0.12", "0 0 1.64", SKIN))  # head
    p.append(_geom("sphere", "0.125", "-0.01 0 1.67", "0.20 0.13 0.08 1"))  # hair
    p.append("</body>")
    for name, (_, fromto, rgba, rad) in LIMBS.items():
        p.append(f'<body name="{name}" mocap="true" pos="0 0 0">')
        p.append(f'<geom type="capsule" size="{rad}" fromto="{fromto}" rgba="{rgba}"/></body>')
    k = patrol.PERSON_KEEP
    # The person's true box on the ground: an amber slab. Its corner lines are drawn in 2D.
    p.append('<body name="pbox" mocap="true" pos="0 0 0">')
    p.append(_geom("box", f"{k} {k} 0.03", "0 0 0.06", "1 0.69 0.12 0.38"))
    p.append("</body>")
    return "\n".join(p)


def scene_xml(extra: str = "") -> str:
    """The whole scene as MJCF. ``extra`` is more worldbody XML (the tests add a marker).
    Everything in it is made here; it names no file."""
    return f"""
<mujoco>
  <statistic extent="{EXTENT}" center="20 15 0"/>
  <visual>
    <global offwidth="{W * SS}" offheight="{H * SS}"/>
    <quality offsamples="4"/>
    <headlight ambient="0.5 0.5 0.5" diffuse="0.4 0.4 0.4" specular="0 0 0"/>
    <map znear="0.1" zfar="3"/>
  </visual>
  <asset>
    <texture type="skybox" builtin="gradient" rgb1="0.78 0.85 0.92" rgb2="0.62 0.68 0.76"
             width="32" height="32"/>
    {_house_mesh()}
  </asset>
  <default><geom contype="0" conaffinity="0" condim="1"/></default>
  <worldbody>
    <light directional="true" pos="0 0 10" dir="0.35 0.45 -1" diffuse="0.6 0.6 0.56"
           specular="0.05 0.05 0.05" castshadow="false"/>
    <camera name="main" pos="{" ".join(map(str, CAM_POS))}" xyaxes="{_camera_axes()}"
            fovy="{CAM_FOVY}"/>
    {_ground()}
    {_house()}
    {_person()}
    {_drone()}
    {_volumes()}
    {extra}
  </worldbody>
</mujoco>"""


def build_model(extra: str = ""):
    return mujoco.MjModel.from_xml_string(scene_xml(extra))


def _qmul(a, b):
    w1, x1, y1, z1 = a
    w2, x2, y2, z2 = b
    return (
        w1 * w2 - x1 * x2 - y1 * y2 - z1 * z2,
        w1 * x2 + x1 * w2 + y1 * z2 - z1 * y2,
        w1 * y2 - x1 * z2 + y1 * w2 + z1 * x2,
        w1 * z2 + x1 * y2 - y1 * x2 + z1 * w2,
    )


def drone_quat(heading: float, tilt_deg: float):
    """Yaw to the direction of travel, then lean the nose down by tilt_deg."""
    yaw = (math.cos(heading / 2), 0.0, 0.0, math.sin(heading / 2))
    t = math.radians(tilt_deg)
    pitch = (math.cos(t / 2), 0.0, math.sin(t / 2), 0.0)
    return _qmul(yaw, pitch)


class Scene:
    """The model, its data and a renderer. ``pose`` places everything from a Frame."""

    def __init__(self) -> None:
        self.model = build_model()
        self.data = mujoco.MjData(self.model)
        self.renderer = mujoco.Renderer(self.model, height=H * SS, width=W * SS)
        m = self.model
        self.mocap = {
            n: int(m.body_mocapid[mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_BODY, n)])
            for n in ("drone", "person", "pbox", *LIMBS)
        }

    def pose(self, f: patrol.Frame, heading: float = 0.0, tilt: float = 0.0) -> None:
        d = self.data
        d.mocap_pos[self.mocap["drone"]] = f.drone
        d.mocap_quat[self.mocap["drone"]] = drone_quat(heading, tilt)
        px, py = f.person
        d.mocap_pos[self.mocap["person"]] = (px, py, 0.0)
        d.mocap_quat[self.mocap["person"]] = (1.0, 0.0, 0.0, 0.0)
        d.mocap_pos[self.mocap["pbox"]] = (px, py, 0.0)
        # The legs and arms swing while the person is walking (pose only; the position is
        # the frame's).
        moving = px > patrol.PERSON_TO_X + 1e-9
        swing = 0.5 * math.sin(f.t * 2 * math.pi * 1.0) if moving else 0.0
        for name, sign in (("legN", 1), ("legS", -1), ("armN", -1), ("armS", 1)):
            off = LIMBS[name][0]
            i = self.mocap[name]
            d.mocap_pos[i] = (px + off[0], py + off[1], off[2])
            a = sign * swing
            d.mocap_quat[i] = (math.cos(a / 2), 0.0, math.sin(a / 2), 0.0)
        mujoco.mj_forward(self.model, d)

    def render(self) -> Image.Image:
        self.renderer.update_scene(self.data, camera="main")
        return Image.fromarray(self.renderer.render())

    def px(self, point, scale: int = 1) -> tuple[float, float]:
        return world_to_pixel(self.model, self.data, point, (W * scale, H * scale))

    def close(self) -> None:
        self.renderer.close()


# --- what the plans and legend say, from the verifier's answers ---------------


def taken_plan(fan: tuple[patrol.Judged, ...]) -> patrol.Judged | None:
    """The plan the machine flies: the first one the verifier accepted."""
    return next((j for j in fan if j.ok), None)


def legend_words(fan: tuple[patrol.Judged, ...]) -> list[str]:
    """One short phrase per distinct refusal reason in the fan, in patrol.WORDS."""
    refused = [j for j in fan if not j.ok]
    reasons = dict.fromkeys(r for j in refused for r in j.reasons)
    return [patrol.WORDS.get(r, r) for r in reasons][:3]


def status_text(f: patrol.Frame, take: str) -> str:
    """The status line, in plain words, from the frame."""
    if take == "A":
        return STATUS["breach"] if f.breach else STATUS["unchecked"]
    if f.status == "refused":
        # The plan in hand is the first one the planner offered; it is refused only if the
        # verifier said so.
        first = f.fan[0]
        return f"{STATUS['refused']}: {first.name}" if not first.ok else STATUS["accepted"]
    if f.status == "hold":
        return STATUS["hold"]
    return f"{STATUS['accepted']}: {taken_plan(f.fan).name}"


def replan_counter(f: patrol.Frame, decisions: list[patrol.Decision]) -> str | None:
    """A true count of the verifier's re-checks so far, from the decisions."""
    asked = [d for d in decisions if d.plan == REPLAN and d.t <= f.t + 1e-9]
    if not asked:
        return None
    bad = [d for d in asked if not d.ok]
    if bad:
        return f"asked again {len(asked)} times, {len(bad)} refused"
    return f"asked again {len(asked)} times, all accepted"


# --- drawing -------------------------------------------------------------------


def box_edges(box):
    """The twelve edges of a box, as pairs of 3D corners."""
    (x0, y0, z0), (x1, y1, z1) = box
    c = {(i, j, k): (x1 if i else x0, y1 if j else y0, z1 if k else z0)
         for i in (0, 1) for j in (0, 1) for k in (0, 1)}  # fmt: skip
    out = []
    for a in c:
        for axis in range(3):
            if a[axis] == 0:
                b = list(a)
                b[axis] = 1
                out.append((c[a], c[tuple(b)]))
    return out


def _path_px(scene: Scene, points, scale: int) -> list[tuple[float, float]]:
    """A plan's path in pixels, each point at its own height."""
    return [scene.px(p, scale) for p in patrol.densify(points)]


def draw_scene_layer(img, scene: Scene, f: patrol.Frame, take: str, age: int,
                     trail: list[tuple[patrol.Point, bool]]) -> None:  # fmt: skip
    """Lines drawn on the supersampled picture (blended, antialiased by the shrink): the
    fence of air, the no-fly box's edges, the person's column, the flown path, the plans."""
    dr = ImageDraw.Draw(img, "RGBA")
    for a, b in box_edges(patrol.FENCE):
        dr.line([scene.px(a, SS), scene.px(b, SS)], fill=(*LINE_FENCE, 120), width=SS)
    for a, b in box_edges(patrol.NO_FLY):
        dr.line([scene.px(a, SS), scene.px(b, SS)], fill=(*RED, 235), width=2 * SS)
    # The person's true box: a column. Corner lines up to the top, footprint on the ground.
    lo, hi = patrol.person_box(f.person)
    for x, y in ((lo[0], lo[1]), (hi[0], lo[1]), (hi[0], hi[1]), (lo[0], hi[1])):
        dr.line([scene.px((x, y, lo[2] + 0.05), SS), scene.px((x, y, hi[2]), SS)],
                fill=(*AMBER, 200), width=SS)  # fmt: skip
    ring = [scene.px((x, y, 0.1), SS) for x, y in
            ((lo[0], lo[1]), (hi[0], lo[1]), (hi[0], hi[1]), (lo[0], hi[1]))]  # fmt: skip
    dr.line([*ring, ring[0]], fill=(*AMBER, 240), width=2 * SS, joint="curve")

    if take == "A" and len(trail) > 1:
        # The flown path behind the drone: red where it was inside the no-fly box.
        for (p, _), (q, bad) in zip(trail, trail[1:], strict=False):
            a, b = scene.px(p, SS), scene.px(q, SS)
            dr.line([a, b], fill=(255, 255, 255, 230) if bad else (15, 18, 24, 150), width=7 * SS)
            dr.line([a, b], fill=(*(RED if bad else (255, 255, 255)), 255), width=4 * SS)
    if f.fan is not None:
        first_ok = taken_plan(f.fan)
        # Refused plans first, so the plans that stay are drawn on top of them.
        for j in sorted(f.fan, key=lambda j: j.ok):
            st = plan_style(j, age)
            if st.alpha <= 0:
                continue
            pts = _path_px(scene, j.points, SS)
            width = (4 if j is first_ok else 3) * SS
            dr.line(pts, fill=(15, 18, 24, int(120 * st.alpha)), width=width + 2 * SS,
                    joint="curve")  # fmt: skip
            dr.line(pts, fill=(*st.color, int(255 * st.alpha)), width=width, joint="curve")
    # A thin line down from the drone to the ground, so its height reads.
    gx, gy, _ = f.drone
    dr.line([scene.px(f.drone, SS), scene.px((gx, gy, 0.0), SS)], fill=(255, 255, 255, 110),
            width=SS)  # fmt: skip


def draw_text_layer(img, scene: Scene, f: patrol.Frame, take: str, age: int,
                    decisions: list[patrol.Decision]) -> None:  # fmt: skip
    """Everything written on the picture, at the final size: the take tag and status,
    labels, the reasons for refusals, the footer."""
    bar = Image.new("RGBA", (W, 27), (12, 14, 18, 215))
    img.alpha_composite(bar, (0, 0))
    dr = ImageDraw.Draw(img, "RGBA")
    tag = TAKE_NAME[take]
    tw = int(math.ceil(font(14, True).getlength(tag)))
    dr.rounded_rectangle((4, 3, 4 + tw + 12, 24), 6, fill=(*TAKE_COLOR[take], 255))
    dr.text((10, 5), tag, font=font(14, True), fill=(255, 255, 255))
    text = status_text(f, take)
    color = (255, 150, 150) if (take == "A" and f.breach) else (255, 255, 255)
    dr.text((4 + tw + 12 + 10, 4), text, font=font(16, True), fill=color)
    bar = Image.new("RGBA", (W, 21), (12, 14, 18, 215))
    img.alpha_composite(bar, (0, H - 21))
    ImageDraw.Draw(img).text((6, H - 18), FOOTER, font=font(13), fill=(224, 228, 236))

    # "no-fly box" on the red volume: tied to the middle of its west top edge, with the pill
    # out on the lawn to the left, clear of the route and of the person's label.
    (x0, y0, _), (x1, y1, z1) = patrol.NO_FLY
    nx, ny = scene.px((x0, (y0 + y1) / 2, z1))
    box = _pill(img, LABEL_NO_FLY, (nx - 22, ny - 6), 13, True, color=(255, 190, 190),
                anchor="rb", border=RED)  # fmt: skip
    dr.line([(box[2], (box[1] + box[3]) / 2), (nx, ny)], fill=(255, 190, 190, 235), width=1)
    dr.ellipse((nx - 2, ny - 2, nx + 2, ny + 2), fill=(255, 190, 190, 255))
    # The person's box: tied to the middle of its front edge on the ground, pill below it.
    lo = patrol.person_box(f.person)[0]
    tx, ty = scene.px((f.person[0], lo[1], 0.1))
    # The pill moves in steps of 6 px, not every frame: fewer changed pixels per frame keeps
    # the animated WebP small. Its pointer still ends on the true point.
    pill_xy = (round(tx / 6) * 6, round((ty + 12) / 6) * 6)
    box = _pill(img, person_label(take), pill_xy, 13, True, color=(255, 215, 130),
                anchor="mt", border=AMBER)  # fmt: skip
    dr.line([((box[0] + box[2]) / 2, box[1]), (tx, ty)], fill=(255, 215, 130, 235), width=1)
    dr.ellipse((tx - 2, ty - 2, tx + 2, ty + 2), fill=(255, 215, 130, 255))

    if take == "B":
        count = replan_counter(f, decisions)
        if count:
            _pill(img, count, (W - 6, 30), 13, color=(190, 245, 210), anchor="rt")
        if f.fan is not None:
            alpha = max((plan_style(j, age).alpha for j in f.fan if not j.ok), default=0.0)
            words = legend_words(f.fan)
            if alpha > 0 and words:
                # A compact legend of the distinct refusal reasons, while the red plans fade.
                y = 31  # top left, clear of the start of the route
                for k, word in enumerate(words):
                    _pill(img, "refused: " + word, (6, y + 19 * k), 13, color=(255, 175, 175),
                          alpha=alpha)  # fmt: skip


def compose(scene: Scene, f: patrol.Frame, take: str, age: int, heading: float, tilt: float,
            trail, decisions) -> Image.Image:  # fmt: skip
    """One finished picture (W x H, RGB) for a frame."""
    scene.pose(f, heading, tilt)
    big = scene.render().convert("RGB")
    draw_scene_layer(big, scene, f, take, age, trail)
    img = big.resize((W, H), Image.LANCZOS).convert("RGBA")
    draw_text_layer(img, scene, f, take, age, decisions)
    return img.convert("RGB")


# --- the run ---------------------------------------------------------------------


@dataclass(frozen=True)
class Shot:
    frame: patrol.Frame
    take: str  # "A" without the warden, "B" with it
    index: int  # frame number inside its take
    age: int  # frames since the fan first appeared (0 if there is none yet)
    image: Image.Image


def headings(frames: list[patrol.Frame]) -> list[tuple[float, float]]:
    """(heading, tilt) per frame: the direction of the next move, and a lean while moving."""
    out: list[tuple[float, float]] = []
    last = 0.0
    for i, f in enumerate(frames):
        nxt = next((g for g in frames[i + 1 :] if g.drone != f.drone), None)
        moving = i + 1 < len(frames) and frames[i + 1].drone != f.drone
        if nxt is not None:
            last = math.atan2(nxt.drone[1] - f.drone[1], nxt.drone[0] - f.drone[0])
        out.append((last, TILT_DEG if moving else 0.0))
    return out


def render_frames() -> list[Shot]:
    """Prove first, then draw what the two takes returned, one picture per frame."""
    patrol.prove()  # before any renderer exists: no picture of an unproven scene
    take_a = patrol.without_warden()
    take_b, decisions = patrol.with_warden()
    scene = Scene()
    shots: list[Shot] = []
    try:
        for take, frames in (("A", take_a), ("B", take_b)):
            first_fan = next((i for i, f in enumerate(frames) if f.fan is not None), 0)
            lean = headings(frames)
            for i, f in enumerate(frames):
                age = max(0, i - first_fan)
                trail = [(g.drone, g.breach) for g in frames[: i + 1]]
                img = compose(scene, f, take, age, *lean[i], trail, decisions)
                shots.append(Shot(f, take, i, age, img))
    finally:
        scene.close()
    return shots


def still_shot(shots: list[Shot]) -> Shot:
    """A frame of take B in mid-flight on the north leg: the green route, both boxes, the
    labels, the drone clear of the house."""
    house = patrol.HOUSE
    ok = [
        s for s in shots
        if s.take == "B" and s.frame.fan is not None and s.age >= FADE_FRAMES
        and s.frame.drone[1] > house[3] + 2
        and s.frame.drone[0] > patrol.NO_FLY[1][0] + 1
    ]  # fmt: skip
    return ok[0] if ok else [s for s in shots if s.take == "B"][len(shots) // 4]


def sheet_picks(shots: list[Shot]) -> list[int]:
    """10 frames: 4 of take A including a breach frame, 6 of take B including the first fan
    frame, a mid-fade frame and the last frame."""
    a = [i for i, s in enumerate(shots) if s.take == "A"]
    b = [i for i, s in enumerate(shots) if s.take == "B"]
    breach = [i for i in a if shots[i].frame.breach]
    picks_a = [a[0], a[len(a) // 4], breach[len(breach) // 2] if breach else a[len(a) // 2], a[-1]]
    picks_b = [b[0], b[FADE_FRAMES // 2], b[len(b) // 3], b[len(b) // 2], b[2 * len(b) // 3], b[-1]]
    return list(dict.fromkeys(picks_a + picks_b))


def contact_sheet(shots: list[Shot], cols: int = 5) -> Image.Image:
    picks = sheet_picks(shots)
    rows = math.ceil(len(picks) / cols)
    cell_h = H + 24
    sheet = Image.new("RGB", (cols * W, rows * cell_h), (30, 33, 40))
    dr = ImageDraw.Draw(sheet)
    for k, idx in enumerate(picks):
        s = shots[idx]
        x, y = (k % cols) * W, (k // cols) * cell_h
        sheet.paste(s.image, (x, y))
        label = f"take {s.take} ({TAKE_NAME[s.take]}) · frame {s.index}"
        dr.text((x + 8, y + H + 3), label, font=font(15, True), fill=(235, 238, 244))
    return sheet


def save_webp(shots: list[Shot], path: Path) -> None:
    """Animated WebP, 110 ms a frame and a hold on the last. Lossy quality is lowered until
    the file fits in WEBP_LIMIT. minimize_size makes the encoder keep only what changed
    between frames, which is what makes a scene like this fit."""
    images = [s.image for s in shots]
    durations = [FRAME_MS] * (len(images) - 1) + [LAST_HOLD_MS]
    for quality in (60, 45, 35, 28, 22, 18, 15, 12, 10, 8):
        images[0].save(path, save_all=True, append_images=images[1:], duration=durations,
                       loop=0, format="WEBP", quality=quality, method=6,
                       minimize_size=True)  # fmt: skip
        if path.stat().st_size <= WEBP_LIMIT:
            return
    raise RuntimeError(f"patrol.webp is {path.stat().st_size} bytes, over {WEBP_LIMIT}")


def save_outputs(shots: list[Shot], out_dir: Path) -> dict[str, int]:
    """Write every picture into out_dir. Returns file name -> size in bytes."""
    out_dir.mkdir(parents=True, exist_ok=True)
    # Every data frame is one picture; none is repeated to make a hold (the encoders merge
    # identical neighbours and add their times). The last frame carries the hold.
    images = [s.image for s in shots]
    save_webp(shots, out_dir / "patrol.webp")
    durations = [FRAME_MS] * (len(images) - 1) + [LAST_HOLD_MS]
    pal = [im.quantize(colors=128, method=Image.MEDIANCUT, dither=Image.NONE) for im in images]
    pal[0].save(out_dir / "patrol.gif", save_all=True, append_images=pal[1:],
                duration=durations, loop=0, optimize=True)  # fmt: skip
    still = still_shot(shots).image
    still.save(out_dir / "patrol.png", optimize=True)
    _paste_on_page(still, (255, 255, 255)).save(out_dir / "patrol-check-light.png")
    _paste_on_page(still, (0x11, 0x14, 0x18)).save(out_dir / "patrol-check-dark.png")
    contact_sheet(shots).save(out_dir / "patrol-sheet.png")
    names = ["patrol.webp", "patrol.gif", "patrol.png", "patrol-check-light.png",
             "patrol-check-dark.png", "patrol-sheet.png"]  # fmt: skip
    return {n: (out_dir / n).stat().st_size for n in names}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT, help="output directory")
    args = ap.parse_args(argv)
    shots = render_frames()
    sizes = save_outputs(shots, args.out)
    n_a = sum(1 for s in shots if s.take == "A")
    print(f"frames: {len(shots)} (take A without the warden: {n_a}, "
          f"take B with the warden: {len(shots) - n_a})")  # fmt: skip
    for name, size in sizes.items():
        print(f"  {args.out / name}: {size} bytes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
