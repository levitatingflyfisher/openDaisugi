"""v0.27.0 hardening — strict mode must not make OUTER opaque invariants LESS
visible than non-strict. They were routed into outer_strict_blocking (discarded)
instead of surfacing in unverified_invariants, so strict mode silently hid them.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission
from opendaisugi.subsumption import envelope_subsumes


def _env(invs, allowlist=("echo",)):
    return Envelope(generated_by="t", task="t",
                    permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
                    invariants=invs)


def test_outer_opaque_invariant_surfaces_under_strict():
    outer = _env([Invariant(type="no_external_calls", description="opaque", expr=None)])
    inner = _env([])
    result = envelope_subsumes(outer, inner, strict=True)
    assert result.holds is True  # echo ⊆ echo; outer's own opaque constraint doesn't block
    assert "no_external_calls" in result.unverified_invariants


def test_outer_opaque_visibility_matches_nonstrict():
    outer = _env([Invariant(type="no_external_calls", description="opaque", expr=None)])
    inner = _env([])
    strict_res = envelope_subsumes(outer, inner, strict=True)
    nonstrict_res = envelope_subsumes(outer, inner, strict=False)
    # strict visibility must be a superset of non-strict (never hide what non-strict shows).
    assert set(nonstrict_res.unverified_invariants) <= set(strict_res.unverified_invariants)


def test_inner_hardfail_still_surfaces_outer_opaque():
    # Even when an inner opaque invariant hard-fails delegation, the caller should
    # still see the outer envelope's opaque constraints — not an empty list.
    outer = _env([Invariant(type="outer_guard", description="opaque", expr=None)])
    inner = _env([Invariant(type="inner_block", description="opaque", expr=None)])
    result = envelope_subsumes(outer, inner, strict=True)
    assert result.holds is False  # inner opaque hard-fails
    assert "outer_guard" in result.unverified_invariants
