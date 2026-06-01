"""End-to-end v0.2.1 flow: supervisor rejection → refinement → regen with hints."""

from unittest.mock import patch

import pytest

from opendaisugi import Daisugi
from opendaisugi.approval import AllowlistBypassStrategy, DenyStrategy
from opendaisugi.executor import ExecutorResult, FakeExecutor
from opendaisugi.fallback import HaltHandler
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
    Violation,
)
from opendaisugi.supervisor import Supervisor


def _reject_only_per_step(real_verify_step):
    """Force per-step verify to reject. v0.22+ supervisor uses verify_step
    for per-step gating, so the patch target shifted accordingly.
    """
    def _patched(step, envelope, *, z3_timeout_ms=500):
        return VerificationResult(
                ok=False,
                violations=[Violation(
                    stage="permissions",
                    message="shell command 'rm' is not safe",
                    detail={},
                )],
                warnings=[],
                envelope_id=envelope.id,
                plan_id="per-step-verify",
                duration_ms=0.1,
            )
    return _patched


@pytest.mark.asyncio
async def test_full_loop_rejection_then_hinted_regeneration(tmp_path, mock_llm_client):
    """Generate → run → reject → regenerate. Second generation sees prior rejection."""
    d = Daisugi(
        data_dir=tmp_path,
        model="anthropic/test-model",
        cache=False,  # simpler — no cache-bust path to reason about here
    )

    env = await d.generate_envelope("clean up stale files")
    assert env.cache_key is not None

    plan = ActionPlan(
        source="t", task="clean up stale files",
        steps=[ShellStep(id="s1", command="rm -rf /tmp/stale")],
    )

    sup = Supervisor(
        executors={"shell": FakeExecutor(default=ExecutorResult(0, "", 0.1, False))},
        approval=AllowlistBypassStrategy(DenyStrategy()),
        journal=d.journal,
        fallback=HaltHandler(),
    )

    from opendaisugi.verify import verify_step as _real_verify

    def _accept_whole_plan(plan, envelope, *, z3_timeout_ms=500, strict=None, aliases=None):
        return VerificationResult(
            ok=True, violations=[], warnings=[],
            envelope_id=envelope.id, plan_id=plan.id, duration_ms=0.1,
        )

    with patch("opendaisugi.supervisor.verify", side_effect=_accept_whole_plan), \
         patch("opendaisugi.supervisor.verify_step",
               side_effect=_reject_only_per_step(_real_verify)):
        session = await sup.run(plan, env)

    # Refinement was written and tagged with env.cache_key.
    by_key = d.journal.get_refinements_by_key(env.cache_key)
    assert len(by_key) == 1
    assert by_key[0].envelope_id == env.id

    # Second generation: same task → hints appear.
    mock_llm_client.chat.completions.last_call = {}
    await d.generate_envelope("clean up stale files")
    assert mock_llm_client.call_count == 2, "second generate_envelope must hit the LLM (not short-circuit)"
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" in user_msg
    assert "shell command 'rm' is not safe" in user_msg
