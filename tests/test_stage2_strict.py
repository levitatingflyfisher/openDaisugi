"""v0.27.0 hardening — stage2 (the runtime gate before effects commit) must not
silently pass opaque enforced postconditions under strict mode. Pre-existing
fail-open: verify.py was hardened but the parallel stage2 path was not.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, Permission, Postcondition, ShellStep
from opendaisugi.stage2 import verify_completed_step


def _env(stakes, *, postconditions):
    return Envelope(generated_by="t", task="t", permissions=Permission(),
                    stakes=stakes, postconditions=postconditions)


def _step():
    return ShellStep(id="s1", command="ls")


def test_stage2_rejects_opaque_postcondition_under_strict():
    env = _env("high", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=True)])
    violations = verify_completed_step(_step(), env)
    assert any(v.detail.get("reason") == "opaque_unrecognized" for v in violations)


def test_stage2_allows_opaque_postcondition_at_low_stakes():
    env = _env("low", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=True)])
    assert verify_completed_step(_step(), env) == []


def test_stage2_skips_opaque_postcondition_enforce_false():
    env = _env("physical", postconditions=[
        Postcondition(type="no_pii_in_output", description="custom", expr=None, enforce=False)])
    assert verify_completed_step(_step(), env) == []


# v0.28.3 — concrete handlers for the three opaque postcondition types the
# envelope few-shot prompt teaches the LLM to author. Pre-v0.28.3 these
# silently passed at non-strict and raised "no verifiable expr" at strict.


def test_stage2_exit_code_pass_at_low_stakes():
    env = _env("low", postconditions=[Postcondition(type="exit_code", expected=0)])
    step = ShellStep(id="s1", command="ls", metadata={"rc": 0})
    assert verify_completed_step(step, env) == []


def test_stage2_exit_code_fail_at_low_stakes():
    env = _env("low", postconditions=[Postcondition(type="exit_code", expected=0)])
    step = ShellStep(id="s1", command="ls", metadata={"rc": 1})
    violations = verify_completed_step(step, env)
    assert len(violations) == 1
    assert violations[0].detail["observed_rc"] == 1


def test_stage2_file_exists_pass(tmp_path):
    p = tmp_path / "out.png"
    p.write_bytes(b"x" * 200)
    env = _env("low", postconditions=[Postcondition(type="file_exists", path=str(p))])
    assert verify_completed_step(_step(), env) == []


def test_stage2_file_exists_fail(tmp_path):
    missing = tmp_path / "absent.png"
    env = _env("low", postconditions=[Postcondition(type="file_exists", path=str(missing))])
    violations = verify_completed_step(_step(), env)
    assert len(violations) == 1
    assert violations[0].detail["exists"] is False


def test_stage2_file_size_range_pass(tmp_path):
    p = tmp_path / "out.png"
    p.write_bytes(b"x" * 500)
    env = _env("low", postconditions=[
        Postcondition(type="file_size_range", path=str(p), min=100, max=10_000)])
    assert verify_completed_step(_step(), env) == []


def test_stage2_file_size_range_fail_too_small(tmp_path):
    p = tmp_path / "out.png"
    p.write_bytes(b"x" * 50)
    env = _env("low", postconditions=[
        Postcondition(type="file_size_range", path=str(p), min=100, max=10_000)])
    violations = verify_completed_step(_step(), env)
    assert len(violations) == 1
    assert violations[0].detail["size"] == 50


def test_stage2_known_opaque_postcondition_runs_handler_at_low_stakes():
    """Regression: pre-v0.28.3, opaque postconditions at low stakes were
    silent passes. Even with no expr, exit_code now actually checks rc."""
    env = _env("low", postconditions=[Postcondition(type="exit_code", expected=0)])
    step = ShellStep(id="s1", command="ls", metadata={"rc": 1})
    assert verify_completed_step(step, env) != []
