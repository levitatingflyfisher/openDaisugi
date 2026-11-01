"""Scene 3 picture: many random plans for one flight, each one checked.

This script draws; it decides nothing. ``many.prove()`` runs first, before any renderer
exists. After that every line is a plan from ``many.judge_all()``: it is green when
``Trial.ok`` is true and red when it is false, and nothing else decides its colour. Every
number on the picture is worked out from the trials shown so far (``tally``).

The animation: on frame k the first ``many.batches(trials)[k]`` plans have been checked.
An accepted plan is drawn green and stays. A refused plan is drawn red on the frame it
appears and fades out over FADE frames. Extra frames at the end let the last red plans
fade, so the final picture shows only the accepted routes.

Who makes the plans: a random generator. The person's position comes from the simulator.
No machine is drawn, only its plans, and the scene loads no file.

Run:  MUJOCO_GL=egl python examples/gallery3d/render_many.py [--out DIR]
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

import many
import patrol

# Scene 2's renderer is the house style; its scene pieces, camera maths, labels and writers
# are imported, not copied.
from render_delivery import (  # noqa: E402
    AMBER, FRAME_MS, GREEN, H, LAST_HOLD_MS, RED, SS, W, WEBP_LIMIT,
    _paste_on_page, _pill, font, world_to_pixel,
)  # fmt: skip
from render_patrol import (  # noqa: E402
    EXTENT, LIMBS, LINE_FENCE, _ground, _house, _house_mesh, _person, _volumes,
    box_edges,
)  # fmt: skip

DEFAULT_OUT = Path(__file__).resolve().parent / "out"

FADE = 4  # a refused plan fades out over this many frames (fewer lines, a smaller file)
RED_ALPHA = 125  # opacity of a new red line out of 255: faint lines are cheaper to encode
TAIL = FADE  # frames after the last batch, so the last red plans are gone on the last frame

# --- the words in the picture -----------------------------------------------------
LABEL_NO_FLY = "no-fly box"
LABEL_PERSON = "keep-out"
STATUS_DONE = "{n} random plans for one flight, each checked"
STATUS_RUNNING = "checking random plans: {n} of {total}"
FOOTER = f"plans: {many.MAKER} · the person's position comes from the simulator"

# --- the camera: a steep view from the south, a little west --------------------------
CAM_LOOK = (21.0, 10.0, 2.0)
CAM_ELEV_DEG = 70.0  # steep, so hundreds of routes do not hide behind each other
CAM_AZIM_DEG = 250.0
CAM_DIST = 76.0  # chosen so the whole property, and most of the places plans may stray to, fit
_HORIZ = CAM_DIST * math.cos(math.radians(CAM_ELEV_DEG))
CAM_POS = (
    CAM_LOOK[0] + _HORIZ * math.cos(math.radians(CAM_AZIM_DEG)),
    CAM_LOOK[1] + _HORIZ * math.sin(math.radians(CAM_AZIM_DEG)),
    CAM_LOOK[2] + CAM_DIST * math.sin(math.radians(CAM_ELEV_DEG)),
)
CAM_FOVY = 40.0


def _camera_axes() -> str:
    """xyaxes of the camera: its x to the right of the picture, its y up."""
    pos, look = np.array(CAM_POS), np.array(CAM_LOOK)
    fwd = look - pos
    fwd /= np.linalg.norm(fwd)
    right = np.cross(fwd, [0.0, 0.0, 1.0])
    right /= np.linalg.norm(right)
    up = np.cross(right, fwd)
    return " ".join(f"{v:.5f}" for v in (*right, *up))


def build_model() -> mujoco.MjModel:
    """Scene 2's property (lawn, fence, house, person, red box) without the drone."""
    xml = f"""
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
    {_volumes()}
  </worldbody>
</mujoco>"""
    return mujoco.MjModel.from_xml_string(xml)


class Scene:
    """The model, its data and a renderer, with the person standing at many.PERSON."""

    def __init__(self) -> None:
        self.model = build_model()
        self.data = mujoco.MjData(self.model)
        self.renderer = mujoco.Renderer(self.model, height=H * SS, width=W * SS)
        m, d = self.model, self.data
        px, py = many.PERSON
        for name in ("person", "pbox", *LIMBS):
            i = int(m.body_mocapid[mujoco.mj_name2id(m, mujoco.mjtObj.mjOBJ_BODY, name)])
            off = LIMBS[name][0] if name in LIMBS else (0.0, 0.0, 0.0)
            d.mocap_pos[i] = (px + off[0], py + off[1], off[2])
        mujoco.mj_forward(m, d)

    def render(self) -> Image.Image:
        self.renderer.update_scene(self.data, camera="main")
        return Image.fromarray(self.renderer.render())

    def px(self, point, scale: int = 1) -> tuple[float, float]:
        return world_to_pixel(self.model, self.data, point, (W * scale, H * scale))

    def close(self) -> None:
        self.renderer.close()


# --- what the picture says, from the trials ---------------------------------------


def tally(trials: list[many.Trial], n: int) -> dict[str, int]:
    """The four numbers on the picture, worked out from the first n trials."""
    shown = trials[:n]
    return {
        "checked": len(shown),
        "refused": sum(1 for t in shown if not t.ok),
        "accepted": sum(1 for t in shown if t.ok),
        "breaches_among_accepted": sum(1 for t in shown if t.ok and t.breach),
    }


def status_text(c: dict[str, int], total: int) -> str:
    if c["checked"] >= total:
        return STATUS_DONE.format(n=c["checked"])
    return STATUS_RUNNING.format(n=c["checked"], total=total)


def counter_texts(c: dict[str, int]) -> list[str]:
    """The lines of the counter panel: three counts, then the one that matters."""
    return [
        f"checked {c['checked']}",
        f"refused {c['refused']}",
        f"accepted {c['accepted']}",
        f"breaches among accepted {c['breaches_among_accepted']}",
    ]


def style(ok: bool, age: int) -> tuple[tuple[int, int, int], float]:
    """(colour, opacity) of a plan, from the verifier's answer alone. An accepted plan is
    green and stays. A refused one is red and fades to nothing FADE frames after it appeared."""
    if ok:
        return GREEN, 1.0
    return RED, max(0.0, 1.0 - max(age, 0) / FADE)


def schedule(
    trials: list[many.Trial], ns: list[int], tail: int | None = None
) -> list[list[tuple[int, float]]]:
    """For every frame, the (trial index, opacity) of each plan drawn on it. Frame k has
    checked ns[k] plans; the plans first shown on frame k are those from ns[k-1] to ns[k].
    ``tail`` frames are added after the last batch for the red plans to fade."""
    tail = TAIL if tail is None else tail
    appeared = {}  # trial index -> the frame it first appears on
    for k, n in enumerate(ns):
        for i in range(ns[k - 1] if k else 0, n):
            appeared[i] = k
    out = []
    for frame in range(len(ns) + tail):
        shown = []
        for i, k in appeared.items():
            if k > frame:  # not checked yet on this frame
                continue
            _, alpha = style(trials[i].ok, frame - k)
            if alpha > 0:
                shown.append((i, alpha))
        out.append(shown)
    return out


# --- drawing --------------------------------------------------------------------------


def draw_property(img, scene: Scene) -> None:
    """The fence of air, the red box's edges and the person's column, on the supersampled
    picture. They are the same on every frame, so they are drawn once, under the plans."""
    dr = ImageDraw.Draw(img, "RGBA")
    for a, b in box_edges(patrol.FENCE):
        dr.line([scene.px(a, SS), scene.px(b, SS)], fill=(*LINE_FENCE, 120), width=SS)
    for a, b in box_edges(patrol.NO_FLY):
        dr.line([scene.px(a, SS), scene.px(b, SS)], fill=(*RED, 235), width=2 * SS)
    lo, hi = patrol.person_box(many.PERSON)
    corners = ((lo[0], lo[1]), (hi[0], lo[1]), (hi[0], hi[1]), (lo[0], hi[1]))
    for x, y in corners:
        dr.line([scene.px((x, y, lo[2] + 0.05), SS), scene.px((x, y, hi[2]), SS)],
                fill=(*AMBER, 200), width=SS)  # fmt: skip
    ring = [scene.px((x, y, 0.1), SS) for x, y in corners]
    dr.line([*ring, ring[0]], fill=(*AMBER, 240), width=2 * SS, joint="curve")


def path_pixels(scene: Scene, trials: list[many.Trial]) -> list[list[tuple[float, float]]]:
    """Every plan's path in pixels of the supersampled picture, each point at its own
    height. The camera is fixed, so this is worked out once."""
    return [[scene.px(p, SS) for p in patrol.densify(t.points)] for t in trials]


def draw_plans(img, trials, paths, shown: list[tuple[int, float]]) -> None:
    """Thin lines. Refused plans first, so the accepted ones are drawn on top and a little
    thicker, with a dark edge so they read on the lawn."""
    dr = ImageDraw.Draw(img, "RGBA")
    for i, alpha in sorted(shown, key=lambda s: trials[s[0]].ok):
        t = trials[i]
        color, _ = style(t.ok, 0)
        if t.ok:
            dr.line(paths[i], fill=(15, 18, 24, 120), width=5 * SS, joint="curve")
            dr.line(paths[i], fill=(*color, 255), width=3 * SS, joint="curve")
        else:
            dr.line(paths[i], fill=(*color, int(RED_ALPHA * alpha)), width=2 * SS, joint="curve")


def draw_counters(img, c: dict[str, int]) -> None:
    """A compact panel at the bottom left: checked, refused, accepted, and under them the
    breach count, the largest. Colours follow the numbers."""
    checked, refused, accepted, breaches = counter_texts(c)
    size, big = 17, 21
    pad = 6
    widths = [font(size, True).getlength(s) for s in (checked, refused, accepted)]
    row1 = int(sum(widths)) + 2 * 14 + 2 * pad
    row2 = int(font(big, True).getlength(breaches)) + 2 * pad
    w, h = max(row1, row2) + 2, 2 * 26 + 2 * 2 + 4
    x0, y0 = 6, H - 21 - h - 5
    bad = c["breaches_among_accepted"] > 0
    layer = Image.new("RGBA", (w, h), (0, 0, 0, 0))
    ld = ImageDraw.Draw(layer)
    ld.rounded_rectangle((0, 0, w - 1, h - 1), 7, fill=(12, 14, 18, 215),
                         outline=(*(RED if bad else GREEN), 255))  # fmt: skip
    x = pad + 1
    for text, color in ((checked, (255, 255, 255)), (refused, (255, 150, 150)),
                        (accepted, (150, 240, 185))):  # fmt: skip
        ld.text((x, 5), text, font=font(size, True), fill=color)
        x += int(font(size, True).getlength(text)) + 14
    ld.text((pad + 1, 31), breaches, font=font(big, True),
            fill=(255, 120, 120) if bad else (110, 245, 160))  # fmt: skip
    img.alpha_composite(layer, (x0, y0))


def draw_text_layer(img, scene: Scene, c: dict[str, int], total: int) -> None:
    """Everything written on the picture, at the final size."""
    bar = Image.new("RGBA", (W, 27), (12, 14, 18, 255))
    img.alpha_composite(bar, (0, 0))
    ImageDraw.Draw(img).text((8, 4), status_text(c, total), font=font(17, True),
                             fill=(255, 255, 255))  # fmt: skip
    bar = Image.new("RGBA", (W, 21), (12, 14, 18, 255))
    img.alpha_composite(bar, (0, H - 21))
    ImageDraw.Draw(img).text((6, H - 18), FOOTER, font=font(13), fill=(224, 228, 236))
    # (The bars are opaque, so the footer never changes underneath: the encoder then leaves
    # its pixels out of each animation frame.)
    # Labels tied to the true boxes: the red box at its west top edge, the person's box at
    # the middle of its front edge on the ground.
    dr = ImageDraw.Draw(img, "RGBA")
    (x0, y0, _), (x1, y1, z1) = patrol.NO_FLY
    nx, ny = scene.px((x0, (y0 + y1) / 2, z1))
    box = _pill(img, LABEL_NO_FLY, (nx - 14, ny - 4), 13, True, color=(255, 190, 190),
                anchor="rb", border=RED)  # fmt: skip
    dr.line([(box[2], (box[1] + box[3]) / 2), (nx, ny)], fill=(255, 190, 190, 235), width=1)
    dr.ellipse((nx - 2, ny - 2, nx + 2, ny + 2), fill=(255, 190, 190, 255))
    lo = patrol.person_box(many.PERSON)[0]
    tx, ty = scene.px((many.PERSON[0], lo[1], 0.1))
    box = _pill(img, LABEL_PERSON, (tx, ty + 12), 13, True, color=(255, 215, 130),
                anchor="mt", border=AMBER)  # fmt: skip
    dr.line([((box[0] + box[2]) / 2, box[1]), (tx, ty)], fill=(255, 215, 130, 235), width=1)
    dr.ellipse((tx - 2, ty - 2, tx + 2, ty + 2), fill=(255, 215, 130, 255))
    draw_counters(img, c)


# --- the run ---------------------------------------------------------------------------


@dataclass(frozen=True)
class Shot:
    index: int
    checked: int  # n: how many plans have been checked on this frame
    image: Image.Image


def render_frames() -> tuple[list[Shot], dict[str, int]]:
    """Prove first, then draw what many.judge_all() returned, one picture per frame.
    Returns the shots and the final counts."""
    many.prove()  # before any renderer exists: no picture of an unproven scene
    trials = many.judge_all()
    ns = many.batches(trials)
    plan = schedule(trials, ns)
    scene = Scene()
    shots: list[Shot] = []
    try:
        # The camera, the property and the person never move: render and mark them once.
        base = scene.render().convert("RGB")
        draw_property(base, scene)
        paths = path_pixels(scene, trials)
        for k, shown in enumerate(plan):
            n = ns[min(k, len(ns) - 1)]
            big = base.copy()
            draw_plans(big, trials, paths, shown)
            img = big.resize((W, H), Image.LANCZOS).convert("RGBA")
            draw_text_layer(img, scene, tally(trials, n), len(trials))
            shots.append(Shot(k, n, img.convert("RGB")))
    finally:
        scene.close()
    return shots, tally(trials, len(trials))


def contact_sheet(shots: list[Shot], n: int = 8, cols: int = 4) -> Image.Image:
    picks = [round(i * (len(shots) - 1) / (n - 1)) for i in range(n)]
    rows = math.ceil(n / cols)
    cell_h = H + 24
    sheet = Image.new("RGB", (cols * W, rows * cell_h), (30, 33, 40))
    dr = ImageDraw.Draw(sheet)
    for k, idx in enumerate(picks):
        x, y = (k % cols) * W, (k // cols) * cell_h
        sheet.paste(shots[idx].image, (x, y))
        dr.text((x + 8, y + H + 3), f"frame {idx} · n = {shots[idx].checked}",
                font=font(15, True), fill=(235, 238, 244))  # fmt: skip
    return sheet


def save_webp(shots: list[Shot], path: Path) -> None:
    """Animated WebP, 110 ms a frame and a hold on the last. Every frame has thin lines
    across the whole property, so each one is nearly a new picture; the lossy quality is
    chosen as the highest, up to 60, that fits in WEBP_LIMIT. The file is never made to
    fit by leaving plans out."""
    images = [s.image for s in shots]
    durations = [FRAME_MS] * (len(images) - 1) + [LAST_HOLD_MS]

    def encode(quality: int) -> int:
        images[0].save(path, save_all=True, append_images=images[1:], duration=durations,
                       loop=0, format="WEBP", quality=quality, method=6,
                       minimize_size=True)  # fmt: skip
        return path.stat().st_size

    # The best quality that fits, by bisection (the size grows with the quality).
    lo, hi = 0, 60
    if encode(lo) > WEBP_LIMIT:
        raise RuntimeError(f"many.webp is {path.stat().st_size} bytes at quality 0")
    while lo < hi:
        mid = (lo + hi + 1) // 2
        if encode(mid) <= WEBP_LIMIT:
            lo = mid
        else:
            hi = mid - 1
    if encode(lo) > WEBP_LIMIT:  # leave the file that fits
        raise RuntimeError("many.webp does not fit")


def save_outputs(shots: list[Shot], out_dir: Path) -> dict[str, int]:
    """Write every picture into out_dir. Returns file name -> size in bytes."""
    out_dir.mkdir(parents=True, exist_ok=True)
    save_webp(shots, out_dir / "many.webp")
    durations = [FRAME_MS] * (len(shots) - 1) + [LAST_HOLD_MS]
    # One shared palette (from 8 frames across the clip), so the lawn does not flicker and
    # the file stays small.
    picks = [round(i * (len(shots) - 1) / 7) for i in range(8)]
    montage = Image.new("RGB", (W, H * 8))
    for k, idx in enumerate(picks):
        montage.paste(shots[idx].image, (0, H * k))
    shared = montage.quantize(colors=64, method=Image.MEDIANCUT, dither=Image.NONE)
    pal = [s.image.quantize(palette=shared, dither=Image.NONE) for s in shots]
    pal[0].save(out_dir / "many.gif", save_all=True, append_images=pal[1:],
                duration=durations, loop=0, optimize=True)  # fmt: skip
    still = shots[-1].image  # all checked, the fades finished: only the green routes remain
    still.save(out_dir / "many.png", optimize=True)
    _paste_on_page(still, (255, 255, 255)).save(out_dir / "many-check-light.png")
    _paste_on_page(still, (0x11, 0x14, 0x18)).save(out_dir / "many-check-dark.png")
    contact_sheet(shots).save(out_dir / "many-sheet.png")
    names = ["many.webp", "many.gif", "many.png", "many-check-light.png",
             "many-check-dark.png", "many-sheet.png"]  # fmt: skip
    return {n: (out_dir / n).stat().st_size for n in names}


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--out", type=Path, default=DEFAULT_OUT, help="output directory")
    args = ap.parse_args(argv)
    shots, final = render_frames()
    sizes = save_outputs(shots, args.out)
    print(f"frames: {len(shots)}")
    print("final counts: " + ", ".join(counter_texts(final)))
    for name, size in sizes.items():
        print(f"  {args.out / name}: {size} bytes")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
