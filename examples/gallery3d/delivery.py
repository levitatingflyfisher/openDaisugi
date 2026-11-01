"""Scene 1: a delivery robot and a child. The plans and the real checks.

A small wheeled robot drives along a sidewalk. A ball rolls out from behind a
parked car and a child runs after it. The robot's plan is to keep driving at
speed. The verifier refuses it. The robot's planner then offers a fan of
plans, the verifier judges each one, and the robot takes the first that is
accepted: slow down and stop short. When the child is clear it drives on.

This module draws nothing. It builds the envelopes and the plans, asks the
real ``verify`` and returns what happened, frame by frame. A script that
renders the scene calls ``prove()`` first and draws only what ``timeline()``
returns, so no refusal in the picture is scripted and every plan in the fan
is a plan the verifier judged.

Who drives: a scripted planner (``candidates``), not a learned policy.

What is checked, and by which existing check:

- the keep-out zone around the child: ``obstacles`` with the
  ``no_obstacle_penetration`` invariant, as in the old gallery's dynamic
  keep-out. The zone is a box whose half width is 2 m plus the robot's own
  half length, so a robot centre outside the box has its edge more than 2 m
  from the child. The check samples each segment of the path.
- the speed limit: ``velocity_limit`` with the ``velocity_bounded`` invariant.
  It bounds a joint's speed in rad/s, so the plan carries the drive wheels'
  rotation, and the limit in m/s is divided by the wheel radius.
- the sidewalk: ``workspace_bounds`` with ``end_effector_in_workspace``.

What is not checked:

- where the child is. The scene takes the child's position from the
  simulator. No vision model runs here.
- where the child goes next. Each check is against the child's place at
  that moment, so the robot asks again while it waits. The plan it takes is
  safe wherever the child runs, because it stops before the zone's whole
  width; that is the planner's doing, not a proof.
- that the wheel rotation a plan states matches its path. Both are made
  from the same distance in ``route``.
"""

from __future__ import annotations

import math
from dataclasses import dataclass

from opendaisugi import ActionPlan, Envelope, Invariant, Permission, verify
from opendaisugi.models import CartesianMoveStep, JointMoveStep
from opendaisugi.z3_checks import OBSTACLE_MIDPOINT_SAMPLES as SAMPLES

# The world, in metres. The sidewalk runs along x; the road is at negative y
# and the yard at positive y.
SIDEWALK = ((0.0, -1.5, 0.0), (20.0, 1.5, 1.0))
LANE_Y = 0.0
BODY_Z = 0.25
WHEEL_RADIUS = 0.10
ROBOT_RADIUS = 0.40  # half the robot's length

KEEP_AWAY = 2.0  # the distance the picture names
KEEP = KEEP_AWAY + ROBOT_RADIUS  # half width of the zone around the child

LIMIT_CLEAR = 3.0  # m/s: the speed limit with nobody on the sidewalk
LIMIT_NEAR_PERSON = 1.0  # m/s: the speed limit while a person is in the scene
# The speeds the robot plans with. Each sits under its limit, not on it: a
# plan exactly at a limit passes or fails on the last digit of a division.
FAST = 2.8
SLOW = 0.9
STEP = 1.0  # metres between the waypoints of a plan, at most

# The keep-out check tests sample points on each leg, ends included, and
# nothing between them. A leg could clip a corner of a box between two
# samples. So the box the verifier is given is the true zone grown by half
# the largest gap between samples: any point of a leg is within half a gap
# of a sample, so a plan with no refusal clears the true zone.
SAMPLE_GAP = STEP / (SAMPLES - 1)
SAMPLE_MARGIN = SAMPLE_GAP / 2

START_X = 4.0
GOAL_X = 19.0
CHILD_X = 12.6
# Where the robot stops short: its front edge stays clear of the zone.
WAIT_X = CHILD_X - KEEP - ROBOT_RADIUS - 0.4

# The parked car, on the road beside the sidewalk: (x_min, y_min, x_max, y_max).
CAR = (8.0, -4.0, 12.0, -2.2)

CHILD_START_Y = -3.4  # behind the car, as the robot sees it
CHILD_END_Y = 4.6  # in the yard, where the ball stops
CHILD_SPEED = 1.2  # m/s
BALL_LEAD = 1.4  # the ball stays this far ahead of the child, in metres

# What drives the robot in this scene, in the words a caption may use.
DRIVER = "a scripted planner"

# Each check's refusal in plain words, for the picture.
WORDS = {
    "no_obstacle_penetration": "enters the keep-out",
    "velocity_bounded": "too fast near a person",
    "end_effector_in_workspace": "leaves the sidewalk",
}

Point = tuple[float, float]
Point3 = tuple[float, float, float]
Box = tuple[Point3, Point3]


def zone(person: Point) -> Box:
    """The true keep-out zone around a person: the box the picture draws."""
    x, y = person
    return ((x - KEEP, y - KEEP, 0.0), (x + KEEP, y + KEEP, 2.0))


def checked(box: Box) -> Box:
    """The box the verifier is given: box grown by SAMPLE_MARGIN on every side."""
    lo, hi = box
    m = SAMPLE_MARGIN
    return ((lo[0] - m, lo[1] - m, lo[2] - m), (hi[0] + m, hi[1] + m, hi[2] + m))


def _moved(box: Box, origin: Point3) -> Box:
    lo, hi = box
    return (
        (lo[0] - origin[0], lo[1] - origin[1], lo[2] - origin[2]),
        (hi[0] - origin[0], hi[1] - origin[1], hi[2] - origin[2]),
    )


def envelope(person: Point | None, origin: Point3 = (0.0, 0.0, 0.0)) -> Envelope:
    """The envelope in force, in a frame whose origin is origin. With a
    person in the scene it holds the keep-out zone around them and the lower
    speed limit; with nobody, the cruise limit."""
    obstacles = []
    limit = LIMIT_CLEAR
    if person is not None:
        obstacles = [_moved(checked(zone(person)), origin)]
        limit = LIMIT_NEAR_PERSON
    return Envelope(
        generated_by="gallery3d",
        task="deliver along the sidewalk",
        stakes="physical",
        permissions=Permission(
            workspace_bounds=_moved(SIDEWALK, origin),
            obstacles=obstacles,
            velocity_limit=limit / WHEEL_RADIUS,
        ),
        invariants=[
            Invariant(type="end_effector_in_workspace", description="stay on the sidewalk"),
            Invariant(type="no_obstacle_penetration", description="keep out of the person's zone"),
            Invariant(type="velocity_bounded", description="drive wheel speed limit"),
        ],
    )


def densify(points: tuple[Point, ...] | list[Point]) -> list[Point]:
    """The points of a path with no two of them more than STEP apart."""
    out = [points[0]]
    for (x0, y0), (x1, y1) in zip(points, points[1:], strict=False):
        n = max(1, math.ceil(math.hypot(x1 - x0, y1 - y0) / STEP - 1e-9))
        out += [(x0 + (x1 - x0) * k / n, y0 + (y1 - y0) * k / n) for k in range(1, n + 1)]
    return out


def frame_of(points: tuple[Point, ...] | list[Point]) -> Point3:
    """The origin a plan is checked in: its own first point. The obstacle
    check takes the origin as the place the machine starts from."""
    return (points[0][0], points[0][1], BODY_Z)


def route(points: tuple[Point, ...] | list[Point], speed: float) -> ActionPlan:
    """A plan to drive along the path through points at speed (m/s), in the
    frame whose origin is its first point.

    Each leg is a move of the body and the rotation of the two drive wheels
    over the time the leg takes, made from the same distance."""
    ox, oy, _ = frame_of(points)
    steps: list = []
    prev, angle, prev_id = None, 0.0, None
    for i, (x, y) in enumerate(densify(points)):
        dist = 0.0 if prev is None else math.hypot(x - prev[0], y - prev[1])
        angle += dist / WHEEL_RADIUS
        move = CartesianMoveStep(
            id=f"move{i}",
            target_position=(x - ox, y - oy, 0.0),
            depends_on=[prev_id] if prev_id else [],
        )
        wheels = JointMoveStep(
            id=f"wheels{i}",
            joint_targets={"wheel_left": angle, "wheel_right": angle},
            duration_s=max(dist / speed, 0.1),
            depends_on=[move.id],
        )
        steps += [move, wheels]
        prev, prev_id = (x, y), wheels.id
    return ActionPlan(source="gallery3d", task="deliver along the sidewalk", steps=steps)


def lane(x_from: float, x_to: float) -> tuple[Point, Point]:
    """The path straight along the lane from x_from to x_to."""
    return ((x_from, LANE_Y), (x_to, LANE_Y))


def judge(points: tuple[Point, ...] | list[Point], speed: float, person: Point | None):
    """The real verifier's answer to the plan along points at speed, with
    the person where they are now. The plan and every box are in the plan's
    own frame."""
    return verify(route(points, speed), envelope(person, frame_of(points)))


@dataclass(frozen=True)
class Candidate:
    """One plan the planner offers: its name, its path and its speed."""

    name: str
    points: tuple[Point, ...]
    speed: float


def candidates(x: float) -> list[Candidate]:
    """The scripted planner's plans from x, in the order it likes them:
    the most progress first. The robot takes the first one the verifier
    accepts."""
    here = (x, LANE_Y)
    goal = (GOAL_X, LANE_Y)
    past = CHILD_X + KEEP + 1.2

    def round_by(y: float) -> tuple[Point, ...]:
        return (here, (x + 0.5, y), (past, y), (past + 1.0, LANE_Y), goal)

    return [
        Candidate("straight on, fast", (here, goal), FAST),
        Candidate("straight on, slowly", (here, goal), SLOW),
        Candidate("stop close, slowly", (here, (CHILD_X - 1.5, LANE_Y)), SLOW),
        Candidate("round by the road", round_by(-5.0), SLOW),
        Candidate("round by the yard", round_by(4.6), SLOW),
        Candidate("stop short, fast", (here, (WAIT_X, LANE_Y)), FAST),
        Candidate("slow down and stop short", (here, (WAIT_X, LANE_Y)), SLOW),
        Candidate("stop at once", (here, (x + 0.6, LANE_Y)), SLOW),
    ]


def _reasons(result) -> tuple[str, ...]:
    return tuple(sorted({str(v.detail.get("invariant")) for v in result.violations}))


def prove() -> dict[str, str]:
    """Ask the real verifier, and fail unless it accepts the safe plans and
    refuses the unsafe ones for the reasons the scene shows. A script calls
    this before it renders one frame."""
    child = (CHILD_X, 0.0)
    through, short = lane(START_X, GOAL_X), lane(START_X, WAIT_X)
    cases = [
        ("fast plan near the child", through, FAST, child,
         ("no_obstacle_penetration", "velocity_bounded")),
        ("slow plan through the zone", through, SLOW, child, ("no_obstacle_penetration",)),
        ("fast plan that stops short", short, FAST, child, ("velocity_bounded",)),
        ("slow plan that stops short", short, SLOW, child, ()),
        ("fast plan, nobody there", through, FAST, None, ()),
    ]  # fmt: skip
    out: dict[str, str] = {}
    for name, points, speed, person, want in cases:
        result = judge(points, speed, person)
        got = () if result.ok else _reasons(result)
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
    speed: float
    ok: bool
    reasons: tuple[str, ...]


@dataclass(frozen=True)
class Frame:
    """What one frame shows. status is cruise, refused, slow, wait or go.
    fan is the judged plans, from the refusal until the robot drives on."""

    t: float
    robot_x: float
    speed: float
    child: Point
    child_seen: bool
    ball: Point
    zone: tuple[tuple[float, float, float], tuple[float, float, float]] | None
    status: str
    fan: tuple[Judged, ...] | None


def child_at(t: float) -> Point:
    """The child's place at time t: across the sidewalk and into the yard."""
    return (CHILD_X, min(CHILD_START_Y + CHILD_SPEED * t, CHILD_END_Y))


def ball_at(t: float) -> Point:
    """The ball rolls ahead of the child and stops in the yard."""
    return (CHILD_X + 0.3, min(CHILD_START_Y + BALL_LEAD + CHILD_SPEED * t, CHILD_END_Y + 0.5))


def child_seen(t: float) -> bool:
    """True once the child is past the parked car and in view of the robot."""
    return child_at(t)[1] > CAR[3]


def timeline(dt: float = 0.2) -> tuple[list[Frame], list[Decision]]:
    """Run the scene and return each frame and each decision of the verifier.

    The robot's state comes only from those decisions. When the verifier
    does not refuse the plan to keep driving at speed, or accepts a plan of
    the fan that this scene does not tell, this raises: the scene never
    draws an answer the verifier did not give."""
    frames: list[Frame] = []
    decisions: list[Decision] = []

    def ask(t: float, name: str, points, speed: float) -> tuple[bool, tuple[str, ...]]:
        result = judge(points, speed, child_at(t) if child_seen(t) else None)
        ok = bool(result.ok)
        reasons = () if ok else _reasons(result)
        decisions.append(Decision(t, name, ok, reasons))
        return ok, reasons

    x, status, fan = START_X, "cruise", None
    for k in range(400):
        t = k * dt
        seen = child_seen(t)
        child = child_at(t)
        speed = 0.0
        if status == "cruise":
            if seen:
                # The plan in hand: keep driving at speed. Is it still allowed?
                if ask(t, "keep driving at speed", lane(x, GOAL_X), FAST)[0]:
                    raise RuntimeError(
                        "the verifier accepted the plan to keep driving at speed near the child; "
                        "this scene does not draw a refusal the verifier did not make"
                    )
                # The planner offers its plans; the verifier judges each one.
                judged = []
                for c in candidates(x):
                    ok, reasons = ask(t, c.name, c.points, c.speed)
                    judged.append(Judged(c.name, c.points, c.speed, ok, reasons))
                fan = tuple(judged)
                taken = next((j.name for j in fan if j.ok), None)
                if taken != "slow down and stop short":
                    raise RuntimeError(
                        f"the first plan the verifier accepted is {taken!r}; this scene tells "
                        "the story of the plan that slows down and stops short"
                    )
                status = "refused"
            else:
                speed = FAST
        elif status == "refused":
            status, speed = "slow", SLOW
        elif status == "slow":
            speed = SLOW
            if x >= WAIT_X - 1e-9:
                status, speed = "wait", 0.0
        elif status == "wait":
            if ask(t, "drive on, slowly", lane(x, GOAL_X), SLOW)[0]:
                status, speed, fan = "go", SLOW, None
        elif status == "go":
            speed = SLOW
        box = zone(child) if seen else None
        frames.append(Frame(t, x, speed, child, seen, ball_at(t), box, status, fan))
        x = min(x + speed * dt, WAIT_X if status == "slow" else GOAL_X)
        if status == "go" and x > CHILD_X + KEEP + 0.4:
            end = t + dt
            last = Frame(end, x, speed, child_at(end), True, ball_at(end), box, status, None)
            frames.append(last)
            break
    else:
        raise RuntimeError("the scene did not finish")
    return frames, decisions
