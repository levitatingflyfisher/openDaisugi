"""v0.27.0 — strict subsumption hard-fails on unprovable opaque inner invariants."""
from __future__ import annotations
from opendaisugi.models import Envelope, Invariant, Permission
from opendaisugi.subsumption import envelope_subsumes


def _env(invs, stakes="high"):
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=["ls"]),
                    stakes=stakes, invariants=invs)


def test_strict_subsumption_fails_on_opaque_inner_invariant():
    outer = _env([])
    inner = _env([Invariant(type="custom_block", description="x", expr=None)])
    result = envelope_subsumes(outer, inner, strict=True)
    assert result.holds is False
    assert any("custom_block" in r for r in result.reasons)


def test_nonstrict_subsumption_surfaces_opaque_as_warning():
    outer = _env([])
    inner = _env([Invariant(type="custom_block", description="x", expr=None)])
    result = envelope_subsumes(outer, inner, strict=False)
    assert result.holds is True
    assert "custom_block" in result.unverified_invariants


def test_strict_subsumption_allows_recognized_robotics_opaque():
    outer = _env([])
    inner = _env([Invariant(type="joint_limits_respected", description="x", expr=None)])
    result = envelope_subsumes(outer, inner, strict=True)
    assert result.holds is True
