"""Skills as contracts — verifiable delegation between agents.

A ``Contract`` is a skill's published declaration of what it will do: an
envelope (the permissions + invariants it promises to stay within), a JSON
input/output schema, a version, and an optional Ed25519 signature. A
delegating agent verifies a contract by asking:

    "Does my envelope subsume this contract's envelope?"

If yes, the delegation is safe: any plan the skill can legally produce is
also allowed under the caller's own envelope. If no, Z3 returns a
concrete step the skill could emit that would violate the caller's policy
— that step *is* the explanation of why the delegation is unsafe.

This is the other half of the v0.11.0 thesis. A human trusts an LLM
because its plan was verified against an envelope. An LLM trusts a
smaller specialist (a LoRA-distilled 1.5B model, a domain-tuned MCP
tool) because the contract's envelope was proved to fit inside the
caller's envelope. Verification all the way down.

v0.15.0: real Ed25519 signature verification behind the ``[sign]`` extra.
Pass ``trusted_signers=["name", ...]`` to ``verify_delegation`` to have
signatures checked against a persistent :class:`TrustedSignerRegistry`
(or pass the registry directly as ``signer_registry=``).
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any

from pydantic import BaseModel, Field

from opendaisugi.models import Envelope
from opendaisugi.subsumption import Counterexample, SubsumptionResult, envelope_subsumes

_log = logging.getLogger("opendaisugi.contracts")


class Contract(BaseModel):
    """A skill's published envelope contract."""

    contract_id: str
    skill_id: str
    version: str = "0.1.0"
    envelope: Envelope
    input_schema: dict[str, Any] = Field(default_factory=dict)
    output_schema: dict[str, Any] = Field(default_factory=dict)
    guarantees: list[str] = Field(default_factory=list)
    created_at: str | None = None
    signature: str | None = None
    signer: str | None = None


@dataclass
class DelegationDecision:
    allowed: bool
    subsumption: SubsumptionResult
    signature_valid: bool | None
    unverified_invariants: list[str]
    counterexample: Counterexample | None
    reason: str


def _verify_signature(
    contract: Contract,
    trusted_signers: list[str] | None,
    signer_registry: Any | None,
) -> bool | None:
    """Resolve a contract's signature against the caller's trust policy.

    Returns:
        None  — contract has no signature (caller decides whether to trust
                unsigned skills via policy)
        False — signature present but does not verify under any trusted
                signer, or the ``[sign]`` extra is not installed
        True  — signature verified under a registered trusted signer
    """
    if contract.signature is None:
        return None
    if not trusted_signers:
        return False
    try:
        from opendaisugi.signing import (
            SigningUnavailable,
            TrustedSignerRegistry,
            default_registry_path,
        )
    except ImportError:
        return False
    registry = signer_registry
    if registry is None:
        try:
            registry = TrustedSignerRegistry.load(default_registry_path())
        except (ValueError, OSError):
            return False
    try:
        return registry.verify(contract, trusted_signers)
    except SigningUnavailable:
        return False


def verify_delegation(
    caller_envelope: Envelope,
    contract: Contract,
    *,
    trusted_signers: list[str] | None = None,
    signer_registry: Any | None = None,
    timeout_ms: int = 2000,
    strict: bool | None = None,
) -> DelegationDecision:
    """Decide whether ``caller_envelope`` can safely delegate to ``contract``.

    The check is structural (not a run-time assertion): we ask whether
    *any plan the skill could produce* is admissible under the caller's
    envelope. Z3 answers this symbolically and, when the answer is no,
    hands back the concrete step that falsifies the delegation.

    Unverified invariants (those without an ``expr``) are surfaced for the
    caller to inspect. Under strict mode — default-on when the caller's
    ``stakes`` is ``high``/``physical`` (v0.27.0), overridable via ``strict=``
    — a callee declaring opaque, unprovable safety invariants causes
    subsumption to fail rather than merely surfacing them: a high-stakes
    delegator refuses what it cannot verify instead of trusting on faith.
    """
    from opendaisugi.verify import resolve_strict  # local import avoids import cycle

    effective_strict = resolve_strict(strict, caller_envelope)
    sub = envelope_subsumes(
        caller_envelope, contract.envelope, timeout_ms=timeout_ms, strict=effective_strict
    )
    sig_valid = _verify_signature(contract, trusted_signers, signer_registry)

    reasons: list[str] = []
    allowed = True
    if not sub.holds:
        allowed = False
        if sub.counterexample is not None:
            reasons.append(
                f"subsumption failed: inner allows {sub.counterexample.step.command!r} "
                f"but outer rejects via {sub.counterexample.outer_violation}"
            )
        elif sub.reasons:
            # Reason-based refusals (robot-capability, strict-opaque invariants)
            # carry no Z3 counterexample — surface their explanations directly.
            reasons.extend(sub.reasons)
        else:
            reasons.append("subsumption failed: no counterexample produced")
    if sig_valid is False:
        allowed = False
        if not trusted_signers:
            reasons.append("signature present but no trusted_signers supplied to verify against")
        else:
            reasons.append("signature present but does not verify under any trusted signer")
    if sub.unverified_invariants:
        reasons.append(
            f"unverified invariants (no predicate expr): "
            f"{sorted(sub.unverified_invariants)}"
        )
    if not reasons:
        reasons.append("subsumption holds; delegation safe")

    decision = DelegationDecision(
        allowed=allowed,
        subsumption=sub,
        signature_valid=sig_valid,
        unverified_invariants=sub.unverified_invariants,
        counterexample=sub.counterexample,
        reason="; ".join(reasons),
    )
    log_payload = {
        "contract_id": contract.contract_id,
        "skill_id": contract.skill_id,
        "signer": contract.signer,
        "signature_valid": sig_valid,
        "unverified_invariants": sub.unverified_invariants,
    }
    if allowed:
        _log.info("delegation.allow", extra=log_payload)
    else:
        _log.warning("delegation.deny", extra={**log_payload, "reason": decision.reason})
    return decision


__all__ = ["Contract", "DelegationDecision", "verify_delegation"]
