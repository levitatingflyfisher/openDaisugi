"""Scene 3, many plans: hundreds of random plans for one task, all checked.

Run: pytest examples/gallery3d -q   (the default suite collects tests/ only)

The picture shows four true counts: checked, refused, accepted, and
breaches among the accepted. The last must be zero. These tests count the
breaches without the verifier, by exact geometry against the true boxes.
"""

import many as m
import patrol as p
import pytest


def test_there_are_hundreds_of_plans_and_the_same_seed_gives_the_same_plans():
    plans = m.random_plans()
    assert len(plans) == m.N >= 300
    assert plans == m.random_plans()
    assert plans != m.random_plans(seed=m.SEED + 1)
    for points in plans:
        assert points[0] == (*p.START, p.ALT) and points[-1] == (*p.GOAL, p.ALT)
        assert 3 <= len(points) <= 5  # the start, one to three places on the way, the goal


def test_the_plans_are_random_not_picked_some_breach_and_some_do_not():
    trials = m.judge_all()
    breaching = [t for t in trials if t.breach]
    assert len(breaching) > m.N // 4, "too few random plans breach to show anything"
    assert len(trials) - len(breaching) > 10, "too few random plans are clean"
    assert {b for t in trials for b in t.breach} == {"fence", "no_fly", "person"}


def test_the_counts_add_up_and_breaches_among_the_accepted_are_zero():
    trials = m.judge_all()
    c = m.counts(trials)
    assert c["checked"] == m.N == len(trials)
    assert c["refused"] + c["accepted"] == c["checked"]
    assert c["accepted"] > 0 and c["refused"] > 0
    assert c["breaches_among_accepted"] == 0
    assert c["accepted"] == sum(1 for t in trials if t.ok)
    # Counted here again, without the module's own counter.
    assert not [t for t in trials if t.ok and t.breach]


def test_a_breach_is_counted_without_the_verifier_by_exact_geometry():
    """The breach of a plan is worked out from its legs and the true boxes:
    a leg that touches the no-fly box or the person's box, or a waypoint
    outside the fence. No sample points, and no call of verify."""
    walker_box = p.person_box(m.PERSON)
    for t in m.judge_all():
        want = set()
        for a, b in zip(t.points, t.points[1:], strict=False):
            if m.leg_hits_box(a, b, p.NO_FLY):
                want.add("no_fly")
            if m.leg_hits_box(a, b, walker_box):
                want.add("person")
        if any(not all(p.FENCE[0][i] <= q[i] <= p.FENCE[1][i] for i in range(3)) for q in t.points):
            want.add("fence")
        assert set(t.breach) == want


@pytest.mark.parametrize(
    "a,b,hits",
    [
        ((0, 0, 5), (10, 0, 5), False),  # passes beside the box
        ((0, 5, 5), (10, 5, 5), True),  # straight through
        ((3.3, 4.5, 5), (4.5, 3.3, 5), False),  # cuts past a corner, 14 cm outside
        ((3.5, 4.7, 5), (4.7, 3.5, 5), True),  # clips the corner, 14 cm inside
        ((5, 5, 5), (5.5, 5.5, 5), True),  # wholly inside
        ((2, 2, 0), (2, 2, 10), False),  # vertical, beside the box
        ((4, 4, 0), (4, 4, 10), True),  # vertical, through its edge
        ((0, 0, 9), (10, 10, 9), False),  # over the top
    ],
)
def test_the_leg_against_box_test_is_exact(a, b, hits):
    box = ((4.0, 4.0, 2.0), (6.0, 6.0, 8.0))
    assert m.leg_hits_box(a, b, box) is hits
    assert m.leg_hits_box(b, a, box) is hits


def test_each_answer_is_the_real_verifiers(monkeypatch):
    trials = m.judge_all()
    for t in trials[:: max(1, len(trials) // 40)]:
        again = p.judge(t.points, m.PERSON)
        assert bool(again.ok) == t.ok
        assert (() if again.ok else p.reasons(again)) == t.reasons


def test_every_refusal_has_a_reason_and_some_are_for_the_margin_alone():
    """A plan the verifier refuses either breaches a true box or passes
    inside the margin the checked box adds. The count of the second kind is
    reported, so the picture does not claim every refusal was a breach."""
    trials = m.judge_all()
    c = m.counts(trials)
    assert all(t.reasons for t in trials if not t.ok)
    assert c["refused_but_clear"] == sum(1 for t in trials if not t.ok and not t.breach)
    assert c["refused_but_clear"] < c["refused"] // 10


def test_prove_passes_and_gives_the_counts_the_picture_shows():
    c = m.prove()
    assert c == m.counts(m.judge_all())


def test_prove_fails_when_the_verifier_lets_a_breach_through(monkeypatch):
    class Yes:
        ok = True
        violations = []

    monkeypatch.setattr(p, "verify", lambda plan, env: Yes())
    with pytest.raises(AssertionError):
        m.prove()


def test_the_batches_show_every_plan_once_in_the_order_it_was_checked():
    trials = m.judge_all()
    steps = m.batches(trials)
    assert steps[0] == 0 and steps[-1] == len(trials)
    assert all(a < b for a, b in zip(steps, steps[1:], strict=False))
    assert 20 <= len(steps) <= 60
