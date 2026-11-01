"""Scene 3 picture: tests for render_many.py.

Run: pytest examples/gallery3d -q

Only the output-contract test needs an EGL renderer; it is skipped without one.
"""

from pathlib import Path

import many as m
import pytest
import render_many as r
from PIL import Image, ImageFont, features

TRIALS = m.judge_all()  # the real 600 trials, worked out once (fast)
NS = m.batches(TRIALS)


def _trial(ok: bool, breach: tuple[str, ...] = ()) -> m.Trial:
    pts = ((4.0, 4.0, 9.0), (9.0, 4.0, 9.0), (36.0, 26.0, 9.0))
    return m.Trial(pts, ok, () if ok else ("no_fly",), breach)


def _have_renderer() -> bool:
    try:
        r.Scene().close()
        return True
    except Exception:  # no EGL, no GPU, no driver
        return False


def test_prove_runs_before_any_renderer_is_made(monkeypatch):
    made = []

    def no_proof():
        raise AssertionError("the checks failed")

    class Recorder:
        def __init__(self, *a, **k):
            made.append(1)

    monkeypatch.setattr(m, "prove", no_proof)
    monkeypatch.setattr(r.mujoco, "Renderer", Recorder)
    with pytest.raises(AssertionError):
        r.render_frames()
    with pytest.raises(AssertionError):
        r.main(["--out", "/nonexistent/never-written"])
    assert made == []


@pytest.mark.parametrize("n", [0, 1, 37, 300, 599, 600])
def test_tally_equals_the_shared_counts(n):
    c = r.tally(TRIALS, n)
    shared = m.counts(TRIALS[:n])
    assert set(c) == {"checked", "refused", "accepted", "breaches_among_accepted"}
    assert all(c[k] == shared[k] for k in c)
    assert c["checked"] == n == c["refused"] + c["accepted"]


def test_breaches_is_computed_not_typed():
    trials = [_trial(True), _trial(True, ("fence",)), _trial(False, ("fence",)), _trial(True)]
    # Only an accepted plan with a breach counts; a refused breaching plan does not.
    assert r.tally(trials, 4)["breaches_among_accepted"] == 1
    assert r.tally(trials, 1)["breaches_among_accepted"] == 0
    assert r.tally(trials, 3)["breaches_among_accepted"] == 1
    assert r.tally([_trial(True)] * 5, 5)["breaches_among_accepted"] == 0


def test_the_final_text_holds_the_four_numbers():
    c = r.tally(TRIALS, len(TRIALS))
    lines = r.counter_texts(c)
    assert lines == [
        f"checked {c['checked']}",
        f"refused {c['refused']}",
        f"accepted {c['accepted']}",
        f"breaches among accepted {c['breaches_among_accepted']}",
    ]
    assert c["checked"] == 600 and c["refused"] + c["accepted"] == 600
    assert r.status_text(c, 600) == "600 random plans for one flight, each checked"
    half = r.tally(TRIALS, 300)
    assert r.status_text(half, 600) == "checking random plans: 300 of 600"


def test_colour_comes_from_ok_only():
    for age in (0, 3, r.FADE, 500):
        color, alpha = r.style(True, age)
        assert color == r.GREEN and alpha == 1.0
    color, alpha = r.style(False, 0)
    assert color == r.RED and alpha == 1.0
    alphas = [r.style(False, k)[1] for k in range(r.FADE + 1)]
    assert alphas == sorted(alphas, reverse=True) and alphas[-1] == 0.0
    # Two trials with the same path and opposite answers get opposite styles.
    sched = r.schedule([_trial(True), _trial(False)], [0, 2], tail=r.FADE)
    last = dict(sched[-1])
    assert last == {0: 1.0}


def test_every_plan_is_shown_and_accepted_plans_stay():
    sched = r.schedule(TRIALS, NS)
    assert len(sched) == len(NS) + r.TAIL
    seen = {i for frame in sched for i, _ in frame}
    assert seen == set(range(len(TRIALS)))
    first = {}
    for k, frame in enumerate(sched):
        for i, _ in frame:
            first.setdefault(i, k)
    for i, t in enumerate(TRIALS):
        if t.ok:
            assert all(i in {j for j, _ in sched[k]} for k in range(first[i], len(sched)))
    # The last frame has only the accepted plans, and all of them.
    final = {i for i, _ in sched[-1]}
    assert final == {i for i, t in enumerate(TRIALS) if t.ok}
    # Nothing is drawn on frame 0 (nothing checked yet).
    assert sched[0] == []


def test_the_words_in_the_picture():
    assert m.MAKER in r.FOOTER and "from the simulator" in r.FOOTER
    texts = [v for k, v in vars(r).items() if isinstance(v, str) and not k.startswith("_")]
    assert texts
    assert not [t for t in texts if "learned" in t.lower() or " ai " in f" {t.lower()} "]
    assert "planner" not in r.FOOTER.lower()


def test_fonts_are_truetype():
    for size, bold in ((13, False), (17, True), (21, True)):
        assert isinstance(r.font(size, bold), ImageFont.FreeTypeFont)


def test_the_scene_loads_no_file_and_names_no_real_product():
    src = Path(r.__file__).read_text().lower()
    for word in ("menagerie", "skydio", "file=", ".obj", ".stl"):
        assert word not in src, word


@pytest.mark.skipif(not _have_renderer(), reason="mujoco cannot create an EGL renderer")
def test_output_contract(tmp_path):
    if not features.check("webp"):
        pytest.skip("Pillow has no WebP")
    assert r.main(["--out", str(tmp_path)]) == 0
    webp = tmp_path / "many.webp"
    assert webp.stat().st_size <= 150_000
    with Image.open(webp) as im:
        assert im.size[0] == 480
        assert getattr(im, "is_animated", False)
        assert im.n_frames == len(NS) + r.TAIL
        times = []
        for k in range(im.n_frames):
            im.seek(k)
            im.load()  # the frame's duration is read when it is loaded
            times.append(im.info["duration"])
    assert all(t == 110 for t in times[:-1]), times
    assert times[-1] >= 1500
    assert (tmp_path / "many.gif").stat().st_size < 2_000_000
    with Image.open(tmp_path / "many.png") as im:
        assert im.size[0] == 480
    for name in ("many-check-light.png", "many-check-dark.png", "many-sheet.png"):
        assert (tmp_path / name).exists()
