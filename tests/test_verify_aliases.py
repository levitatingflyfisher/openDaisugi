"""v0.27.0 — core verify() resolves invariant exprs through an AliasRegistry."""
from __future__ import annotations

from opendaisugi.aliases import Alias, AliasRef, AliasRegistry
from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.verify import verify


def _reg():
    reg = AliasRegistry()
    reg.register(Alias(name="only_ls", tier="household",
        expr=parse_expression({"op": "forall_steps",
            "pred": {"op": "equals", "path": "command", "value": "ls"}})))
    return reg


def _env_referencing_alias():
    return Envelope(generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls", "rm"]),
        stakes="high",
        invariants=[Invariant(type="only_ls", description="via alias",
                              expr=AliasRef(name="only_ls"))])


def test_alias_resolved_and_enforced_with_registry():
    bad = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="rm")])
    result = verify(bad, _env_referencing_alias(), aliases=_reg())
    assert not result.ok
    good = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    assert verify(good, _env_referencing_alias(), aliases=_reg()).ok


def test_unresolved_alias_without_registry_is_violation_not_silent_pass():
    bad = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="rm")])
    result = verify(bad, _env_referencing_alias())  # no registry
    assert not result.ok
    assert any("alias" in v.message.lower() for v in result.violations)
