"""Tests for opendaisugi.z3_checks — Z3 satisfiability checks."""

from opendaisugi.models import Envelope, Permission, Postcondition
from opendaisugi.z3_checks import check_envelope_self_consistency


def _envelope(
    permissions: Permission,
    postconditions: list[Postcondition] | None = None,
) -> Envelope:
    return Envelope(
        generated_by="test",
        task="test",
        permissions=permissions,
        postconditions=postconditions or [],
    )


# ----- Self-consistent envelopes -----


def test_consistent_empty_envelope():
    env = _envelope(Permission())
    assert check_envelope_self_consistency(env) == []


def test_consistent_shell_with_allowlist():
    env = _envelope(Permission(shell=True, shell_allowlist=["python3"]))
    assert check_envelope_self_consistency(env) == []


def test_consistent_file_write_with_postcondition():
    env = _envelope(
        Permission(file_write=["out/*.png"]),
        postconditions=[Postcondition(type="file_exists", path="out/chart.png")],
    )
    assert check_envelope_self_consistency(env) == []


# ----- Inconsistent envelopes (shell_allowlist implies shell=True) -----


def test_inconsistent_shell_allowlist_without_shell():
    env = _envelope(Permission(shell=False, shell_allowlist=["python3"]))
    violations = check_envelope_self_consistency(env)
    assert len(violations) == 1
    assert violations[0].stage == "z3"


# ----- Inconsistent envelopes (file_exists postcondition requires file_write) -----


def test_inconsistent_file_exists_without_file_write():
    env = _envelope(
        Permission(file_write=[]),  # empty — no write permission
        postconditions=[Postcondition(type="file_exists", path="out/chart.png")],
    )
    violations = check_envelope_self_consistency(env)
    assert len(violations) == 1
    assert violations[0].stage == "z3"


# ----- Invalid bounds -----


def test_inconsistent_zero_execution_time():
    env = _envelope(Permission(max_execution_time_s=0))
    violations = check_envelope_self_consistency(env)
    assert len(violations) == 1


def test_inconsistent_negative_execution_time():
    env = _envelope(Permission(max_execution_time_s=-5))
    violations = check_envelope_self_consistency(env)
    assert len(violations) == 1


def test_inconsistent_execution_time_above_ceiling():
    env = _envelope(Permission(max_execution_time_s=99999))
    violations = check_envelope_self_consistency(env)
    assert len(violations) == 1


# ----- Plan-vs-envelope checks -----


from opendaisugi.models import ActionPlan, ActionStep, FileWriteStep, ShellStep  # noqa: E402
from opendaisugi.z3_checks import check_plan_against_envelope  # noqa: E402


def _plan(steps: list[ActionStep]) -> ActionPlan:
    return ActionPlan(source="test", task="test", steps=steps)


def test_plan_shell_step_with_shell_permission_passes():
    env = _envelope(Permission(shell=True, shell_allowlist=["python3"]))
    plan = _plan([ShellStep(id="s1", command="python3 chart.py")])
    assert check_plan_against_envelope(plan, env) == []


def test_plan_shell_step_without_shell_permission_fails():
    env = _envelope(Permission(shell=False))
    plan = _plan([ShellStep(id="s1", command="echo hi")])
    violations = check_plan_against_envelope(plan, env)
    assert len(violations) == 1
    assert violations[0].stage == "z3"


def test_plan_file_write_with_write_permission_passes():
    env = _envelope(Permission(file_write=["out/*.png"]))
    plan = _plan([
        FileWriteStep(id="s1", path="out/chart.png", content="x"),
    ])
    assert check_plan_against_envelope(plan, env) == []


def test_plan_file_write_without_write_permission_fails():
    env = _envelope(Permission(file_write=[]))
    plan = _plan([
        FileWriteStep(id="s1", path="out/chart.png", content="x"),
    ])
    violations = check_plan_against_envelope(plan, env)
    assert len(violations) == 1
