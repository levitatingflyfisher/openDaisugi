"""The orchestrator runs TaskSteps as natural-language subtasks, not evidence-JSON.

Regression test for the headline-path bug: a TaskStep must be prompted with its
own subtask (free text), not the DelegatingExecutor default that asks for
postcondition-evidence JSON, and must not be forced into JSON response mode.
"""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import patch

from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.models import TaskStep
from opendaisugi.orchestrator import BudgetAwareDelegatingExecutor
from opendaisugi.budget import BudgetTracker


def test_json_mode_false_omits_response_format():
    exe = DelegatingExecutor(default_model="haiku", json_mode=False)
    captured = {}

    def fake_completion(**kwargs):
        captured.update(kwargs)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="a plain answer"))],
            usage=SimpleNamespace(total_tokens=10),
        )

    with patch("litellm.completion", fake_completion):
        r = exe.run(TaskStep(id="t1", prompt="write a haiku"), timeout_s=5, max_output_bytes=1024)
    assert "response_format" not in captured
    assert r.stdout == "a plain answer"


def test_json_mode_true_still_forces_json():
    exe = DelegatingExecutor(default_model="haiku", json_mode=True)
    captured = {}

    def fake_completion(**kwargs):
        captured.update(kwargs)
        return SimpleNamespace(
            choices=[SimpleNamespace(message=SimpleNamespace(content="{}"))],
            usage=SimpleNamespace(total_tokens=5),
        )

    with patch("litellm.completion", fake_completion):
        exe.run(TaskStep(id="t1", prompt="x"), timeout_s=5, max_output_bytes=1024)
    assert captured.get("response_format") == {"type": "json_object"}


def test_orchestrator_task_executor_prompts_with_the_subtask():
    # The orchestrator's task executor must render the TaskStep's own prompt,
    # not the evidence-JSON default.
    exe = BudgetAwareDelegatingExecutor(tracker=BudgetTracker())
    prompt = exe.prompt_template(TaskStep(id="t1", prompt="summarize the sales figures"))
    assert "summarize the sales figures" in prompt
    assert "postcondition" not in prompt.lower()


def test_orchestrator_task_executor_is_not_json_forced():
    exe = BudgetAwareDelegatingExecutor(tracker=BudgetTracker())
    assert exe.json_mode is False


def test_claude_code_backend_returns_prose_when_json_mode_false(monkeypatch):
    # On the claude-code backend a prose TaskStep must get raw text, not be forced
    # through JSON extraction (which fails on a prose answer). This is the only
    # backend available without an API key.
    exe = DelegatingExecutor(default_model="haiku", backend="claude-code", json_mode=False)
    called = {}

    def fake_sync(prompt, *, timeout_s, model, binary="claude"):
        called["sync"] = True
        return "Here is a plain prose answer with no JSON."

    def fake_json_sync(*a, **k):
        called["json"] = True
        raise AssertionError("json path must not be used when json_mode=False")

    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_sync", fake_sync)
    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_json_sync", fake_json_sync)

    r = exe.run(TaskStep(id="t1", prompt="summarize"), timeout_s=30, max_output_bytes=2048)
    assert r.rc == 0
    assert r.stdout == "Here is a plain prose answer with no JSON."
    assert called.get("sync") and not called.get("json")


def test_claude_code_backend_still_json_when_json_mode_true(monkeypatch):
    exe = DelegatingExecutor(default_model="haiku", backend="claude-code", json_mode=True)
    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_sync",
        lambda *a, **k: {"evidence": "x"},
    )
    r = exe.run(TaskStep(id="t1", prompt="x"), timeout_s=30, max_output_bytes=2048)
    assert '"evidence"' in r.stdout
