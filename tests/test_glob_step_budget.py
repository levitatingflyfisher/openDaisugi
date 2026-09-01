"""The file-scope glob matcher stops after a fixed number of steps.

`_match_glob` is exponential in the number of `**` segments. It used to run
until the gate's wall-clock verify budget ran out, so whether a glob denied
depended on the speed of the box, and a faster port could allow what the
oracle denied. It now counts its own steps (calls of its inner matcher) and
raises `GlobTooComplex` past `GLOB_MATCH_STEP_LIMIT`, the same count on
every box and in every client.
"""

import pytest

from opendaisugi.models import ActionPlan, Envelope, FileWriteStep, Permission
from opendaisugi.verify import (
    GLOB_MATCH_STEP_LIMIT,
    GlobTooComplex,
    _match_glob,
    _path_matches_any,
    glob_match_steps,
    verify,
)

DEEP = "/" + "/".join(["a"] * 30) + "/b"


def test_the_limit_is_a_fixed_count():
    assert GLOB_MATCH_STEP_LIMIT == 100_000


def test_steps_are_counted_calls_of_the_inner_matcher():
    assert glob_match_steps("/work/a", "/work/*") == 4
    assert glob_match_steps("/work/a/b", "/work/**/b") == 6
    assert glob_match_steps("/work/a", "/work/**") == 0  # the prefix form does not recurse


def test_two_double_stars_stay_under_the_budget():
    assert glob_match_steps(DEEP, "/**/x/**/c") < GLOB_MATCH_STEP_LIMIT
    assert not _match_glob(DEEP, "/**/x/**/c")


def test_many_double_stars_over_the_budget_raise():
    glob = "/" + "/".join(["**"] * 6) + "/c"
    with pytest.raises(GlobTooComplex) as err:
        _match_glob(DEEP, glob)
    assert str(err.value) == (
        f"file glob {glob!r} is too complex to match: more than 100000 steps"
    )


def test_three_double_stars_under_the_budget_still_match():
    glob = "/**/a/**/a/**/b"
    assert glob_match_steps(DEEP, glob) <= GLOB_MATCH_STEP_LIMIT
    assert _match_glob(DEEP, glob)


def test_the_budget_is_per_glob():
    over = "/" + "/".join(["**"] * 6) + "/c"
    with pytest.raises(GlobTooComplex):
        _path_matches_any(DEEP, ["/work/**", over])
    # a glob that matches first ends the search before the costly one
    assert _path_matches_any(DEEP, ["/**", over])


def test_verify_denies_over_the_budget_by_raising():
    env = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(file_write=["/" + "/".join(["**"] * 6) + "/c"]),
    )
    plan = ActionPlan(source="t", task="t", steps=[FileWriteStep(id="s0", path=DEEP, content="")])
    with pytest.raises(GlobTooComplex):
        verify(plan, env)
