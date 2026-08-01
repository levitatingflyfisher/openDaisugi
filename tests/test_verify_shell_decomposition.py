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


# --- ADR-0014: substitutions verify their inner heads, redirects their targets --


def test_command_substitution_inner_head_checked_and_rejected():
    # $(rm ...) no longer rejects on shape; it rejects because 'rm' is not
    # on the allowlist — the inner head faces the same check as any other.
    result = verify(_plan("echo $(rm -rf /) ok"), _env(_perm(["echo"])))
    assert not result.ok
    assert any("rm" in v.message for v in result.violations)


def test_command_substitution_allowlisted_inner_head_passes():
    result = verify(_plan("echo $(git rev-parse HEAD)"), _env(_perm(["echo", "git"])))
    assert result.ok, result.violations


def test_redirect_write_outside_file_scope_rejected():
    # Harmless head, dangerous write: the envelope has no file_write scope
    # covering /etc, so the redirect target is rejected — by scope, not shape.
    result = verify(_plan("echo x > /etc/cron.d/pwn"), _env(_perm(["echo"])))
    assert not result.ok
    assert any("/etc/cron.d/pwn" in v.message for v in result.violations)


def test_redirect_write_inside_file_scope_passes():
    perm = Permission(
        shell=True,
        shell_allowlist=["sort"],
        shell_allow_decomposition=True,
        file_write=["out/**"],
    )
    result = verify(_plan("sort x > out/sorted.txt"), _env(perm))
    assert result.ok, result.violations


def test_redirect_read_outside_file_scope_rejected():
    result = verify(_plan("wc -l < /etc/shadow"), _env(_perm(["wc"])))
    assert not result.ok
    assert any("/etc/shadow" in v.message for v in result.violations)


def test_redirect_read_inside_file_scope_passes():
    perm = Permission(
        shell=True,
        shell_allowlist=["wc"],
        shell_allow_decomposition=True,
        file_read=["data/**"],
    )
    result = verify(_plan("wc -l < data/in.txt"), _env(perm))
    assert result.ok, result.violations


def test_dev_null_write_is_always_in_scope():
    # `> /dev/null` discards data — it cannot exfiltrate or persist anything,
    # and rejecting it would fail half of real-world shell. Sanctioned sink.
    result = verify(_plan("prog --version > /dev/null 2>&1"), _env(_perm(["prog"])))
    assert result.ok, result.violations


def test_quoted_metacharacter_argument_passes_after_decomposition():
    # The bash grammar proved the pipe-in-quotes is data, so the decomposed
    # part must not be re-rejected by the raw metachar regex.
    result = verify(_plan('grep -E "a|b" f && ls'), _env(_perm(["grep", "ls"])))
    assert result.ok, result.violations


def test_redirect_target_from_variable_rejected():
    result = verify(_plan("echo hi > $OUT"), _env(_perm(["echo"])))
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
