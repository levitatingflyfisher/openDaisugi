"""Transparent command wrappers (ADR-0014).

``timeout 30 CMD``, ``nice -n 10 CMD``, ``nohup CMD``, ``time CMD``,
``stdbuf -o0 CMD``, ``command CMD``, ``setsid CMD``, ``ionice -c2 CMD`` all run
CMD with an adjusted execution context. Before this change none of them were in
``SHELL_INTERPRETERS``, so ``timeout 30 <anything>`` ran <anything> completely
unverified once ``timeout`` was allowlisted — a real hole, closed here by
parsing the wrapped command and recursing, exactly like ``xargs``/``env``.

``sudo`` / ``doas`` / ``watch`` are classified opaque instead: their payload
semantics (privilege change, re-execution loops, ``-i``/``-s`` shells) are not
worth a subtle transparent parse — strict policy rejects them, which is the
fail-closed direction.

With the interpreter layer handling wrappers, the decomposer's blanket wrapper
denylist is gone: a wrapper inside a compound command faces the same allowlist
+ recursive payload check a standalone wrapper faces.
"""

from __future__ import annotations

import pytest

from opendaisugi.interpreter_parse import parse_interpreter
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.verify import verify

# --- parse layer ---------------------------------------------------------------


def test_timeout_extracts_wrapped_command():
    p = parse_interpreter("timeout 30 git fetch --all")
    assert p is not None and not p.opaque
    assert p.inner_commands == ["git fetch --all"]


def test_timeout_value_flags_are_skipped():
    p = parse_interpreter("timeout -k 5 -s TERM 30 make -j4")
    assert p is not None and not p.opaque
    assert p.inner_commands == ["make -j4"]


def test_nice_extracts_wrapped_command():
    p = parse_interpreter("nice -n 10 make")
    assert p is not None and not p.opaque
    assert p.inner_commands == ["make"]


def test_nohup_and_time_and_command_and_setsid_extract():
    for cmd, inner in [
        ("nohup python3 job.py", "python3 job.py"),
        ("time make -j4", "make -j4"),
        ("command ls -la", "ls -la"),
        ("setsid daemon --fg", "daemon --fg"),
    ]:
        p = parse_interpreter(cmd)
        assert p is not None and not p.opaque, cmd
        assert p.inner_commands == [inner], cmd


def test_stdbuf_value_flags_are_skipped():
    p = parse_interpreter("stdbuf -o0 -eL tail -f log")
    assert p is not None and not p.opaque
    assert p.inner_commands == ["tail -f log"]


def test_bare_wrapper_has_no_inner_command():
    p = parse_interpreter("timeout 30")
    assert p is not None
    assert p.inner_commands == []


def test_sudo_doas_watch_are_opaque():
    for cmd in ("sudo rm -rf /", "doas rm x", "watch -n1 date"):
        p = parse_interpreter(cmd)
        assert p is not None and p.opaque, cmd


# --- verify layer: the hole is closed -------------------------------------------


def _env(allowlist, *, decompose=False, policy="surface") -> Envelope:
    return Envelope(
        generated_by="test",
        task="test",
        permissions=Permission(
            shell=True, shell_allowlist=allowlist, shell_allow_decomposition=decompose
        ),
        shell_interpreter_policy=policy,
    )


def _plan(command: str) -> ActionPlan:
    return ActionPlan(source="test", task="test", steps=[ShellStep(id="s1", command=command)])


def test_timeout_payload_faces_the_allowlist():
    # THE closed hole: before, allowlisting 'timeout' ran anything unverified.
    result = verify(_plan("timeout 30 rm -rf /"), _env(["timeout"]))
    assert not result.ok
    assert any("rm" in v.message for v in result.violations)


def test_timeout_allowlisted_payload_passes():
    result = verify(_plan("timeout 30 git fetch"), _env(["timeout", "git"]))
    assert result.ok, result.violations


def test_sudo_strict_rejected_surface_passes():
    strict = verify(_plan("sudo systemctl restart nginx"), _env(["sudo"], policy="strict"))
    assert not strict.ok
    assert any("opaque" in v.message for v in strict.violations)
    surface = verify(_plan("sudo systemctl restart nginx"), _env(["sudo"], policy="surface"))
    assert surface.ok, surface.violations


# --- decomposer: wrappers flow through instead of blanket-rejecting -------------


@pytest.fixture(autouse=False)
def _needs_parser():
    pytest.importorskip("tree_sitter_bash")


def test_wrapper_in_compound_decomposes_and_payload_is_checked(_needs_parser):
    # 'timeout' and 'git' allowlisted, 'make' not: the wrapped git passes,
    # the second command fails — every piece faces the allowlist.
    result = verify(
        _plan("timeout 30 git fetch && make install"),
        _env(["timeout", "git"], decompose=True),
    )
    assert not result.ok
    assert any("make" in v.message for v in result.violations)

    ok = verify(
        _plan("timeout 30 git fetch && make install"),
        _env(["timeout", "git", "make"], decompose=True),
    )
    assert ok.ok, ok.violations


def test_sh_dash_c_in_compound_recursed_not_blanket_rejected(_needs_parser):
    # sh must be allowlisted AND its payload's head must be allowlisted.
    result = verify(
        _plan("sh -c 'rm -rf /' && ls"),
        _env(["sh", "ls"], decompose=True),
    )
    assert not result.ok
    assert any("rm" in v.message for v in result.violations)


def test_xargs_in_compound_payload_checked(_needs_parser):
    result = verify(
        _plan("find . -name '*.tmp' | xargs rm"),
        _env(["find", "xargs"], decompose=True),
    )
    assert not result.ok
    assert any("rm" in v.message for v in result.violations)
