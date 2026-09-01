"""A Z3 timeout (Z3 answered `unknown`) is a Violation under strict mode or
for a physical-stakes envelope, not verify()'s longstanding warning. An
unfinished check must never let a call through. Every timeout here is
forced with a monkeypatch; none depends on real Z3 timing.
"""

from __future__ import annotations

import importlib

import pytest

from opendaisugi.exceptions import VerificationTimeout
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep, SkillStep
from opendaisugi.verify import (
    check_skill_delegations,
    is_z3_timeout_violation,
    is_z3_timeout_warning,
    verify,
    verify_step,
)

_verify_mod = importlib.import_module("opendaisugi.verify")
_contracts_mod = importlib.import_module("opendaisugi.contracts")


def _env(stakes="low", **perm_kwargs) -> Envelope:
    perms = {"shell": True, "shell_allowlist": ["ls"], **perm_kwargs}
    return Envelope(generated_by="t", task="t", permissions=Permission(**perms), stakes=stakes)


def _plan(*steps) -> ActionPlan:
    steps = steps or (ShellStep(id="s1", command="ls"),)
    return ActionPlan(source="t", task="t", steps=list(steps))


def _raiser(message):
    def _raise(*_a, **_kw):
        raise VerificationTimeout(message)

    return _raise


# --------------------------------------------------------------- unit tests


def test_is_z3_timeout_warning_matches_the_shape_verify_uses():
    assert is_z3_timeout_warning("Z3 self-consistency check exceeded 500ms")
    assert not is_z3_timeout_warning("something else entirely")


def test_is_z3_timeout_violation_reads_the_detail_marker():
    from opendaisugi.models import Violation

    timeout_v = Violation(stage="z3", message="x", detail={"reason": "z3_timeout"})
    other_v = Violation(stage="z3", message="x", detail={"reason": "not_subsumed"})
    assert is_z3_timeout_violation(timeout_v)
    assert not is_z3_timeout_violation(other_v)


# ------------------------------------------------- envelope self-consistency


def test_strict_mode_turns_self_consistency_timeout_into_violation(monkeypatch):
    monkeypatch.setattr(
        _verify_mod,
        "check_envelope_self_consistency",
        _raiser("Z3 self-consistency check exceeded 7ms"),
    )
    result = verify(_plan(), _env("high"), strict=True)
    assert not result.ok
    assert not result.warnings
    hits = [v for v in result.violations if v.stage == "z3"]
    assert hits, result.violations
    assert "verifier timed out" in hits[0].message
    assert "envelope self-consistency" in hits[0].message
    assert is_z3_timeout_violation(hits[0])


def test_lenient_default_keeps_self_consistency_timeout_as_warning(monkeypatch):
    monkeypatch.setattr(
        _verify_mod,
        "check_envelope_self_consistency",
        _raiser("Z3 self-consistency check exceeded 7ms"),
    )
    result = verify(_plan(), _env("low"))
    assert result.ok, result.violations
    assert any(is_z3_timeout_warning(w) for w in result.warnings)


# ------------------------------------------------------------ plan-vs-envelope


def test_physical_stakes_forces_violation_even_with_explicit_strict_false(monkeypatch):
    """Physical stakes cannot be waived by an explicit strict=False."""
    monkeypatch.setattr(
        _verify_mod,
        "check_plan_against_envelope",
        _raiser("Z3 plan-vs-envelope check exceeded 5ms"),
    )
    result = verify(_plan(), _env("physical"), strict=False)
    assert not result.ok
    hits = [v for v in result.violations if v.stage == "z3"]
    assert hits, result.violations
    assert "plan-vs-envelope" in hits[0].message


# --------------------------------------------------------- robotics trajectory


def test_lenient_default_keeps_robotics_trajectory_timeout_as_warning(monkeypatch):
    monkeypatch.setattr(
        _verify_mod, "check_plan_invariants", _raiser("Z3 robotics check exceeded 3ms")
    )
    result = verify(_plan(), _env("low"))
    assert result.ok, result.violations
    assert any(is_z3_timeout_warning(w) for w in result.warnings)


def test_strict_mode_turns_robotics_trajectory_timeout_into_violation(monkeypatch):
    monkeypatch.setattr(
        _verify_mod, "check_plan_invariants", _raiser("Z3 robotics check exceeded 3ms")
    )
    result = verify(_plan(), _env("high"), strict=True)
    assert not result.ok
    hits = [v for v in result.violations if v.stage == "z3"]
    assert hits and "robotics trajectory" in hits[0].message


def test_verify_step_physical_stakes_turns_robotics_timeout_into_violation(monkeypatch):
    """verify_step is the per-step hot path a robot's own motion is checked
    against before it runs. It must fail closed the same way verify() does.
    """
    monkeypatch.setattr(
        _verify_mod, "check_plan_invariants", _raiser("Z3 robotics check exceeded 3ms")
    )
    result = verify_step(ShellStep(id="s1", command="ls"), _env("physical"))
    assert not result.ok
    hits = [v for v in result.violations if v.stage == "z3"]
    assert hits, result.violations


# -------------------------------------------------- skill-delegation subsumption


def _skill_plan_and_caller(stakes):
    caller = _env(stakes)
    skill_env = _env("low")
    step = SkillStep(id="k1", skill_id="cleanup", contract_envelope=skill_env)
    return _plan(step), caller


def test_strict_mode_turns_delegation_timeout_into_violation(monkeypatch):
    monkeypatch.setattr(
        _contracts_mod, "verify_delegation", _raiser("Z3 subsumption check exceeded 4ms")
    )
    plan, caller = _skill_plan_and_caller("high")
    result = verify(plan, caller, strict=True)
    assert not result.ok
    hits = [v for v in result.violations if v.stage == "delegation"]
    assert hits, result.violations
    assert "skill-delegation subsumption" in hits[0].message
    assert is_z3_timeout_violation(hits[0])


def test_lenient_delegation_timeout_stays_a_warning(monkeypatch):
    monkeypatch.setattr(
        _contracts_mod, "verify_delegation", _raiser("Z3 subsumption check exceeded 4ms")
    )
    plan, caller = _skill_plan_and_caller("low")
    result = verify(plan, caller, strict=False)
    assert result.ok, result.violations
    assert any(is_z3_timeout_warning(w) for w in result.warnings)


def test_lenient_delegation_timeout_reraises_with_no_warnings_sink(monkeypatch):
    """check_skill_delegations must never drop a timeout it cannot warn
    about: that would be a silent allow."""
    monkeypatch.setattr(
        _contracts_mod, "verify_delegation", _raiser("Z3 subsumption check exceeded 4ms")
    )
    plan, caller = _skill_plan_and_caller("low")
    with pytest.raises(VerificationTimeout):
        check_skill_delegations(plan, caller, strict=False, warnings_out=None)
