"""The orchestrator runs TaskSteps as natural-language subtasks, not evidence-JSON.

Regression test for the headline-path bug: a TaskStep must be prompted with its
own subtask (free text), not the DelegatingExecutor default that asks for
postcondition-evidence JSON, and must not be forced into JSON response mode.
"""

from __future__ import annotations

from types import SimpleNamespace
from unittest.mock import patch

from opendaisugi.budget import BudgetTracker
from opendaisugi.delegating_executor import DelegatingExecutor
from opendaisugi.models import TaskStep
from opendaisugi.orchestrator import BudgetAwareDelegatingExecutor


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


def test_claude_code_backend_returns_prose_and_meters_when_json_mode_false(monkeypatch):
    # On the claude-code backend a prose TaskStep must get raw text (not forced
    # through JSON extraction) AND capture Claude Code's exact usage + cost.
    exe = DelegatingExecutor(default_model="haiku", backend="claude-code", json_mode=False)
    called = {}

    def fake_metered(prompt, *, timeout_s, model, binary="claude", cwd=None):
        called["metered"] = True
        return "Here is a plain prose answer with no JSON.", {"tokens": 72, "cost_usd": 0.0207}

    def fake_json_sync(*a, **k):
        raise AssertionError("json path must not be used when json_mode=False")

    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_metered", fake_metered)
    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_json_sync", fake_json_sync)

    r = exe.run(TaskStep(id="t1", prompt="summarize"), timeout_s=30, max_output_bytes=2048)
    assert r.rc == 0
    assert r.stdout == "Here is a plain prose answer with no JSON."
    assert called.get("metered")
    # Exact usage + cost captured for the budget tracker.
    assert exe.last.tokens == 72
    assert exe.last.cost_usd == 0.0207


def test_claude_code_backend_still_json_when_json_mode_true(monkeypatch):
    # json_mode=True now routes through the metered variant: same JSON contract,
    # plus is_error surfacing and exact usage/cost for the budget tracker
    # (previously last.tokens stayed None on this path and spend went uncounted).
    exe = DelegatingExecutor(default_model="haiku", backend="claude-code", json_mode=True)
    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_metered",
        lambda *a, **k: ({"evidence": "x"}, {"tokens": 42, "cost_usd": 0.003}),
    )
    r = exe.run(TaskStep(id="t1", prompt="x"), timeout_s=30, max_output_bytes=2048)
    assert '"evidence"' in r.stdout
    assert exe.last.tokens == 42
    assert exe.last.cost_usd == 0.003


def test_claude_code_json_mode_prose_failure_surfaces_cause(monkeypatch):
    # When the delegated model answers in prose (typically: the prompt asked for
    # file/tool access the sandbox doesn't have), the step must fail with the
    # REAL cause in stdout, not a JSON-formatting complaint.
    from opendaisugi.exceptions import EnvelopeGenerationError

    exe = DelegatingExecutor(
        default_model="haiku", backend="claude-code", json_mode=True, max_retries=0,
    )

    def fake_json_metered(*a, **k):
        raise EnvelopeGenerationError(
            "delegated model replied with prose, not JSON: 'The README.md file "
            "does not exist...'. Delegated steps run in an isolated working "
            "directory with no project files and no tools; a prompt that asks "
            "the model to read files, browse, or run commands cannot succeed "
            "on this path — restate the step as pure reasoning or use a "
            "capability step type."
        )

    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_metered", fake_json_metered,
    )
    r = exe.run(TaskStep(id="t1", prompt="read README.md"), timeout_s=30, max_output_bytes=2048)
    assert r.rc == 1
    assert "no project files and no tools" in r.stdout
