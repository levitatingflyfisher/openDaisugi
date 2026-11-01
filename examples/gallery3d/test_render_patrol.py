"""Scene 2 picture: tests for render_patrol.py.

Run: pytest examples/gallery3d -q

Rendering tests need an EGL renderer; they are skipped without one.
"""

import dataclasses
import math
from pathlib import Path

import numpy as np
import patrol as p
import pytest
import render_patrol as r
from PIL import Image, ImageFont, features
from render_delivery import GREEN


def _have_renderer() -> bool:
    try:
        r.mujoco.Renderer(r.build_model(), height=64, width=64).close()
        return True
    except Exception:  # no EGL, no driver
        return False


needs_renderer = pytest.mark.skipif(
    not _have_renderer(), reason="mujoco cannot create an EGL renderer"
)


def _judged(ok: bool, name: str = "x") -> p.Judged:
    return p.Judged(name, ((4.0, 4.0, 9.0), (9.0, 4.0, 9.0)), ok, () if ok else ("no_fly",))


def test_prove_runs_before_any_renderer_is_made(monkeypatch):
    made = []

    def no_proof():
        raise AssertionError("the checks failed")

    class Recorder:
        def __init__(self, *a, **k):
            made.append(1)

    monkeypatch.setattr(p, "prove", no_proof)
    monkeypatch.setattr(r.mujoco, "Renderer", Recorder)
    with pytest.raises(AssertionError):
        r.render_frames()
    with pytest.raises(AssertionError):
        r.main(["--out", "/nonexistent/never-written"])
    assert made == []


def test_the_quadrotor_is_plain_shapes_and_no_real_product():
    """The owner's rule: no real product in a picture we publish. The machine is a body,
    four arms and four rotors made here, and the scene loads no file at all."""
    assert "file=" not in r.scene_xml()
    source = Path(r.__file__).read_text().lower()
    for word in ("menagerie", "skydio", ".obj", ".stl"):
        assert word not in source, word
    examples = Path(__file__).resolve().parents[1]
    assert not [f for f in examples.rglob("*") if f.suffix.lower() in (".obj", ".stl")]
    m = r.build_model()
    drone = r.mujoco.mj_name2id(m, r.mujoco.mjtObj.mjOBJ_BODY, "drone")
    kinds: dict[str, set[int]] = {}
    for g in range(m.ngeom):
        if m.geom_bodyid[g] == drone:
            name = r.mujoco.mj_id2name(m, r.mujoco.mjtObj.mjOBJ_GEOM, g)
            kinds.setdefault(name.removeprefix("drone_").rstrip("0123456789"), set()).add(g)
    assert {k: len(v) for k, v in kinds.items()} == {
        "body": 1, "nose": 1, "arm": 4, "motor": 4, "rotor": 4, "skid": 2,
    }  # fmt: skip
    geom = r.mujoco.mjtGeom
    plain = {
        int(t)
        for t in (geom.mjGEOM_ELLIPSOID, geom.mjGEOM_BOX, geom.mjGEOM_CAPSULE, geom.mjGEOM_CYLINDER)
    }
    assert all(int(m.geom_type[g]) in plain for ids in kinds.values() for g in ids)


@needs_renderer
@pytest.mark.parametrize("point", [(5.0, 3.0, 2.0), (36.0, 26.0, 11.0)])
def test_world_to_pixel_hits_a_marker_sphere(point):
    x, y, z = point
    marker = f'<geom type="sphere" size="0.4" pos="{x} {y} {z}" rgba="1 0 1 1"/>'
    model = r.build_model(marker)
    data = r.mujoco.MjData(model)
    r.mujoco.mj_forward(model, data)
    big = (r.W * r.SS, r.H * r.SS)
    renderer = r.mujoco.Renderer(model, height=big[1], width=big[0])
    try:
        renderer.update_scene(data, camera="main")
        rgb = renderer.render().astype(int)
    finally:
        renderer.close()
    mask = (rgb[..., 0] > rgb[..., 1] + 60) & (rgb[..., 2] > rgb[..., 1] + 60)
    ys, xs = np.nonzero(mask)
    assert len(xs) > 20, "the marker is not in the picture"
    u, v = r.world_to_pixel(model, data, point, big)
    # within 3 px of the 480 x 360 picture
    assert math.hypot((xs.mean() - u) / r.SS, (ys.mean() - v) / r.SS) <= 3.0


def test_plan_style_comes_from_the_answer_not_the_name():
    refused = r.plan_style(_judged(False), 0)
    assert refused.color == r.RED and refused.alpha > 0.9
    assert r.plan_style(_judged(False), r.FADE_FRAMES).alpha == 0.0
    alphas = [r.plan_style(_judged(False), k).alpha for k in range(r.FADE_FRAMES + 1)]
    assert alphas == sorted(alphas, reverse=True)
    for k in (0, r.FADE_FRAMES, 500):
        ok = r.plan_style(_judged(True), k)
        assert ok.color == GREEN and ok.alpha == 1.0
    # The same name with the opposite answer gets the opposite style.
    assert r.plan_style(_judged(True, "round by the north"), 99).alpha == 1.0
    assert r.plan_style(_judged(False, "round by the north"), 99).alpha == 0.0


def test_the_taken_plan_and_status_come_from_the_fan():
    frames, _ = p.with_warden()
    fan = frames[0].fan
    taken = r.taken_plan(fan)
    assert taken is next(j for j in fan if j.ok)
    assert r.status_text(frames[-1], "B") == "accepted: " + taken.name
    assert r.status_text(frames[0], "B") == f"plan refused: {fan[0].name}"
    # If the verifier had accepted the first plan, the picture would not say "refused".
    flipped = tuple(
        p.Judged(j.name, j.points, True, ()) if i == 0 else j for i, j in enumerate(fan)
    )
    assert "refused" not in r.status_text(dataclasses.replace(frames[0], fan=flipped), "B")
    take_a = p.without_warden()
    breach = [f for f in take_a if f.breach]
    assert breach and r.status_text(breach[0], "A") == "inside the no-fly box"
    assert r.status_text(take_a[0], "A") == "the plan is flown as it is"


def test_legend_and_counter_use_the_data():
    frames, decisions = p.with_warden()
    words = r.legend_words(frames[0].fan)
    assert set(words) == set(p.WORDS.values())
    assert r.replan_counter(frames[0], decisions) is None
    last = r.replan_counter(frames[-1], decisions)
    n = sum(1 for d in decisions if d.plan == "the rest of the route" and d.ok)
    assert last == f"asked again {n} times, all accepted"
    bad = decisions + [p.Decision(frames[-1].t, "the rest of the route", False, ("person",))]
    assert "1 refused" in r.replan_counter(frames[-1], bad)


def test_the_words_in_the_picture():
    assert r.LABEL_NO_FLY == "no-fly box"
    assert r.LABEL_PERSON == f"keeps {p.PERSON_KEEP_AWAY:.0f} m away: proven"
    # Nothing is proven in the take where nothing checks: there the box is only named.
    assert r.person_label("B") == r.LABEL_PERSON
    assert r.person_label("A") == f"keep-out: {p.PERSON_KEEP_AWAY:.0f} m around the person"
    assert "proven" not in r.person_label("A")
    assert p.DRIVER in r.FOOTER and "from the simulator" in r.FOOTER
    texts = [v for k, v in vars(r).items() if isinstance(v, str) and not k.startswith("_")]
    texts += list(r.STATUS.values()) + list(r.TAKE_NAME.values())
    assert texts
    assert not [t for t in texts if "learned" in t.lower() or "ai pilot" in t.lower()]
    assert r.TAKE_NAME == {"A": "without the warden", "B": "with the warden"}


def test_fonts_are_truetype():
    for size, bold in ((13, False), (16, True)):
        assert isinstance(r.font(size, bold), ImageFont.FreeTypeFont)


@needs_renderer
def test_the_drone_is_posed_at_its_frame():
    frames, _ = p.with_warden()
    scene = r.Scene()
    try:
        for f in (frames[0], frames[len(frames) // 2], frames[-1]):
            scene.pose(f)
            assert tuple(scene.data.mocap_pos[scene.mocap["drone"]]) == f.drone
            assert not p._inside(f.drone, p.person_box(f.person))
            assert not p._inside(f.drone, p.NO_FLY)
    finally:
        scene.close()


@needs_renderer
def test_output_contract(tmp_path):
    if not features.check("webp"):
        pytest.skip("Pillow has no WebP")
    assert r.main(["--out", str(tmp_path)]) == 0
    webp = tmp_path / "patrol.webp"
    assert webp.stat().st_size <= 150_000
    expected = len(p.without_warden()) + len(p.with_warden()[0])
    with Image.open(webp) as im:
        assert im.size[0] == 480
        assert getattr(im, "is_animated", False)
        assert im.n_frames == expected
        times = []
        for k in range(im.n_frames):
            im.seek(k)
            im.load()  # the frame's duration is read when it is loaded
            times.append(im.info["duration"])
    assert all(t == 110 for t in times[:-1]), times
    assert times[-1] >= 1500
    assert (tmp_path / "patrol.gif").stat().st_size < 2_000_000
    with Image.open(tmp_path / "patrol.png") as im:
        assert im.size[0] == 480
    for name in ("patrol-check-light.png", "patrol-check-dark.png", "patrol-sheet.png"):
        assert (tmp_path / name).exists()


def _dist_to_box(pt, box):
    """Distance from a point to a box, 0 inside."""
    return math.sqrt(sum(max(box[0][i] - pt[i], 0.0, pt[i] - box[1][i]) ** 2 for i in range(3)))


def _drawn_half_span() -> float:
    """The farthest any drawn shape of the drone reaches from its centre, measured on the
    compiled model: each shape's distance plus the radius of the ball that holds it."""
    m = r.build_model()
    drone = r.mujoco.mj_name2id(m, r.mujoco.mjtObj.mjOBJ_BODY, "drone")
    reach = [
        float(np.linalg.norm(m.geom_pos[g]) + m.geom_rbound[g])
        for g in range(m.ngeom)
        if m.geom_bodyid[g] == drone
    ]
    assert len(reach) == 16
    return max(reach)


def test_the_true_size_machine_fits_the_radius_the_checks_use():
    assert r.HALF_SPAN <= p.DRONE_RADIUS
    assert r.DRAW_SCALE >= 1
    # The model is the true-size machine times DRAW_SCALE, no larger.
    drawn = _drawn_half_span()
    assert drawn <= p.DRONE_RADIUS * r.DRAW_SCALE
    assert drawn == pytest.approx(r.HALF_SPAN * r.DRAW_SCALE, abs=0.05)


def test_the_drawn_drone_never_looks_inside_a_box():
    frames, _ = p.with_warden()
    clearance = min(
        min(_dist_to_box(f.drone, p.NO_FLY), _dist_to_box(f.drone, p.person_box(f.person)))
        for f in frames
    )
    assert clearance > 0
    # Three quarters of the clearance: the drawn body stops a quarter short of the box.
    drawn = _drawn_half_span()
    assert drawn <= 0.75 * clearance, (drawn, clearance)


def _hull(points):
    """Convex hull (monotone chain) of 2D points."""
    pts = sorted(set(points))

    def cross(o, a, b):
        return (a[0] - o[0]) * (b[1] - o[1]) - (a[1] - o[1]) * (b[0] - o[0])

    out = []
    for seq in (pts, pts[::-1]):
        half = []
        for q in seq:
            while len(half) >= 2 and cross(half[-2], half[-1], q) <= 0:
                half.pop()
            half.append(q)
        out += half[:-1]
    return out


def _inside_hull(hull, q):
    n = len(hull)
    return all(
        (hull[(i + 1) % n][0] - hull[i][0]) * (q[1] - hull[i][1])
        - (hull[(i + 1) % n][1] - hull[i][1]) * (q[0] - hull[i][0])
        >= 0
        for i in range(n)
    )


def test_the_camera_looks_down_steeply_enough():
    cam, look = np.array(r.CAM_POS), np.array(r.CAM_LOOK)
    d = look - cam
    elevation = math.degrees(math.atan2(-d[2], math.hypot(d[0], d[1])))
    assert elevation >= 55.0
    assert d[0] > 0 and d[1] > 0  # looking from the south and the west


@needs_renderer
def test_on_the_north_leg_the_drone_is_seen_beyond_the_no_fly_box():
    frames, _ = p.with_warden()
    north = [f for f in frames if abs(f.drone[1] - p.GOAL[1]) < 1e-6]
    assert len(north) > 10
    scene = r.Scene()
    try:
        scene.pose(frames[0])  # the camera is fixed: any pose will do
        (x0, y0, z0), (x1, y1, z1) = p.NO_FLY
        corners = [scene.px((x, y, z)) for x in (x0, x1) for y in (y0, y1) for z in (z0, z1)]
        hull = _hull(corners)
        for f in north:
            assert not _inside_hull(hull, scene.px(f.drone)), f.drone
    finally:
        scene.close()
