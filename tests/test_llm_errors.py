# tests/test_llm_errors.py
"""LLM error translation: one plain line, never instructor's XML dump."""

from __future__ import annotations

from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.llm import translate_llm_error


class _Attempt:
    def __init__(self, n: int, exc: BaseException) -> None:
        self.attempt_number = n
        self.exception = exc
        self.completion = None


class _RetryExc(Exception):
    """Shape of instructor.core.exceptions.InstructorRetryException."""

    def __init__(self, attempts: list[_Attempt]) -> None:
        super().__init__("<failed_attempts>\n<generation number=\"1\">...</failed_attempts>")
        self.failed_attempts = attempts


def test_retry_exception_becomes_one_line():
    exc = _RetryExc([_Attempt(1, ValueError("bad json")), _Attempt(2, ValueError("bad json"))])
    out = translate_llm_error(exc)
    assert isinstance(out, EnvelopeGenerationError)
    text = str(out)
    assert "<failed_attempts>" not in text
    assert "ValueError: bad json" in text
    assert "(2 attempts)" in text
    assert "\n" not in text


def test_plain_exception_keeps_its_message():
    assert str(translate_llm_error(RuntimeError("boom"))) == "boom"


def test_retry_exception_with_multiline_inner_keeps_first_line():
    exc = _RetryExc([_Attempt(1, ValueError("line one\nline two"))])
    assert str(translate_llm_error(exc)) == "ValueError: line one (1 attempts)"


import pytest

from opendaisugi.exceptions import LLMNotConfigured, OpenDaisugiError
from opendaisugi.llm import preflight


def test_litellm_anthropic_model_without_key_fails_with_the_fix(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    with pytest.raises(LLMNotConfigured) as ei:
        preflight("litellm", model="anthropic/claude-sonnet-4-20250514")
    text = str(ei.value)
    assert text.count("\n") == 2, text  # what / why / next action
    assert "ANTHROPIC_API_KEY" in text
    assert "--llm claude-code" in text
    assert isinstance(ei.value, OpenDaisugiError)


def test_litellm_other_provider_is_not_checked_here(monkeypatch):
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    assert preflight("litellm", model="openai/gpt-4o") == "litellm"


def test_litellm_with_key_passes(monkeypatch):
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
    assert preflight("litellm", model="anthropic/x") == "litellm"


def test_claude_code_without_binary_fails_with_the_fix():
    with pytest.raises(LLMNotConfigured) as ei:
        preflight("claude-code", which=lambda _name: None)
    text = str(ei.value)
    assert text.count("\n") == 2
    assert "claude" in text and "PATH" in text
    assert "--llm litellm" in text


def test_claude_code_with_binary_passes():
    assert preflight("claude-code", which=lambda _name: "/usr/bin/claude") == "claude-code"


def test_claude_code_honors_monkeypatched_shutil_which(monkeypatch):
    """Fix round 1, item 4: `which=shutil.which` bound the function object at def
    time, so `monkeypatch.setattr("shutil.which", ...)` couldn't reach the
    default — only an explicitly-passed `which=` was ever injectable. Callers
    (e.g. tests, or any caller not passing which=) must see the real environment.
    """
    monkeypatch.setattr("shutil.which", lambda _name: None)
    with pytest.raises(LLMNotConfigured):
        preflight("claude-code")


def test_get_instructor_client_preflights_before_importing_instructor(monkeypatch):
    import sys

    from opendaisugi import llm

    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    monkeypatch.delenv("ANTHROPIC_AUTH_TOKEN", raising=False)
    monkeypatch.setitem(sys.modules, "instructor", None)  # import would now fail loudly
    with pytest.raises(LLMNotConfigured):
        llm.get_instructor_client(model="anthropic/x", backend="litellm")
