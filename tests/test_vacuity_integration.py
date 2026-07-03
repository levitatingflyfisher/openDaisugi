"""v0.27.0 — vacuity verdicts gate alias registration and invariant compilation."""
from __future__ import annotations

import pytest

from opendaisugi.aliases import Alias, AliasRegistry, VacuousAliasError
from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.verify import verify


def _contradiction():
    return parse_expression({"op": "forall_steps", "pred": {"op": "and", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]}})


def test_contradiction_invariant_is_hard_error_even_at_low_stakes():
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["ls"]),
                   stakes="low",
                   invariants=[Invariant(type="always_false", description="x",
                                         expr=_contradiction(), enforce=True)])
    result = verify(plan, env)
    assert not result.ok
    assert any("never be satisfied" in v.message or "unsatisfiable" in v.message
               for v in result.violations)


def test_tautological_alias_rejected_on_register():
    reg = AliasRegistry()
    taut = parse_expression({"op": "or", "children": [
        {"op": "equals", "path": "type", "value": "shell"},
        {"op": "not_equals", "path": "type", "value": "shell"}]})
    with pytest.raises(VacuousAliasError):
        reg.register(Alias(name="useless", expr=taut, tier="household"))
