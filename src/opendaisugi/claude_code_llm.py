"""ClaudeCode-as-LLM backend: route opendaisugi LLM calls through ``claude -p``.

Subscription-credits users exercise the full pipeline (envelope generation,
distillation, recompute fallback, LLMCheck, transcript parsing) without an
API key. The ``ClaudeCodeTier1Provider`` subprocess machinery is the
blueprint — we generalize it so every LLM call site can use this path.

Select this backend by setting ``OPENDAISUGI_LLM_BACKEND=claude-code`` or by
passing ``--llm claude-code`` to the CLI.
"""

from __future__ import annotations

import asyncio
import json
import logging
import subprocess
from typing import Any, TypeVar

from pydantic import BaseModel

from opendaisugi.exceptions import EnvelopeGenerationError

_log = logging.getLogger("opendaisugi.claude_code_llm")
T = TypeVar("T", bound=BaseModel)


async def call_claude_p_async(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
    extra_args: tuple[str, ...] = (),
) -> str:
    """Call ``claude -p <prompt>`` asynchronously; return stdout stripped."""
    args = [binary, "-p", prompt]
    if model is not None:
        args.extend(["--model", model])
    args.extend(extra_args)

    try:
        proc = await asyncio.create_subprocess_exec(
            *args,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
        )
    except FileNotFoundError as exc:
        raise EnvelopeGenerationError(
            f"claude binary not found: {binary!r}"
        ) from exc

    try:
        stdout_bytes, stderr_bytes = await asyncio.wait_for(
            proc.communicate(), timeout=timeout_s,
        )
    except asyncio.TimeoutError:
        try:
            proc.terminate()
        except ProcessLookupError:
            pass
        raise
    except BaseException:
        try:
            proc.terminate()
        except ProcessLookupError:
            pass
        raise

    if proc.returncode != 0:
        stderr = stderr_bytes[:500].decode("utf-8", "replace")
        raise EnvelopeGenerationError(
            f"claude -p exited {proc.returncode}: {stderr!r}"
        )

    return stdout_bytes.decode("utf-8", "replace").strip()


def call_claude_p_sync(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
    extra_args: tuple[str, ...] = (),
) -> str:
    """Synchronous variant of :func:`call_claude_p_async`."""
    args = [binary, "-p", prompt]
    if model is not None:
        args.extend(["--model", model])
    args.extend(extra_args)

    try:
        result = subprocess.run(
            args,
            capture_output=True,
            text=True,
            timeout=timeout_s,
            check=False,
        )
    except FileNotFoundError as exc:
        raise EnvelopeGenerationError(
            f"claude binary not found: {binary!r}"
        ) from exc
    except subprocess.TimeoutExpired as exc:
        raise EnvelopeGenerationError(
            f"claude -p timed out after {timeout_s}s"
        ) from exc

    if result.returncode != 0:
        stderr = (result.stderr or "")[:500]
        raise EnvelopeGenerationError(
            f"claude -p exited {result.returncode}: {stderr!r}"
        )

    return (result.stdout or "").strip()


def _extract_first_json_object(text: str) -> dict:
    start = text.find("{")
    end = text.rfind("}")
    if start == -1 or end <= start:
        raise EnvelopeGenerationError(
            f"no JSON object in claude -p stdout: {text[:200]!r}"
        )
    try:
        return json.loads(text[start : end + 1])
    except json.JSONDecodeError as exc:
        raise EnvelopeGenerationError(
            f"claude -p stdout was not valid JSON: {exc}"
        ) from exc


def call_claude_p_json_sync(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
) -> dict:
    """Call ``claude -p`` synchronously; return the first JSON object in stdout."""
    stdout = call_claude_p_sync(
        prompt, timeout_s=timeout_s, model=model, binary=binary,
    )
    return _extract_first_json_object(stdout)


_SCHEMA_PREAMBLE = (
    "Respond with ONLY a JSON object that validates against the following schema.\n"
    "No prose, no code fences, no explanation.\n\n"
)


def _augment_prompt_with_schema(prompt: str, model: type[BaseModel]) -> str:
    schema = json.dumps(model.model_json_schema(), indent=2)
    return (
        f"{_SCHEMA_PREAMBLE}<json_schema>\n{schema}\n</json_schema>\n\n{prompt}"
    )


def _flatten_messages(messages: list[dict]) -> str:
    parts: list[str] = []
    for m in messages:
        role = m.get("role", "user")
        content = m.get("content", "")
        parts.append(f"[{role}]\n{content}")
    return "\n\n".join(parts)


async def call_claude_p_structured(
    prompt: str,
    response_model: type[T],
    *,
    timeout_s: float = 120.0,
    model: str | None = "haiku",
    binary: str = "claude",
) -> T:
    """Call ``claude -p`` with a schema-augmented prompt; validate the output."""
    augmented = _augment_prompt_with_schema(prompt, response_model)
    stdout = await call_claude_p_async(
        augmented, timeout_s=timeout_s, model=model, binary=binary,
    )
    payload = _extract_first_json_object(stdout)
    try:
        return response_model.model_validate(payload)
    except Exception as exc:
        raise EnvelopeGenerationError(
            f"claude -p output failed {response_model.__name__} validation: {exc}"
        ) from exc


class _Completions:
    def __init__(self, parent: "ClaudeCodeInstructorClient") -> None:
        self._parent = parent

    async def create(
        self,
        *,
        model: str,
        messages: list[dict],
        response_model: type[BaseModel] | None = None,
        max_retries: int = 0,
        **_: Any,
    ) -> Any:
        del model, max_retries  # accepted for instructor signature-compat
        prompt = _flatten_messages(messages)
        if response_model is None:
            return await call_claude_p_async(
                prompt,
                timeout_s=self._parent.timeout_s,
                model=self._parent.model_flag,
                binary=self._parent.binary,
            )
        return await call_claude_p_structured(
            prompt,
            response_model,
            timeout_s=self._parent.timeout_s,
            model=self._parent.model_flag,
            binary=self._parent.binary,
        )


class _Chat:
    def __init__(self, parent: "ClaudeCodeInstructorClient") -> None:
        self.completions = _Completions(parent)


class ClaudeCodeInstructorClient:
    """Instructor-compatible shim that routes through ``claude -p``.

    Exposes ``.chat.completions.create(model=..., response_model=..., messages=[...])``
    so any call site written against ``instructor.AsyncInstructor`` works
    unchanged when the backend is switched to ``claude-code``.
    """

    def __init__(
        self,
        *,
        binary: str = "claude",
        model_flag: str | None = "haiku",
        timeout_s: float = 120.0,
    ) -> None:
        self.binary = binary
        self.model_flag = model_flag
        self.timeout_s = timeout_s
        self.chat = _Chat(self)


__all__ = [
    "call_claude_p_async",
    "call_claude_p_sync",
    "call_claude_p_json_sync",
    "call_claude_p_structured",
    "ClaudeCodeInstructorClient",
]
