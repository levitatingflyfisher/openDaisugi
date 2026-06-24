"""SafeSubagent — a local-model subagent confined to a verified, delegated scope (v0.31).

'Safe' = (1) the subagent's scope is proven to fit inside the parent's authority
via verify_delegation (subsumption, including the fail-closed robot-capability
check), refused at creation otherwise; and (2) every plan it runs is verified
against that scope before execution, dry-run by default. 'From a local model' =
an optional local Tier-1 provider (cheap/free tokens) it can use to generate
envelopes. This is PLAN-LEVEL runtime assurance, NOT an OS sandbox.
"""

import asyncio

import pytest

from opendaisugi.contracts import Contract
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.subagent import DelegationDenied, SafeSubagent


def _env(**k) -> Envelope:
    return Envelope(generated_by="t", task="t", permissions=Permission(**k))


def _contract(env, *, id="c1", skill="sub") -> Contract:
    return Contract(contract_id=id, skill_id=skill, envelope=env)


def test_create_allows_when_parent_subsumes_subagent():
    parent = _env(shell=True, shell_allowlist=["echo", "ls"])
    sub = SafeSubagent.create(
        parent_envelope=parent, contract=_contract(_env(shell=True, shell_allowlist=["echo"]))
    )
    assert isinstance(sub, SafeSubagent)
    assert sub.decision.allowed is True


def test_create_denies_when_subagent_scope_exceeds_parent():
    parent = _env(shell=True, shell_allowlist=["echo"])
    with pytest.raises(DelegationDenied):
        SafeSubagent.create(
            parent_envelope=parent,
            contract=_contract(_env(shell=True, shell_allowlist=["echo", "rm"])),
        )


def test_create_denies_robot_overreach():
    # Ties to the fail-closed robot subsumption: a wider-reach robot subagent
    # cannot be created under a tighter-reach parent.
    parent = _env(workspace_bounds=((0.0, 0.0, 0.0), (0.1, 0.1, 0.1)))
    with pytest.raises(DelegationDenied) as ei:
        SafeSubagent.create(
            parent_envelope=parent,
            contract=_contract(_env(workspace_bounds=((-5.0, -5.0, -5.0), (9.0, 9.0, 9.0)))),
        )
    assert "robot" in str(ei.value).lower() or "workspace" in str(ei.value).lower()


def test_verify_rejects_plan_outside_scope():
    parent = _env(shell=True, shell_allowlist=["echo"])
    sub = SafeSubagent.create(
        parent_envelope=parent, contract=_contract(_env(shell=True, shell_allowlist=["echo"]))
    )
    bad = ActionPlan(source="sub", task="t", steps=[ShellStep(id="s1", command="rm -rf /")])
    assert sub.verify(bad).ok is False


def test_verify_accepts_in_scope_plan():
    parent = _env(shell=True, shell_allowlist=["echo"])
    sub = SafeSubagent.create(
        parent_envelope=parent, contract=_contract(_env(shell=True, shell_allowlist=["echo"]))
    )
    good = ActionPlan(source="sub", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    assert sub.verify(good).ok is True


def test_run_is_dry_by_default(tmp_path):
    from opendaisugi.journal import Journal

    parent = _env(shell=True, shell_allowlist=["echo"])
    sub = SafeSubagent.create(
        parent_envelope=parent,
        contract=_contract(_env(shell=True, shell_allowlist=["echo"])),
        journal=Journal(data_dir=tmp_path),
    )
    good = ActionPlan(source="sub", task="t", steps=[ShellStep(id="s1", command="echo hi")])
    session = asyncio.run(sub.run(good))
    assert session is not None
    assert session.status.name == "SUCCEEDED"
    # dry-run: each step records intent ("[dry-run] would shell: …"), not real execution
    assert any("dry-run" in (getattr(s, "stdout", "") or "").lower() for s in session.steps)


def test_carries_local_tier1_provider():
    class _Local:
        name = "local"

        async def generate_envelope(self, task, *, context=None):
            return None

    provider = _Local()
    parent = _env(shell=True, shell_allowlist=["echo"])
    sub = SafeSubagent.create(
        parent_envelope=parent,
        contract=_contract(_env(shell=True, shell_allowlist=["echo"])),
        tier1=provider,
    )
    assert sub.tier1 is provider
