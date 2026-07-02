"""Tests for the ClaudeCodeTier1Provider adapter (v0.4.0).

The adapter shells out to ``claude -p``. Tests monkeypatch
``asyncio.create_subprocess_exec`` with a fake process so nothing real runs.
"""

from __future__ import annotations

import asyncio
import json
from unittest.mock import AsyncMock, patch

import pytest

from opendaisugi.tier1 import ClaudeCodeTier1Provider, Tier1Provider


_VALID_ENVELOPE_JSON = json.dumps({
    "generated_by": "claude-code-tier1",
    "task": "demo",
    "permissions": {
        "file_read": [], "file_write": [],
        "network": False, "shell": False, "shell_allowlist": [],
        "max_execution_time_s": 30, "max_output_size_mb": 10,
    },
    "invariants": [],
    "postconditions": [{"type": "exit_code", "expected": 0}],
})


class _FakeProc:
    def __init__(self, *, stdout: bytes, stderr: bytes = b"", returncode: int = 0) -> None:
        self._stdout = stdout
        self._stderr = stderr
        self.returncode = returncode
        self._terminated = False

    async def communicate(self) -> tuple[bytes, bytes]:
        return self._stdout, self._stderr

    def terminate(self) -> None:
        self._terminated = True


@pytest.mark.asyncio
async def test_happy_path_parses_envelope() -> None:
    proc = _FakeProc(stdout=_VALID_ENVELOPE_JSON.encode())
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=proc)):
        env = await ClaudeCodeTier1Provider().generate_envelope("demo")
    assert env is not None
    assert env.task == "demo"


@pytest.mark.asyncio
async def test_wraps_plain_text_with_json_inside() -> None:
    """Binary sometimes prints prose around the JSON; adapter tolerates it."""
    wrapped = f"Here is the envelope:\n\n{_VALID_ENVELOPE_JSON}\n\nDone."
    proc = _FakeProc(stdout=wrapped.encode())
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=proc)):
        env = await ClaudeCodeTier1Provider().generate_envelope("demo")
    assert env is not None


@pytest.mark.asyncio
async def test_missing_binary_declines() -> None:
    with patch("asyncio.create_subprocess_exec", AsyncMock(side_effect=FileNotFoundError)):
        assert await ClaudeCodeTier1Provider().generate_envelope("t") is None


@pytest.mark.asyncio
async def test_nonzero_exit_declines() -> None:
    proc = _FakeProc(stdout=b"", stderr=b"api key missing", returncode=1)
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=proc)):
        assert await ClaudeCodeTier1Provider().generate_envelope("t") is None


@pytest.mark.asyncio
async def test_no_json_in_stdout_declines() -> None:
    proc = _FakeProc(stdout=b"sorry, I cannot help with that request\n")
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=proc)):
        assert await ClaudeCodeTier1Provider().generate_envelope("t") is None


@pytest.mark.asyncio
async def test_invalid_json_declines() -> None:
    proc = _FakeProc(stdout=b'{"task": "t", "permissions": }')
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=proc)):
        assert await ClaudeCodeTier1Provider().generate_envelope("t") is None


@pytest.mark.asyncio
async def test_timeout_terminates_subprocess() -> None:
    class _HangProc:
        returncode = None
        terminated = False
        killed = False

        async def communicate(self):
            await asyncio.sleep(10)
            return b"", b""

        def terminate(self):
            self.terminated = True

        def kill(self):
            self.killed = True

        async def wait(self):
            return 0

    hang = _HangProc()
    with patch("asyncio.create_subprocess_exec", AsyncMock(return_value=hang)):
        provider = ClaudeCodeTier1Provider(timeout_s=0.05)
        assert await provider.generate_envelope("t") is None
    assert hang.terminated


def test_satisfies_protocol() -> None:
    assert isinstance(ClaudeCodeTier1Provider(), Tier1Provider)
