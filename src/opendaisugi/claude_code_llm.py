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
import os
import shlex
import subprocess
import tempfile
from typing import Any, TypeVar

from pydantic import BaseModel

from opendaisugi.exceptions import EnvelopeGenerationError

_log = logging.getLogger("opendaisugi.claude_code_llm")
T = TypeVar("T", bound=BaseModel)

# ``claude -p`` auto-loads project context (CLAUDE.md, .git) from its working
# directory and ancestors. When openDaisugi uses claude as a *pure LLM* (envelope
# generation, task execution, synthesis) that context is contamination — a task
# like "design a drone protocol" gets refused as "out of context with your
# openDaisugi working directory". Run every claude -p subprocess in a fresh,
# empty directory so no CLAUDE.md is ever in scope. Created lazily, once.
_NEUTRAL_CWD: str | None = None


def _neutral_cwd() -> str:
    global _NEUTRAL_CWD
    if _NEUTRAL_CWD is None:
        _NEUTRAL_CWD = tempfile.mkdtemp(prefix="opendaisugi-claude-")
    return _NEUTRAL_CWD


_CLAUDE_ARGS_ENV = "DAISUGI_CLAUDE_ARGS"


def _configured_extra_args() -> tuple[str, ...]:
    """Extra ``claude -p`` flags from the ``DAISUGI_CLAUDE_ARGS`` env var.

    Lets an operator forward flags to EVERY claude -p call site (orchestrator,
    envelope generation, LLMCheck) without threading a parameter through each —
    e.g. ``DAISUGI_CLAUDE_ARGS='--dangerously-skip-permissions'`` or
    ``DAISUGI_CLAUDE_ARGS='--allowedTools "Bash(ls:*) Read"'``. Parsed with shlex
    so quoted multi-word values survive. Opt-in: unset means no extra flags.
    """
    raw = os.environ.get(_CLAUDE_ARGS_ENV, "").strip()
    if not raw:
        return ()
    try:
        return tuple(shlex.split(raw))
    except ValueError as exc:
        _log.warning("%s is not parseable (%s); ignoring", _CLAUDE_ARGS_ENV, exc)
        return ()


def _build_claude_args(
    binary: str, prompt: str, model: str | None, extra_args: tuple[str, ...]
) -> list[str]:
    """Build an injection-safe ``claude -p`` argv.

    The ``prompt`` and ``model`` can be LLM-authored (a decomposed TaskStep's text,
    a plan's ``preferred_model``), so a value starting with ``-`` must NOT be
    reparsable as a claude CLI flag (e.g. ``--dangerously-skip-permissions``):
    - ``model`` is bound with the ``--model=<value>`` form (the value can't become
      a separate flag);
    - the ``prompt`` positional is placed after a ``--`` end-of-options separator,
      so the parser always treats it as the query, never a flag.

    Operator-configured flags (``DAISUGI_CLAUDE_ARGS``) and any call-site
    ``extra_args`` are inserted as real options BEFORE the ``--`` separator.
    """
    args = [binary, "-p"]
    if model is not None:
        args.append(f"--model={model}")
    args.extend(_configured_extra_args())
    args.extend(extra_args)
    args.append("--")
    args.append(prompt)
    return args


async def _terminate_and_reap(proc) -> None:
    """SIGTERM the subprocess and AWAIT its exit so it doesn't linger as a zombie.

    ``proc.terminate()`` alone leaves the child unreaped (and the transport open)
    until GC — under load-timeouts these accumulate. Escalate to kill if TERM is
    slow, then await so the OS reaps it.
    """
    try:
        proc.terminate()
    except ProcessLookupError:
        return
    try:
        await asyncio.wait_for(proc.wait(), timeout=2.0)
    except (asyncio.TimeoutError, Exception):
        try:
            proc.kill()
        except ProcessLookupError:
            pass
        try:
            await proc.wait()
        except Exception:
            pass


async def call_claude_p_async(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
    extra_args: tuple[str, ...] = (),
    cwd: str | None = None,
) -> str:
    """Call ``claude -p <prompt>`` asynchronously; return stdout stripped.

    Runs in a neutral working directory by default (``cwd=None``) so project
    context (CLAUDE.md/.git) never leaks into the LLM call; pass an explicit
    ``cwd`` to override.
    """
    args = _build_claude_args(binary, prompt, model, extra_args)

    try:
        proc = await asyncio.create_subprocess_exec(
            *args,
            stdout=asyncio.subprocess.PIPE,
            stderr=asyncio.subprocess.PIPE,
            stdin=asyncio.subprocess.DEVNULL,
            cwd=cwd if cwd is not None else _neutral_cwd(),
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
        await _terminate_and_reap(proc)
        raise
    except BaseException:
        await _terminate_and_reap(proc)
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
    cwd: str | None = None,
) -> str:
    """Synchronous variant of :func:`call_claude_p_async`.

    Runs in a neutral working directory by default so project context never
    leaks into the LLM call; pass an explicit ``cwd`` to override.
    """
    args = _build_claude_args(binary, prompt, model, extra_args)

    try:
        result = subprocess.run(
            args,
            capture_output=True,
            text=True,
            timeout=timeout_s,
            check=False,
            stdin=subprocess.DEVNULL,
            cwd=cwd if cwd is not None else _neutral_cwd(),
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


def call_claude_p_metered(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
    cwd: str | None = None,
) -> tuple[str, dict]:
    """Call ``claude -p --output-format json``; return (result_text, meter).

    ``meter`` is ``{"tokens": int | None, "cost_usd": float | None}`` taken from
    Claude Code's OWN accounting (``usage`` + ``total_cost_usd``) — exact, and it
    works on a Claude Code subscription with no API key. Falls back to raw text
    with empty meter if the envelope can't be parsed.
    """
    raw = call_claude_p_sync(
        prompt, timeout_s=timeout_s, model=model, binary=binary, cwd=cwd,
        extra_args=("--output-format", "json"),
    )
    try:
        obj = json.loads(raw)
    except json.JSONDecodeError:
        return raw, {"tokens": None, "cost_usd": None}
    # An in-turn error (max-turns, refusal, execution error) can exit 0 with
    # is_error=true; don't return its ``result`` as if it were a real answer.
    if obj.get("is_error"):
        raise EnvelopeGenerationError(
            f"claude -p reported is_error: {str(obj.get('result'))[:200]!r}"
        )
    text = obj.get("result", "") or ""
    usage = obj.get("usage") if isinstance(obj.get("usage"), dict) else {}
    # Count ALL token kinds — input, output, AND cache creation/read. On the
    # claude-code backend the reloaded system prompt makes cache tokens dominate
    # (often ~99% of the total); summing only input+output undercounts by ~100x,
    # so the token budget would never bite. total_cost_usd already prices the
    # cache discount, so the exact dollar figure is unaffected either way.
    tokens: int | None = None
    fields = ("input_tokens", "output_tokens",
              "cache_creation_input_tokens", "cache_read_input_tokens")
    present = [usage.get(f) for f in fields if usage.get(f) is not None]
    if present:
        tokens = sum(int(v) for v in present)
    return text, {"tokens": tokens, "cost_usd": obj.get("total_cost_usd")}


def call_claude_p_json_metered(
    prompt: str,
    *,
    timeout_s: float = 60.0,
    model: str | None = "haiku",
    binary: str = "claude",
    cwd: str | None = None,
) -> tuple[dict, dict]:
    """Metered JSON call: ``--output-format json`` + first-JSON-object extraction.

    Unlike :func:`call_claude_p_json_sync` this surfaces ``is_error`` (via the
    metered envelope) and returns Claude Code's exact usage/cost meter. And when
    the model answers in prose instead of JSON, the error names the REAL cause —
    the delegated sandbox has no project files and no tools, so a tool-needing
    prompt gets an apologetic prose reply — instead of blaming JSON formatting
    (the old message sent one operator debugging the wrong layer entirely).
    """
    text, meter = call_claude_p_metered(
        prompt, timeout_s=timeout_s, model=model, binary=binary, cwd=cwd,
    )
    try:
        return _extract_first_json_object(text), meter
    except EnvelopeGenerationError as exc:
        raise EnvelopeGenerationError(
            f"delegated model replied with prose, not JSON: {text[:300]!r}. "
            "Delegated steps run in an isolated working directory with no "
            "project files and no tools; a prompt that asks the model to read "
            "files, browse, or run commands cannot succeed on this path — "
            "restate the step as pure reasoning or use a capability step type."
        ) from exc


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
    "call_claude_p_json_metered",
    "call_claude_p_json_sync",
    "call_claude_p_metered",
    "call_claude_p_structured",
    "ClaudeCodeInstructorClient",
]
