"""Verifier integration for opt-in compound-shell decomposition (v0.40)."""

from __future__ import annotations

import pytest

pytest.importorskip("tree_sitter_bash")

from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verify import verify


def _env(perm: Permission, *, policy: str = "surface") -> Envelope:
    return Envelope(
        generated_by="test", task="test", permissions=perm, shell_interpreter_policy=policy
    )


def _plan(command: str) -> ActionPlan:
    return ActionPlan(source="test", task="test", steps=[ShellStep(id="s1", command=command)])


def _perm(allowlist, *, decompose=True) -> Permission:
    return Permission(shell=True, shell_allowlist=allowlist, shell_allow_decomposition=decompose)


# --- opt-in: safe compound of allowlisted heads now verifies --------------------


def test_pipe_of_allowlisted_heads_passes_when_opted_in():
    result = verify(_plan("grep -r TODO src | head -20"), _env(_perm(["grep", "head"])))
    assert result.ok, result.violations


def test_newline_joined_allowlisted_heads_pass():
    result = verify(_plan("cd x\nsed -n 1,5p f"), _env(_perm(["cd", "sed"])))
    assert result.ok, result.violations


# --- opt-in still catches the smuggle: EVERY head is checked --------------------


def test_semicolon_smuggle_is_caught_second_head_not_allowlisted():
    result = verify(_plan("git status; rm -rf /"), _env(_perm(["git"])))
    assert not result.ok
    assert any("rm" in v.message for v in result.violations)


# --- default (not opted in): behaviour is exactly the old blanket rejection -----


def test_default_off_still_blanket_rejects_compound():
    result = verify(_plan("grep x f | head"), _env(_perm(["grep", "head"], decompose=False)))
    assert not result.ok
    assert any("metacharacter" in v.message for v in result.violations)


# --- fail-closed: unsafe constructs are rejected even when opted in --------------


def test_command_substitution_rejected_even_when_opted_in():
    result = verify(_plan("echo $(rm -rf /) ok"), _env(_perm(["echo"])))
    assert not result.ok


def test_redirection_rejected_even_when_opted_in():
    result = verify(_plan("echo x > /etc/cron.d/pwn"), _env(_perm(["echo"])))
    assert not result.ok


# --- opt-in must NOT weaken strict opaque-interpreter policy ---------------------


def test_decomposition_preserves_strict_opaque_interpreter_rejection():
    # 'sed' is an opaque interpreter; under strict policy a standalone `sed`
    # step is rejected, and decomposition must not launder it into an allow.
    result = verify(
        _plan("grep x f | sed -n 1,5p f"),
        _env(_perm(["grep", "sed"]), policy="strict"),
    )
    assert not result.ok
    assert any("opaque" in v.message for v in result.violations)
