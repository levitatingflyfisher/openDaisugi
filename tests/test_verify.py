"""Tests for the verify() orchestration function."""

import pytest

from opendaisugi.models import (
    ActionPlan,
    ActionStep,
    Envelope,
    NetworkStep,
    Permission,
    Postcondition,
    ShellStep,
)
from opendaisugi.verify import check_permissions, verify


def _env(permissions: Permission, postconditions=None) -> Envelope:
    return Envelope(
        generated_by="test",
        task="test",
        permissions=permissions,
        postconditions=postconditions or [],
    )


def _plan(steps: list[ActionStep]) -> ActionPlan:
    return ActionPlan(source="test", task="test", steps=steps)


def test_verify_happy_path():
    env = _env(Permission(shell=True, shell_allowlist=["find"], file_read=["/var/log/**"]))
    plan = _plan([
        ShellStep(id="s1", command="find /var/log -name '*.tmp' -delete"),
    ])
    result = verify(plan, env)
    assert result.ok is True
    assert result.violations == []
    assert result.envelope_id == env.id
    assert result.plan_id == plan.id
    assert result.duration_ms >= 0


def test_verify_permission_failure_short_circuits_before_z3():
    # Shell forbidden — permissions stage fails. z3/dag should not run.
    env = _env(Permission(shell=False))
    plan = _plan([ShellStep(id="s1", command="echo hi")])
    result = verify(plan, env)
    assert result.ok is False
    assert len(result.violations) == 1
    assert result.violations[0].stage == "permissions"


def test_verify_z3_catches_inconsistent_envelope():
    # Envelope is internally inconsistent: file_exists postcondition
    # requires file_write, but file_write is empty.
    env = _env(
        Permission(file_write=[]),
        postconditions=[Postcondition(type="file_exists", path="out.png")],
    )
    plan = _plan([])  # empty plan passes permissions trivially
    result = verify(plan, env)
    assert result.ok is False
    assert any(v.stage == "z3" for v in result.violations)


def test_verify_dag_catches_cycle():
    env = _env(Permission(shell=True, shell_allowlist=["echo"]))
    plan = _plan([
        ShellStep(id="s1", command="echo a", depends_on=["s2"]),
        ShellStep(id="s2", command="echo b", depends_on=["s1"]),
    ])
    result = verify(plan, env)
    assert result.ok is False
    assert any(v.stage == "dag" for v in result.violations)


def test_verify_returns_duration_ms():
    env = _env(Permission())
    plan = _plan([])
    result = verify(plan, env)
    assert isinstance(result.duration_ms, float)
    assert result.duration_ms >= 0


# ----- network_hosts allowlist (v0.1.1) -----


def test_network_hosts_empty_list_allows_any_host():
    env = _env(Permission(network=True, network_hosts=[]))
    plan = _plan([NetworkStep(id="s1", url="https://example.com")])
    assert check_permissions(plan, env) == []


def test_network_hosts_allowlist_allows_matching():
    env = _env(Permission(network=True, network_hosts=["example.com"]))
    plan = _plan([NetworkStep(id="s1", url="https://example.com/foo")])
    assert check_permissions(plan, env) == []


def test_network_hosts_allowlist_blocks_mismatch():
    env = _env(Permission(network=True, network_hosts=["example.com"]))
    plan = _plan([NetworkStep(id="s1", url="https://evil.com/foo")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    msg = violations[0].message
    assert "evil.com" in msg
    assert "not in" in msg or "allowlist" in msg


def test_network_hosts_without_network_permission_still_blocks():
    # network=False should block regardless of host — order of checks matters.
    env = _env(Permission(network=False, network_hosts=["example.com"]))
    plan = _plan([NetworkStep(id="s1", url="https://example.com")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "network" in violations[0].message.lower()
    # Must be the network-permission reason, not the host allowlist reason.
    assert "not in" not in violations[0].message
    assert "allowlist" not in violations[0].message


def test_network_hosts_handles_subdomain_strict_match():
    # Strict host match: api.example.com is not a match for "example.com".
    env = _env(Permission(network=True, network_hosts=["example.com"]))
    plan = _plan([NetworkStep(id="s1", url="https://api.example.com")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "api.example.com" in violations[0].message


def test_network_hosts_allowlist_case_insensitive():
    """Allowlist matching normalizes case so envelope authors don't get bitten by typos."""
    # Envelope with mixed-case allowlist, URL with lowercase host
    env = _env(Permission(network=True, network_hosts=["Example.COM"]))
    plan = _plan([NetworkStep(id="s1", url="https://example.com/ok")])
    assert check_permissions(plan, env) == []


# ----- shell_allowlist glob patterns (v0.17) -----


def test_shell_allowlist_literal_exact_match_still_works():
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="git status")])
    assert check_permissions(plan, env) == []


def test_shell_allowlist_literal_does_not_prefix_match():
    """Backward compat: literal 'git' must NOT match 'gitevil'."""
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="gitevil status")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "gitevil" in violations[0].message


def test_shell_allowlist_glob_matches_venv_bin():
    """v0.17: path-qualified venv launchers should match ``.venv/bin/*``."""
    env = _env(Permission(shell=True, shell_allowlist=[".venv/bin/*"]))
    plan = _plan([
        ShellStep(id="s1", command=".venv/bin/python -V"),
        ShellStep(id="s2", command=".venv/bin/pytest"),
    ])
    assert check_permissions(plan, env) == []


def test_shell_allowlist_glob_does_not_cross_path_separator():
    """Star does NOT span ``/``: ``.venv/bin/*`` must NOT match
    ``.venv/bin/subdir/python`` (prevents sneaking a nested binary
    through by burying it in a subdirectory the allowlist never named).
    """
    env = _env(Permission(shell=True, shell_allowlist=[".venv/bin/*"]))
    plan = _plan([ShellStep(id="s1", command=".venv/bin/subdir/python")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1


def test_shell_allowlist_glob_does_not_match_absolute_path():
    """``.venv/bin/*`` is relative; an absolute path head like
    ``/usr/local/.venv/bin/python`` is a different string and must not match
    a relative pattern.
    """
    env = _env(Permission(shell=True, shell_allowlist=[".venv/bin/*"]))
    plan = _plan([ShellStep(id="s1", command="/usr/local/.venv/bin/python -V")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1


def test_shell_allowlist_glob_mixed_with_literals():
    env = _env(Permission(
        shell=True,
        shell_allowlist=["git", "python", ".venv/bin/*"],
    ))
    plan = _plan([
        ShellStep(id="s1", command="git status"),
        ShellStep(id="s2", command="python --version"),
        ShellStep(id="s3", command=".venv/bin/pytest -v"),
    ])
    assert check_permissions(plan, env) == []


def test_shell_allowlist_glob_still_triggers_metachar_gate():
    """INVARIANT: adding glob semantics must never soften the metachar gate.
    A pipeline is still rejected even when the head matches a glob pattern.
    """
    env = _env(Permission(shell=True, shell_allowlist=[".venv/bin/*"]))
    plan = _plan([
        ShellStep(id="s1", command=".venv/bin/python -c 'x' | tee out.log"),
    ])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "metacharacter" in violations[0].message


# ----- smarter head extraction (v0.17) -----


def test_env_prefix_extracts_real_command_head():
    """``FOO=1 BAR=2 git status`` should look up ``git`` in the allowlist,
    not ``FOO=1``. Real Claude Code transcripts routinely emit env-prefixed
    invocations (``GIT_DISCOVERY_ACROSS_FILESYSTEM=1 git status``).
    """
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([
        ShellStep(id="s1", command="GIT_DISCOVERY_ACROSS_FILESYSTEM=1 git status"),
        ShellStep(id="s2", command="FOO=1 BAR=2 git log"),
    ])
    assert check_permissions(plan, env) == []


def test_env_prefix_still_rejects_unallowlisted_real_head():
    """Even with env prefixes, an unallowlisted real command must reject."""
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="FOO=1 rm -rf /tmp/x")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "'rm'" in violations[0].message


def test_comment_line_is_no_op_no_violation():
    """A shell step whose content is a comment has nothing to execute.
    This can happen when a transcript captures an exploratory comment.
    Return no violation; no command ran.
    """
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="# note: inspect state first")])
    assert check_permissions(plan, env) == []


def test_env_prefix_injection_still_caught_by_metachar_gate():
    """CRITICAL INVARIANT: smarter head extraction must never defeat the
    raw-command metachar gate. ``A=$(rm -rf /) git`` resolves to head ``git``
    under the new classifier, but ``$(`` is still a dangerous metachar on
    the raw command and must still cause rejection.
    """
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="A=$(rm -rf /) git status")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "metacharacter" in violations[0].message


def test_bare_env_assignment_no_command_is_no_op():
    """``FOO=1`` alone is a bare env assignment — no command to run."""
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="FOO=1 BAR=2")])
    assert check_permissions(plan, env) == []


def test_comment_with_metachar_still_rejects():
    """INVARIANT: even a line that *looks* like a comment (starts with #)
    but contains metacharacters gets rejected — we don't trust # to neuter
    everything after it, because a transcript may have reconstructed a
    multi-line command as a single step.
    """
    env = _env(Permission(shell=True, shell_allowlist=["git"]))
    plan = _plan([ShellStep(id="s1", command="# inline note; rm -rf /tmp")])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "metacharacter" in violations[0].message


# v0.28.2 — regression tests for the metachar gate's redirect + newline
# coverage. Prior to v0.28.2 the gate only matched ;, |, &, `, $( — so
# `cat > /etc/passwd` with a `cat`-only allowlist passed verify and got
# handed to SubprocessExecutor(shell=True). See REVIEW_FINDINGS.md C1.
@pytest.mark.parametrize(
    "command",
    [
        "cat > /etc/passwd_hacked",
        "cat >> /etc/passwd_hacked",
        "cat < /etc/shadow",
        "cat 2> /tmp/log",
        "cat <<EOF\nhi\nEOF",
        "echo hi\nrm -rf /tmp",
        "echo hi\rrm -rf /tmp",
    ],
)
def test_redirect_and_newline_metachars_rejected(command):
    env = _env(Permission(shell=True, shell_allowlist=["cat", "echo"]))
    plan = _plan([ShellStep(id="s1", command=command)])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert "metacharacter" in violations[0].message


def test_verify_rejects_non_http_network_scheme():
    from opendaisugi.models import ActionPlan, Envelope, NetworkStep, Permission
    from opendaisugi.verify import verify
    env = Envelope(generated_by="t", task="x", permissions=Permission(network=True, network_hosts=[]))
    for url in ["file:///etc/passwd", "ftp://evil.com/x", "data:text/plain,x"]:
        p = ActionPlan(source="t", task="x", steps=[NetworkStep(id="s", url=url)])
        assert not verify(p, env).ok, url
    # a normal http(s) URL still verifies
    ok = ActionPlan(source="t", task="x", steps=[NetworkStep(id="s", url="https://api.example.com/data")])
    assert verify(ok, env).ok


def test_unknown_custom_step_type_rejected_under_strict():
    # A custom @step_type with an external effect and no permission surface must
    # fail closed under strict mode (high/physical stakes), not pass silently.
    from typing import Literal

    from opendaisugi.models import ActionPlan, Envelope, Permission, StepBase, step_type
    from opendaisugi.verify import verify

    @step_type
    class _DraftEmailStep(StepBase):
        type: Literal["_sgcm_draft_email"] = "_sgcm_draft_email"
        to: str = "x@example.com"

    plan = ActionPlan(source="t", task="x", steps=[_DraftEmailStep(id="s")])
    # low stakes (non-strict) → passes (trust mode)
    low = Envelope(generated_by="t", task="x", permissions=Permission(), stakes="low")
    assert verify(plan, low).ok
    # high stakes (strict) → rejected
    high = Envelope(generated_by="t", task="x", permissions=Permission(), stakes="high")
    r = verify(plan, high)
    assert not r.ok
    assert any("unverifiable step type" in v.message for v in r.violations)
    # explicit strict override also rejects at low stakes
    assert not verify(plan, low, strict=True).ok
