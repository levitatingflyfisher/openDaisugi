"""The local rung must not route to a placeholder by default (v0.32).

Regression guard: making the local rung reachable must not make the default route
to a non-existent 'openai/local-model'. The default ladder is safe (cheap+frontier);
the local rung appears only when a real local model is configured, and its endpoint
is threaded so the call actually reaches the local server.
"""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import patch

from opendaisugi.budget import BudgetTracker
from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.model_sizer import DEFAULT_LADDER, build_ladder, size_step
from opendaisugi.models import TaskStep


def test_default_ladder_has_no_local_placeholder():
    models = [r.model for r in DEFAULT_LADDER.rungs]
    assert "openai/local-model" not in models
    assert all("local-model" not in m for m in models)
    # An easy reasoning task falls back to the cheapest *real* model, not a placeholder.
    sized = size_step(TaskStep(id="t1", prompt="summarize the notes"))
    assert "local-model" not in sized.model


def test_build_ladder_adds_local_rung_with_real_model():
    ladder = build_ladder(local_model="openai/qwen2.5-3b")
    assert ladder.rungs[0].name == "local"
    assert ladder.rungs[0].model == "openai/qwen2.5-3b"
    sized = size_step(TaskStep(id="t1", prompt="summarize the notes"), ladder=ladder)
    assert sized.tier == "local"
    assert sized.model == "openai/qwen2.5-3b"


def test_build_ladder_without_local_is_two_rung():
    ladder = build_ladder()
    assert [r.name for r in ladder.rungs] == ["cheap", "frontier"]


def test_endpoint_override_threads_base_url_to_completion():
    exe = DelegatingExecutor(
        default_model="openai/qwen2.5-3b",
        json_mode=False,
        endpoint_overrides={"openai/qwen2.5-3b": {"api_base": "http://localhost:8080/v1", "api_key": "x"}},
    )
    captured = {}

    def fake_completion(**kwargs):
        captured.update(kwargs)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="hi"))],
            usage=SimpleNamespace(total_tokens=3),
        )

    with patch("litellm.completion", fake_completion):
        exe.run(TaskStep(id="t1", prompt="x"), timeout_s=5, max_output_bytes=512)
    assert captured.get("api_base") == "http://localhost:8080/v1"
    assert captured.get("api_key") == "x"


def test_endpoint_override_not_applied_to_other_models():
    exe = DelegatingExecutor(
        default_model="claude-haiku-4-5",
        json_mode=False,
        endpoint_overrides={"openai/qwen2.5-3b": {"api_base": "http://localhost:8080/v1"}},
    )
    captured = {}

    def fake_completion(**kwargs):
        captured.update(kwargs)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="hi"))],
            usage=SimpleNamespace(total_tokens=3),
        )

    with patch("litellm.completion", fake_completion):
        exe.run(TaskStep(id="t1", prompt="x"), timeout_s=5, max_output_bytes=512)
    assert "api_base" not in captured  # cloud model must not get the local endpoint


async def test_facade_threads_configured_local_model(monkeypatch):
    """Daisugi.orchestrate must wire a configured Tier-1 local model into the
    ladder's local rung AND pass its endpoint to the executor."""
    import opendaisugi
    from opendaisugi import Daisugi, Envelope, Permission
    from opendaisugi.tier1 import LiteLLMTier1Provider

    captured = {}

    class _FakeOrch:
        def __init__(self, **kwargs):
            captured.update(kwargs)

        async def orchestrate(self, prompt, **kwargs):
            return "sentinel"

    monkeypatch.setattr(opendaisugi.orchestrator, "Orchestrator", _FakeOrch)

    tier1 = LiteLLMTier1Provider(model="qwen2.5-3b", base_url="http://localhost:8080/v1")
    dai = Daisugi(tier1=tier1, pathway_store=False, cache=False)
    env = Envelope(generated_by="t", task="x", permissions=Permission())
    await dai.orchestrate("summarize", envelope=env)

    ladder = captured["ladder"]
    assert ladder.rungs[0].name == "local"
    assert ladder.rungs[0].model == "openai/qwen2.5-3b"  # provider auto-prefixes
    assert captured["endpoint_overrides"]["openai/qwen2.5-3b"]["api_base"] == "http://localhost:8080/v1"


async def test_facade_no_local_model_uses_safe_default(monkeypatch):
    import opendaisugi
    from opendaisugi import Daisugi, Envelope, Permission

    captured = {}

    class _FakeOrch:
        def __init__(self, **kwargs):
            captured.update(kwargs)

        async def orchestrate(self, prompt, **kwargs):
            return "sentinel"

    monkeypatch.setattr(opendaisugi.orchestrator, "Orchestrator", _FakeOrch)

    dai = Daisugi(pathway_store=False, cache=False)  # no tier1
    env = Envelope(generated_by="t", task="x", permissions=Permission())
    await dai.orchestrate("summarize", envelope=env)

    assert [r.name for r in captured["ladder"].rungs] == ["cheap", "frontier"]
    assert captured["endpoint_overrides"] == {}
