"""Scene 3: many random plans for one task, all checked.

The task is the patrol's flight from one corner of the property to the far
corner (``patrol.py``): a fence of air, a no-fly box over the house, a person
with a keep-out. Hundreds of random plans are made for it, each one is given
to the real verifier, and the picture shows four true counts: checked,
refused, accepted, and breaches among the accepted.

The last count must be zero, and it is not the verifier's own word. Each
plan's breaches are worked out here by exact geometry against the true boxes:
a leg that touches a box, a waypoint outside the fence. ``prove()`` fails
unless no accepted plan has a breach, so a picture that says zero can only be
made when it is true.

The plans are random, not picked. They come from one seeded generator, in
the order it gives them, and none is dropped. Some refusals are for the
margin alone: a plan that clears the true box and passes inside the half
sample gap the checked box adds (see ``patrol.SAMPLE_MARGIN``). They are
counted apart, so the picture does not say every refusal was a breach.

Who makes the plans: a random generator, not a planner and not a learned
policy.
"""

from __future__ import annotations

import random
from dataclasses import dataclass

import patrol

Point = tuple[float, float, float]
Box = tuple[Point, Point]

N = 600  # the plans checked
SEED = 20261110
PERSON = patrol.person_at(0.0)  # the person stands where the patrol scene starts

# Where the random places on the way come from: a box a little larger than
# the fence on every side, so that some plans leave it.
REACH: Box = ((-6.0, -6.0, 3.0), (46.0, 36.0, 14.0))

MAKER = "a random generator"


def random_plans(n: int = N, seed: int = SEED) -> list[tuple[Point, ...]]:
    """n plans from the start to the goal, each through one to three random
    places. The same seed gives the same plans."""
    rng = random.Random(seed)
    start, goal = (*patrol.START, patrol.ALT), (*patrol.GOAL, patrol.ALT)
    plans = []
    for _ in range(n):
        via = [
            tuple(round(rng.uniform(REACH[0][i], REACH[1][i]), 2) for i in range(3))
            for _ in range(rng.randint(1, 3))
        ]
        plans.append((start, *via, goal))
    return plans


def leg_hits_box(a: Point, b: Point, box: Box) -> bool:
    """True when the straight leg from a to b touches the closed box. Exact:
    the leg is cut down to the part inside each pair of faces in turn."""
    t0, t1 = 0.0, 1.0
    for i in range(3):
        lo, hi = box[0][i], box[1][i]
        d = b[i] - a[i]
        if d == 0:
            if a[i] < lo or a[i] > hi:
                return False
            continue
        ta, tb = (lo - a[i]) / d, (hi - a[i]) / d
        if ta > tb:
            ta, tb = tb, ta
        t0, t1 = max(t0, ta), min(t1, tb)
        if t0 > t1:
            return False
    return True


def breaches(points: tuple[Point, ...]) -> tuple[str, ...]:
    """What a plan truly breaches, by geometry alone: fence, no_fly, person."""
    out = set()
    walker_box = patrol.person_box(PERSON)
    for a, b in zip(points, points[1:], strict=False):
        if leg_hits_box(a, b, patrol.NO_FLY):
            out.add("no_fly")
        if leg_hits_box(a, b, walker_box):
            out.add("person")
    lo, hi = patrol.FENCE
    if any(not all(lo[i] <= q[i] <= hi[i] for i in range(3)) for q in points):
        out.add("fence")
    return tuple(sorted(out))


@dataclass(frozen=True)
class Trial:
    """One random plan: the verifier's answer and its true breaches."""

    points: tuple[Point, ...]
    ok: bool
    reasons: tuple[str, ...]
    breach: tuple[str, ...]


def judge_all(n: int = N, seed: int = SEED) -> list[Trial]:
    """Every random plan with the real verifier's answer, in order."""
    trials = []
    for points in random_plans(n, seed):
        result = patrol.judge(points, PERSON)
        ok = bool(result.ok)
        trials.append(Trial(points, ok, () if ok else patrol.reasons(result), breaches(points)))
    return trials


def counts(trials: list[Trial]) -> dict[str, int]:
    """The counts the picture shows, and two beside them for the report."""
    return {
        "checked": len(trials),
        "refused": sum(1 for t in trials if not t.ok),
        "accepted": sum(1 for t in trials if t.ok),
        "breaches_among_accepted": sum(1 for t in trials if t.ok and t.breach),
        "truly_breaching": sum(1 for t in trials if t.breach),
        "refused_but_clear": sum(1 for t in trials if not t.ok and not t.breach),
    }


def prove() -> dict[str, int]:
    """Check every plan and fail unless the picture's counts are true and
    worth showing: hundreds checked, some refused, some accepted, and no
    accepted plan with a breach. A script calls this before it draws."""
    c = counts(judge_all())
    assert c["checked"] == N >= 300, c
    assert c["refused"] > 0 and c["accepted"] > 0, c
    assert c["breaches_among_accepted"] == 0, c
    return c


def batches(trials: list[Trial], frames: int = 40) -> list[int]:
    """How many plans have been checked at each frame of the picture: from
    none to all, in equal steps, in the order they were checked."""
    n = len(trials)
    return sorted({round(n * k / frames) for k in range(frames + 1)})
