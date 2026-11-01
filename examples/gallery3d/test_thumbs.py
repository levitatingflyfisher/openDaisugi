"""The small line drawings of the scenes are made from the scenes' own data."""

import functools
import re
from pathlib import Path

import delivery
import many
import patrol
import thumbs

PAGE = Path(__file__).resolve().parents[2] / "site" / "index.html"


@functools.cache
def _made() -> dict[str, str]:
    """The three drawings, made once. A plain function and not a fixture, so
    tests/test_gallery3d.py can run these tests from the default suite."""
    return thumbs.drawings()


def paths(svg: str, cls: str) -> list[str]:
    return re.findall(rf'<path class="{cls}" d="([^"]+)"', svg)


def test_the_delivery_drawing_shows_every_judged_plan():
    made = _made()
    frames, _ = delivery.timeline()
    fan = next(f.fan for f in frames if f.fan)
    refused = [j for j in fan if not j.ok]
    assert len(paths(made["delivery"], "plan no")) == len(refused) == 6
    # the accepted plan and the bar where it stops
    assert len(paths(made["delivery"], "plan ok")) == 2
    assert f"{len(refused)} plans refused" in made["delivery"]


def test_the_patrol_drawing_shows_every_judged_plan():
    made = _made()
    frames, _ = patrol.with_warden()
    fan = next(f.fan for f in frames if f.fan)
    refused = [j for j in fan if not j.ok]
    assert len(paths(made["patrol"], "plan no")) == len(refused) == 5
    assert len(paths(made["patrol"], "plan ok")) == 1


def test_the_many_drawing_shows_every_accepted_plan_and_the_true_counts():
    made = _made()
    trials = many.judge_all()
    accepted = [t for t in trials if t.ok]
    refused = [t for t in trials if not t.ok]
    assert len(paths(made["many"], "plan ok thin")) == len(accepted) == 13
    assert len(paths(made["many"], "plan no faint")) == len(refused[:: thumbs.DRAW])
    assert "587 of 600 refused" in made["many"]
    assert "13 accepted, 0 in a box" in made["many"]


def test_the_drawings_are_the_same_each_time():
    made = _made()
    assert thumbs.drawings() == made


def test_a_drawing_holds_no_picture_and_no_outside_file():
    made = _made()
    for svg in made.values():
        assert "<image" not in svg and "href" not in svg and "url(" not in svg


def test_the_page_holds_exactly_these_drawings():
    made = _made()
    page = PAGE.read_text(encoding="utf-8")
    for name, svg in made.items():
        assert svg in page, name


def test_the_six_refused_delivery_plans_can_be_counted():
    made = _made()
    """Several plans run along one lane. The drawing sets them side by side."""
    refused = paths(made["delivery"], "plan no")
    assert len(set(refused)) == 6
    starts = {d.split(" L")[0] for d in refused}
    assert len(starts) == 6
