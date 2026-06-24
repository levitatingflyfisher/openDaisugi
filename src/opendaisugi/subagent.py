"""SafeSubagent — a local-model subagent confined to a verified, delegated scope.

Composes three things openDaisugi already provides into the turnkey pattern a
coworker wants for "safe subagents made from local models":

1. **Delegated scope, proven safe.** ``SafeSubagent.create`` runs
   :func:`verify_delegation` — the subagent's contract envelope must be subsumed
   by the parent's envelope (including the fail-closed robot-capability check).
   If it isn't, creation raises :class:`DelegationDenied`; you cannot mint a
   subagent with more authority than its parent holds.
2. **Per-plan verification.** ``verify`` / ``run`` check every plan against the
   subagent's (subsumed) envelope before anything executes; ``run`` is dry-run by
   default (pass ``live=True`` to use real executors).
3. **Local model = cheap tokens.** ``tier1`` records the local model this
   subagent is configured to reason with (a free-ish local provider). Your agent
   loop uses it to *propose* plans; SafeSubagent's job is the safety gate
   (delegation + per-plan verification), not the generation. It is carried here
   so the subagent's model is part of its identity, and is also threaded into any
   envelope generation you route through this subagent's facade.

This is PLAN-LEVEL runtime assurance — a Python-level gate over structured
plans. It is NOT an OS sandbox; pair it with OS isolation for untrusted code.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING

from opendaisugi.approval import CallbackStrategy
from opendaisugi.contracts import Contract, DelegationDecision, verify_delegation
from opendaisugi.executor import dry_run_executor_map
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify

if TYPE_CHECKING:
    from opendaisugi.journal import Journal
    from opendaisugi.models import ActionPlan, Envelope, VerifyResult
    from opendaisugi.run_session import RunSession
    from opendaisugi.tier1 import Tier1Provider

_log = logging.getLogger("opendaisugi.subagent")


class DelegationDenied(Exception):
    """Raised when a subagent's requested scope is not subsumed by its parent."""

    def __init__(self, reason: str, decision: "DelegationDecision | None" = None) -> None:
        super().__init__(reason)
        self.reason = reason
        self.decision = decision


@dataclass
class SafeSubagent:
    contract: Contract
    parent_envelope: "Envelope"
    decision: DelegationDecision
    tier1: "Tier1Provider | None" = None
    journal: "Journal | None" = None
    strict: bool | None = None

    @classmethod
    def create(
        cls,
        *,
        parent_envelope: "Envelope",
        contract: Contract,
        tier1: "Tier1Provider | None" = None,
        trusted_signers: "list[str] | None" = None,
        strict: bool | None = None,
        journal: "Journal | None" = None,
    ) -> "SafeSubagent":
        """Mint a subagent only if its contract is subsumed by ``parent_envelope``.

        Raises :class:`DelegationDenied` (with the subsumption reason) otherwise —
        the subagent can never hold more authority than its parent grants.
        """
        decision = verify_delegation(
            parent_envelope, contract, trusted_signers=trusted_signers, strict=strict
        )
        if not decision.allowed:
            raise DelegationDenied(decision.reason, decision)
        return cls(
            contract=contract,
            parent_envelope=parent_envelope,
            decision=decision,
            tier1=tier1,
            journal=journal,
            strict=strict,
        )

    @property
    def envelope(self) -> "Envelope":
        """The subagent's operating envelope (its contract's, proven subsumed)."""
        return self.contract.envelope

    def verify(self, plan: "ActionPlan") -> "VerifyResult":
        """Verify a plan against the subagent's subsumed envelope (pure, no I/O)."""
        return verify(plan, self.contract.envelope, strict=self.strict)

    async def run(self, plan: "ActionPlan", *, live: bool = False) -> "RunSession":
        """Verify then execute ``plan`` under the subagent's envelope.

        Dry-run by default — every step routes through a ``DryRunExecutor`` so
        nothing touches the shell/disk/network. Pass ``live=True`` to use real
        executors. The Supervisor re-verifies each step against the envelope
        before executing it, so an out-of-scope plan is rejected regardless.
        """
        executors = None if live else dry_run_executor_map(plan)
        supervisor = Supervisor(
            journal=self.journal,
            approval=CallbackStrategy(lambda step, env: True),
            executors=executors,
            strict=self.strict,
        )
        return await supervisor.run(plan, self.contract.envelope)
