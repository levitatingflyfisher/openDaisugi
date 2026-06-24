"""Contract + verify_delegation tests (v0.11.0)."""

from __future__ import annotations

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import Envelope, Invariant, Permission


def _contract(skill_id: str, allowlist, invariants=None, signature=None) -> Contract:
    return Contract(
        contract_id=f"c_{skill_id}",
        skill_id=skill_id,
        version="0.1.0",
        envelope=Envelope(
            generated_by=skill_id, task="skill task",
            permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
            invariants=invariants or [],
        ),
        guarantees=["safe"],
        signature=signature,
    )


def test_delegation_allowed_when_skill_envelope_is_narrow():
    caller = Envelope(
        generated_by="orchestrator", task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls", "pytest"]),
    )
    skill = _contract("echo-skill", ["echo"])
    d = verify_delegation(caller, skill)
    assert d.allowed
    assert d.counterexample is None
    assert "safe" in d.reason or "holds" in d.reason


def test_delegation_rejected_when_skill_wider_with_counterexample():
    caller = Envelope(
        generated_by="orchestrator", task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    skill = _contract("dangerous", ["echo", "rm"])
    d = verify_delegation(caller, skill)
    assert not d.allowed
    assert d.counterexample is not None
    assert "rm" in d.reason or "rm" in d.counterexample.step.command


def test_delegation_surfaces_unverified_invariants():
    opaque = Invariant(type="vague", description="no predicate expr")
    caller = Envelope(
        generated_by="o", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        invariants=[opaque],
    )
    skill = _contract("echo", ["echo"])
    d = verify_delegation(caller, skill)
    # Subsumption still passes structurally; the opaque invariant is flagged.
    assert "vague" in d.unverified_invariants
    assert "unverified" in d.reason


def test_delegation_rejects_signed_contract_without_trusted_signers():
    """Signature present but caller supplied no trusted signers ⇒ reject."""
    caller = Envelope(
        generated_by="o", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    skill = _contract("echo", ["echo"], signature="deadbeef")
    d = verify_delegation(caller, skill)
    assert d.signature_valid is False
    assert not d.allowed
    assert "signature" in d.reason.lower()


def test_unsigned_contract_signature_valid_is_none():
    caller = Envelope(
        generated_by="o", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    skill = _contract("echo", ["echo"])
    d = verify_delegation(caller, skill)
    assert d.signature_valid is None
