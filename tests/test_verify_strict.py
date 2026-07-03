"""v0.27.0 — strict-mode resolution from stakes."""
from __future__ import annotations

from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep
from opendaisugi.verify import resolve_strict, verify


def _env(stakes):
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=["ls"]),
                    stakes=stakes)


def test_strict_defaults_on_for_high_and_physical():
    assert resolve_strict(None, _env("high")) is True
    assert resolve_strict(None, _env("physical")) is True


def test_strict_defaults_off_for_low_and_medium():
    assert resolve_strict(None, _env("low")) is False
    assert resolve_strict(None, _env("medium")) is False


def test_explicit_strict_overrides_stakes():
    assert resolve_strict(True, _env("low")) is True
    assert resolve_strict(False, _env("physical")) is False


def _opaque_env(stakes, inv_type="no_credit_cards"):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        stakes=stakes,
        invariants=[Invariant(type=inv_type, description="custom safety property",
                              expr=None, enforce=True)],
    )


def _plan():
    return ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])


def test_opaque_invariant_rejected_under_strict_high():
    result = verify(_plan(), _opaque_env("high"))
    assert not result.ok
    assert any(v.stage == "predicate" and "no_credit_cards" in v.message
               for v in result.violations)


def test_opaque_invariant_passes_as_documentation_at_low_stakes():
    result = verify(_plan(), _opaque_env("low"))
    assert result.ok, result.violations


def test_opaque_invariant_with_enforce_false_skipped_even_at_high():
    env = _opaque_env("high")
    env.invariants[0].enforce = False
    result = verify(_plan(), env)
    assert result.ok, result.violations
