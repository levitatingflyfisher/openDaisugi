"""Scene 1 picture: tests for render_delivery.py.

Run: pytest examples/gallery3d -q

Rendering tests need an EGL renderer and are skipped without one.
"""

import math

import delivery as d
import numpy as np
import pytest
import render_delivery as r
from PIL import Image, ImageFont, features


def _have_renderer() -> bool:
    try:
        model = r.build_model()
        r.mujoco.Renderer(model, height=64, width=64).close()
        return True
    except Exception:  # no EGL, no GPU, no driver
        return False


needs_renderer = pytest.mark.skipif(
    not _have_renderer(), reason="mujoco cannot create an EGL renderer"
)


def _judged(ok: bool, name: str = "x") -> d.Judged:
    return d.Judged(name, ((4.0, 0.0), (9.0, 0.0)), 0.9, ok, () if ok else ("velocity_bounded",))


def test_prove_runs_before_any_renderer_is_made(monkeypatch):
    made = []

    def no_proof():
        raise AssertionError("the checks failed")

    class Recorder:
        def __init__(self, *a, **k):
            made.append(1)

    monkeypatch.setattr(d, "prove", no_proof)
    monkeypatch.setattr(r.mujoco, "Renderer", Recorder)
    with pytest.raises(AssertionError):
        r.render_frames()
    with pytest.raises(AssertionError):
        r.main(["--out", "/nonexistent/never-written"])
    assert made == []


@needs_renderer
@pytest.mark.parametrize("point", [(4.0, -1.0, 0.3), (15.5, 3.5, 0.3)])
def test_world_to_pixel_hits_a_marker_sphere(point):
    x, y, z = point
    marker = f'<geom type="sphere" size="0.12" pos="{x} {y} {z}" rgba="1 0 1 1"/>'
    model = r.build_model(marker)
    data = r.mujoco.MjData(model)
    r.mujoco.mj_forward(model, data)
    big = (r.W * r.SS, r.H * r.SS)
    with_renderer = r.mujoco.Renderer(model, height=big[1], width=big[0])
    try:
        with_renderer.update_scene(data, camera="main")
        rgb = with_renderer.render().astype(int)
    finally:
        with_renderer.close()
    mask = (rgb[..., 0] > rgb[..., 1] + 60) & (rgb[..., 2] > rgb[..., 1] + 60)
    ys, xs = np.nonzero(mask)
    assert len(xs) > 20, "the marker is not in the picture"
    u, v = r.world_to_pixel(model, data, point, big)
    assert math.hypot(xs.mean() - u, ys.mean() - v) <= 3.0


def test_plan_style_comes_from_the_answer_not_the_name():
    refused = r.plan_style(_judged(False), 0)
    assert refused.color == r.RED and refused.alpha > 0.9
    assert r.plan_style(_judged(False), r.FADE_FRAMES).alpha == 0.0
    assert r.plan_style(_judged(False), r.FADE_FRAMES + 40).alpha == 0.0
    alphas = [r.plan_style(_judged(False), k).alpha for k in range(r.FADE_FRAMES + 1)]
    assert alphas == sorted(alphas, reverse=True)
    for k in (0, r.FADE_FRAMES, 500):
        ok = r.plan_style(_judged(True), k)
        assert ok.color == r.GREEN and ok.alpha == 1.0
    # The same name with the opposite answer gets the opposite style.
    assert r.plan_style(_judged(True, "slow down and stop short"), 99).alpha == 1.0
    assert r.plan_style(_judged(False, "slow down and stop short"), 99).alpha == 0.0


def test_legend_uses_the_words_of_the_checks():
    frames, _ = d.timeline()
    fan = next(f.fan for f in frames if f.fan)
    assert set(r.legend_words(fan)) == set(d.WORDS.values())


def test_the_words_in_the_picture():
    assert r.LABEL_CHILD == "a model judges: a person"
    assert r.LABEL_ZONE == "keeps 2 m away: proven"
    assert d.DRIVER in r.FOOTER and "from the simulator" in r.FOOTER
    texts = [v for k, v in vars(r).items() if isinstance(v, str) and not k.startswith("_")]
    texts += list(r.STATUS.values())
    assert texts
    assert not [t for t in texts if "learned" in t.lower()]
    assert r.STATUS["cruise"] == f"driving at {d.FAST:.1f} m/s"


def test_fonts_are_truetype():
    for size, bold in ((13, False), (16, True)):
        assert isinstance(r.font(size, bold), ImageFont.FreeTypeFont)


@needs_renderer
def test_the_robot_moves_only_along_the_lane():
    frames, _ = d.timeline()
    scene = r.Scene()
    try:
        for f in (frames[0], frames[len(frames) // 2], frames[-1]):
            scene.pose(f)
            i = scene.mocap["robot"]
            assert tuple(scene.data.mocap_pos[i]) == (f.robot_x, d.LANE_Y, 0.0)
    finally:
        scene.close()


@needs_renderer
def test_output_contract(tmp_path):
    if not features.check("webp"):
        pytest.skip("Pillow has no WebP")
    assert r.main(["--out", str(tmp_path)]) == 0
    webp = tmp_path / "delivery.webp"
    assert webp.stat().st_size <= 150_000
    with Image.open(webp) as im:
        assert im.size[0] == 480
        assert getattr(im, "is_animated", False)
        times = []
        for k in range(im.n_frames):
            im.seek(k)
            im.load()  # the frame's duration is read when it is loaded
            times.append(im.info["duration"])
    assert all(t == 110 for t in times[:-1]), times
    assert times[-1] >= 1500
    assert (tmp_path / "delivery.gif").stat().st_size < 2_000_000
    with Image.open(tmp_path / "delivery.png") as im:
        assert im.size[0] == 480
    for name in ("delivery-check-light.png", "delivery-check-dark.png", "delivery-sheet.png"):
        assert (tmp_path / name).exists()
