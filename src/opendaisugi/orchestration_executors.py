"""Executors for the skill and mcp orchestration step types (v0.32).

TaskStep runs through the existing :class:`~opendaisugi.delegating_executor.DelegatingExecutor`
(an LLM produces the subtask's answer). Skill and MCP steps need their own thin,
pluggable executors:

- :class:`SkillExecutor` resolves a ``SkillStep`` against a handler registry. A
  handler is any callable ``(SkillStep) -> str`` — in a real deployment it runs a
  distilled pathway or a contract-backed skill; in tests it's a lambda. This is
  the "repeated prompts via skills" execution seam.
- :class:`MCPExecutor` calls a pluggable transport ``(server, tool, arguments) -> Any``.
  No live MCP client is a hard dependency (D3): ship the step type + executor +
  protocol; wire a real transport at deployment.

Both satisfy the :class:`~opendaisugi.executor.StepExecutor` protocol and raise
``TypeError`` when handed the wrong step kind, matching the other step-type
specialists (FileReadExecutor etc.).
"""

from __future__ import annotations

import json
import time
from typing import Any, Callable, Protocol, runtime_checkable

from opendaisugi.executor import ExecutorResult, truncate_output
from opendaisugi.models import MCPStep, SkillStep

SkillHandler = Callable[[SkillStep], str]


@runtime_checkable
class MCPTransport(Protocol):
    """Invokes an MCP tool and returns its (JSON-serializable) result."""

    def __call__(self, server: str, tool: str, arguments: dict[str, Any]) -> Any: ...


class SkillExecutor:
    """Runs a SkillStep by dispatching to a registered handler.

    ``handlers`` maps ``skill_id`` to a callable that takes the step and returns
    the skill's output string. An unknown skill or a handler that raises yields
    ``rc=1`` (a failed step the Supervisor halts on) rather than an exception —
    executor infrastructure failures must not crash the run.
    """

    def __init__(self, *, handlers: dict[str, SkillHandler] | None = None) -> None:
        self.handlers: dict[str, SkillHandler] = dict(handlers or {})

    def run(self, step, *, timeout_s: int, max_output_bytes: int) -> ExecutorResult:
        if not isinstance(step, SkillStep):
            raise TypeError(f"SkillExecutor cannot run step of type {type(step).__name__}")
        start = time.monotonic()
        handler = self.handlers.get(step.skill_id)
        if handler is None:
            return ExecutorResult(
                rc=1,
                stdout=f"no handler registered for skill {step.skill_id!r}",
                duration_ms=(time.monotonic() - start) * 1000.0,
                timed_out=False,
            )
        try:
            out = handler(step)
        except Exception as exc:  # noqa: BLE001 — surface as a failed step
            return ExecutorResult(
                rc=1,
                stdout=f"skill {step.skill_id!r} error: {type(exc).__name__}: {exc}",
                duration_ms=(time.monotonic() - start) * 1000.0,
                timed_out=False,
            )
        text = out if isinstance(out, str) else json.dumps(out, default=str)
        return ExecutorResult(
            rc=0, stdout=truncate_output(text, max_output_bytes),
            duration_ms=(time.monotonic() - start) * 1000.0, timed_out=False,
        )


class MCPExecutor:
    """Runs an MCPStep by calling a pluggable transport.

    ``transport(server, tool, arguments)`` performs the actual MCP call and
    returns a JSON-serializable result. With no transport configured the step
    fails (``rc=1``) — the executor never pretends an MCP call succeeded.
    """

    def __init__(self, *, transport: MCPTransport | None = None) -> None:
        self.transport = transport

    def run(self, step, *, timeout_s: int, max_output_bytes: int) -> ExecutorResult:
        if not isinstance(step, MCPStep):
            raise TypeError(f"MCPExecutor cannot run step of type {type(step).__name__}")
        start = time.monotonic()
        if self.transport is None:
            return ExecutorResult(
                rc=1,
                stdout="no MCP transport configured; pass MCPExecutor(transport=...)",
                duration_ms=(time.monotonic() - start) * 1000.0,
                timed_out=False,
            )
        try:
            result = self.transport(step.server, step.tool, step.arguments)
        except Exception as exc:  # noqa: BLE001 — surface as a failed step
            return ExecutorResult(
                rc=1,
                stdout=f"mcp {step.server}/{step.tool} error: {type(exc).__name__}: {exc}",
                duration_ms=(time.monotonic() - start) * 1000.0,
                timed_out=False,
            )
        text = json.dumps(result, default=str)
        return ExecutorResult(
            rc=0, stdout=truncate_output(text, max_output_bytes),
            duration_ms=(time.monotonic() - start) * 1000.0, timed_out=False,
        )


__all__ = ["MCPExecutor", "MCPTransport", "SkillExecutor", "SkillHandler"]
