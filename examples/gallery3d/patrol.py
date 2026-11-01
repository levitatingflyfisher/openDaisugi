"""Scene 2: a quadrotor patrols a property. The plans and the real checks.

A quadrotor flies from one corner of a property to the far corner. A house
stands in between, with a no-fly box over it. A person walks along the south
side with a keep-out around them. The whole property has a fence of air: a
box the machine may not leave.

The same flight is shown twice.

- Without the warden, the plan in hand is flown as it is: straight across,
  through the no-fly box. Nothing checks it.
- With the warden, that plan is refused. The planner offers a fan of plans,
  the verifier judges each one, and the machine takes the first that is
  accepted: round by the north. While it flies it asks again on every frame,
  because the person walks and the keep-out moves with them.

This module draws nothing. A script that renders the scene calls ``prove()``
first and draws only what ``with_warden()`` and ``without_warden()`` return.

Who flies: a scripted planner (``candidates``), not a learned policy.

What is checked, and by which existing check:

- the fence: ``workspace_bounds`` with ``end_effector_in_workspace``.
- the no-fly box over the house and the keep-out around the person: two
  ``obstacles`` with ``no_obstacle_penetration``. Each box is grown by the
  machine's radius. The check samples each segment of the path.

The obstacle check takes the world's origin as the place the machine starts
from. So every check here is made in a frame whose origin is the first point
of the plan: the boxes and the path are moved by the same amount, and the
answer is about the path alone.

What is not checked: where the person is (the simulator says), where they go
next (each check is against their place at that moment), and the flight
itself. These are plans, checked as plans.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from opendaisugi import ActionPlan, Envelope, Invariant, Permission, verify
from opendaisugi.models import CartesianMoveStep
from opendaisugi.z3_checks import OBSTACLE_MIDPOINT_SAMPLES as SAMPLES

Point = tuple[float, float, float]
Box = tuple[Point, Point]

# The world, in metres.
FENCE: Box = ((0.0, 0.0, 1.0), (40.0, 30.0, 12.0))
HOUSE = (16.0, 10.0, 26.0, 20.0)  # footprint: x_min, y_min, x_max, y_max
HOUSE_HEIGHT = 7.0
DRONE_RADIUS = 0.5
# The no-fly box: the house, 1.5 m of margin and the machine's radius, up to
# the top of the fence, so no route goes over the roof inside the fence.
NO_FLY: Box = ((14.0, 8.0, 0.0), (28.0, 22.0, 12.0))

PERSON_KEEP_AWAY = 3.0  # the distance the picture names
PERSON_KEEP = PERSON_KEEP_AWAY + DRONE_RADIUS  # half width of the person's box
PERSON_Y = 5.0
PERSON_FROM_X, PERSON_TO_X = 30.0, 10.0
PERSON_SPEED = 1.2  # m/s

ALT = 9.0  # the patrol height, above the roof and under the fence's top
START = (4.0, 4.0)
GOAL = (36.0, 26.0)
SPEED = 4.0  # m/s
STEP = 2.0  # metres between the waypoints of a plan, at most

# The obstacle check tests sample points on each leg, ends included, and
# nothing between them. A leg could clip a corner of a box between two
# samples. So each box the verifier is given is the true box grown by half
# the largest gap between samples: any point of a leg is within half a gap
# of a sample, so a plan with no refusal clears the true box.
SAMPLE_GAP = STEP / (SAMPLES - 1)
SAMPLE_MARGIN = SAMPLE_GAP / 2

DRIVER = "a scripted planner"

# Each refusal in plain words, for the picture.
WORDS = {
    "no_fly": "crosses the no-fly box",
    "person": "enters the person's keep-out",
    "fence": "leaves the fence",
}


def person_at(t: float) -> tuple[float, float]:
    """The person's place at time t: west along the south side."""
    return (max(PERSON_FROM_X - PERSON_SPEED * t, PERSON_TO_X), PERSON_Y)


def person_box(person: tuple[float, float]) -> Box:
    x, y = person
    k = PERSON_KEEP
    return ((x - k, y - k, 0.0), (x + k, y + k, FENCE[1][2]))


def checked(box: Box) -> Box:
    """The box the verifier is given: box grown by SAMPLE_MARGIN on every side."""
    lo, hi = box
    m = SAMPLE_MARGIN
    return ((lo[0] - m, lo[1] - m, lo[2] - m), (hi[0] + m, hi[1] + m, hi[2] + m))


def _moved(box: Box, origin: Point) -> Box:
    lo, hi = box
    return (
        (lo[0] - origin[0], lo[1] - origin[1], lo[2] - origin[2]),
        (hi[0] - origin[0], hi[1] - origin[1], hi[2] - origin[2]),
    )


def envelope(person: tuple[float, float] | None, origin: Point = (0.0, 0.0, 0.0)) -> Envelope:
    """The envelope in force, in a frame whose origin is origin: the fence,
    the no-fly box, and the keep-out around the person when one is there."""
    boxes = [NO_FLY] + ([person_box(person)] if person is not None else [])
    return Envelope(
        generated_by="gallery3d",
        task="patrol the property",
        stakes="physical",
        permissions=Permission(
            workspace_bounds=_moved(FENCE, origin),
            obstacles=[_moved(checked(b), origin) for b in boxes],
        ),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="stay inside the fence"),
            Invariant(type="no_obstacle_penetration", description="stay out of the boxes"),
        ],
    )


def densify(points: tuple[Point, ...] | list[Point]) -> list[Point]:
    """The points of a path with no two of them more than STEP apart."""
    out = [points[0]]
    for a, b in zip(points, points[1:], strict=False):
        n = max(1, math.ceil(math.dist(a, b) / STEP - 1e-9))
        out += [tuple(a[i] + (b[i] - a[i]) * k / n for i in range(3)) for k in range(1, n + 1)]
    return out


def route(points: tuple[Point, ...] | list[Point], origin: Point | None = None) -> ActionPlan:
    """A plan to fly the path through points, in a frame whose origin is
    origin (the first point when none is given)."""
    o = points[0] if origin is None else origin
    steps = []
    for i, q in enumerate(densify(points)):
        steps.append(
            CartesianMoveStep(
                id=f"move{i}",
                target_position=(q[0] - o[0], q[1] - o[1], q[2] - o[2]),
                depends_on=[f"move{i - 1}"] if i else [],
            )
        )
    return ActionPlan(source="gallery3d", task="patrol the property", steps=steps)


def judge(points: tuple[Point, ...] | list[Point], person: tuple[float, float] | None):
    """The real verifier's answer to the plan through points, with the
    person where they are now."""
    return verify(route(points, points[0]), envelope(person, points[0]))


def reasons(result) -> tuple[str, ...]:
    """Why a plan was refused: fence, no_fly or person, sorted."""
    out = set()
    for v in result.violations:
        if v.detail.get("invariant") == "end_effector_in_workspace":
            out.add("fence")
        elif v.detail.get("invariant") == "no_obstacle_penetration":
            out.add("no_fly" if v.detail.get("obstacle_index") == 0 else "person")
        else:
            out.add(str(v.detail.get("invariant")))
    return tuple(sorted(out))


@dataclass(frozen=True)
class Candidate:
    """One plan the planner offers: its name and its path."""

    name: str
    points: tuple[Point, ...]


def straight_across() -> tuple[Point, ...]:
    """The plan in hand: straight from the start to the goal."""
    return ((*START, ALT), (*GOAL, ALT))


def candidates() -> list[Candidate]:
    """The scripted planner's plans from the start, in the order it likes
    them: the shortest first. The machine takes the first one the verifier
    accepts."""
    s, g = (*START, ALT), (*GOAL, ALT)
    return [
        Candidate("straight across", (s, g)),
        Candidate("cut the corner by the house", (s, (15.0, 6.0, ALT), (30.0, 20.0, ALT), g)),
        Candidate("round by the south", (s, (GOAL[0], START[1], ALT), g)),
        Candidate("over the roof, above the fence", (s, (10.0, 8.0, 13.5), (31.0, 22.5, 13.5), g)),
        Candidate(
            "wide by the south, outside the fence",
            (s, (8.0, -4.0, ALT), (38.0, -4.0, ALT), (38.0, GOAL[1], ALT), g),
        ),
        Candidate("round by the north", (s, (START[0], GOAL[1], ALT), g)),
        Candidate("hover here", (s, s)),
    ]


def prove() -> dict[str, str]:
    """Ask the real verifier, and fail unless it accepts the safe plans and
    refuses the unsafe ones for the reasons the scene shows. A script calls
    this before it renders one frame."""
    plans = {c.name: c.points for c in candidates()}
    walker = person_at(0.0)
    cases = [
        ("straight across", plans["straight across"], walker, ("no_fly",)),
        ("round by the south", plans["round by the south"], walker, ("person",)),
        ("over the roof", plans["over the roof, above the fence"], walker, ("fence",)),
        ("round by the north", plans["round by the north"], walker, ()),
        ("round by the south, nobody there", plans["round by the south"], None, ()),
    ]
    out: dict[str, str] = {}
    for name, points, person, want in cases:
        result = judge(points, person)
        got = () if result.ok else reasons(result)
        assert bool(result.ok) == (not want), f"{name}: verify gave ok={result.ok}"
        assert got == want, f"{name}: refused for {got}, the scene shows {want}"
        out[name] = "accepted" if result.ok else "refused: " + ", ".join(got)
    return out


@dataclass(frozen=True)
class Decision:
    """One answer of the verifier: when, to which plan, and why it refused."""

    t: float
    plan: str
    ok: bool
    reasons: tuple[str, ...]


@dataclass(frozen=True)
class Judged:
    """One plan of the fan with the verifier's answer, as the picture draws it."""

    name: str
    points: tuple[Point, ...]
    ok: bool
    reasons: tuple[str, ...]


@dataclass(frozen=True)
class Frame:
    """What one frame shows. status is unchecked, refused, fly or hold.
    breach is true while the machine is inside the no-fly box."""

    t: float
    drone: Point
    person: tuple[float, float]
    status: str
    fan: tuple[Judged, ...] | None
    breach: bool


def _inside(point: Point, box: Box) -> bool:
    return all(box[0][i] <= point[i] <= box[1][i] for i in range(3))


def _advance(path: list[Point], dist: float) -> list[Point]:
    """The rest of path after moving dist along it. Its first point is where
    the machine now is."""
    path = list(path)
    while len(path) > 1 and dist > 0:
        leg = math.dist(path[0], path[1])
        if leg <= dist:
            dist -= leg
            path.pop(0)
        else:
            a, b = path[0], path[1]
            path[0] = tuple(a[i] + (b[i] - a[i]) * dist / leg for i in range(3))
            dist = 0
    return path


def without_warden(dt: float = 0.4) -> list[Frame]:
    """The plan in hand, flown as it is. Nothing checks it, and it crosses
    the no-fly box. The breach in each frame is plain geometry."""
    frames: list[Frame] = []
    path = list(straight_across())
    t = 0.0
    while True:
        here = path[0]
        frames.append(Frame(t, here, person_at(t), "unchecked", None, _inside(here, NO_FLY)))
        if len(path) == 1:
            return frames
        path = _advance(path, SPEED * dt)
        t += dt


def with_warden(dt: float = 0.4) -> tuple[list[Frame], list[Decision]]:
    """The same task with the warden, and each decision of the verifier.

    The machine's state comes only from those decisions. When the verifier
    accepts the plan straight across, or the first plan it accepts is not
    the one this scene tells, this raises: the scene never draws an answer
    the verifier did not give."""
    frames: list[Frame] = []
    decisions: list[Decision] = []

    def ask(t: float, name: str, points) -> tuple[bool, tuple[str, ...]]:
        result = judge(points, person_at(t))
        ok = bool(result.ok)
        why = () if ok else reasons(result)
        decisions.append(Decision(t, name, ok, why))
        return ok, why

    if ask(0.0, "fly straight across", straight_across())[0]:
        raise RuntimeError(
            "the verifier accepted the plan straight across the no-fly box; "
            "this scene does not draw a refusal the verifier did not make"
        )
    judged = []
    for c in candidates():
        ok, why = ask(0.0, c.name, c.points)
        judged.append(Judged(c.name, c.points, ok, why))
    fan = tuple(judged)
    taken = next((j for j in fan if j.ok), None)
    if taken is None or taken.name != "round by the north":
        raise RuntimeError(
            f"the first plan the verifier accepted is {taken.name if taken else None!r}; "
            "this scene tells the story of the route round by the north"
        )
    path = list(taken.points)
    frames.append(Frame(0.0, path[0], person_at(0.0), "refused", fan, False))
    t = dt
    for _ in range(600):
        here = path[0]
        if len(path) == 1:
            frames.append(Frame(t, here, person_at(t), "fly", fan, _inside(here, NO_FLY)))
            return frames, decisions
        # The person has moved. Is the rest of the route still allowed?
        ok, _ = ask(t, "the rest of the route", path)
        status = "fly" if ok else "hold"
        frames.append(Frame(t, here, person_at(t), status, fan, _inside(here, NO_FLY)))
        if ok:
            path = _advance(path, SPEED * dt)
        t += dt
    raise RuntimeError("the scene did not finish")
