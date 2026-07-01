"""Acceptance tests for v0.14 semantic recursion (closes audit #333).

v0.13 surfaced the interpreter-escape risk in subsumption and offered a
``strict`` mode that refused subsumption when an interpreter appeared in
the inner allowlist — but Stage 1 ``verify`` still approved
``sh -c "rm -rf /home"`` when ``"sh"`` was allowlisted, because the
command head was literally ``"sh"`` and the regex invariant didn't match
the outer command string.

v0.14 recurses through tractable interpreter payloads at verify time.
``sh -c "rm"`` parses to an inner command ``rm``; that inner command is
checked against the same allowlist, and the violation now fires.

Opaque interpreters (python/perl/ruby/node/awk/sed/make) still cannot be
parsed as shell. Under ``strict`` policy, invoking them is a violation
(we can't prove anything about their payload). Under ``surface`` / ``allow``
they pass verify — v0.13's subsumption-time surfacing is the safety net.
"""

from __future__ import annotations

from opendaisugi.interpreter_parse import InterpreterPayload, parse_interpreter
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verify import check_permissions


def _env(allowlist, *, policy="surface"):
    return Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=list(allowlist)),
        shell_interpreter_policy=policy,
    )


def _plan(command):
    return ActionPlan(
        source="t", task="t",
        steps=[ShellStep(id="s1", command=command)],
    )


# ---------- parser --------------------------------------------------


def test_parse_non_interpreter_returns_none():
    assert parse_interpreter("echo hi") is None
    assert parse_interpreter("ls -la /tmp") is None
    assert parse_interpreter("") is None


def test_parse_sh_c_extracts_payload():
    p = parse_interpreter('sh -c "rm -rf /home"')
    assert isinstance(p, InterpreterPayload)
    assert p.head == "sh"
    assert p.inner_commands == ["rm -rf /home"]
    assert not p.opaque


def test_parse_bash_c_extracts_payload():
    p = parse_interpreter('bash -c "echo hi"')
    assert p is not None and p.inner_commands == ["echo hi"]


def test_parse_sh_without_c_has_no_inner():
    p = parse_interpreter("sh script.sh")
    assert p is not None and p.inner_commands == []
    assert not p.opaque


def test_parse_xargs_extracts_command():
    p = parse_interpreter("xargs -n1 rm -rf")
    assert p is not None and p.inner_commands == ["rm -rf"]


def test_parse_xargs_with_double_dash():
    p = parse_interpreter("xargs -0 -- rm foo")
    assert p is not None and p.inner_commands == ["rm foo"]


def test_parse_find_exec_single():
    p = parse_interpreter("find /tmp -name '*.log' -exec rm {} ;")
    assert p is not None and p.inner_commands == ["rm '{}'"]


def test_parse_find_exec_multiple():
    p = parse_interpreter(
        "find /tmp -exec rm {} ; -name '*.bak' -exec cat {} +"
    )
    assert p is not None and p.inner_commands == ["rm '{}'", "cat '{}'"]


def test_parse_env_strips_vars():
    p = parse_interpreter("env PATH=/bin HOME=/root rm file")
    assert p is not None and p.inner_commands == ["rm file"]


def test_parse_env_i_flag():
    p = parse_interpreter("env -i rm file")
    assert p is not None and p.inner_commands == ["rm file"]


def test_parse_python_is_opaque():
    p = parse_interpreter("python -c 'import os; os.system(\"rm -rf /\")'")
    assert p is not None and p.opaque
    assert p.inner_commands == []


def test_parse_perl_is_opaque():
    p = parse_interpreter('perl -e "system(\'rm\')"')
    assert p is not None and p.opaque


def test_parse_awk_is_opaque():
    p = parse_interpreter("awk 'BEGIN{system(\"rm\")}'")
    assert p is not None and p.opaque


def test_parse_make_is_opaque():
    p = parse_interpreter("make clean")
    assert p is not None and p.opaque


def test_parse_unbalanced_quotes_returns_none():
    assert parse_interpreter('sh -c "unclosed') is None


# ---------- verify integration -------------------------------------


def test_verify_blocks_sh_dash_c_rm():
    """The canonical Attack A/B: ``sh -c "rm ..."`` with ``sh`` allowlisted.
    Pre-v0.14 this passed verify. v0.14 rejects it because the inner
    ``rm`` is not in the allowlist."""
    env = _env(["sh", "echo"])
    plan = _plan('sh -c "rm -rf /home"')
    violations = check_permissions(plan, env)
    assert violations, "expected interpreter recursion to catch rm"
    assert any("rm" in v.message for v in violations)
    # The violation is attributed to a depth>0 check.
    depths = [v.detail.get("depth", 0) for v in violations]
    assert any(d > 0 for d in depths), (
        f"expected a depth>0 violation, got depths={depths}"
    )


def test_verify_allows_sh_c_with_permitted_inner():
    """If the inner command IS allowlisted, recursion passes cleanly."""
    env = _env(["sh", "echo"])
    plan = _plan('sh -c "echo hello"')
    violations = check_permissions(plan, env)
    assert not violations, f"unexpected violations: {violations}"


def test_verify_blocks_nested_sh_c_rm():
    """``bash -c "sh -c 'rm'"`` — two levels deep. v0.14 recurses."""
    env = _env(["bash", "sh", "echo"])
    plan = _plan("""bash -c "sh -c 'rm file'" """)
    violations = check_permissions(plan, env)
    assert violations
    depths = [v.detail.get("depth", 0) for v in violations]
    assert any(d >= 2 for d in depths), (
        f"expected a depth>=2 violation, got depths={depths}"
    )


def test_verify_blocks_xargs_rm():
    """``xargs rm`` when ``rm`` not allowlisted — was the Attack B vector."""
    env = _env(["xargs", "echo"])
    plan = _plan("xargs -n1 rm")
    violations = check_permissions(plan, env)
    assert violations
    assert any("rm" in v.message for v in violations)


def test_verify_blocks_find_exec_rm():
    """``find ... -exec rm {} +`` when ``rm`` not allowlisted.

    (The ``;`` form trips the pre-existing shell-metachar check, so
    ``find -exec ... ;`` was already blocked before v0.14. The ``+``
    form is the real bypass surface v0.14 closes.)
    """
    env = _env(["find"])
    plan = _plan("find /tmp -name '*.log' -exec rm {} +")
    violations = check_permissions(plan, env)
    assert violations
    assert any("rm" in v.message for v in violations)


def test_verify_allows_find_exec_with_permitted_inner():
    """``find ... -exec cat {} +`` when ``cat`` is allowlisted — recursion passes."""
    env = _env(["find", "cat"])
    plan = _plan("find /tmp -name '*.log' -exec cat {} +")
    violations = check_permissions(plan, env)
    assert not violations, f"unexpected violations: {violations}"


def test_verify_blocks_env_rm():
    """``env PATH=/bin rm file`` with only env allowlisted — recurses to rm."""
    env = _env(["env"])
    plan = _plan("env PATH=/bin rm file")
    violations = check_permissions(plan, env)
    assert violations


def test_verify_allows_sh_without_c():
    """``sh script.sh`` is NOT an interpreter-escape vector: the plan
    string is verifiable (head=sh, allowlisted). The script file's
    contents aren't in the plan, and that's true for every allowlisted
    binary. No regression from pre-v0.14."""
    env = _env(["sh"])
    plan = _plan("sh script.sh")
    violations = check_permissions(plan, env)
    assert not violations, f"sh script.sh should still pass: {violations}"


def test_verify_under_strict_blocks_opaque_interpreter():
    """Opaque interpreters (python/perl/...) can't be parsed as shell.
    Under strict policy, invoking them is a violation."""
    env = _env(["python"], policy="strict")
    plan = _plan('python -c "print(1)"')
    violations = check_permissions(plan, env)
    assert violations
    assert any("opaque interpreter" in v.message for v in violations)


def test_verify_under_surface_allows_opaque_interpreter():
    """Under default surface policy, opaque interpreters pass verify —
    subsumption-time surfacing is the warning mechanism, not verify."""
    env = _env(["python"])  # default surface policy
    plan = _plan('python -c "print(1)"')
    violations = check_permissions(plan, env)
    assert not violations, f"surface should allow: {violations}"


def test_verify_under_allow_allows_opaque_interpreter():
    env = _env(["python"], policy="allow")
    plan = _plan('python -c "print(1)"')
    violations = check_permissions(plan, env)
    assert not violations, f"allow should allow: {violations}"


def test_verify_depth_limit_prevents_pathological_nesting(monkeypatch):
    """A recursion bomb — bash wrapping bash wrapping bash … — must hit
    the depth limit rather than do unbounded work. We monkey-patch the
    parser to return an infinite-recursion payload since real shlex
    can't survive 6+ levels of escaping."""
    import sys

    from opendaisugi.interpreter_parse import InterpreterPayload

    verify_mod = sys.modules["opendaisugi.verify"]

    def _always_recurse(cmd):
        if cmd.strip().startswith("bash "):
            return InterpreterPayload(head="bash", inner_commands=["bash payload"])
        return None

    monkeypatch.setattr(verify_mod, "parse_interpreter", _always_recurse)
    env = _env(["bash"])
    plan = _plan("bash payload")
    violations = check_permissions(plan, env)
    assert violations
    assert any("max depth" in viol.message for viol in violations)
