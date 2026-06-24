"""Tests that verify() runs predicate-algebra invariants (v0.9.0)."""

from __future__ import annotations

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Invariant,
    Permission,
    ShellStep,
)
from opendaisugi.predicate import parse_expression
from opendaisugi.verify import verify


def _shell_plan(command: str) -> ActionPlan:
    return ActionPlan(source="test", task="t", steps=[ShellStep(id="s1", command=command)])


def test_predicate_invariant_satisfied_passes():
    plan = _shell_plan("ls")
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["ls"]),
        invariants=[
            Invariant(
                type="must_be_shell",
                description="all steps are shell",
                expr=parse_expression({
                    "op": "forall_steps",
                    "pred": {"op": "equals", "path": "type", "value": "shell"},
                }),
            )
        ],
    )
    result = verify(plan, envelope)
    assert result.ok, result.violations


def test_predicate_invariant_violated_rejects():
    plan = _shell_plan("rm -rf /")
    envelope = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["rm"]),
        invariants=[
            Invariant(
                type="no_destructive",
                description="command must not be destructive",
                expr=parse_expression({
                    "op": "forall_steps",
                    "pred": {"op": "not_matches", "path": "command", "regex": "rm -rf"},
                }),
            )
        ],
    )
    result = verify(plan, envelope)
    assert not result.ok
    assert any("no_destructive" in v.message for v in result.violations)
