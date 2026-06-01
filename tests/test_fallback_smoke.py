"""Smoke test: RecomputeHandler with real LLM call.

Marked @pytest.mark.smoke so CI can skip on fast runs.
Requires ANTHROPIC_API_KEY (or equivalent litellm provider key) in env.
"""

import pytest

from opendaisugi.fallback import RecomputeHandler
from opendaisugi.models import (
    Envelope,
    FileWriteStep,
    Permission,
    VerificationResult,
    Violation,
)


@pytest.mark.smoke
async def test_recompute_handler_live_llm():
    """Craft a step that violates a simple envelope, let the handler recompute."""
    env = Envelope(
        generated_by="smoke-test",
        task="Write a greeting to /tmp/hello.txt",
        permissions=Permission(
            file_read=[],
            file_write=["/tmp/hello.txt"],
            shell=False,
            shell_allowlist=[],
        ),
    )
    # This step violates because it tries to write to /etc/passwd
    bad_step = FileWriteStep(id="fw1", path="/etc/passwd", content="hacked")
    failed_result = VerificationResult(
        ok=False,
        violations=[
            Violation(
                stage="permissions",
                message="file_write path '/etc/passwd' not permitted by file_write ['/tmp/hello.txt']",
                detail={"step": "fw1", "path": "/etc/passwd"},
            )
        ],
        warnings=[],
        envelope_id=env.id,
        plan_id="plan_smoke",
        duration_ms=1.0,
    )

    handler = RecomputeHandler(model="anthropic/claude-sonnet-4-20250514")
    outcome = await handler.handle(bad_step, failed_result, env)

    # The LLM should produce a replacement that writes to /tmp/hello.txt instead.
    # We can't assert exact content, but we can assert the outcome structure.
    assert outcome.action in ("recomputed", "halted")
    if outcome.action == "recomputed":
        assert outcome.replacement_step is not None
        assert outcome.replacement_result is not None
        assert outcome.replacement_result.ok is True
