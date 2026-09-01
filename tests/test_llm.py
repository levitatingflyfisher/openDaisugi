"""Tests for opendaisugi.llm — instructor client factory + error translation."""

import pytest

instructor = pytest.importorskip("instructor")  # v0.47: [generate] extra, not base

from opendaisugi.exceptions import EnvelopeGenerationError
from opendaisugi.llm import _redact_keys, get_instructor_client, translate_llm_error


def test_get_instructor_client_returns_async_client(monkeypatch):
    # preflight (v0.41) requires a key for an Anthropic model on litellm; a
    # dead client that fails only at call time was the defect it fixes.
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
    client = get_instructor_client(model="anthropic/claude-sonnet-4-20250514")
    # instructor.from_litellm returns an instance with .chat.completions.create
    assert hasattr(client, "chat")
    assert hasattr(client.chat, "completions")
    assert hasattr(client.chat.completions, "create")


def test_get_instructor_client_uses_json_mode(monkeypatch):
    # We want Mode.JSON, not Mode.TOOLS, per spec §"Structured output mode".
    # instructor exposes .mode on the wrapped client.
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
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


# GD-16: on the litellm path too, NaN, Infinity or -Infinity in a reply's
# numbers is schema-invalid: instructor re-asks with the error, then gives up.
_NAN_REPLY = '{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": NaN}}'
_NAN_ERROR = (
    "1 validation error for Envelope\n"
    "permissions.velocity_limit\n"
    "  Input should be a finite number [type=finite_number, input_value=nan, input_type=float]\n"
    "    For further information visit https://errors.pydantic.dev/2.13/v/finite_number"
)


def _fake_litellm(monkeypatch, replies):
    import litellm

    sent = []

    async def fake(**kw):
        sent.append(kw["messages"])
        return litellm.ModelResponse(
            model="claude-x",
            choices=[{"message": {"role": "assistant", "content": replies.pop(0)},
                      "finish_reason": "stop", "index": 0}],
        )

    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-test")
    monkeypatch.setattr(litellm, "acompletion", fake)
    return sent


def _create(response_model, max_retries):
    import asyncio

    client = get_instructor_client(model="anthropic/claude-sonnet-4-20250514", backend="litellm")
    return asyncio.run(client.chat.completions.create(
        model="anthropic/claude-sonnet-4-20250514",
        response_model=response_model,
        messages=[{"role": "user", "content": "go"}],
        max_retries=max_retries,
    ))


def test_litellm_reply_with_nan_is_reasked_then_refused(monkeypatch):
    from opendaisugi.models import Envelope

    sent = _fake_litellm(monkeypatch, [_NAN_REPLY, _NAN_REPLY])
    with pytest.raises(Exception) as exc:
        _create(Envelope, 1)
    assert translate_llm_error(exc.value).args[0] == (
        "ValidationError: 1 validation error for Envelope (2 attempts)"
    )
    assert _NAN_ERROR in sent[1][-1]["content"]


def test_litellm_reply_after_a_reask_is_the_callers_own_model(monkeypatch):
    from opendaisugi.models import Envelope

    good = '{"task": "t", "generated_by": "g", "permissions": {"velocity_limit": 1e3}}'
    _fake_litellm(monkeypatch, [_NAN_REPLY.replace("NaN", "-Infinity"), good])
    env = _create(Envelope, 1)
    assert type(env) is Envelope
    assert env.permissions.velocity_limit == 1000.0


def test_litellm_client_keeps_instructors_prompt_bytes(monkeypatch):
    # The check must not change what instructor sends: same schema text.
    from opendaisugi.models import Envelope

    sent = _fake_litellm(monkeypatch, ['{"task": "t", "generated_by": "g", "permissions": {}}'])
    _create(Envelope, 0)
    import litellm

    plain = []

    async def fake(**kw):
        plain.append(kw["messages"])
        return litellm.ModelResponse(
            model="claude-x",
            choices=[{"message": {"role": "assistant", "content": '{"task": "t", "generated_by": "g", "permissions": {}}'},
                      "finish_reason": "stop", "index": 0}],
        )

    import asyncio

    asyncio.run(instructor.from_litellm(fake, mode=instructor.Mode.JSON).chat.completions.create(
        model="anthropic/claude-sonnet-4-20250514",
        response_model=Envelope,
        messages=[{"role": "user", "content": "go"}],
        max_retries=0,
    ))
    assert sent == plain
