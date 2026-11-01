"""An opaque invariant nothing can check is never a silent pass (SG-8)."""

import pytest

from opendaisugi import dialect
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    Postcondition,
    opaque_unrecognized,
)
from opendaisugi.verify import verify


def env(stakes="medium", invariants=(), postconditions=()):
    return Envelope(
        generated_by="t",
        task="t",
        stakes=stakes,
        permissions=Permission(shell=True, shell_allowlist=["ls"], file_read=["**"]),
        invariants=list(invariants),
        postconditions=list(postconditions),
    )


PLAN = ActionPlan(source="t", task="t", steps=[{"id": "s1", "type": "shell", "command": "ls"}])


@pytest.mark.parametrize("stakes", ["low", "medium", "high"])
def test_unrecognized_opaque_is_a_violation_at_every_stakes(stakes):
    r = verify(PLAN, env(stakes, [Invariant(type="no_side_effects", description="d")]))
    assert not r.ok
    v = r.violations[0]
    assert v.detail["reason"] == "opaque_unrecognized"
    assert v.message == (
        "invariant 'no_side_effects' declares a safety property with no verifiable expr "
        "and no definition; nothing can discharge it"
    )


def test_enforce_false_is_documentation():
    r = verify(
        PLAN, env("medium", [Invariant(type="no_side_effects", description="d", enforce=False)])
    )
    assert r.ok and r.warnings == []


def test_word_stays_audited_below_strict():
    word_env = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(file_write=["**"]),
        invariants=[Invariant(type="file_unchanged", target="/etc/**", description="d")],
    )
    plan = ActionPlan(
        source="t",
        task="t",
        steps=[{"id": "s1", "type": "file_write", "path": "/etc/x", "content": "x"}],
    )
    r = verify(plan, word_env)
    assert r.ok and any(w.startswith(dialect.AUDIT_PREFIX) for w in r.warnings)


def test_word_rejected_at_strict_without_pin_as_before():
    r = verify(
        PLAN, env("high", [Invariant(type="file_unchanged", target="/etc/**", description="d")])
    )
    assert not r.ok and r.violations[0].detail["reason"] == "opaque_unrecognized"


def test_opaque_unrecognized_names_the_items():
    e = env(
        "low",
        [
            Invariant(type="no_side_effects", description="d"),
            Invariant(type="file_unchanged", description="d"),
            Invariant(type="joint_limits_respected", description="d"),
        ],
        [Postcondition(type="exit_code", expected=0), Postcondition(type="tests_pass")],
    )
    assert opaque_unrecognized(e) == ["no_side_effects", "tests_pass"]


def test_demote_unchecked_turns_protection_into_documentation():
    from opendaisugi.envelope import demote_unchecked

    e = env(
        "medium",
        [
            Invariant(type="no_side_effects", description="d"),
            Invariant(type="file_unchanged", description="d"),
        ],
        [Postcondition(type="tests_pass")],
    )
    out, warnings = demote_unchecked(e)
    assert [i.enforce for i in out.invariants] == [False, True]
    assert [p.enforce for p in out.postconditions] == [False]
    assert warnings == [
        "invariant 'no_side_effects' is documentation only: nothing can check it",
        "postcondition 'tests_pass' is documentation only: nothing can check it",
    ]
    assert opaque_unrecognized(out) == []
    assert e.invariants[0].enforce is True


def test_demote_leaves_a_clean_envelope_alone():
    from opendaisugi.envelope import demote_unchecked

    e = env("medium", [Invariant(type="file_unchanged", description="d")])
    out, warnings = demote_unchecked(e)
    assert out is e and warnings == []
