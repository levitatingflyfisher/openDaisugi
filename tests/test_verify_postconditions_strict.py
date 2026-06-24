"""v0.27.0 fixups — the postcondition loop must reach parity with the invariant
loop (opaque strict-reject, alias resolution, vacuity), and non-strict tautologies
must surface in VerificationResult.warnings (not just the logger).

These tests close fail-open holes the first implementation pass left in the
PARALLEL postcondition path and the tautology-warning path.
"""
from __future__ import annotations

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    Postcondition,
    ShellStep,
)
from opendaisugi.predicate import AliasRef, parse_expression
from opendaisugi.verify import verify


def _plan():
    return ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])


def _env(stakes, *, invariants=None, postconditions=None):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes=stakes,
        invariants=invariants or [],
        postconditions=postconditions or [],
    )


def _contradiction():
    return parse_expression({"op": "forall_steps", "pred": {"op": "and", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]}})


def _tautology():
    return parse_expression({"op": "forall_steps", "pred": {"op": "or", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]}})


# --- opaque postcondition: the same fail-open hole that invariants had ---

def test_opaque_postcondition_rejected_under_strict_high():
    env = _env("high", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=True)])
    result = verify(_plan(), env)
    assert not result.ok
    assert any(v.detail.get("reason") == "opaque_unrecognized"
               and v.detail.get("postcondition") == "no_pii_in_output"
               for v in result.violations)


def test_opaque_postcondition_passes_as_documentation_at_low_stakes():
    env = _env("low", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=True)])
    assert verify(_plan(), env).ok


def test_opaque_postcondition_enforce_false_skipped_even_at_high():
    env = _env("high", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=False)])
    assert verify(_plan(), env).ok


# --- postcondition alias resolution parity ---

def test_postcondition_unresolved_alias_is_violation_not_silent_pass():
    env = _env("high", postconditions=[
        Postcondition(type="no_pii", description="via alias", expr=AliasRef(name="no_pii"))])
    result = verify(_plan(), env)  # no registry passed
    assert not result.ok
    assert any(v.detail.get("reason") == "unresolved_alias" for v in result.violations)


# --- contradiction in a postcondition is a hard error at ANY stakes ---

def test_contradiction_postcondition_hard_error_even_low_stakes():
    env = _env("low", postconditions=[
        Postcondition(type="always_false", description="x", expr=_contradiction(), enforce=True)])
    result = verify(_plan(), env)
    assert not result.ok
    assert any(v.detail.get("reason") == "contradiction"
               and v.detail.get("postcondition") == "always_false"
               for v in result.violations)


# --- tautology warning must reach result.warnings (plan Task 6 contract) ---

def test_nonstrict_tautology_invariant_surfaces_in_warnings():
    env = _env("low", invariants=[
        Invariant(type="useless", description="x", expr=_tautology(), enforce=True)])
    result = verify(_plan(), env)
    assert result.ok, result.violations  # non-strict: tautology is a warning, not a rejection
    assert any("tautolog" in w.lower() for w in result.warnings)


# --- tautology invariant under strict is a rejection (was previously untested) ---

def test_tautology_invariant_rejected_under_strict_high():
    env = _env("high", invariants=[
        Invariant(type="useless", description="x", expr=_tautology(), enforce=True)])
    result = verify(_plan(), env)
    assert not result.ok
    assert any(v.detail.get("reason") == "tautology" for v in result.violations)


# --- the RECOGNIZED_OPAQUE_TYPES carve-out is for INVARIANTS only ---
# check_plan_invariants dispatches envelope.invariants, never postconditions, so a
# recognized-type postcondition is discharged by NOTHING and must be rejected.

def test_recognized_type_as_postcondition_still_rejected_under_strict():
    env = _env("physical", postconditions=[
        Postcondition(type="joint_limits_respected", description="x", expr=None, enforce=True)])
    result = verify(_plan(), env)
    assert not result.ok
    assert any(v.detail.get("reason") == "opaque_unrecognized"
               and v.detail.get("postcondition") == "joint_limits_respected"
               for v in result.violations)


def test_recognized_type_as_invariant_still_passes_at_physical():
    # The carve-out is preserved for invariants (discharged by z3_checks).
    env = _env("physical", invariants=[
        Invariant(type="joint_limits_respected", description="x", expr=None, enforce=True)])
    result = verify(_plan(), env)
    assert not any(v.detail.get("reason") == "opaque_unrecognized" for v in result.violations)
