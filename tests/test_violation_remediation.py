"""Violations carry suggested_remediation so agents can self-correct (v0.18 L7)."""
from __future__ import annotations
from opendaisugi.models import ActionPlan, Envelope, Invariant, Permission, ShellStep
from opendaisugi.verify import check_permissions, verify


def _env(shell_allowlist):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=shell_allowlist),
    )


def test_metachar_violation_suggests_decomposed_form():
    env = _env(["ls", "cat"])
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="ls dir && cat dir/file"),
    ])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert violations[0].suggested_remediation is not None
    text = violations[0].suggested_remediation.lower()
    assert "shellstep" in text or "depends_on" in text


def test_non_decomposable_metachar_has_no_remediation():
    """Command substitution can't be safely decomposed; no remediation."""
    env = _env(["cat"])
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="cat $(find . -name foo)"),
    ])
    violations = check_permissions(plan, env)
    assert len(violations) == 1
    assert violations[0].suggested_remediation is None


def test_allowlist_miss_has_no_decomposition_remediation():
    """Head not in allowlist is a different failure mode — no decomp suggestion."""
    env = _env(["cat"])
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="rm -rf /tmp/x")])
    violations = check_permissions(plan, env)
    # head not in allowlist; decomposition remediation doesn't apply here
    assert violations[0].suggested_remediation is None


def test_remediation_includes_sequential_depends_on():
    env = _env(["ls", "cat", "echo"])
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="ls && cat foo && echo done"),
    ])
    violations = check_permissions(plan, env)
    assert violations[0].suggested_remediation is not None
    # three parts → three step suggestions with second and third having depends_on
    text = violations[0].suggested_remediation
    # Header "Decompose into sequential ShellSteps" + three ShellStep(...) entries
    assert text.count("ShellStep(") == 3
    assert "depends_on" in text


# ─── Task 11: actionable remediation on strict-mode + alias violations ────────


def _strict_plan():
    return ActionPlan(source="t", task="t", steps=[ShellStep(id="s1", command="ls")])


def test_opaque_rejection_has_remediation():
    """v0.27.0 — opaque_unrecognized violation detail carries suggested_remediation."""
    env = Envelope(generated_by="t", task="t",
                   permissions=Permission(shell=True, shell_allowlist=["ls"]),
                   stakes="high",
                   invariants=[Invariant(type="no_secrets", description="x", expr=None)])
    result = verify(_strict_plan(), env)
    v = next(v for v in result.violations if v.detail.get("reason") == "opaque_unrecognized")
    assert v.detail.get("suggested_remediation")
    assert v.detail.get("invariant") == "no_secrets"
