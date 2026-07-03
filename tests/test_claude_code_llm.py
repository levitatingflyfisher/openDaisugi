"""Unit tests for claude_code_llm helpers."""

from __future__ import annotations

import asyncio
import subprocess
from unittest.mock import AsyncMock, MagicMock, patch

import pytest
from pydantic import BaseModel

from opendaisugi.claude_code_llm import (
    ClaudeCodeInstructorClient,
    call_claude_p_async,
    call_claude_p_json_sync,
    call_claude_p_structured,
    call_claude_p_sync,
)
from opendaisugi.exceptions import EnvelopeGenerationError


@pytest.mark.asyncio
async def test_call_async_returns_stdout():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b"hello world\n", b""))
    mock_proc.returncode = 0
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        result = await call_claude_p_async("prompt text", timeout_s=5.0)
    assert result == "hello world"


@pytest.mark.asyncio
async def test_call_async_missing_binary_raises():
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(side_effect=FileNotFoundError("no claude")),
    ):
        with pytest.raises(EnvelopeGenerationError, match="claude binary not found"):
            await call_claude_p_async("prompt", timeout_s=1.0)


@pytest.mark.asyncio
async def test_call_async_timeout_raises():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(side_effect=asyncio.TimeoutError)
    mock_proc.terminate = MagicMock()
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        with patch(
            "opendaisugi.claude_code_llm.asyncio.wait_for",
            AsyncMock(side_effect=asyncio.TimeoutError),
        ):
            with pytest.raises(asyncio.TimeoutError):
                await call_claude_p_async("prompt", timeout_s=0.01)
    mock_proc.terminate.assert_called_once()


@pytest.mark.asyncio
async def test_call_async_nonzero_exit_raises():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b"", b"boom\n"))
    mock_proc.returncode = 2
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        with pytest.raises(EnvelopeGenerationError, match="exited 2"):
            await call_claude_p_async("prompt", timeout_s=5.0)


def test_call_sync_returns_stdout():
    fake = MagicMock(returncode=0, stdout="ok\n", stderr="")
    with patch("opendaisugi.claude_code_llm.subprocess.run", return_value=fake):
        out = call_claude_p_sync("prompt", timeout_s=5.0)
    assert out == "ok"


def test_call_sync_missing_binary_raises():
    with patch(
        "opendaisugi.claude_code_llm.subprocess.run",
        side_effect=FileNotFoundError("nope"),
    ):
        with pytest.raises(EnvelopeGenerationError, match="claude binary not found"):
            call_claude_p_sync("prompt", timeout_s=5.0)


def test_call_sync_timeout_raises():
    with patch(
        "opendaisugi.claude_code_llm.subprocess.run",
        side_effect=subprocess.TimeoutExpired(cmd="claude", timeout=5.0),
    ):
        with pytest.raises(EnvelopeGenerationError, match="timed out"):
            call_claude_p_sync("prompt", timeout_s=5.0)


def test_call_sync_nonzero_exit_raises():
    fake = MagicMock(returncode=3, stdout="", stderr="nope\n")
    with patch("opendaisugi.claude_code_llm.subprocess.run", return_value=fake):
        with pytest.raises(EnvelopeGenerationError, match="exited 3"):
            call_claude_p_sync("prompt", timeout_s=5.0)


def test_call_json_sync_extracts_first_object():
    fake = MagicMock(
        returncode=0,
        stdout='preamble {"k": 42, "v": "x"} trailing',
        stderr="",
    )
    with patch("opendaisugi.claude_code_llm.subprocess.run", return_value=fake):
        out = call_claude_p_json_sync("prompt", timeout_s=5.0)
    assert out == {"k": 42, "v": "x"}


def test_call_json_sync_raises_on_no_json():
    fake = MagicMock(returncode=0, stdout="no json here at all", stderr="")
    with patch("opendaisugi.claude_code_llm.subprocess.run", return_value=fake):
        with pytest.raises(EnvelopeGenerationError, match="no JSON"):
            call_claude_p_json_sync("prompt", timeout_s=5.0)


def test_call_json_sync_raises_on_invalid_json():
    fake = MagicMock(returncode=0, stdout="preamble { not valid } trailing", stderr="")
    with patch("opendaisugi.claude_code_llm.subprocess.run", return_value=fake):
        with pytest.raises(EnvelopeGenerationError, match="not valid JSON"):
            call_claude_p_json_sync("prompt", timeout_s=5.0)


class _Toy(BaseModel):
    a: int
    b: str


@pytest.mark.asyncio
async def test_call_structured_validates():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b'{"a": 7, "b": "hi"}', b""))
    mock_proc.returncode = 0
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        out = await call_claude_p_structured("prompt", _Toy, timeout_s=5.0)
    assert out == _Toy(a=7, b="hi")


@pytest.mark.asyncio
async def test_call_structured_raises_on_validation_failure():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b'{"a": "not-an-int", "b": "hi"}', b""))
    mock_proc.returncode = 0
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        with pytest.raises(EnvelopeGenerationError, match="_Toy"):
            await call_claude_p_structured("prompt", _Toy, timeout_s=5.0)


@pytest.mark.asyncio
async def test_instructor_shim_matches_signature():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b'{"a": 1, "b": "x"}', b""))
    mock_proc.returncode = 0
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        client = ClaudeCodeInstructorClient()
        result = await client.chat.completions.create(
            model="haiku",
            response_model=_Toy,
            messages=[
                {"role": "system", "content": "you are a json emitter"},
                {"role": "user", "content": "emit"},
            ],
        )
    assert result == _Toy(a=1, b="x")


@pytest.mark.asyncio
async def test_instructor_shim_without_response_model_returns_text():
    mock_proc = MagicMock()
    mock_proc.communicate = AsyncMock(return_value=(b"freeform text", b""))
    mock_proc.returncode = 0
    with patch(
        "opendaisugi.claude_code_llm.asyncio.create_subprocess_exec",
        AsyncMock(return_value=mock_proc),
    ):
        client = ClaudeCodeInstructorClient()
        result = await client.chat.completions.create(
            model="haiku",
            messages=[{"role": "user", "content": "say hi"}],
        )
    assert result == "freeform text"


# --------------------- arg-injection + stdin guards (SGCM review) ---------------------

def test_sync_model_and_prompt_cannot_inject_flags(monkeypatch):
    import subprocess as _sp
    from opendaisugi.claude_code_llm import call_claude_p_sync
    captured = {}

    def fake_run(args, **kwargs):
        captured["args"] = args
        captured["stdin"] = kwargs.get("stdin")
        return _sp.CompletedProcess(args, 0, stdout="ok", stderr="")

    monkeypatch.setattr("subprocess.run", fake_run)
    # A malicious model/prompt starting with '-' must NOT become a claude flag.
    call_claude_p_sync("--dangerously-skip-permissions", model="--output-format", timeout_s=1)
    args = captured["args"]
    # model bound with '=' form (value can't be reparsed as a flag)
    assert "--model=--output-format" in args
    assert "--output-format" not in args  # never a bare token
    # prompt placed after a '--' separator (parser treats it as positional, not a flag)
    assert "--" in args
    assert args.index("--") < args.index("--dangerously-skip-permissions")
    # stdin closed so claude -p doesn't block waiting for piped input
    assert captured["stdin"] is _sp.DEVNULL


def test_sync_prompt_after_separator(monkeypatch):
    import subprocess as _sp
    from opendaisugi.claude_code_llm import call_claude_p_sync
    captured = {}
    monkeypatch.setattr("subprocess.run",
                        lambda args, **k: captured.update(args=args) or _sp.CompletedProcess(args, 0, stdout="x", stderr=""))
    call_claude_p_sync("hello", model=None, timeout_s=1)
    args = captured["args"]
    assert args[-2:] == ["--", "hello"]  # prompt is the final positional


# --------------------- metered accounting (SGCM review) ---------------------

def test_metered_counts_all_token_kinds_including_cache(monkeypatch):
    import json as _json
    from opendaisugi.claude_code_llm import call_claude_p_metered
    envelope = {
        "result": "the answer",
        "total_cost_usd": 0.0126,
        "usage": {"input_tokens": 10, "output_tokens": 166,
                  "cache_creation_input_tokens": 4800, "cache_read_input_tokens": 22011},
    }
    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_sync",
                        lambda *a, **k: _json.dumps(envelope))
    text, meter = call_claude_p_metered("x", timeout_s=1)
    assert text == "the answer"
    assert meter["tokens"] == 10 + 166 + 4800 + 22011  # NOT just 176
    assert meter["cost_usd"] == 0.0126


def test_metered_raises_on_is_error(monkeypatch):
    import json as _json
    from opendaisugi.claude_code_llm import call_claude_p_metered
    from opendaisugi.exceptions import EnvelopeGenerationError
    envelope = {"result": "hit max turns", "is_error": True, "usage": {}}
    monkeypatch.setattr("opendaisugi.claude_code_llm.call_claude_p_sync",
                        lambda *a, **k: _json.dumps(envelope))
    with pytest.raises(EnvelopeGenerationError):
        call_claude_p_metered("x", timeout_s=1)


# --------------------- DAISUGI_CLAUDE_ARGS passthrough (issue: --dangerously-skip-permissions) ---

def test_build_args_merges_configured_claude_args(monkeypatch):
    from opendaisugi.claude_code_llm import _build_claude_args
    monkeypatch.setenv("DAISUGI_CLAUDE_ARGS", "--dangerously-skip-permissions")
    args = _build_claude_args("claude", "do the thing", "haiku", ())
    assert "--dangerously-skip-permissions" in args
    # still injection-safe: model bound with =, prompt after the -- separator
    assert "--model=haiku" in args
    sep = args.index("--")
    assert args[sep + 1] == "do the thing"
    assert args.index("--dangerously-skip-permissions") < sep  # a real flag, before --


def test_build_args_shlex_splits_quoted_allowed_tools(monkeypatch):
    from opendaisugi.claude_code_llm import _build_claude_args
    monkeypatch.setenv("DAISUGI_CLAUDE_ARGS", '--allowedTools "Bash(ls:*) Read"')
    args = _build_claude_args("claude", "q", None, ())
    assert "--allowedTools" in args
    assert "Bash(ls:*) Read" in args  # the quoted value stays one token


def test_build_args_no_env_is_noop(monkeypatch):
    from opendaisugi.claude_code_llm import _build_claude_args
    monkeypatch.delenv("DAISUGI_CLAUDE_ARGS", raising=False)
    args = _build_claude_args("claude", "q", "haiku", ("--output-format", "json"))
    assert args == ["claude", "-p", "--model=haiku", "--output-format", "json", "--", "q"]


async def test_tier1_uses_safe_builder_and_configured_args(monkeypatch):
    # tier1 previously built args itself (prompt right after -p, --model space form);
    # it now routes through _build_claude_args → injection-safe + env flags merged.
    from unittest.mock import patch
    from opendaisugi.tier1 import ClaudeCodeTier1Provider

    monkeypatch.setenv("DAISUGI_CLAUDE_ARGS", "--dangerously-skip-permissions")
    captured = {}

    class _Proc:
        returncode = 0
        async def communicate(self): return (b"{}", b"")
        def terminate(self): ...
        def kill(self): ...
        async def wait(self): return 0

    async def fake_exec(*args, **kwargs):
        captured["args"] = list(args)
        return _Proc()

    with patch("asyncio.create_subprocess_exec", side_effect=fake_exec):
        await ClaudeCodeTier1Provider(model_flag="haiku").generate_envelope("t")
    a = captured["args"]
    assert "--dangerously-skip-permissions" in a
    assert "--model=haiku" in a           # = form, not the old ["--model","haiku"]
    assert a[-2] == "--" and a[-1]         # prompt after the separator
