"""Tests for the Daisugi facade class."""

from pathlib import Path

import pytest

from opendaisugi import Daisugi
from opendaisugi.exceptions import TaskTooLongError


def test_daisugi_default_construction():
    dai = Daisugi()
    assert dai.model == "anthropic/claude-sonnet-4-20250514"
    assert dai.max_task_chars == 4000
    assert dai.z3_timeout_ms == 500
    assert dai.data_dir == Path.home() / ".opendaisugi"


def test_daisugi_kwargs_override_defaults():
    dai = Daisugi(
        model="openai/gpt-4o-mini",
        max_task_chars=1000,
        z3_timeout_ms=250,
        data_dir=Path("/tmp/daisugi-test"),
    )
    assert dai.model == "openai/gpt-4o-mini"
    assert dai.max_task_chars == 1000
    assert dai.z3_timeout_ms == 250
    assert dai.data_dir == Path("/tmp/daisugi-test")


def test_daisugi_partial_override():
    # Overriding one kwarg leaves the rest at defaults.
    dai = Daisugi(max_task_chars=2000)
    assert dai.model == "anthropic/claude-sonnet-4-20250514"
    assert dai.max_task_chars == 2000
    assert dai.z3_timeout_ms == 500


async def test_daisugi_generate_envelope_returns_envelope(mock_llm_client, sample_envelope):
    # cache=False so this test exercises the LLM path, not a stale on-disk cache.
    dai = Daisugi(cache=False)
    result = await dai.generate_envelope("Delete .tmp files in /var/log")
    assert result is sample_envelope


async def test_daisugi_generate_envelope_uses_facade_model(mock_llm_client):
    dai = Daisugi(model="openai/gpt-4o-mini", cache=False)
    await dai.generate_envelope("test task")
    assert mock_llm_client.chat.completions.last_call["model"] == "openai/gpt-4o-mini"


async def test_daisugi_generate_envelope_uses_facade_max_task_chars(mock_llm_client):
    dai = Daisugi(max_task_chars=50, cache=False)
    with pytest.raises(TaskTooLongError):
        await dai.generate_envelope("x" * 100)


async def test_daisugi_generate_envelope_passes_context(mock_llm_client):
    dai = Daisugi(cache=False)
    await dai.generate_envelope("task", context="extra info")
    user_msg = mock_llm_client.chat.completions.last_call["messages"][1]["content"]
    assert "extra info" in user_msg


def test_daisugi_verify_delegates_to_free_function(sample_plan, sample_envelope):
    dai = Daisugi()
    result = dai.verify(sample_plan, sample_envelope)
    assert result.ok is True
    assert result.envelope_id == sample_envelope.id
    assert result.plan_id == sample_plan.id


def test_daisugi_verify_uses_facade_z3_timeout(sample_plan, sample_envelope, monkeypatch):
    # Patch the private _verify alias that Daisugi.verify calls and capture
    # the z3_timeout_ms kwarg the facade passes through. We return a minimal
    # VerificationResult rather than running the real pipeline — this test
    # is about the delegation contract, not the pipeline itself.
    import opendaisugi
    from opendaisugi.models import VerificationResult

    captured = {}

    def fake_verify(plan, envelope, *, z3_timeout_ms, strict=None, aliases=None):
        captured["z3_timeout_ms"] = z3_timeout_ms
        return VerificationResult(
            ok=True,
            violations=[],
            warnings=[],
            envelope_id=envelope.id,
            plan_id=plan.id,
            duration_ms=0.0,
        )

    monkeypatch.setattr(opendaisugi, "_verify", fake_verify)

    dai = Daisugi(z3_timeout_ms=1234)
    dai.verify(sample_plan, sample_envelope)
    assert captured["z3_timeout_ms"] == 1234


from opendaisugi.journal import Journal


def test_daisugi_journal_returns_journal_rooted_at_data_dir(tmp_path):
    dai = Daisugi(data_dir=tmp_path)
    j = dai.journal
    assert isinstance(j, Journal)
    assert j.data_dir == tmp_path


def test_daisugi_journal_is_memoized(tmp_path):
    dai = Daisugi(data_dir=tmp_path)
    assert dai.journal is dai.journal


def test_daisugi_journal_forwards_z3_timeout(tmp_path):
    dai = Daisugi(data_dir=tmp_path, z3_timeout_ms=999)
    assert dai.journal.z3_timeout_ms == 999
