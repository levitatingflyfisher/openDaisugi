"""v0.27.0 hardening — stage2 must resolve postcondition AliasRefs when a registry
is supplied, so alias-backed postconditions are usable through the supervised path
(they previously always produced a spurious fail-closed violation).
"""
from __future__ import annotations

from opendaisugi.aliases import Alias, AliasRef, AliasRegistry
from opendaisugi.models import Envelope, Permission, Postcondition, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.stage2 import verify_completed_step


def _reg():
    reg = AliasRegistry()
    reg.register(Alias(name="cmd_is_ls", tier="household",
        expr=parse_expression({"op": "forall_steps",
            "pred": {"op": "equals", "path": "command", "value": "ls"}})))
    return reg


def _env():
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=["ls"]),
                    postconditions=[Postcondition(type="cmd_is_ls", description="via alias",
                                                  expr=AliasRef(name="cmd_is_ls"), enforce=True)])


def test_stage2_resolves_postcondition_alias_with_registry():
    step = ShellStep(id="s1", command="ls")
    # With the registry the alias resolves and the conforming step passes.
    assert verify_completed_step(step, _env(), aliases=_reg()) == []


def test_stage2_alias_postcondition_enforced_with_registry():
    step = ShellStep(id="s1", command="rm")  # violates cmd_is_ls
    violations = verify_completed_step(step, _env(), aliases=_reg())
    assert any("cmd_is_ls" in v.message for v in violations)


def test_stage2_alias_without_registry_fails_closed():
    step = ShellStep(id="s1", command="ls")
    # No registry: the AliasRef can't resolve — fail closed (a violation), never silent pass.
    assert verify_completed_step(step, _env()) != []
