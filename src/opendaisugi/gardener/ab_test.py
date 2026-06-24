"""A/B harness: compiled pathway vs fresh Tier-2 (v0.4.0).

Runs the same task through both the compiled pathway (Tier 0) and a
fresh Tier-2 generation, then compares the resulting envelopes. A
match means the pathway is still producing an envelope that is at
least as strict as the frontier would — divergence is a signal that
the pathway has drifted or the task has evolved.

The harness does *not* execute plans. Plan execution is costly and
has side effects; postcondition-set equality + permission-set equality
are strong proxies for "the pathway is still doing the right thing."
Callers who want a stronger check can pass ``judge=`` — an async
callable that takes both envelopes and returns a boolean.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from typing import Awaitable, Callable

from opendaisugi.models import Envelope
from opendaisugi.pathway import CompiledPathway

_log = logging.getLogger("opendaisugi.gardener.ab_test")

Tier2Generator = Callable[[str], Awaitable[Envelope]]
"""Signature: ``async def gen(task: str) -> Envelope``."""

Judge = Callable[[Envelope, Envelope], Awaitable[bool]]
"""Optional LLM-as-a-judge: returns True when the two envelopes are
semantically equivalent. When None, structural comparison is used."""


@dataclass
class ABResult:
    """Outcome of one A/B comparison."""

    pathway_id: str
    task: str
    postconditions_match: bool
    permissions_match: bool
    judge_verdict: bool | None = None
    tier0_latency_ms: float = 0.0
    tier2_latency_ms: float = 0.0
    tier0_tokens: int = 0
    tier2_tokens: int = 0
    notes: list[str] = field(default_factory=list)

    @property
    def passed(self) -> bool:
        """Overall pass: structural match + judge (if provided) agreed."""
        structural = self.postconditions_match and self.permissions_match
        if self.judge_verdict is None:
            return structural
        return structural and self.judge_verdict


def _postconditions_equivalent(a: Envelope, b: Envelope) -> bool:
    """Full-shape order-independent equality.

    v0.28.4: pre-fix this compared only ``(type, repr(expected))`` and
    silently ignored ``path``/``min``/``max``/``expr``/``description`` —
    the gardener's drift-detection pitch failed for the most common
    drift shapes (range bounds, paths, predicate exprs). Now compares
    the full model_dump like ``_permissions_equivalent`` already does.
    """
    import json

    def key(post) -> str:
        return json.dumps(post.model_dump(mode="json"), sort_keys=True)
    return {key(p) for p in a.postconditions} == {key(p) for p in b.postconditions}


def _permissions_equivalent(a: Envelope, b: Envelope) -> bool:
    return a.permissions.model_dump() == b.permissions.model_dump()


async def ab_test(
    pathway: CompiledPathway,
    task: str,
    *,
    tier2_generator: Tier2Generator,
    judge: Judge | None = None,
) -> ABResult:
    """Compare a compiled pathway's envelope against a fresh Tier-2 generation.

    ``tier2_generator`` is injected so tests (and cost-conscious production
    runs) can stub the frontier call. In real deployments, pass a thin
    wrapper around ``generate_envelope(..., tier1=None, pathway_store=None)``.
    """
    # Tier-0: deterministic pathway hit — latency is essentially zero.
    t0_start = time.monotonic()
    tier0_env = pathway.envelope
    tier0_ms = (time.monotonic() - t0_start) * 1000.0

    # Tier-2: full generation.
    t2_start = time.monotonic()
    tier2_env = await tier2_generator(task)
    tier2_ms = (time.monotonic() - t2_start) * 1000.0

    result = ABResult(
        pathway_id=pathway.id,
        task=task,
        postconditions_match=_postconditions_equivalent(tier0_env, tier2_env),
        permissions_match=_permissions_equivalent(tier0_env, tier2_env),
        tier0_latency_ms=tier0_ms,
        tier2_latency_ms=tier2_ms,
        tier0_tokens=0,          # Tier-0 is deterministic
        # v0.28.4: tier2_tokens is a placeholder, not a measured value.
        # Downstream cost analysis MUST NOT treat this as real telemetry —
        # wire ``litellm.token_counter`` against the generator's prompt
        # before relying on it. Tracked as planned work.
        tier2_tokens=0,
    )

    if judge is not None:
        try:
            result.judge_verdict = await judge(tier0_env, tier2_env)
        except Exception as e:
            _log.warning("judge raised %s; falling back to structural verdict", e)
            result.notes.append(f"judge_error: {e.__class__.__name__}")

    return result
