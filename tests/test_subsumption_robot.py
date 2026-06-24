"""Fail-closed robot-capability subsumption (v0.31).

PLAN-LEVEL verification only — this proves an inner (callee) robot envelope's
*declared* physical capabilities fit inside an outer (caller) envelope's. It is
NOT a robot safety system, NOT a fleet controller, and does NOT model executed
trajectories. Its job: close the "robot bounds fail open" hole the v0.28.6 review
found, where a 90×-reach robot was "subsumed" into a 0.1m envelope because
subsumption ignored every robot field.

Fail-closed rule: when the OUTER declares a capability bound and the inner either
exceeds it OR leaves it undeclared (= unbounded), subsumption FAILS. Undeclared =
denied.
"""

from opendaisugi.contracts import Contract, verify_delegation
from opendaisugi.models import Envelope, Permission
from opendaisugi.subsumption import envelope_subsumes


def _env(**perm_kwargs) -> Envelope:
    return Envelope(generated_by="t", task="t", permissions=Permission(**perm_kwargs))


WS_SMALL = ((0.0, 0.0, 0.0), (0.1, 0.1, 0.1))
WS_BIG = ((-5.0, -5.0, -5.0), (9.0, 9.0, 9.0))


def test_inner_workspace_exceeds_outer_fails():
    # The headline hole: a far-reaching inner robot must NOT subsume into a tiny outer envelope.
    outer = _env(workspace_bounds=WS_SMALL)
    inner = _env(workspace_bounds=WS_BIG)
    assert envelope_subsumes(outer, inner).holds is False


def test_inner_undeclared_workspace_fails_closed():
    # Outer constrains the workspace; inner declares none → unbounded → DENIED.
    outer = _env(workspace_bounds=WS_SMALL)
    inner = _env()  # no workspace_bounds
    assert envelope_subsumes(outer, inner).holds is False


def test_inner_workspace_within_outer_holds():
    outer = _env(workspace_bounds=WS_BIG)
    inner = _env(workspace_bounds=WS_SMALL)
    assert envelope_subsumes(outer, inner).holds is True


def test_velocity_limit_fail_closed_and_exceed():
    assert envelope_subsumes(_env(velocity_limit=1.0), _env(velocity_limit=5.0)).holds is False  # exceeds
    assert envelope_subsumes(_env(velocity_limit=1.0), _env()).holds is False                    # undeclared
    assert envelope_subsumes(_env(velocity_limit=5.0), _env(velocity_limit=1.0)).holds is True    # within


def test_torque_limit_fail_closed():
    assert envelope_subsumes(_env(torque_limit=10.0), _env()).holds is False
    assert envelope_subsumes(_env(torque_limit=10.0), _env(torque_limit=50.0)).holds is False
    assert envelope_subsumes(_env(torque_limit=50.0), _env(torque_limit=10.0)).holds is True


def test_joint_limits_undeclared_and_exceed():
    outer = _env(joint_limits={"j1": (-1.0, 1.0)})
    assert envelope_subsumes(outer, _env()).holds is False                                # joint undeclared
    assert envelope_subsumes(outer, _env(joint_limits={"j1": (-2.0, 2.0)})).holds is False  # range exceeds
    assert envelope_subsumes(outer, _env(joint_limits={"j1": (-0.5, 0.5)})).holds is True   # within


def test_inner_must_avoid_at_least_outer_obstacles():
    obs = [((1.0, 1.0, 1.0), (2.0, 2.0, 2.0))]
    outer = _env(obstacles=obs)
    assert envelope_subsumes(outer, _env()).holds is False          # inner omits a forbidden region
    assert envelope_subsumes(outer, _env(obstacles=obs)).holds is True  # inner avoids it too


def test_non_robot_envelopes_unaffected():
    # Regression guard: no robot fields → robot check is a no-op; ordinary subsumption stands.
    same = _env(shell=True, shell_allowlist=["echo"])
    assert envelope_subsumes(same, same).holds is True


def test_verify_delegation_surfaces_robot_reason():
    caller = _env(workspace_bounds=WS_SMALL)
    contract = Contract(
        contract_id="c1", skill_id="wide-arm", envelope=_env(workspace_bounds=WS_BIG),
    )
    decision = verify_delegation(caller, contract)
    assert decision.allowed is False
    assert "robot" in decision.reason.lower() or "workspace" in decision.reason.lower()
