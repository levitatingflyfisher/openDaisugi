"""Facade-level refinement-aware generation (v0.2.1)."""

import pytest

from opendaisugi import Daisugi
from opendaisugi.envelope_cache import make_cache_key
from opendaisugi.models import ShellStep, Violation
from opendaisugi.refinement import RefinementRecord


@pytest.mark.asyncio
async def test_daisugi_threads_journal_into_envelope_generation(tmp_path, mock_llm_client):
    """Daisugi.generate_envelope() passes self.journal to the module fn."""
    d = Daisugi(data_dir=tmp_path, model="anthropic/test-model")
    key = make_cache_key(
        task="poke", context=None, model="anthropic/test-model",
        parent_envelope_id=None, summarize=False, thinking_budget="standard",
    )
    d.journal.write_refinement(
        RefinementRecord(
            step=ShellStep(id="s1", command="rm"),
            violations=[Violation(stage="permissions",
                                   message="shell not allowed",
                                   detail={})],
            z3_counterexample=None, envelope_id="e",
            fallback_action="halted", timestamp=1.0, cache_key=key,
        ),
        session_id="prev",
    )

    await d.generate_envelope("poke")

    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "Prior Rejections" in user_msg
