"""Scene 2, the quadrotor property patrol: the checks behind the picture.

Run: pytest examples/gallery3d -q   (the default suite collects tests/ only)

The same flight is shown twice. Without the warden the plan in hand is
flown and cuts across the no-fly box. With the warden that plan is refused
and the first accepted plan of the fan goes round. Every answer is a real
verify() result.
"""

import patrol as p
import pytest


def _inside(point, box):
    (x0, y0, z0), (x1, y1, z1) = box
    x, y, z = point
    return x0 <= x <= x1 and y0 <= y <= y1 and z0 <= z <= z1


def _fan(frames):
    return next(f.fan for f in frames if f.fan)


def test_the_plan_straight_across_is_refused_for_the_no_fly_box():
    result = p.judge(p.straight_across(), p.person_at(0.0))
    assert not result.ok
    assert p.reasons(result) == ("no_fly",)


def test_the_route_round_by_the_north_is_accepted():
    points = next(c.points for c in p.candidates() if c.name == "round by the north")
    assert p.judge(points, p.person_at(0.0)).ok


def test_prove_runs_the_real_checks_and_passes():
    proof = p.prove()
    assert proof["straight across"] == "refused: no_fly"
    assert proof["round by the north"] == "accepted"


def test_prove_fails_when_the_verifier_would_accept_the_unsafe_plan(monkeypatch):
    class Yes:
        ok = True
        violations = []

    monkeypatch.setattr(p, "verify", lambda plan, env: Yes())
    with pytest.raises(AssertionError):
        p.prove()


def test_the_guarded_take_cannot_draw_a_refusal_the_verifier_did_not_make(monkeypatch):
    class Yes:
        ok = True
        violations = []

    monkeypatch.setattr(p, "verify", lambda plan, env: Yes())
    with pytest.raises(RuntimeError):
        p.with_warden()


def test_the_fan_is_refused_for_three_different_reasons_and_two_plans_remain():
    frames, _ = p.with_warden()
    fan = {j.name: j for j in _fan(frames)}
    assert len(fan) == 7
    assert fan["straight across"].reasons == ("no_fly",)
    assert fan["cut the corner by the house"].reasons == ("no_fly",)
    assert fan["round by the south"].reasons == ("person",)
    assert fan["over the roof, above the fence"].reasons == ("fence",)
    assert fan["wide by the south, outside the fence"].reasons == ("fence",)
    assert sorted(n for n, j in fan.items() if j.ok) == ["hover here", "round by the north"]
    assert set(p.WORDS) == {"no_fly", "person", "fence"}


def test_every_plan_in_the_fan_was_judged_and_is_drawn_as_judged():
    frames, decisions = p.with_warden()
    judged = {x.plan: x for x in decisions}
    for j in _fan(frames):
        assert judged[j.name].ok == j.ok and judged[j.name].reasons == j.reasons
        again = p.judge(j.points, p.person_at(judged[j.name].t))
        assert bool(again.ok) == j.ok, j.name


def test_the_drone_takes_the_first_plan_the_verifier_accepts():
    frames, _ = p.with_warden()
    fan = _fan(frames)
    first_ok = next(j for j in fan if j.ok)
    assert first_ok.name == "round by the north"
    assert all(not j.ok for j in fan[: fan.index(first_ok)])


def test_no_accepted_plan_breaches_checked_without_the_verifier():
    """Breaches among accepted plans: zero. Every point of an accepted plan
    is inside the fence and outside both boxes it was judged against."""
    frames, decisions = p.with_warden()
    when = {x.plan: x.t for x in decisions}
    for j in _fan(frames):
        if not j.ok:
            continue
        person_box = p.person_box(p.person_at(when[j.name]))
        for point in p.densify(j.points):
            assert _inside(point, p.FENCE), f"{j.name} leaves the fence at {point}"
            assert not _inside(point, p.NO_FLY), f"{j.name} is in the no-fly box at {point}"
            assert not _inside(point, person_box), f"{j.name} is in the person's keep-out"


def test_with_the_warden_no_frame_is_in_a_box_or_outside_the_fence():
    frames, _ = p.with_warden()
    assert len(frames) > 20
    for f in frames:
        assert _inside(f.drone, p.FENCE), f"t={f.t:.1f}: outside the fence"
        assert not _inside(f.drone, p.NO_FLY), f"t={f.t:.1f}: in the no-fly box"
        assert not _inside(f.drone, p.person_box(f.person)), f"t={f.t:.1f}: in the keep-out"
        assert not f.breach
    assert frames[-1].drone[:2] == p.GOAL


def test_while_it_flies_the_rest_of_the_route_is_asked_again_and_stays_accepted():
    """The person walks, so the keep-out moves. The drone asks about the
    rest of its route on every frame."""
    frames, decisions = p.with_warden()
    again = [x for x in decisions if x.plan == "the rest of the route"]
    assert len(again) >= len(frames) - 3
    assert all(x.ok for x in again)


def test_without_the_warden_the_same_plan_is_flown_and_cuts_the_no_fly_box():
    frames = p.without_warden()
    assert frames[0].drone[:2] == p.START and frames[-1].drone[:2] == p.GOAL
    breaches = [f for f in frames if f.breach]
    assert breaches, "the unguarded flight never enters the no-fly box"
    assert all(_inside(f.drone, p.NO_FLY) for f in breaches)
    # The flight that is drawn is the plan the verifier refuses, not another:
    # every frame is on the straight line from the start to the goal.
    (sx, sy), (gx, gy) = p.START, p.GOAL
    for f in frames:
        x, y, z = f.drone
        assert abs((x - sx) * (gy - sy) - (y - sy) * (gx - sx)) < 1e-6 and z == p.ALT
    assert not p.judge(p.straight_across(), p.person_at(0.0)).ok


def test_the_keep_out_samples_are_closer_together_than_the_boxes_are_wide():
    from opendaisugi.z3_checks import OBSTACLE_MIDPOINT_SAMPLES

    spacing = p.STEP / (OBSTACLE_MIDPOINT_SAMPLES - 1)
    assert spacing < 2 * p.PERSON_KEEP / 10


def test_the_driver_is_named_for_what_it_is():
    assert p.DRIVER == "a scripted planner"


def test_why_each_check_is_made_in_the_plans_own_frame():
    """The obstacle check samples the line from the world's origin to the
    first waypoint. For a machine already past the house, that line crosses
    the no-fly box, so the rest of a clear route is refused in the world
    frame. In a frame whose origin is the plan's first point it is accepted."""
    from opendaisugi import verify

    rest = ((20.0, 26.0, p.ALT), (*p.GOAL, p.ALT))
    origin = (0.0, 0.0, 0.0)
    assert not verify(p.route(rest, origin), p.envelope(None, origin)).ok
    assert p.judge(rest, None).ok


# --- what "proven" rests on (board ruling 6) --------------------------------
# The obstacle check tests 8 sample points on each leg and nothing between
# them. The verifier is given each true box grown by half the largest gap
# between samples, so a plan with no refusal clears the true box.


def _every_point(points, per_leg=400):
    for a, b in zip(points, points[1:], strict=False):
        for k in range(per_leg + 1):
            yield tuple(a[i] + (b[i] - a[i]) * k / per_leg for i in range(3))


def test_the_checked_boxes_are_the_true_boxes_grown_by_half_the_sample_gap():
    from opendaisugi.z3_checks import OBSTACLE_MIDPOINT_SAMPLES

    assert p.SAMPLES == OBSTACLE_MIDPOINT_SAMPLES == 8
    assert p.SAMPLE_GAP == p.STEP / (p.SAMPLES - 1)
    assert p.SAMPLE_MARGIN == p.SAMPLE_GAP / 2
    walker = p.person_at(0.0)
    origin = (*p.START, p.ALT)
    env = p.envelope(walker, origin)
    true_boxes = (p.NO_FLY, p.person_box(walker))
    for true_box, given in zip(true_boxes, env.permissions.obstacles, strict=True):
        grown = p._moved(p.checked(true_box), origin)
        assert given == grown
        lo, hi = true_box
        glo, ghi = p.checked(true_box)
        assert all(abs((lo[i] - glo[i]) - p.SAMPLE_MARGIN) < 1e-12 for i in range(3))
        assert all(abs((ghi[i] - hi[i]) - p.SAMPLE_MARGIN) < 1e-12 for i in range(3))


def test_no_leg_of_any_judged_plan_has_a_sample_gap_wider_than_the_margin_covers():
    import math

    for c in p.candidates():
        dense = p.densify(c.points)
        legs = [math.dist(a, b) for a, b in zip(dense, dense[1:], strict=False)]
        assert max(legs) <= p.STEP + 1e-9
        assert max(legs) / (p.SAMPLES - 1) / 2 <= p.SAMPLE_MARGIN + 1e-12
        # The plan is checked in its own frame: no leg runs from the world's origin.
        assert p.route(c.points).steps[0].target_position == (0.0, 0.0, 0.0)


def test_every_accepted_plan_clears_the_true_boxes_at_every_point():
    """Breaches among accepted plans: zero, checked without the verifier,
    at 400 points on each leg against the true boxes, not the grown ones."""
    frames, decisions = p.with_warden()
    when = {x.plan: x.t for x in decisions}
    accepted = [j for j in _fan(frames) if j.ok]
    assert accepted
    for j in accepted:
        walker_box = p.person_box(p.person_at(when[j.name]))
        for point in _every_point(j.points):
            assert _inside(point, p.FENCE), f"{j.name} leaves the fence"
            assert not _inside(point, p.NO_FLY), f"{j.name} is in the no-fly box"
            assert not _inside(point, walker_box), f"{j.name} is in the person's keep-out"
