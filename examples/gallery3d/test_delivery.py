"""Scene 1, the delivery robot and the child: the checks behind the picture.

Run: pytest examples/gallery3d -q   (the default suite collects tests/ only)

Every refusal the scene draws is a real verify() result. These tests hold
the scene to that before any frame is rendered.
"""

import math

import delivery as d
import pytest

CHILD_ON_THE_SIDEWALK = (d.CHILD_X, 0.0)
THROUGH = d.lane(d.START_X, d.GOAL_X)
SHORT = d.lane(d.START_X, d.WAIT_X)


def _reasons(result):
    return {v.detail.get("invariant") for v in result.violations}


def _inside(x, y, box):
    (x0, y0, _), (x1, y1, _) = box
    return x0 <= x <= x1 and y0 <= y <= y1


def _fan(frames):
    return next(f.fan for f in frames if f.fan)


def _every_point(points, per_leg=400):
    """Points along the whole path, far closer together than the samples."""
    for (x0, y0), (x1, y1) in zip(points, points[1:], strict=False):
        for k in range(per_leg + 1):
            yield (x0 + (x1 - x0) * k / per_leg, y0 + (y1 - y0) * k / per_leg)


def test_the_plan_to_keep_driving_at_speed_is_refused_for_both_reasons():
    result = d.judge(THROUGH, d.FAST, CHILD_ON_THE_SIDEWALK)
    assert not result.ok
    assert _reasons(result) == {"no_obstacle_penetration", "velocity_bounded"}


def test_the_slower_plan_that_stops_short_is_accepted():
    assert d.judge(SHORT, d.SLOW, CHILD_ON_THE_SIDEWALK).ok


def test_slow_is_not_enough_a_slow_plan_through_the_zone_is_refused():
    result = d.judge(THROUGH, d.SLOW, CHILD_ON_THE_SIDEWALK)
    assert not result.ok
    assert _reasons(result) == {"no_obstacle_penetration"}


def test_stopping_short_is_not_enough_a_fast_plan_to_the_wait_point_is_refused():
    result = d.judge(SHORT, d.FAST, CHILD_ON_THE_SIDEWALK)
    assert not result.ok
    assert _reasons(result) == {"velocity_bounded"}


def test_with_nobody_on_the_sidewalk_the_fast_plan_is_accepted():
    assert d.judge(THROUGH, d.FAST, None).ok


def test_a_plan_that_leaves_the_sidewalk_is_refused():
    result = d.judge(d.lane(d.START_X, d.SIDEWALK[1][0] + 2.0), d.SLOW, None)
    assert not result.ok
    assert _reasons(result) == {"end_effector_in_workspace"}


def test_prove_runs_the_real_checks_and_passes():
    proof = d.prove()
    assert proof["fast plan near the child"] == "refused: no_obstacle_penetration, velocity_bounded"
    assert proof["slow plan that stops short"] == "accepted"


def test_prove_fails_when_the_verifier_would_accept_the_unsafe_plan(monkeypatch):
    """prove() is the gate before rendering. With a verifier that accepts
    everything it must fail, so a scripted refusal cannot be drawn."""

    class Yes:
        ok = True
        violations = []

    monkeypatch.setattr(d, "verify", lambda plan, env: Yes())
    with pytest.raises(AssertionError):
        d.prove()


def test_the_timeline_cannot_draw_a_refusal_the_verifier_did_not_make(monkeypatch):
    class Yes:
        ok = True
        violations = []

    monkeypatch.setattr(d, "verify", lambda plan, env: Yes())
    with pytest.raises(RuntimeError):
        d.timeline()


def test_no_frame_puts_the_robot_in_the_zone_or_within_2_m_of_the_child():
    frames, _ = d.timeline()
    assert len(frames) > 20
    for f in frames:
        cx, cy = f.child
        gap = math.hypot(f.robot_x - cx, d.LANE_Y - cy) - d.ROBOT_RADIUS
        assert gap >= d.KEEP_AWAY, f"t={f.t:.2f}: the robot's edge is {gap:.2f} m from the child"
        if f.zone is not None:
            assert f.zone == d.zone(f.child)
            assert not _inside(f.robot_x, d.LANE_Y, f.zone), f"t={f.t:.2f}: in the zone"


def test_the_story_is_refuse_then_wait_then_go_and_each_step_is_a_decision():
    frames, decisions = d.timeline()
    order = []
    for f in frames:
        if not order or order[-1] != f.status:
            order.append(f.status)
    assert order == ["cruise", "refused", "slow", "wait", "go"]
    assert not decisions[0].ok and decisions[0].plan == "keep driving at speed"
    assert set(decisions[0].reasons) == {"no_obstacle_penetration", "velocity_bounded"}
    # While it waits, the plan to drive on is asked again and refused by the
    # keep-out alone, until the child is clear.
    waits = [x for x in decisions if x.plan == "drive on, slowly" and not x.ok]
    assert waits and all(x.reasons == ("no_obstacle_penetration",) for x in waits)
    assert decisions[-1].ok and decisions[-1].plan == "drive on, slowly"


def test_the_robot_stops_short_and_only_then_drives_on():
    frames, _ = d.timeline()
    waiting = [f for f in frames if f.status == "wait"]
    assert waiting and all(abs(f.robot_x - d.WAIT_X) < 1e-6 for f in waiting)
    assert frames[-1].robot_x > d.CHILD_X + d.KEEP


# --- the fan of candidate plans (owner's addendum) --------------------------
# The picture draws every plan the planner offered. A refused plan turns red
# and fades. Each drawn plan is a real plan with a real answer.


def test_every_plan_in_the_fan_was_judged_by_the_verifier():
    frames, decisions = d.timeline()
    fan = _fan(frames)
    assert len(fan) >= 8
    judged = {x.plan: x for x in decisions}
    for j in fan:
        assert j.name in judged, f"{j.name} is drawn and was never judged"
        assert judged[j.name].ok == j.ok and judged[j.name].reasons == j.reasons
        # What is drawn is the plan that was judged, point for point.
        again = d.judge(j.points, j.speed, d.child_at(judged[j.name].t))
        assert bool(again.ok) == j.ok, j.name


def test_the_fan_is_refused_for_three_different_reasons_and_two_plans_remain():
    frames, _ = d.timeline()
    fan = {j.name: j for j in _fan(frames)}
    reasons = {r for j in fan.values() for r in j.reasons}
    assert reasons == {"no_obstacle_penetration", "velocity_bounded", "end_effector_in_workspace"}
    assert fan["straight on, fast"].reasons == ("no_obstacle_penetration", "velocity_bounded")
    assert fan["straight on, slowly"].reasons == ("no_obstacle_penetration",)
    assert fan["stop close, slowly"].reasons == ("no_obstacle_penetration",)
    assert fan["round by the road"].reasons == ("end_effector_in_workspace",)
    assert fan["round by the yard"].reasons == ("end_effector_in_workspace",)
    assert fan["stop short, fast"].reasons == ("velocity_bounded",)
    assert sorted(n for n, j in fan.items() if j.ok) == ["slow down and stop short", "stop at once"]


def test_the_robot_takes_the_first_plan_the_verifier_accepts():
    """The planner offers its plans in the order it likes them. The robot
    takes the first one that is accepted, so the choice is the verifier's."""
    frames, _ = d.timeline()
    fan = _fan(frames)
    first_ok = next(j for j in fan if j.ok)
    assert first_ok.name == "slow down and stop short"
    assert all(not j.ok for j in fan[: fan.index(first_ok)])


def test_the_driver_is_named_for_what_it_is():
    assert d.DRIVER == "a scripted planner"
    assert "learned" not in d.DRIVER
    assert d.WORDS["no_obstacle_penetration"] == "enters the keep-out"
    assert d.WORDS["velocity_bounded"] == "too fast near a person"
    assert d.WORDS["end_effector_in_workspace"] == "leaves the sidewalk"


# --- what "proven" rests on (board ruling 6) --------------------------------
# The keep-out check tests 8 sample points on each leg and nothing between
# them. The verifier is given the true zone grown by half the largest gap
# between samples, so a plan with no refusal clears the true zone.


def test_the_checked_box_is_the_true_zone_grown_by_half_the_sample_gap():
    from opendaisugi.z3_checks import OBSTACLE_MIDPOINT_SAMPLES

    assert d.SAMPLES == OBSTACLE_MIDPOINT_SAMPLES == 8
    assert d.SAMPLE_GAP == d.STEP / (d.SAMPLES - 1)
    assert d.SAMPLE_MARGIN == d.SAMPLE_GAP / 2
    lo, hi = d.zone(CHILD_ON_THE_SIDEWALK)
    glo, ghi = d.checked(d.zone(CHILD_ON_THE_SIDEWALK))
    assert all(abs((lo[i] - glo[i]) - d.SAMPLE_MARGIN) < 1e-12 for i in range(3))
    assert all(abs((ghi[i] - hi[i]) - d.SAMPLE_MARGIN) < 1e-12 for i in range(3))
    # The envelope carries the grown box, moved with the plan's frame.
    origin = d.frame_of(THROUGH)
    env = d.envelope(CHILD_ON_THE_SIDEWALK, origin)
    grown = d.checked(d.zone(CHILD_ON_THE_SIDEWALK))
    assert env.permissions.obstacles == [d._moved(grown, origin)]


def test_no_leg_of_any_judged_plan_has_a_sample_gap_wider_than_the_margin_covers():
    """Every leg the verifier samples is at most STEP long, the first one
    included: each plan is checked in its own frame, so no leg runs from the
    world's origin."""
    frames, _ = d.timeline()
    plans = [j.points for j in _fan(frames)] + [THROUGH, SHORT]
    for points in plans:
        dense = d.densify(points)
        legs = [math.dist(a, b) for a, b in zip(dense, dense[1:], strict=False)]
        assert max(legs) <= d.STEP + 1e-9
        assert max(legs) / (d.SAMPLES - 1) / 2 <= d.SAMPLE_MARGIN + 1e-12
        first = d.route(points, d.SLOW).steps[0]
        assert first.target_position == (0.0, 0.0, 0.0)


def test_every_accepted_plan_clears_the_true_zone_and_stays_on_the_sidewalk():
    """Breaches among accepted plans: zero, checked without the verifier,
    at 400 points on each leg against the true zone, not the grown one."""
    frames, decisions = d.timeline()
    when = {x.plan: x.t for x in decisions}
    (sx0, sy0, _), (sx1, sy1, _) = d.SIDEWALK
    accepted = [j for j in _fan(frames) if j.ok]
    assert accepted
    for j in accepted:
        true_zone = d.zone(d.child_at(when[j.name]))
        for x, y in _every_point(j.points):
            assert sx0 <= x <= sx1 and sy0 <= y <= sy1, f"{j.name} leaves the sidewalk"
            assert not _inside(x, y, true_zone), f"{j.name} is in the zone at ({x:.2f}, {y:.2f})"
        assert j.speed <= d.LIMIT_NEAR_PERSON


def test_a_leg_that_clips_a_corner_between_two_samples_is_refused():
    """The case the margin is for. A leg cuts 2 cm off a corner of the true
    zone, between two samples: no sample is inside the true zone, and the
    plan is refused all the same, because a sample is inside the grown one."""
    lo, hi = d.zone(CHILD_ON_THE_SIDEWALK)
    cx, cy = lo[0], hi[1]  # the corner nearest the robot, on the yard side
    # A diagonal through the point 2 cm inside the corner, placed so that
    # the two samples nearest the corner fall 3.2 cm each side of that point.
    a, b = (cx - 0.398, cy - 0.438), (cx + 0.502, cy + 0.462)
    assert math.dist(a, b) > d.STEP  # so the leg is cut in two by densify
    samples = [
        (p[0] + (q[0] - p[0]) * k / (d.SAMPLES - 1), p[1] + (q[1] - p[1]) * k / (d.SAMPLES - 1))
        for p, q in zip(d.densify((a, b)), d.densify((a, b))[1:], strict=False)
        for k in range(d.SAMPLES)
    ]
    clipped = any(_inside(x, y, (lo, hi)) for x, y in _every_point((a, b)))
    assert clipped, "the leg does not enter the true zone; the test is wrong"
    assert not any(_inside(x, y, (lo, hi)) for x, y in samples), "a sample is in the true zone"
    result = d.judge((a, b), d.SLOW, CHILD_ON_THE_SIDEWALK)
    assert "no_obstacle_penetration" in _reasons(result)
    # Given the true zone alone, the verifier passes the same leg: the gap.
    from opendaisugi import verify

    origin = d.frame_of((a, b))
    bare = d.envelope(CHILD_ON_THE_SIDEWALK, origin)
    bare.permissions.obstacles = [d._moved((lo, hi), origin)]
    assert "no_obstacle_penetration" not in _reasons(verify(d.route((a, b), d.SLOW), bare))
