"""Integration tests for Supervisor + FallbackHandler."""

from unittest.mock import patch

from opendaisugi.approval import AllowlistBypassStrategy, DenyStrategy
from opendaisugi.executor import ExecutorResult, FakeExecutor
from opendaisugi.fallback import FallbackOutcome, HaltHandler
from opendaisugi.journal import Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
    Violation,
)
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


def _env_shell_only(allowlist):
    return Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=allowlist),
    )


def _plan(*steps):
    return ActionPlan(source="t", task="t", steps=list(steps))


def _make_verify_step_that_rejects(reject_step_id: str, real_verify_step):
    """Return a verify_step function that rejects a specific step id.

    v0.22+: the supervisor now calls ``verify_step(step, envelope)`` for
    per-step gates rather than ``verify(singleton_plan, envelope)``. This
    helper mirrors the rejection shape on the new signature.
    """
    def _patched(step, envelope, *, z3_timeout_ms=500):
        if step.id == reject_step_id:
            return VerificationResult(
                ok=False,
                violations=[Violation(
                    stage="permissions",
                    message=f"Step '{reject_step_id}' blocked by per-step policy",
                    detail={"step": reject_step_id},
                )],
                warnings=[],
                envelope_id=envelope.id,
                plan_id="per-step-verify",
                duration_ms=0.1,
            )
        return real_verify_step(step, envelope, z3_timeout_ms=z3_timeout_ms)
    return _patched


# Back-compat alias for any out-of-tree caller
_make_verify_step_that_rejects = _make_verify_step_that_rejects


async def test_supervisor_halts_on_per_step_rejection(tmp_path):
    """A step that fails per-step verification triggers the fallback handler.

    The whole-plan verify passes (both steps are allowlisted shell commands),
    but per-step verify is patched to reject s2 — simulating a policy gate
    that is stricter at the per-step level.
    """
    env = _env_shell_only(["echo"])
    plan = _plan(
        ShellStep(id="s1", command="echo hi"),
        ShellStep(id="s2", command="echo bye"),
    )
    executor = FakeExecutor(
        {"echo hi": ExecutorResult(0, "hi\n", 1.0, False)},
        default=ExecutorResult(0, "", 0.1, False),
    )
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=HaltHandler(),
    )

    from opendaisugi.verify import verify_step as _real_verify
    patched = _make_verify_step_that_rejects("s2", _real_verify)

    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        session = await sup.run(plan, env)

    assert session.status == RunStatus.HALTED_BY_SIMPLEX
    assert any(s.step_id == "s1" and s.status == "succeeded" for s in session.steps)
    assert any(s.step_id == "s2" and s.status == "rejected_halted" for s in session.steps)


async def test_supervisor_halt_writes_refinement_to_journal(tmp_path):
    """A halted step writes a RefinementRecord to the journal."""
    env = _env_shell_only(["echo"])
    plan = _plan(
        ShellStep(id="s1", command="echo blocked"),
    )
    executor = FakeExecutor(default=ExecutorResult(0, "", 0.1, False))
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=HaltHandler(),
    )

    from opendaisugi.verify import verify_step as _real_verify
    patched = _make_verify_step_that_rejects("s1", _real_verify)

    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        session = await sup.run(plan, env)

    assert session.status == RunStatus.HALTED_BY_SIMPLEX
    log = journal.get_refinements(session.id)
    assert len(log.records) == 1
    assert log.records[0].fallback_action == "halted"
    assert log.records[0].envelope_id == env.id


class FakeRecomputeHandler:
    """Deterministic handler that returns a pre-configured replacement."""

    def __init__(self, replacement_step, replacement_ok=True):
        self._replacement = replacement_step
        self._ok = replacement_ok

    async def handle(self, step, result, envelope):
        if self._ok and self._replacement is not None:
            vr = VerificationResult(
                ok=True, violations=[], warnings=[],
                envelope_id=envelope.id, plan_id="plan_x", duration_ms=0.5,
            )
            return FallbackOutcome(
                action="recomputed",
                replacement_step=self._replacement,
                replacement_result=vr,
            )
        return FallbackOutcome(action="halted")


async def test_supervisor_recompute_continues_run(tmp_path):
    """Rejected step -> recomputed -> replacement executes -> run succeeds."""
    env = _env_shell_only(["echo"])
    plan = _plan(
        ShellStep(id="s1", command="echo blocked"),
    )
    replacement = ShellStep(id="s1_v2", command="echo repaired")
    executor = FakeExecutor(
        {"echo repaired": ExecutorResult(0, "repaired\n", 1.0, False)},
        default=ExecutorResult(0, "", 0.1, False),
    )
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=FakeRecomputeHandler(replacement, replacement_ok=True),
    )

    from opendaisugi.verify import verify_step as _real_verify
    patched = _make_verify_step_that_rejects("s1", _real_verify)

    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        session = await sup.run(plan, env)

    assert session.status == RunStatus.SUCCEEDED
    assert any(s.status == "rejected_recomputed" for s in session.steps)
    assert any(s.status == "succeeded" for s in session.steps)
    log = journal.get_refinements(session.id)
    assert len(log.records) == 1
    assert log.records[0].fallback_action == "recomputed"


async def test_supervisor_recompute_fails_then_halts(tmp_path):
    """Rejected step -> recompute handler also fails -> HALTED_BY_SIMPLEX."""
    env = _env_shell_only(["echo"])
    plan = _plan(
        ShellStep(id="s1", command="echo blocked"),
    )
    executor = FakeExecutor(default=ExecutorResult(0, "", 0.1, False))
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=FakeRecomputeHandler(None, replacement_ok=False),
    )

    from opendaisugi.verify import verify_step as _real_verify
    patched = _make_verify_step_that_rejects("s1", _real_verify)

    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        session = await sup.run(plan, env)

    assert session.status == RunStatus.HALTED_BY_SIMPLEX
    log = journal.get_refinements(session.id)
    assert len(log.records) == 1
    assert log.records[0].fallback_action == "halted"


async def test_supervisor_auto_selects_recompute_from_envelope(tmp_path):
    """When no fallback handler is injected, _resolve_fallback selects from envelope.

    The envelope's default FallbackStrategy is tier2_recompute, so
    _resolve_fallback constructs a RecomputeHandler. We patch the LLM
    client factory to prove RecomputeHandler was actually selected (not
    just HaltHandler), then assert the run halts because the LLM call
    fails.
    """
    env = _env_shell_only(["echo"])
    plan = _plan(ShellStep(id="s1", command="echo hi"))
    executor = FakeExecutor(default=ExecutorResult(0, "", 0.1, False))
    sup = Supervisor(
        executors={"shell": executor},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=Journal(data_dir=tmp_path),
        # No fallback= kwarg — auto-selection from envelope FallbackStrategy
    )

    from opendaisugi.verify import verify_step as _real_verify
    patched = _make_verify_step_that_rejects("s1", _real_verify)

    with patch("opendaisugi.supervisor.verify_step", side_effect=patched), \
         patch("opendaisugi.fallback._get_recompute_client") as mock_client:
        mock_client.side_effect = RuntimeError("no LLM in tests")
        session = await sup.run(plan, env)

    assert session.status == RunStatus.HALTED_BY_SIMPLEX
    # Prove RecomputeHandler was actually constructed and invoked (not HaltHandler)
    mock_client.assert_called_once()


async def test_supervisor_tags_refinement_with_envelope_cache_key(tmp_path):
    """When the envelope carries a cache_key, the RefinementRecord gets it.

    The Supervisor does not compute the key — it copies from envelope.cache_key.
    This proves the v0.2.1 end-to-end wiring: generator stamps key on envelope,
    supervisor propagates to refinement record, journal persists to column.
    """
    from unittest.mock import patch

    from opendaisugi.verify import verify_step as _real_verify

    env = Envelope(
        generated_by="t", task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
        cache_key="test_cache_key_xyz",  # stamped by (simulated) generator
    )
    plan = _plan(ShellStep(id="s1", command="echo blocked"))
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=HaltHandler(),
    )
    patched = _make_verify_step_that_rejects("s1", _real_verify)
    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        await sup.run(plan, env)

    # Retrieve by cache_key — proves the column was populated.
    retrieved = journal.get_refinements_by_key("test_cache_key_xyz")
    assert len(retrieved) == 1
    assert retrieved[0].envelope_id == env.id
    assert retrieved[0].cache_key == "test_cache_key_xyz"


async def test_supervisor_tags_refinement_none_for_handbuilt_envelope(tmp_path):
    """Hand-built envelopes (no generator) leave cache_key=None on refinements."""
    from unittest.mock import patch

    from opendaisugi.verify import verify_step as _real_verify

    env = _env_shell_only(["echo"])  # cache_key defaults to None
    assert env.cache_key is None
    plan = _plan(ShellStep(id="s1", command="echo blocked"))
    journal = Journal(data_dir=tmp_path)
    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=journal,
        fallback=HaltHandler(),
    )
    patched = _make_verify_step_that_rejects("s1", _real_verify)
    with patch("opendaisugi.supervisor.verify_step", side_effect=patched):
        session = await sup.run(plan, env)

    # get_refinements_by_key ignores null cache_key rows.
    assert journal.get_refinements_by_key("anything") == []
    # Session-scoped lookup still returns the record with cache_key=None.
    log = journal.get_refinements(session.id)
    assert len(log.records) == 1
    assert log.records[0].cache_key is None
