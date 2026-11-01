"""Scene 1 picture: a delivery robot and a child, drawn from delivery.timeline().

This script draws; it decides nothing. ``delivery.prove()`` runs first, before any
renderer exists. After that every frame is what ``delivery.timeline()`` returned:
where the robot, the child and the ball are, whether the keep-out zone is drawn,
and the fan of plans with the verifier's answer on each. A plan is coloured and
faded from its ``Judged.ok`` and ``Judged.reasons``, never from its name.

Who drives: a scripted planner. The child's position comes from the simulator.

Run:  MUJOCO_GL=egl python examples/gallery3d/render_delivery.py [--out DIR]
"""

# ruff: noqa: I001  -- MUJOCO_GL must be set before `import mujoco`.
from __future__ import annotations

import argparse
import functools
import math
import os
from dataclasses import dataclass
from pathlib import Path

os.environ.setdefault("MUJOCO_GL", "egl")

import mujoco
import numpy as np
from PIL import Image, ImageDraw, ImageFont

import delivery

# --- sizes ------------------------------------------------------------------
W, H = 480, 360  # the picture
SS = 2  # the scene is rendered at SS times the size and shrunk, for smooth edges
FRAME_MS = 110
LAST_HOLD_MS = 2000
FADE_FRAMES = 10  # refused plans fade out over this many frames
WEBP_LIMIT = 150_000
DEFAULT_OUT = Path(__file__).resolve().parent / "out"

FONT_REGULAR = "/usr/share/fonts/liberation/LiberationSans-Regular.ttf"
FONT_BOLD = "/usr/share/fonts/liberation/LiberationSans-Bold.ttf"

# --- the words in the picture -----------------------------------------------
LABEL_CHILD = "a model judges: a person"
LABEL_ZONE = "keeps 2 m away: proven"
FOOTER = f"driver: {delivery.DRIVER} · the child's position comes from the simulator"
STATUS = {
    "cruise": f"driving at {delivery.FAST:.1f} m/s",
    "refused": "plan refused: keep driving at speed",
    "slow": "accepted: slow down and stop short",
    "wait": "waiting for the child to clear",
    "go": "accepted: drive on, slowly",
}

GREEN = (46, 204, 100)
RED = (236, 62, 62)
AMBER = (255, 176, 32)

# --- the camera: one fixed oblique view, left to right along the sidewalk ----
CAM_POS = (9.8, -11.0, 9.5)
CAM_LOOK = (9.8, 0.6, 0.0)
CAM_FOVY = 40.0
PLAN_Z = 0.09  # plans are drawn on the ground, just over the sidewalk slabs


@functools.lru_cache(maxsize=None)
def font(size: int, bold: bool = False) -> ImageFont.FreeTypeFont:
    """A TrueType font. A missing file is an error: PIL's default font is not used."""
    path = FONT_BOLD if bold else FONT_REGULAR
    if not Path(path).is_file():
        raise FileNotFoundError(f"font file missing: {path}")
    return ImageFont.truetype(path, size)


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


def _ground() -> str:
    """Road, kerb, sidewalk with slab joints, yard, hedge."""
    g = [
        _geom("box", "24 9 0.05", "10 -10.5 -0.05", "0.25 0.26 0.29 1"),  # road
        _geom("box", "24 0.07 0.035", "10 -1.57 0.0", "0.78 0.77 0.74 1"),  # kerb
        _geom("box", "10 1.5 0.03", "10 0 0", "0.80 0.79 0.76 1"),  # sidewalk, x 0..20
        _geom("box", "24 6.5 0.05", "10 8 -0.05", "0.40 0.60 0.30 1"),  # yard
        _geom("box", "10 0.45 0.5", "10 7.4 0.5", "0.16 0.36 0.20 1"),  # hedge
    ]
    for x in range(2, 20, 2):  # slab joints
        g.append(_geom("box", "0.012 1.5 0.004", f"{x} 0 0.031", "0.58 0.57 0.55 1"))
    for x in range(-2, 24, 4):  # dashed line in the middle of the road
        g.append(_geom("box", "0.8 0.07 0.004", f"{x + 0.5} -9.0 0.004", "0.85 0.82 0.55 1"))
    return "\n".join(g)


def _car() -> str:
    x0, y0, x1, y1 = delivery.CAR
    cx, cy = (x0 + x1) / 2, (y0 + y1) / 2
    hx, hy = (x1 - x0) / 2, (y1 - y0) / 2
    parts = [
        _geom("box", f"{hx} {hy} 0.34", f"{cx} {cy} 0.62", "0.22 0.38 0.72 1"),  # body
        _geom("box", f"{hx * 0.5} {hy * 0.88} 0.3", f"{cx - 0.1} {cy} 1.26", "0.20 0.33 0.62 1"),
        _geom("box", f"{hx * 0.46} {hy * 0.9} 0.12", f"{cx - 0.1} {cy} 1.28", "0.62 0.78 0.9 1"),
        _geom("box", f"0.03 {hy * 0.8} 0.08", f"{x1 + 0.01} {cy} 0.62", "1 0.95 0.7 1"),  # lights
    ]
    for wx in (x0 + 0.85, x1 - 0.85):
        for wy in (y0 + 0.05, y1 - 0.05):
            parts.append(
                _geom("cylinder", "0.34 0.12", f"{wx} {wy} 0.34", "0.07 0.07 0.08 1",
                      'euler="90 0 0"')
            )  # fmt: skip
    return "\n".join(parts)


def _robot() -> str:
    """A six-wheeled box, about 0.8 m long, 0.55 wide, 0.55 tall, with a lid stripe and a flag."""
    r = ['<body name="robot" mocap="true" pos="0 0 0">']
    r.append(_geom("box", "0.4 0.25 0.16", "0 0 0.37", "0.96 0.96 0.94 1"))  # chassis
    r.append(_geom("box", "0.38 0.23 0.03", "0 0 0.56", "0.93 0.93 0.91 1"))  # lid
    r.append(_geom("box", "0.38 0.06 0.012", "0 0 0.595", "0.05 0.62 0.62 1"))  # lid stripe
    r.append(_geom("box", "0.012 0.17 0.06", "0.405 0 0.42", "0.10 0.11 0.14 1"))  # front sensor
    r.append(_geom("cylinder", "0.012 0.2", "-0.3 0.16 0.78", "0.10 0.11 0.14 1"))  # antenna
    r.append(_geom("box", "0.11 0.008 0.07", "-0.19 0.16 0.9", "1.0 0.45 0.05 1"))  # flag
    for wx in (-0.28, 0.0, 0.28):
        for wy in (-0.28, 0.28):
            r.append(
                _geom("cylinder", "0.12 0.04", f"{wx} {wy} 0.12", "0.10 0.10 0.12 1",
                      'euler="90 0 0"')
            )  # fmt: skip
    r.append("</body>")
    return "\n".join(r)


# The child: a root body (torso, head) and four limbs. All are mocap bodies; the limbs
# swing about their joint by setting a quaternion.
SHIRT = "1.0 0.82 0.08 1"
LIMBS = {  # name -> (offset from the root, geom)
    "legL": ((-0.07, 0.0, 0.50), "capsule", "0.07", "0 0 0 0 0 -0.43", "0.16 0.28 0.62 1"),
    "legR": ((0.07, 0.0, 0.50), "capsule", "0.07", "0 0 0 0 0 -0.43", "0.16 0.28 0.62 1"),
    "armL": ((-0.18, 0.0, 0.84), "capsule", "0.058", "0 0 0 0 0 -0.34", SHIRT),
    "armR": ((0.18, 0.0, 0.84), "capsule", "0.058", "0 0 0 0 0 -0.34", SHIRT),
}


def _child() -> str:
    c = ['<body name="child" mocap="true" pos="0 0 0">']
    c.append(f'<geom type="capsule" size="0.125" fromto="0 0 0.6 0 0 0.76" rgba="{SHIRT}"/>')
    c.append(_geom("sphere", "0.17", "0 0 0.97", "0.96 0.76 0.60 1"))  # head: big for a child
    c.append(_geom("sphere", "0.174", "0 -0.02 1.0", "0.30 0.19 0.10 1"))  # hair
    c.append("</body>")
    for name, (_, kind, size, fromto, rgba) in LIMBS.items():
        c.append(f'<body name="{name}" mocap="true" pos="0 0 0">')
        c.append(f'<geom type="{kind}" size="{size}" fromto="{fromto}" rgba="{rgba}"/></body>')
    return "\n".join(c)


def build_model(extra: str = "") -> mujoco.MjModel:
    """The whole scene as MJCF. ``extra`` is more worldbody XML (the tests add a marker)."""
    xml = f"""
<mujoco>
  <statistic extent="20" center="10 0 0"/>
  <visual>
    <global offwidth="{W * SS}" offheight="{H * SS}"/>
    <quality offsamples="4"/>
    <headlight ambient="0.45 0.45 0.45" diffuse="0.45 0.45 0.45" specular="0 0 0"/>
    <map znear="0.1" zfar="3"/>
  </visual>
  <asset>
    <texture type="skybox" builtin="gradient" rgb1="0.78 0.85 0.92" rgb2="0.58 0.64 0.72"
             width="32" height="32"/>
  </asset>
  <default><geom contype="0" conaffinity="0" condim="1"/></default>
  <worldbody>
    <light directional="true" pos="0 0 10" dir="0.35 0.45 -1" diffuse="0.55 0.55 0.52"
           specular="0.05 0.05 0.05"/>
    <camera name="main" pos="{" ".join(map(str, CAM_POS))}" xyaxes="{_camera_axes()}"
            fovy="{CAM_FOVY}"/>
    {_ground()}
    {_car()}
    {_robot()}
    {_child()}
    <body name="ball" mocap="true" pos="0 0 0">
      <geom type="sphere" size="0.15" pos="0 0 0.15" rgba="0.92 0.10 0.10 1"/>
    </body>
    <geom name="zone" type="box" size="1 1 0.008" pos="0 0 0.06" rgba="1 0.62 0.1 0"/>
    {extra}
  </worldbody>
</mujoco>"""
    return mujoco.MjModel.from_xml_string(xml)


def world_to_pixel(
    model: mujoco.MjModel, data: mujoco.MjData, point, size: tuple[int, int] = (W, H)
) -> tuple[float, float]:
    """Where a world point (metres) lands in a picture of ``size`` taken by the
    fixed camera. ``data`` must have had mj_forward run, so the camera pose is set."""
    cam = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_CAMERA, "main")
    rot = data.cam_xmat[cam].reshape(3, 3)  # columns: camera x, y, z in the world
    p = rot.T @ (np.asarray(point, dtype=float) - data.cam_xpos[cam])
    width, height = size
    focal = (height / 2) / math.tan(math.radians(model.cam_fovy[cam]) / 2)
    return width / 2 + focal * p[0] / -p[2], height / 2 - focal * p[1] / -p[2]


class Scene:
    """The model, its data and a renderer. ``pose`` places everything from a Frame."""

    def __init__(self) -> None:
        self.model = build_model()
        self.data = mujoco.MjData(self.model)
        self.renderer = mujoco.Renderer(self.model, height=H * SS, width=W * SS)
        m = self.model
        self.mocap = {
            n: int(m.body_mocapid[mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_BODY, n)])
            for n in ("robot", "child", "ball", *LIMBS)
        }
        self.zone = mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_GEOM, "zone")

    def pose(self, f: delivery.Frame) -> None:
        d = self.data
        d.mocap_pos[self.mocap["robot"]] = (f.robot_x, delivery.LANE_Y, 0.0)
        cx, cy = f.child
        d.mocap_pos[self.mocap["child"]] = (cx, cy, 0.0)
        # The legs and arms swing while the child is still on the move. The pose
        # is only for looks; where the child is comes from the frame.
        moving = f.child[1] < delivery.CHILD_END_Y - 1e-9
        swing = 0.55 * math.sin(f.t * 2 * math.pi * 1.6) if moving else 0.0
        for name, (off, *_), sign in (
            ("legL", LIMBS["legL"], 1), ("legR", LIMBS["legR"], -1),
            ("armL", LIMBS["armL"], -1), ("armR", LIMBS["armR"], 1),
        ):  # fmt: skip
            i = self.mocap[name]
            d.mocap_pos[i] = (cx + off[0], cy + off[1], off[2])
            a = sign * swing
            d.mocap_quat[i] = (math.cos(a / 2), math.sin(a / 2), 0.0, 0.0)
        bx, by = f.ball
        d.mocap_pos[self.mocap["ball"]] = (bx, by, 0.0)
        m, z = self.model, self.zone
        if f.zone is None:
            m.geom_rgba[z, 3] = 0.0
        else:
            (x0, y0, _), (x1, y1, _) = f.zone
            m.geom_pos[z] = ((x0 + x1) / 2, (y0 + y1) / 2, 0.06)
            m.geom_size[z] = ((x1 - x0) / 2, (y1 - y0) / 2, 0.008)
            m.geom_rgba[z] = (1.0, 0.62, 0.10, 0.42)
        mujoco.mj_forward(m, d)

    def render(self) -> Image.Image:
        self.renderer.update_scene(self.data, camera="main")
        return Image.fromarray(self.renderer.render())

    def px(self, point, scale: int = 1) -> tuple[float, float]:
        return world_to_pixel(self.model, self.data, point, (W * scale, H * scale))

    def close(self) -> None:
        self.renderer.close()


# --- how a plan looks: from the verifier's answer, not from its name ---------


@dataclass(frozen=True)
class PlanStyle:
    color: tuple[int, int, int]
    alpha: float  # 1 = solid, 0 = gone


def plan_style(judged: delivery.Judged, age_frames: int) -> PlanStyle:
    """Accepted plans are green and stay. Refused plans are red and fade to nothing
    over FADE_FRAMES frames from the frame the fan first appeared (age 0)."""
    if judged.ok:
        return PlanStyle(GREEN, 1.0)
    return PlanStyle(RED, max(0.0, 1.0 - max(age_frames, 0) / FADE_FRAMES))


def legend_words(fan: tuple[delivery.Judged, ...]) -> list[str]:
    """Up to three short reasons for the refusals in the fan, one per distinct
    reason, in the words of delivery.WORDS. Each is taken from the refused plan
    that has the fewest reasons, so the word says what that plan did wrong."""
    words: list[str] = []
    refused = [j for j in fan if not j.ok]
    for reason in dict.fromkeys(r for j in refused for r in j.reasons):
        word = delivery.WORDS.get(reason, reason)
        if word not in words:
            words.append(word)
    return words[:3]


# --- drawing -------------------------------------------------------------------


def _path_px(scene: Scene, points, scale: int) -> list[tuple[float, float]]:
    return [scene.px((x, y, PLAN_Z), scale) for x, y in delivery.densify(points)]


def draw_ground_layer(img: Image.Image, scene: Scene, f: delivery.Frame, age: int) -> None:
    """Lines on the ground, drawn on the supersampled picture (blended, antialiased
    by the shrink): the zone's edge and the plans of the fan."""
    dr = ImageDraw.Draw(img, "RGBA")
    if f.zone is not None:
        (x0, y0, _), (x1, y1, _) = f.zone
        ring = [scene.px((x, y, 0.09), SS) for x, y in ((x0, y0), (x1, y0), (x1, y1), (x0, y1))]
        dr.line([*ring, ring[0]], fill=(*AMBER, 235), width=3 * SS, joint="curve")
    if f.fan is None:
        return
    first_ok = next((j for j in f.fan if j.ok), None)
    # Refused plans first, so the plans that stay are drawn on top of them.
    for j in sorted(f.fan, key=lambda j: j.ok):
        st = plan_style(j, age)
        if st.alpha <= 0:
            continue
        pts = _path_px(scene, j.points, SS)
        # Refused lines are wider than accepted ones, so a refused plan that lies along
        # an accepted one still shows red at its edges until it fades.
        width = (6 if not j.ok else 5 if j is first_ok else 3) * SS
        dr.line(pts, fill=(15, 18, 24, int(110 * st.alpha)), width=width + 2 * SS, joint="curve")
        dr.line(pts, fill=(*st.color, int(255 * st.alpha)), width=width, joint="curve")


def _pill(base: Image.Image, text: str, xy, size: int, bold: bool = False,
          color=(255, 255, 255), bg=(12, 14, 18), bg_alpha=185, alpha=1.0,
          anchor="lt", border=None) -> tuple[int, int, int, int]:  # fmt: skip
    """Text on a dark rounded backing, so it reads on any part of the scene.
    xy is the corner given by ``anchor`` ('lt' left-top, 'mt' middle-top,
    'mb' middle-bottom). Returns the box (x0, y0, x1, y1)."""
    fnt = font(size, bold)
    pad = 4
    tw = int(math.ceil(fnt.getlength(text)))
    asc, desc = fnt.getmetrics()
    w, h = tw + 2 * pad, asc + desc + 2 * pad - 2
    x, y = xy
    if anchor[0] == "m":
        x -= w / 2
    elif anchor[0] == "r":
        x -= w
    if anchor[1] == "b":
        y -= h
    x = int(min(max(x, 3), W - 3 - w))  # keep it inside the picture
    y = int(min(max(y, 30), H - 24 - h))  # clear of the status line and footer
    layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    ld = ImageDraw.Draw(layer)
    ld.rounded_rectangle(
        (0, 0, w - 1, h - 1),
        5,
        fill=(*bg, int(bg_alpha * alpha)),
        outline=None if border is None else (*border, int(255 * alpha)),
    )
    ld.text((pad, pad - 2), text, font=fnt, fill=(*color, int(255 * alpha)))
    base.alpha_composite(layer, (x, y))
    return x, y, x + w, y + h


def draw_text_layer(img: Image.Image, scene: Scene, f: delivery.Frame, age: int) -> None:
    """Everything written on the picture, at the final size: status, labels,
    the reasons for refusals, the footer."""
    # top status line and footer, as full-width bars
    bar = Image.new("RGBA", (W, 27), (12, 14, 18, 215))
    img.alpha_composite(bar, (0, 0))
    status = STATUS[f.status]
    if f.status == "slow" and f.fan is not None:
        # The plan the robot took is the first one the verifier accepted: say its name
        # from the fan, not from a constant.
        status = "accepted: " + next(j.name for j in f.fan if j.ok)
    ImageDraw.Draw(img).text((8, 4), status, font=font(16, True), fill=(255, 255, 255))
    bar = Image.new("RGBA", (W, 21), (12, 14, 18, 215))
    img.alpha_composite(bar, (0, H - 21))
    ImageDraw.Draw(img).text((6, H - 18), FOOTER, font=font(13), fill=(224, 228, 236))

    dr = ImageDraw.Draw(img, "RGBA")
    if f.child_seen:
        hx, hy = scene.px((*f.child, 1.2))
        box = _pill(img, LABEL_CHILD, (hx + 6, hy - 46), 13, True, color=(255, 232, 150),
                    border=(255, 210, 90))  # fmt: skip
        dr.line([((box[0] + box[2]) / 2, box[3]), (hx, hy)], fill=(255, 232, 150, 235), width=1)
        dr.ellipse((hx - 2, hy - 2, hx + 2, hy + 2), fill=(255, 232, 150, 255))
    if f.zone is not None:
        (x0, y0, _), (x1, y1, _) = f.zone
        # Attached to the zone's back-left corner, the pill sits above it in the yard,
        # clear of the car and the road.
        zx, zy = scene.px((x0, y1, 0.09))
        box = _pill(img, LABEL_ZONE, (zx, zy - 28), 13, True, color=(255, 200, 90),
                    anchor="rb", border=(255, 176, 32))  # fmt: skip
        dr.line([(box[2] - 10, box[3]), (zx, zy)], fill=(255, 200, 90, 235), width=1)
        dr.ellipse((zx - 2, zy - 2, zx + 2, zy + 2), fill=(255, 200, 90, 255))
    if f.fan is not None:
        alpha = max((plan_style(j, age).alpha for j in f.fan if not j.ok), default=0.0)
        words = legend_words(f.fan)
        if alpha > 0 and words:
            # A compact legend for the refusals, so the reasons do not crowd the scene.
            y = H - 27 - 19 * len(words)
            for k, word in enumerate(words):
                box = _pill(img, "refused: " + word, (6, y + 19 * k), 13, color=(255, 175, 175),
                            alpha=alpha)  # fmt: skip


def compose(scene: Scene, f: delivery.Frame, age: int) -> Image.Image:
    """One finished picture (W x H, RGB) for a frame."""
    scene.pose(f)
    big = scene.render().convert("RGB")
    draw_ground_layer(big, scene, f, age)
    img = big.resize((W, H), Image.LANCZOS).convert("RGBA")
    draw_text_layer(img, scene, f, age)
    return img.convert("RGB")


# --- the run ---------------------------------------------------------------------


@dataclass(frozen=True)
class Shot:
    frame: delivery.Frame
    age: int  # frames since the fan first appeared (0 if there is none yet)
    image: Image.Image


def render_frames() -> list[Shot]:
    """Prove first, then draw what timeline() returns, one picture per frame."""
    delivery.prove()  # before any renderer exists: no picture of an unproven scene
    frames, _ = delivery.timeline()
    first_fan = next((i for i, f in enumerate(frames) if f.fan is not None), 0)
    scene = Scene()
    try:
        return [
            Shot(f, max(0, i - first_fan), compose(scene, f, max(0, i - first_fan)))
            for i, f in enumerate(frames)
        ]
    finally:
        scene.close()


def still_shot(shots: list[Shot]) -> Shot:
    """A frame during the wait, with the zone, the accepted plans and both labels."""
    waiting = [s for s in shots if s.frame.status == "wait" and s.frame.fan is not None]
    return waiting[len(waiting) // 2]


def _paste_on_page(still: Image.Image, page_rgb, width_px: int = 420, margin: int = 30):
    small = still.resize((360, round(360 * still.height / still.width)), Image.LANCZOS)
    page = Image.new("RGB", (width_px, small.height + 2 * margin), page_rgb)
    page.paste(small, ((width_px - 360) // 2, margin))
    return page


def contact_sheet(clip: list[Shot], n: int = 8, cols: int = 4) -> Image.Image:
    picks = [round(i * (len(clip) - 1) / (n - 1)) for i in range(n)]
    rows = math.ceil(n / cols)
    cell_h = H + 24
    sheet = Image.new("RGB", (cols * W, rows * cell_h), (30, 33, 40))
    dr = ImageDraw.Draw(sheet)
    for k, idx in enumerate(picks):
        x, y = (k % cols) * W, (k // cols) * cell_h
        sheet.paste(clip[idx].image, (x, y))
        dr.text((x + 8, y + H + 3), f"clip frame {idx}", font=font(15, True), fill=(235, 238, 244))
    return sheet


def save_webp(clip: list[Shot], path: Path) -> None:
    """Animated WebP, 110 ms a frame and a hold on the last. Lossy quality is
    lowered until the file fits in WEBP_LIMIT."""
    images = [s.image for s in clip]
    durations = [FRAME_MS] * (len(images) - 1) + [LAST_HOLD_MS]
    for quality in (80, 65, 50, 40, 30, 22, 15, 10):
        images[0].save(path, save_all=True, append_images=images[1:], duration=durations,
                       loop=0, format="WEBP", quality=quality, method=6)  # fmt: skip
        if path.stat().st_size <= WEBP_LIMIT:
            return
    raise RuntimeError(f"delivery.webp is {path.stat().st_size} bytes, over {WEBP_LIMIT}")


def save_outputs(shots: list[Shot], out_dir: Path) -> dict[str, int]:
    """Write every picture into out_dir. Returns file name -> size in bytes."""
    out_dir.mkdir(parents=True, exist_ok=True)
    # No frame is repeated to make a hold: the encoders merge identical neighbours and add
    # their times, which would make a frame longer than 110 ms. The red plans are drawn
    # at full strength on the first frame of the fan and fade over FADE_FRAMES frames.
    clip = shots
    save_webp(clip, out_dir / "delivery.webp")
    durations = [FRAME_MS] * (len(clip) - 1) + [LAST_HOLD_MS]
    pal = [s.image.quantize(colors=128, method=Image.MEDIANCUT, dither=Image.NONE) for s in clip]
    pal[0].save(out_dir / "delivery.gif", save_all=True, append_images=pal[1:],
                duration=durations, loop=0, optimize=True)  # fmt: skip
    still = still_shot(shots).image
    still.save(out_dir / "delivery.png", optimize=True)
    _paste_on_page(still, (255, 255, 255)).save(out_dir / "delivery-check-light.png")
    _paste_on_page(still, (0x11, 0x14, 0x18)).save(out_dir / "delivery-check-dark.png")
    contact_sheet(clip).save(out_dir / "delivery-sheet.png")
    names = ["delivery.webp", "delivery.gif", "delivery.png", "delivery-check-light.png",
             "delivery-check-dark.png", "delivery-sheet.png"]  # fmt: skip
    return {n: (out_dir / n).stat().st_size for n in names}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT, help="output directory")
    args = ap.parse_args(argv)
    shots = render_frames()
    sizes = save_outputs(shots, args.out)
    print(f"frames: {len(shots)}")
    for name, size in sizes.items():
        print(f"  {args.out / name}: {size} bytes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
