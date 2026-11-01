"""Small line drawings of the three scenes, for the site.

Each drawing is a plan view made from the scene's own data: the ground,
the keep-out zones, every plan the verifier refused and the plan it
accepted. Nothing is traced from a render. The drawings use the site's own
classes, so they follow its palette in light and dark.

    python thumbs.py            # prints the three drawings
    python thumbs.py --write ../../site/index.html
    python thumbs.py --check ../../site/index.html

--write puts each drawing in place of the one with its id in the page.
--check fails when the page does not hold exactly these drawings.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import delivery  # noqa: E402
import many  # noqa: E402
import patrol  # noqa: E402

W = 480  # the width of every drawing, in SVG units


def _n(v: float) -> str:
    return f"{v:.1f}".rstrip("0").rstrip(".")


class View:
    """A plan view: world metres to SVG units, y up."""

    def __init__(self, x0: float, y0: float, x1: float, y1: float) -> None:
        self.x0, self.y1 = x0, y1
        self.k = W / (x1 - x0)
        self.h = (y1 - y0) * self.k

    def pt(self, x: float, y: float) -> tuple[float, float]:
        return ((x - self.x0) * self.k, (self.y1 - y) * self.k)

    def path(self, points) -> str:
        return "M" + " L".join(f"{_n(a)} {_n(b)}" for a, b in (self.pt(p[0], p[1]) for p in points))

    def rect(self, x0: float, y0: float, x1: float, y1: float, cls: str, rx: int = 0) -> str:
        ax, ay = self.pt(x0, y1)
        r = f' rx="{rx}"' if rx else ""
        return (
            f'<rect class="{cls}" x="{_n(ax)}" y="{_n(ay)}" width="{_n((x1 - x0) * self.k)}" '
            f'height="{_n((y1 - y0) * self.k)}"{r}/>'
        )

    def dot(self, x: float, y: float, cls: str, r: float) -> str:
        ax, ay = self.pt(x, y)
        return f'<circle class="{cls}" cx="{_n(ax)}" cy="{_n(ay)}" r="{_n(r)}"/>'

    def text(self, x: float, y: float, cls: str, words: str, anchor: str = "start") -> str:
        ax, ay = self.pt(x, y)
        return (
            f'<text class="{cls}" x="{_n(ax)}" y="{_n(ay)}" text-anchor="{anchor}">{words}</text>'
        )


def _svg(name: str, view: View, label: str, body: list[str]) -> str:
    return (
        f'<svg id="dg-{name}" class="dg scene" viewBox="0 0 {W} {_n(view.h)}" role="img" '
        f'aria-label="{label}" xmlns="http://www.w3.org/2000/svg">' + "".join(body) + "</svg>"
    )


def _spread(fan, gap: float) -> list[tuple]:
    """The refused plans, each moved sideways a little so that plans on one
    line can be counted. Several plans of the delivery scene run along the
    same lane and differ only in speed or in where they stop. The move is in
    the drawing only: gap metres between neighbours, centred on the true line."""
    refused = [j for j in fan if not j.ok]
    out = []
    for i, j in enumerate(refused):
        dy = (i - (len(refused) - 1) / 2) * gap
        out.append(tuple((p[0], p[1] + dy) for p in j.points))
    return out


def _plans(view: View, fan, gap: float = 0.0) -> list[str]:
    """Refused plans first, then the accepted ones, the taken plan last."""
    out = [f'<path class="plan no" d="{view.path(points)}"/>' for points in _spread(fan, gap)]
    taken = next(j for j in fan if j.ok)
    out += [
        f'<path class="plan ok faint" d="{view.path(j.points)}"/>'
        for j in fan
        if j.ok and j is not taken
    ]
    out.append(f'<path class="plan ok" d="{view.path(taken.points)}"/>')
    return out


def delivery_svg() -> str:
    frames, _ = delivery.timeline()
    f = next(fr for fr in frames if fr.fan)
    fan = f.fan
    refused = sum(1 for j in fan if not j.ok)
    view = View(0.0, -6.5, 20.0, 6.5)
    (sx0, sy0, _), (sx1, sy1, _) = delivery.SIDEWALK
    (zx0, zy0, _), (zx1, zy1, _) = f.zone
    taken = next(j for j in fan if j.ok)
    stop_x = taken.points[-1][0]
    body = [
        view.rect(sx0, sy0, sx1, sy1, "ground"),
        view.rect(*delivery.CAR, "thing", 4),
        view.rect(zx0, zy0, zx1, zy1, "zone"),
        *_plans(view, fan, gap=0.32),
        f'<path class="plan ok" d="{view.path([(stop_x, -0.9), (stop_x, 0.9)])}"/>',
        view.dot(f.child[0], f.child[1], "who", 6),
        view.rect(f.robot_x - 0.6, -0.45, f.robot_x + 0.6, 0.45, "bot", 3),
        view.text(0.4, 5.2, "sub not", f"{refused} plans refused"),
        view.text(0.4, -5.9, "sub okt", "accepted: stop short"),
        view.text((zx0 + zx1) / 2, 1.95, "sub not", "keep-out", "middle"),
        view.text(f.child[0] + 0.5, -2.85, "sub", "child"),
        view.text(0.5, -0.35, "sub", "sidewalk"),
    ]
    label = (
        f"Plan view. A robot on a sidewalk, a child ahead with a keep-out zone around it. {refused} plans in red "
        "cross the zone or leave the sidewalk. The accepted plan, in green, stops short of the zone."
    )
    return _svg("delivery", view, label, body)


def patrol_svg() -> str:
    frames, _ = patrol.with_warden()
    f = next(fr for fr in frames if fr.fan)
    fan = f.fan
    refused = sum(1 for j in fan if not j.ok)
    view = View(-3.0, -8.6, 43.0, 33.6)
    (fx0, fy0, _), (fx1, fy1, _) = patrol.FENCE
    (nx0, ny0, _), (nx1, ny1, _) = patrol.NO_FLY
    (px0, py0, _), (px1, py1, _) = patrol.person_box(f.person)
    body = [
        view.rect(fx0, fy0, fx1, fy1, "ground"),
        view.rect(nx0, ny0, nx1, ny1, "zone"),
        view.rect(*patrol.HOUSE, "thing"),
        view.rect(px0, py0, px1, py1, "zone"),
        *_plans(view, fan),
        view.dot(f.person[0], f.person[1], "who", 5),
        view.dot(*patrol.START, "bot", 6),
        view.dot(*patrol.GOAL, "who", 6),
        view.text((nx0 + nx1) / 2, ny1 + 1.0, "sub not", "no-fly box", "middle"),
        view.text(px0 - 0.7, f.person[1] + 0.9, "sub", "person", "end"),
        view.text(patrol.START[0] - 0.4, patrol.START[1] - 2.6, "sub", "start"),
        view.text(patrol.GOAL[0] + 1.0, patrol.GOAL[1] + 1.6, "sub", "goal", "end"),
        view.text(42.0, -7.0, "sub not", f"{refused} plans refused", "end"),
        view.text(-2.0, 31.4, "sub okt", "accepted: round by the north"),
    ]
    label = (
        f"Plan view. A fenced yard with a no-fly box over a house and a keep-out box around a person. {refused} "
        "plans in red cross a box or leave the fence. The accepted plan, in green, goes round by the north."
    )
    return _svg("patrol", view, label, body)


# Every DRAW-th refused plan is drawn, faintly, so the picture stays calm
# and the accepted plans stand out. Every accepted plan is drawn. The label
# gives the true counts.
DRAW = 12


def many_svg() -> str:
    trials = many.judge_all()
    got = many.counts(trials)
    refused = [t for t in trials if not t.ok]
    accepted = [t for t in trials if t.ok]
    view = View(-3.0, -7.0, 43.0, 33.0)
    (fx0, fy0, _), (fx1, fy1, _) = patrol.FENCE
    (nx0, ny0, _), (nx1, ny1, _) = patrol.NO_FLY
    (px0, py0, _), (px1, py1, _) = patrol.person_box(many.PERSON)
    body = [
        view.rect(fx0, fy0, fx1, fy1, "ground"),
        *[f'<path class="plan no faint" d="{view.path(t.points)}"/>' for t in refused[::DRAW]],
        view.rect(nx0, ny0, nx1, ny1, "zone"),
        view.rect(*patrol.HOUSE, "thing"),
        view.rect(px0, py0, px1, py1, "zone"),
        *[f'<path class="plan ok thin" d="{view.path(t.points)}"/>' for t in accepted],
        view.text(42.0, -5.4, "sub not", f"{len(refused)} of {len(trials)} refused", "end"),
        view.text(
            -2.0,
            -5.4,
            "sub okt",
            f"{len(accepted)} accepted, {got['breaches_among_accepted']} in a box",
        ),
    ]
    label = (
        f"Plan view of the same yard. {len(trials)} random plans: {len(refused)} refused, drawn in red, and "
        f"{len(accepted)} accepted, in green. No green plan enters a box."
    )
    return _svg("many", view, label, body)


def drawings() -> dict[str, str]:
    return {"delivery": delivery_svg(), "patrol": patrol_svg(), "many": many_svg()}


def main(argv: list[str]) -> int:
    made = drawings()
    if argv[:1] == ["--check"]:
        page = Path(argv[1]).read_text(encoding="utf-8")
        missing = [name for name, svg in made.items() if svg not in page]
        if missing:
            print(
                "the page does not hold these drawings as made here: " + ", ".join(missing),
                file=sys.stderr,
            )
            return 1
        return 0
    if argv[:1] == ["--write"]:
        path = Path(argv[1])
        page = path.read_text(encoding="utf-8")
        for name, svg in made.items():
            start = page.index(f'<svg id="dg-{name}"')
            end = page.index("</svg>", start) + len("</svg>")
            page = page[:start] + svg + page[end:]
        path.write_text(page, encoding="utf-8", newline="\n")
        return 0
    for svg in made.values():
        print(svg)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
