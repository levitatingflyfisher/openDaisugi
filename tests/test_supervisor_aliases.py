"""v0.27.0 hardening — Supervisor must thread an AliasRegistry into verify(), so
alias-bearing envelopes are usable through the supervised path (not just the
Daisugi facade). Without it, AliasRefs fail closed with no way to resolve them.
"""
from __future__ import annotations

from opendaisugi.aliases import AliasRegistry
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
    Violation,
)
from opendaisugi.supervisor import Supervisor


async def test_supervisor_forwards_aliases_to_verify(monkeypatch):
    captured = {}

    def fake_verify(plan, envelope, *, z3_timeout_ms, strict=None, aliases=None):
        captured["aliases"] = aliases
        # Return not-ok so the run halts right after verify (no executors needed).
        return VerificationResult(
            ok=False,
            violations=[Violation(stage="permissions", message="halt for test")],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=0.0,
        )

    monkeypatch.setattr("opendaisugi.supervisor.verify", fake_verify)
    reg = AliasRegistry()
    sup = Supervisor(aliases=reg)
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["ls"]))
    await sup.run(plan, env)
    assert captured["aliases"] is reg
