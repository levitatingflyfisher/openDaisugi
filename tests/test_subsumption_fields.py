"""Subsumption covers every field that grants authority, and fails closed.

``envelope_subsumes`` compared the permission scopes and invariants but not
``custom_step_allowlist``, the two budgets or ``stakes``. So a callee could
list more custom step types than its caller, run longer, write more output,
or lower its stakes to turn strict mode off for itself, and still pass.

A Z3 ``unknown`` in the final check raised ``VerificationTimeout``. Callers
that did not catch it let the raise escape, and ``verify()`` kept it as a
warning and allowed the delegation under lenient mode. It is now a result
that does not hold, and every caller denies it.
"""

from __future__ import annotations

import pytest
import z3

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import ActionPlan, Envelope, Permission, SkillStep
from opendaisugi.subsumption import envelope_subsumes
from opendaisugi.verify import check_skill_delegations, is_z3_timeout_violation, verify


def _env(stakes="low", **perm) -> Envelope:
    p = {"shell": True, "shell_allowlist": ["ls"], **perm}
    return Envelope(generated_by="t", task="t", permissions=Permission(**p), stakes=stakes)


# --- stakes --------------------------------------------------------------------

_ORDER = ["low", "medium", "high", "physical"]


@pytest.mark.parametrize("outer", _ORDER)
@pytest.mark.parametrize("inner", _ORDER)
def test_the_callee_never_lowers_the_stakes(outer, inner):
    r = envelope_subsumes(_env(outer), _env(inner))
    assert r.holds is (_ORDER.index(inner) >= _ORDER.index(outer)), r.reasons
    if not r.holds:
        assert any("stakes" in x for x in r.reasons)


# --- custom step types ---------------------------------------------------------


def test_a_wider_custom_step_list_does_not_hold():
    outer = _env(custom_step_allowlist=["approach_dish"])
    inner = _env(custom_step_allowlist=["approach_dish", "launch"])
    r = envelope_subsumes(outer, inner)
    assert not r.holds
    assert any("custom_step_allowlist" in x and "launch" in x for x in r.reasons)


def test_a_custom_step_list_the_caller_lacks_does_not_hold():
    r = envelope_subsumes(_env(), _env(custom_step_allowlist=["launch"]))
    assert not r.holds


def test_a_narrower_custom_step_list_holds():
    outer = _env(custom_step_allowlist=["a", "b"])
    assert envelope_subsumes(outer, _env(custom_step_allowlist=["b"])).holds
    assert envelope_subsumes(outer, _env()).holds


# --- budgets -------------------------------------------------------------------


@pytest.mark.parametrize("field", ["max_execution_time_s", "max_output_size_mb"])
def test_a_larger_budget_does_not_hold(field):
    r = envelope_subsumes(_env(**{field: 5}), _env(**{field: 6}))
    assert not r.holds
    assert any(field in x for x in r.reasons)


@pytest.mark.parametrize("field", ["max_execution_time_s", "max_output_size_mb"])
def test_an_equal_or_smaller_budget_holds(field):
    assert envelope_subsumes(_env(**{field: 5}), _env(**{field: 5})).holds
    assert envelope_subsumes(_env(**{field: 5}), _env(**{field: 1})).holds


def test_a_default_callee_under_a_tighter_caller_does_not_hold():
    """The defaults are 30 s and 10 MB. A callee that leaves them unset is
    as wide as the defaults, not as narrow as its caller."""
    outer = _env(max_execution_time_s=10)
    assert not envelope_subsumes(outer, _env()).holds


def test_verify_delegation_names_the_field():
    caller = _env("high")
    d = verify_delegation(caller, Contract(contract_id="c", skill_id="s", envelope=_env("low")))
    assert not d.allowed
    assert "stakes" in d.reason


# --- a timeout is a deny, whoever calls -----------------------------------------


@pytest.fixture
def z3_unknown(monkeypatch):
    """Every Z3 check answers unknown, as a real timeout does."""
    monkeypatch.setattr(z3.Solver, "check", lambda self, *a: z3.unknown)


def test_a_timeout_does_not_hold_and_does_not_raise(z3_unknown):
    r = envelope_subsumes(_env(), _env(), timeout_ms=7)
    assert not r.holds
    assert r.timed_out
    assert r.reasons == ["Z3 subsumption check exceeded 7ms"]


def test_verify_delegation_denies_a_timeout(z3_unknown):
    d = verify_delegation(_env(), Contract(contract_id="c", skill_id="s", envelope=_env()))
    assert not d.allowed
    assert "exceeded" in d.reason


def test_safe_subagent_refuses_on_a_timeout(z3_unknown):
    from opendaisugi.subagent import DelegationDenied, SafeSubagent

    with pytest.raises(DelegationDenied):
        SafeSubagent.create(
            parent_envelope=_env(),
            contract=Contract(contract_id="c", skill_id="s", envelope=_env()),
        )


def test_swarm_counts_a_timeout_as_not_subsumed(z3_unknown):
    from opendaisugi.swarm import verify_swarm_tasking

    total = _env()
    report = verify_swarm_tasking(total, {"d1": _env()})
    assert not report.ok
    assert "d1" in report.subsumption_failures


@pytest.mark.parametrize("strict", [False, True])
@pytest.mark.parametrize("stakes", ["low", "medium", "high"])
def test_a_skill_delegation_timeout_is_a_violation_in_every_mode(z3_unknown, strict, stakes):
    caller = _env(stakes)
    step = SkillStep(id="k1", skill_id="s", contract_envelope=_env(stakes))
    plan = ActionPlan(source="t", task="t", steps=[step])
    warnings: list[str] = []
    vs = check_skill_delegations(plan, caller, strict=strict, warnings_out=warnings)
    assert len(vs) == 1 and is_z3_timeout_violation(vs[0])
    assert vs[0].message == (
        "verifier timed out (skill-delegation subsumption); raise the Z3 timeout"
    )
    assert vs[0].detail["z3_message"].startswith("Z3 subsumption check exceeded")
    assert warnings == []
    result = verify(plan, caller, strict=strict)
    assert not result.ok
