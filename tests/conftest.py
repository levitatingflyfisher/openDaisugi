"""Shared pytest fixtures for opendaisugi tests."""

import os
from pathlib import Path
from typing import Any

import pytest

from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    Postcondition,
    ShellStep,
)


MJCF_TWO_JOINT_ARM = Path(__file__).parent / "fixtures" / "mjcf" / "two_joint_arm.xml"


@pytest.fixture(autouse=True)
def _isolate_llm_backend_env():
    """Snapshot+restore OPENDAISUGI_LLM_BACKEND around every test.

    The CLI's ``--llm`` handling sets this env var via a direct ``os.environ[...]``
    write (not through monkeypatch), so an in-process CliRunner invoke with
    ``--llm claude-code`` would otherwise LEAK the var into every later test —
    flipping the global backend and making litellm-mocking tests fire real
    ``claude -p`` subprocesses. Restore it unconditionally so tests stay isolated.
    """
    sentinel = object()
    before = os.environ.get("OPENDAISUGI_LLM_BACKEND", sentinel)
    try:
        yield
    finally:
        if before is sentinel:
            os.environ.pop("OPENDAISUGI_LLM_BACKEND", None)
        else:
            os.environ["OPENDAISUGI_LLM_BACKEND"] = before


@pytest.fixture
def two_joint_arm_mjcf() -> str:
    """Absolute path to the two-DOF planar arm + gripper + block fixture."""
    return str(MJCF_TWO_JOINT_ARM)


@pytest.fixture
def sample_envelope() -> Envelope:
    """A hand-crafted envelope for a known task — no LLM call required."""
    return Envelope(
        generated_by="test",
        task="Delete .tmp files in /var/log",
        permissions=Permission(
            shell=True,
            shell_allowlist=["find"],
            file_read=["/var/log/**"],
        ),
        postconditions=[Postcondition(type="exit_code", expected=0)],
    )


@pytest.fixture
def sample_plan() -> ActionPlan:
    """A minimal single-step plan that satisfies sample_envelope."""
    return ActionPlan(
        source="vanilla-llm",
        task="Delete .tmp files in /var/log",
        steps=[
            ShellStep(
                id="s1",
                command="find /var/log -name '*.tmp' -mtime +7 -delete",
            ),
        ],
    )


class _FakeCompletions:
    """Returned by mock_llm_client.chat.completions.

    The ``create`` coroutine ignores every argument except capturing them on
    ``last_call`` so individual tests can make assertions about what was
    passed (e.g. max_retries, message content).

    Tests that need to return a different envelope for the next call can use
    the ``set_next_envelope`` helper (reset after one use). Call count is
    tracked on ``.call_count``.
    """

    def __init__(self, envelope):
        self._envelope = envelope
        self._next_envelope: Any = None
        self.last_call: dict[str, Any] = {}
        self.call_count: int = 0

    def set_next_envelope(self, envelope) -> None:
        """Override the envelope returned by the next ``create()`` call."""
        self._next_envelope = envelope

    async def create(self, **kwargs) -> Any:
        self.last_call = kwargs
        self.call_count += 1
        if self._next_envelope is not None:
            env = self._next_envelope
            self._next_envelope = None
            return env
        return self._envelope


class _FakeChat:
    def __init__(self, envelope):
        self.completions = _FakeCompletions(envelope)


class _FakeInstructorClient:
    """Minimal stand-in for the instructor AsyncInstructor shape.

    Exposes ``set_next_envelope`` and ``call_count`` as pass-throughs to the
    underlying ``_FakeCompletions`` for ergonomic test usage.
    """

    def __init__(self, envelope):
        self.chat = _FakeChat(envelope)

    def set_next_envelope(self, envelope) -> None:
        self.chat.completions.set_next_envelope(envelope)

    @property
    def call_count(self) -> int:
        return self.chat.completions.call_count


@pytest.fixture
def mock_llm_client(monkeypatch, sample_envelope):
    """Patch opendaisugi.llm.get_instructor_client to return a fake client.

    The fake client's ``chat.completions.create`` always returns
    ``sample_envelope``, and captures all kwargs on ``.last_call`` so tests
    can assert on what generate_envelope passed through.
    """
    fake = _FakeInstructorClient(sample_envelope)

    from opendaisugi import llm
    monkeypatch.setattr(llm, "get_instructor_client", lambda model: fake)
    return fake
