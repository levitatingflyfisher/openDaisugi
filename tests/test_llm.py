"""Tests for opendaisugi.llm — instructor client factory + error translation."""

import instructor
import pytest

from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.llm import _redact_keys, get_instructor_client, translate_llm_error


def test_get_instructor_client_returns_async_client():
    client = get_instructor_client(model="anthropic/claude-sonnet-4-20250514")
    # instructor.from_litellm returns an instance with .chat.completions.create
    assert hasattr(client, "chat")
    assert hasattr(client.chat, "completions")
    assert hasattr(client.chat.completions, "create")


def test_get_instructor_client_uses_json_mode():
    # We want Mode.JSON, not Mode.TOOLS, per spec §"Structured output mode".
    # instructor exposes .mode on the wrapped client.
    client = get_instructor_client(model="anthropic/claude-sonnet-4-20250514")
    assert client.mode == instructor.Mode.JSON


def test_translate_litellm_exception_becomes_envelope_error():
    # Any exception type from the litellm side is normalized. Callers use
    # `raise translate_llm_error(e) from e` to attach __cause__; the function
    # itself just returns the wrapped exception.
    original = RuntimeError("429 rate limit")
    translated = translate_llm_error(original)
    assert isinstance(translated, EnvelopeGenerationError)
    assert "429 rate limit" in str(translated)


def test_translate_preserves_envelope_error_unchanged():
    # If we already have an EnvelopeGenerationError, pass it through.
    original = EnvelopeGenerationError("already typed")
    translated = translate_llm_error(original)
    assert translated is original


def test_translate_usage_in_try_except():
    with pytest.raises(EnvelopeGenerationError, match="simulated"):
        try:
            raise RuntimeError("simulated litellm failure")
        except Exception as e:
            raise translate_llm_error(e) from e


# --- key-redaction tests ---


def test_redact_keys_scrubs_anthropic_key():
    msg = "Incorrect API key provided: sk-ant-api03-aBcDeFgHiJkLmNoPqRsTuVwXyZ"
    redacted = _redact_keys(msg)
    assert "sk-ant-api03-aBcDeFgHiJkLmNoPqRsTuVwXyZ" not in redacted
    assert "sk-ant-..." in redacted  # prefix preserved (sk- + 4 chars = "sk-ant-")
    assert redacted.endswith("wXyZ")  # last 4 chars of key preserved


def test_redact_keys_leaves_non_key_strings_alone():
    msg = "Connection timed out after 30s"
    assert _redact_keys(msg) == msg


def test_translate_llm_error_redacts_key_in_message():
    exc = RuntimeError("Auth failed: sk-ant-api03-verylongkeythatshouldberedacted1234")
    wrapped = translate_llm_error(exc)
    assert isinstance(wrapped, EnvelopeGenerationError)
    assert "verylongkeythatshouldberedacted1234" not in str(wrapped)
