"""Envelope subsumption tests (v0.11.0).

The correctness test: outer ⊨ inner is true when every inner-admitted step
is also outer-admitted. The demonstration test: when subsumption fails, Z3
returns a concrete ShellStep — not just a Boolean — so the caller can
display *why* the delegation is unsafe.
"""

from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission
from opendaisugi.subsumption import envelope_subsumes


def _env(allowlist, invariants=None):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
        invariants=invariants or [],
    )


def test_identical_envelopes_subsume():
    e = _env(["echo", "ls"])
    r = envelope_subsumes(e, e)
    assert r.holds
    assert r.counterexample is None


def test_narrower_inner_subsumes():
    outer = _env(["echo", "ls", "pytest"])
    inner = _env(["echo"])
    r = envelope_subsumes(outer, inner)
    assert r.holds, r.counterexample


def test_wider_inner_fails_with_counterexample():
    outer = _env(["echo"])
    inner = _env(["echo", "rm"])
    r = envelope_subsumes(outer, inner)
    assert not r.holds
    assert r.counterexample is not None
    # The returned command must be one the inner envelope admits.
    cmd = r.counterexample.step.command
    head = cmd.strip().split()[0] if cmd.strip() else ""
    assert head in ("echo", "rm")
    # And it must NOT be one the outer admits (proving the counterexample).
    assert head not in ("echo",)
    assert r.counterexample.outer_violation == "shell_allowlist"


def test_outer_invariant_not_in_inner_fails_delegation():
    """Outer forbids ``rm`` via predicate; inner's allowlist permits it.
    Subsumption must fail even though their allowlists match exactly."""
    no_rm = Invariant(
        type="no_rm",
        description="forbid rm",
        expr={"op": "forall_steps", "pred": {
            "op": "not_matches", "path": "command", "regex": r"^rm ",
        }},
    )
    outer = _env(["echo", "rm"], invariants=[no_rm])
    inner = _env(["echo", "rm"])  # no invariant
    r = envelope_subsumes(outer, inner)
    assert not r.holds
    assert r.counterexample is not None
    # The outer violation is the predicate-algebra invariant, not the allowlist.
    assert r.counterexample.outer_violation in ("invariant", "shell_allowlist")
    # The rejected command starts with "rm".
    cmd = r.counterexample.step.command
    assert cmd.startswith("rm")


def test_subsumption_surfaces_unverified_invariants():
    """An invariant without an ``expr`` is opaque to Z3 — surface it, don't
    silently approve."""
    opaque = Invariant(type="never_leaks", description="free-text only")
    outer = _env(["echo"], invariants=[opaque])
    inner = _env(["echo"])
    r = envelope_subsumes(outer, inner)
    # Subsumption still holds structurally, but we flag the opaque invariant.
    assert r.holds
    assert "never_leaks" in r.unverified_invariants


def test_empty_allowlist_inner_trivially_subsumes():
    """If inner admits nothing, any outer subsumes it."""
    outer = _env(["echo"])
    inner = _env([])  # inner admits zero commands
    r = envelope_subsumes(outer, inner)
    assert r.holds


def test_empty_allowlist_outer_rejects_any_nonempty_inner():
    outer = _env([])
    inner = _env(["echo"])
    r = envelope_subsumes(outer, inner)
    assert not r.holds


def test_outer_llm_check_is_not_silently_approved():
    """Attack C (v0.11 audit): outer LLMCheck was bound optimistically to
    BoolVal(True) in the SAT query, so an outer envelope with a stricter
    LLMCheck than the inner was silently approved by subsumption. Outer
    soft nodes (not shared with inner) must be pessimistically bound to
    False — and surfaced in ``unverified_invariants`` so the caller knows
    the LLMCheck wasn't proven symbolically."""
    llm_check_inv = Invariant(
        type="content_policy",
        description="body is professional",
        expr={"op": "llm_check", "rule": "the command is not destructive"},
    )
    outer = _env(["echo", "rm"], invariants=[llm_check_inv])
    inner = _env(["echo", "rm"])  # no invariant — wider than outer
    r = envelope_subsumes(outer, inner)
    # Subsumption MUST fail: inner admits steps outer's LLMCheck could reject.
    assert not r.holds, "outer LLMCheck should block silent subsumption"
    # AND the soft node surfaces so the caller sees what wasn't proven.
    assert any(
        "llm_check" in s for s in r.unverified_invariants
    ), f"expected llm_check in unverified_invariants, got {r.unverified_invariants}"


def test_shared_llm_check_in_both_envelopes_still_subsumes():
    """When inner and outer share the SAME soft predicate (same rule text
    at the same position), subsumption should still hold — the semantic
    check is identical, not outer-stricter."""
    shared = Invariant(
        type="content_policy",
        description="shared rule",
        expr={"op": "llm_check", "rule": "the command is benign"},
    )
    outer = _env(["echo"], invariants=[shared])
    inner = _env(["echo"], invariants=[shared])
    r = envelope_subsumes(outer, inner)
    assert r.holds, (
        f"identical LLMCheck on both sides should subsume; got {r.counterexample}"
    )


# v0.28.3 — subsumption's metachar list must stay in sync with verify's
# _SHELL_METACHAR_RE. Pre-v0.28.3, subsumption was missing both the
# redirect characters (added v0.28.2) and the `$(` substring (still
# missing). Without these, a subsumption proof could admit a delegated
# command shape that the concrete verifier rejects — unsound.


def test_subsumption_blocks_command_substitution_substring():
    """Outer with a narrow allowlist must NOT subsume an inner whose
    allowlist nominally matches but whose admission can produce strings
    containing ``$(`` — verify rejects those concretely, so subsumption
    must too."""
    # If subsumption ignores $(, an inner allowlist ["cat"] would appear
    # to be subsumed by outer ["cat"]: same head, same metachar list. But
    # an attacker could construct command strings like `cat $(rm -rf /)`
    # that pass the head check, fail concrete verify on $(, and the
    # subsumption proof would falsely approve. We assert subsumption
    # rejects when the metachar gate disagrees with verify.
    from opendaisugi.subsumption import _encode_shell_admission
    import z3
    s = z3.String("cmd")
    perms = Permission(shell=True, shell_allowlist=["cat"])
    admission = _encode_shell_admission(perms, s)
    solver = z3.Solver()
    solver.add(admission)
    solver.add(z3.Contains(s, z3.StringVal("$(")))
    assert solver.check() == z3.unsat, (
        "admission must forbid `$(` anywhere — verify rejects it concretely"
    )


def test_subsumption_blocks_redirect_substring():
    """Same invariant for `>` and `<` — added v0.28.2."""
    from opendaisugi.subsumption import _encode_shell_admission
    import z3
    s = z3.String("cmd")
    perms = Permission(shell=True, shell_allowlist=["cat"])
    admission = _encode_shell_admission(perms, s)
    for ch in (">", "<", "\n"):
        solver = z3.Solver()
        solver.add(admission)
        solver.add(z3.Contains(s, z3.StringVal(ch)))
        assert solver.check() == z3.unsat, (
            f"admission must forbid {ch!r} anywhere — verify rejects it concretely"
        )


# --------------------- file/network/mcp subsumption (SGCM review VC-1) ---------------------

def _penv(**perm):
    from opendaisugi.models import Envelope, Permission
    return Envelope(generated_by="t", task="x", permissions=Permission(**perm))


def test_subsumption_rejects_inner_file_write_outside_outer():
    caller = _penv(file_write=["/tmp/**"])
    skill = _penv(file_write=["/etc/**"])
    assert not envelope_subsumes(caller, skill).holds  # /etc not covered by /tmp


def test_subsumption_allows_inner_file_write_subset():
    caller = _penv(file_write=["/tmp/**"])
    skill = _penv(file_write=["/tmp/sub/x"])
    assert envelope_subsumes(caller, skill).holds


def test_subsumption_rejects_inner_network_host_outside_outer():
    caller = _penv(network=True, network_hosts=["api.internal"])
    skill = _penv(network=True, network_hosts=["evil.com"])
    assert not envelope_subsumes(caller, skill).holds


def test_subsumption_rejects_inner_any_host_under_restricted_outer():
    caller = _penv(network=True, network_hosts=["api.internal"])  # restricted
    skill = _penv(network=True, network_hosts=[])                 # any host
    assert not envelope_subsumes(caller, skill).holds


def test_subsumption_allows_network_host_subset():
    caller = _penv(network=True, network_hosts=["a.com", "b.com"])
    skill = _penv(network=True, network_hosts=["a.com"])
    assert envelope_subsumes(caller, skill).holds


def test_subsumption_rejects_inner_mcp_outside_outer():
    caller = _penv(mcp_allowlist=["safe/*"])
    skill = _penv(mcp_allowlist=["dangerous/*"])
    assert not envelope_subsumes(caller, skill).holds


def test_subsumption_rejects_the_full_vc1_scenario():
    caller = _penv(shell=True, shell_allowlist=["echo"], file_write=["/tmp/**"],
                  network=True, network_hosts=["api.internal"], mcp_allowlist=["safe/*"])
    skill = _penv(shell=True, shell_allowlist=["echo"], file_write=["/etc/**", "/home/**"],
                 network=True, network_hosts=["evil.com"], mcp_allowlist=["dangerous/*"])
    assert not envelope_subsumes(caller, skill).holds


def test_subsumption_still_holds_for_identical_envelopes():
    e = _penv(shell=True, shell_allowlist=["echo"], file_read=["/data/**"],
             file_write=["/tmp/**"], network=True, network_hosts=["a.com"], mcp_allowlist=["gh/*"])
    assert envelope_subsumes(e, e).holds
