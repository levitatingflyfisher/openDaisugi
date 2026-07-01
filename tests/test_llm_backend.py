"""Tests for the OPENDAISUGI_LLM_BACKEND switch in llm.get_instructor_client
and in the two direct-litellm call sites (llm_check, parsers._llm_split).
"""

from __future__ import annotations

from unittest.mock import MagicMock

from opendaisugi.claude_code_llm import ClaudeCodeInstructorClient
from opendaisugi.llm import get_instructor_client


def test_default_backend_is_litellm(monkeypatch):
    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    client = get_instructor_client("anthropic/claude-haiku-4-5-20251001")
    assert not isinstance(client, ClaudeCodeInstructorClient)


def test_env_var_claude_code_routes_to_shim(monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    client = get_instructor_client("anthropic/claude-haiku-4-5-20251001")
    assert isinstance(client, ClaudeCodeInstructorClient)


def test_explicit_backend_overrides_env(monkeypatch):
    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    client = get_instructor_client("x", backend="litellm")
    assert not isinstance(client, ClaudeCodeInstructorClient)


def test_llm_check_routes_through_backend(monkeypatch):
    from opendaisugi import llm_check as lc

    called = {"which": None}

    def fake_claude_json(prompt, *, timeout_s, model, binary="claude"):
        called["which"] = "claude"
        return {"satisfied": True, "rationale": "ok"}

    def fake_litellm(**_):
        called["which"] = "litellm"
        resp = MagicMock()
        resp.choices = [MagicMock()]
        resp.choices[0].message.content = '{"satisfied": true, "rationale": "ok"}'
        return resp

    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_sync", fake_claude_json,
    )
    import litellm as _lt
    monkeypatch.setattr(_lt, "completion", fake_litellm)

    satisfied, rationale = lc.call_llm_check("rule", {"x": 1})
    assert satisfied is True
    assert rationale == "ok"
    assert called["which"] == "claude"


def test_llm_check_default_uses_litellm(monkeypatch):
    from opendaisugi import llm_check as lc

    called = {"which": None}

    def fake_claude_json(prompt, *, timeout_s, model, binary="claude"):
        called["which"] = "claude"
        return {"satisfied": True, "rationale": "via claude"}

    def fake_litellm(**_):
        called["which"] = "litellm"
        resp = MagicMock()
        resp.choices = [MagicMock()]
        resp.choices[0].message.content = '{"satisfied": false, "rationale": "via litellm"}'
        return resp

    monkeypatch.delenv("OPENDAISUGI_LLM_BACKEND", raising=False)
    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_sync", fake_claude_json,
    )
    import litellm as _lt
    monkeypatch.setattr(_lt, "completion", fake_litellm)

    satisfied, rationale = lc.call_llm_check("rule", {"x": 1})
    assert satisfied is False
    assert rationale == "via litellm"
    assert called["which"] == "litellm"


def test_parser_split_routes_through_backend(monkeypatch):
    from opendaisugi.parsers.claude_code import ClaudeCodeParser

    called = {"which": None}

    def fake_claude_json(prompt, *, timeout_s, model, binary="claude"):
        called["which"] = "claude"
        return {"subtasks": [{"task": "T", "start": 0, "end": 0}]}

    monkeypatch.setenv("OPENDAISUGI_LLM_BACKEND", "claude-code")
    monkeypatch.setattr(
        "opendaisugi.claude_code_llm.call_claude_p_json_sync", fake_claude_json,
    )

    parser = ClaudeCodeParser()
    result = parser._llm_split("msg", [{"type": "shell", "command": "ls"}])
    assert called["which"] == "claude"
    assert result == [{"task": "T", "start": 0, "end": 0}]
