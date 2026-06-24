"""v0.27.0 fixup — verify_delegation must thread strict mode from the caller's
stakes into subsumption. Task 4 added strict= to envelope_subsumes but no caller
passed it, so a high-stakes delegator still got the safety-theater behavior this
release set out to eliminate.
"""
from __future__ import annotations

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import Envelope, Invariant, Permission


def _skill_contract(allowlist, invariants=None) -> Contract:
    return Contract(
        contract_id="c_skill", skill_id="skill", version="0.1.0",
        envelope=Envelope(
            generated_by="skill", task="skill task",
            permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
            invariants=invariants or [],
        ),
        guarantees=["safe"],
    )


def _caller(stakes):
    return Envelope(
        generated_by="orchestrator", task="orchestrate",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        stakes=stakes,
    )


def test_high_stakes_caller_refuses_delegation_to_opaque_invariant_skill():
    # Structurally subsuming (echo ⊆ echo) but the skill declares an opaque,
    # unprovable safety invariant. A high-stakes caller must refuse.
    skill = _skill_contract(["echo"], invariants=[
        Invariant(type="custom_block", description="opaque safety claim", expr=None)])
    d = verify_delegation(_caller("high"), skill)
    assert not d.allowed


def test_low_stakes_caller_still_allows_with_surfaced_unverified():
    skill = _skill_contract(["echo"], invariants=[
        Invariant(type="custom_block", description="opaque safety claim", expr=None)])
    d = verify_delegation(_caller("low"), skill)
    assert d.allowed
    assert "custom_block" in d.unverified_invariants


def test_explicit_strict_overrides_low_stakes_caller():
    skill = _skill_contract(["echo"], invariants=[
        Invariant(type="custom_block", description="opaque safety claim", expr=None)])
    d = verify_delegation(_caller("low"), skill, strict=True)
    assert not d.allowed
