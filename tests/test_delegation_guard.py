"""Physical-stakes delegation guard (v0.19 L4)."""
from opendaisugi.models import ActionPlan, Envelope, JointMoveStep, Permission, ShellStep
from opendaisugi.verify import verify


def _env(stakes: str) -> Envelope:
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        stakes=stakes,
    )


def test_physical_stakes_with_preferred_model_rejected():
    """A robotic-stakes envelope refuses to delegate any step to an LLM.
    Joint trajectories cannot be delegated to a model whose arguments
    static verification can't ground."""
    env = _env("physical")
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="s1", joint_targets={"j0": 0.5}, preferred_model="haiku"),
    ])
    vr = verify(plan, env, z3_timeout_ms=200)
    assert vr.ok is False
    assert any("delegation" in v.message.lower() or "physical" in v.message.lower()
               for v in vr.violations)


def test_physical_stakes_without_preferred_model_passes_delegation_check():
    """Robotic-stakes envelope is fine as long as no step requests delegation."""
    env = _env("physical")
    plan = ActionPlan(source="t", task="t", steps=[
        JointMoveStep(id="s1", joint_targets={"j0": 0.5}),
    ])
    vr = verify(plan, env, z3_timeout_ms=200)
    # The delegation guard doesn't reject this. Other stages (e.g. permissions)
    # may still reject for unrelated reasons; we only check the delegation
    # guard didn't fire.
    assert not any("delegation" in v.message.lower() for v in vr.violations)


def test_low_stakes_with_preferred_model_passes():
    """The whole point of delegation: low/medium-stakes plans CAN delegate."""
    env = _env("low")
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi", preferred_model="haiku"),
    ])
    vr = verify(plan, env, z3_timeout_ms=200)
    assert not any("delegation" in v.message.lower() for v in vr.violations)


def test_high_stakes_with_preferred_model_passes():
    """High stakes ≠ physical. Software high-stakes plans can still delegate
    (with whatever envelope-author-provided guards apply); only physical is
    the hard refusal."""
    env = _env("high")
    plan = ActionPlan(source="t", task="t", steps=[
        ShellStep(id="s1", command="echo hi", preferred_model="haiku"),
    ])
    vr = verify(plan, env, z3_timeout_ms=200)
    assert not any("delegation" in v.message.lower() for v in vr.violations)
