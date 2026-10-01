"""Tier-1 local-model slot for envelope generation (v0.4.0).

A ``Tier1Provider`` sits between Tier-0 (compiled-pathway lookup) and Tier-2
(frontier LLM) in ``generate_envelope``. The contract is deliberately narrow:

    async def generate_envelope(task, *, context=None) -> Envelope | None

Returning ``None`` means "I don't want to handle this" and the router falls
through to Tier-2. Any exception raised by the adapter is caught upstream and
treated the same as ``None`` — adapter failure must never block generation.

The router treats ``tier1=None`` as "no provider configured" and skips the
branch entirely. If you want to *opt in* but still decline selectively, use
a custom provider that returns ``None`` on the tasks it won't handle.

Two adapters are shipped in this module:

- :class:`HTTPTier1Provider` — any model our own client (``llm_client``)
  reaches: an OpenAI-compatible endpoint (Ollama, llamafile, llama.cpp
  server) or the Anthropic API (Haiku, etc.)
- :class:`ClaudeCodeTier1Provider` — shells out to ``claude -p`` for users who
  already have Claude Code installed

Users can also provide their own class satisfying the protocol.
"""

from __future__ import annotations

import asyncio
import logging
from typing import Protocol, runtime_checkable

from opendaisugi.models import Envelope

_log = logging.getLogger("opendaisugi.tier1")


@runtime_checkable
class Tier1Provider(Protocol):
    """Protocol for a Tier-1 envelope producer.

    Implementors must expose a stable ``name`` string (used for logging and
    cache-key isolation) and an ``async generate_envelope`` method. Returning
    ``None`` declines the task; the router falls through to Tier-2.
    """

    name: str

    async def generate_envelope(
        self,
        task: str,
        *,
        context: str | None = None,
    ) -> Envelope | None: ...


# Prompt used by real-request adapters. Kept minimal so small local models
# stand a chance; the full system prompt in envelope.py is frontier-tuned.
_TIER1_PROMPT = (
    "Generate a minimal safety envelope (JSON) for this task. "
    "Include generated_by, task, permissions (file_read, file_write, network, "
    "shell, shell_allowlist, max_execution_time_s, max_output_size_mb), "
    "invariants, postconditions. Default network=false and shell=false. "
    "Only allow paths the task mentions. No placeholders."
)


_MAX_REASON = 200
_CUT_PREFIX = "The output is incomplete due to a max_tokens"


def _clean_reason(text: str) -> str:
    """One line, at most ``_MAX_REASON`` characters."""
    line = " ".join(text.split())
    return line if len(line) <= _MAX_REASON else line[: _MAX_REASON - 3] + "..."


def classify_failure(exc: BaseException) -> tuple[str, str]:
    """Name the outcome of a failed probe and a one-line reason.

    ``declined``: the model answered, but not with a valid envelope.
    ``error``: the call failed (a server, a key, a wire).
    ``timeout``: the probe ran past a deadline.
    """
    msg = str(exc) or exc.__class__.__name__
    if msg == "TimeoutError" or "timed out" in msg:
        return "timeout", _clean_reason(msg)
    if msg.startswith("ValidationError"):
        return "declined", _clean_reason(msg)
    if msg.startswith(_CUT_PREFIX):
        return "declined", _clean_reason(
            msg + " A model that thinks first may have spent the token budget on thinking;"
            " raise max_tokens."
        )
    return "error", _clean_reason(msg)


class HTTPTier1Provider:
    """Tier-1 adapter over any OpenAI-compatible endpoint or the Anthropic API.

    Works with:
        - Ollama on localhost:11434 (model="ollama/llama3.2:3b")
        - llamafile (model="openai/model", base_url="http://localhost:8080/v1")
        - llama.cpp server (same shape as llamafile)
        - Anthropic Haiku as a paid-but-cheap Tier-1 (model="anthropic/claude-haiku-4-5-20251001")

    Any failure — timeout, connection error, JSON parse error — returns ``None``
    so the router falls through to Tier-2. ``timeout_s`` wraps the call so a
    dead local endpoint cannot hang envelope generation indefinitely.
    """

    def __init__(
        self,
        model: str,
        *,
        base_url: str | None = None,
        api_key: str | None = None,
        timeout_s: float = 30.0,
        name: str | None = None,
        backend: str | None = None,
    ) -> None:
        # ``backend`` pins the model client's backend for this provider, as
        # ``get_model_client(backend=...)`` does; None leaves the choice to the
        # environment, the config file and auto-detection.
        self.backend = backend
        # The model client picks its wire by provider prefix. A local
        # OpenAI-compatible endpoint (llamafile/llama.cpp/LM Studio) needs the
        # ``openai/`` prefix: a bare model name has no wire and never reaches
        # base_url.
        # Auto-prefix an unprefixed model when a base_url is set (mirrors
        # OllamaTier1Provider's ``ollama/`` auto-prefix); already-prefixed strings
        # (``openai/…``, ``ollama/…``, ``anthropic/…``) are left untouched.
        if base_url is not None and "/" not in model:
            model = f"openai/{model}"
        self.model = model
        self.base_url = base_url
        self.api_key = api_key
        self.timeout_s = timeout_s
        # Default the provider name to the model string so cache keys isolate
        # per-model out of the box. Callers override when running multiple
        # configurations of the same model.
        self.name = name or f"http:{model}"

    async def generate_envelope(
        self,
        task: str,
        *,
        context: str | None = None,
    ) -> Envelope | None:
        envelope, _outcome, _reason = await self.probe_envelope(task, context=context)
        return envelope

    async def probe_envelope(
        self,
        task: str,
        *,
        context: str | None = None,
    ) -> tuple[Envelope | None, str, str]:
        """``generate_envelope`` with the reason: ``(envelope, outcome, reason)``.

        ``outcome`` is ``valid``, ``declined`` (the model answered, but not with
        a valid envelope), ``error`` (the call failed) or ``timeout``. ``reason``
        is one short line, empty for ``valid``. The router reads only the
        envelope; the qualification gate reads the rest.
        """
        from opendaisugi import llm as _llm  # local import; keeps module import cheap

        user_content = f"Task: {task}"
        if context:
            user_content += f"\n\nContext:\n{context}"

        extra: dict = {}
        if self.base_url is not None:
            extra["base_url"] = self.base_url
        if self.api_key is not None:
            extra["api_key"] = self.api_key

        try:
            if self.backend is None:
                client = _llm.get_model_client(model=self.model)
            else:
                client = _llm.get_model_client(model=self.model, backend=self.backend)
            envelope = await asyncio.wait_for(
                client.chat.completions.create(
                    model=self.model,
                    response_model=Envelope,
                    messages=[
                        {"role": "system", "content": _TIER1_PROMPT},
                        {"role": "user", "content": user_content},
                    ],
                    max_retries=1,
                    **extra,
                ),
                timeout=self.timeout_s,
            )
            return envelope, "valid", ""
        except asyncio.TimeoutError:
            _log.info(
                "HTTPTier1Provider %r timed out after %.1fs — declining",
                self.name,
                self.timeout_s,
            )
            return None, "timeout", "the probe ran past its deadline"
        except Exception as exc:  # adapter failure must never block generation
            _log.info("HTTPTier1Provider %r failed: %s — declining", self.name, exc)
            outcome, reason = classify_failure(exc)
            return None, outcome, reason


class OllamaTier1Provider(HTTPTier1Provider):
    """Tier-1 adapter for a locally-running Ollama server (v0.23+).

    Convenience over ``HTTPTier1Provider`` with Ollama-shaped defaults:
    no API key, ``base_url`` defaults to the canonical localhost endpoint,
    and the model name is auto-prefixed with ``ollama/`` so callers can pass
    a bare model name (``llama3.2:3b``) or the fully-qualified form
    (``ollama/llama3.2:3b``) interchangeably.

    Pure-local deployment recipe — no API keys, no claude-code subscription,
    no cloud round-trip:

        from opendaisugi import Daisugi
        from opendaisugi.tier1 import OllamaTier1Provider

        d = Daisugi(tier1=OllamaTier1Provider(model="llama3.2:3b"))

    Closes the home-machine deployment story alongside ``OPENDAISUGI_LLM_BACKEND``
    set elsewhere in the pipeline.
    """

    def __init__(
        self,
        model: str = "llama3.2:3b",
        *,
        base_url: str = "http://localhost:11434",
        timeout_s: float = 60.0,
        name: str | None = None,
    ) -> None:
        if not model.startswith("ollama/"):
            model = f"ollama/{model}"
        super().__init__(
            model=model,
            base_url=base_url,
            api_key=None,
            timeout_s=timeout_s,
            name=name or f"ollama:{model.removeprefix('ollama/')}",
        )


class ClaudeCodeTier1Provider:
    """Tier-1 adapter that shells out to ``claude -p`` for JSON envelope output.

    Useful for users who already have Claude Code installed — they get a
    cheaper-than-frontier envelope source without running a local model.
    Strictly speaking this isn't "local," but it fits the Tier-1 slot's
    intent: a cheaper path the user already controls.

    The adapter:
      - spawns the binary via ``asyncio.create_subprocess_exec``
      - prompts for JSON-only output
      - parses stdout into an ``Envelope``
      - terminates the subprocess on cancellation (try/finally)
      - declines (returns None) on missing binary, nonzero exit, or JSON parse error
    """

    def __init__(
        self,
        *,
        binary: str = "claude",
        model_flag: str | None = "haiku",
        timeout_s: float = 60.0,
        extra_args: tuple[str, ...] = (),
        name: str = "claude-code",
    ) -> None:
        self.binary = binary
        self.model_flag = model_flag
        self.timeout_s = timeout_s
        self.extra_args = extra_args
        self.name = name

    def _build_prompt(self, task: str, context: str | None) -> str:
        ctx = f"\n\nContext:\n{context}" if context else ""
        return (
            f"{_TIER1_PROMPT}\n\n"
            f"Task: {task}{ctx}\n\n"
            "Respond with ONLY the JSON object. No explanation, no code fences."
        )

    async def generate_envelope(
        self,
        task: str,
        *,
        context: str | None = None,
    ) -> Envelope | None:
        import json

        # Reuse the shared injection-safe builder: model bound with --model=,
        # the prompt fed on stdin (unbounded + never option-parsed), and
        # operator-configured DAISUGI_CLAUDE_ARGS (e.g.
        # --dangerously-skip-permissions) merged in alongside self.extra_args.
        from opendaisugi.claude_code_llm import _build_claude_args

        args = _build_claude_args(
            self.binary,
            self.model_flag,
            tuple(self.extra_args),
        )
        prompt = self._build_prompt(task, context)

        proc = None
        try:
            proc = await asyncio.create_subprocess_exec(
                *args,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                stdin=asyncio.subprocess.PIPE,
            )
        except FileNotFoundError:
            _log.info("ClaudeCodeTier1Provider: binary %r not found — declining", self.binary)
            return None
        except Exception as exc:
            _log.info("ClaudeCodeTier1Provider: subprocess exec failed: %s — declining", exc)
            return None

        try:
            stdout_bytes, stderr_bytes = await asyncio.wait_for(
                proc.communicate(input=prompt.encode("utf-8")),
                timeout=self.timeout_s,
            )
        except asyncio.TimeoutError:
            _log.info("ClaudeCodeTier1Provider timed out after %.1fs — declining", self.timeout_s)
            from opendaisugi.claude_code_llm import _terminate_and_reap

            await _terminate_and_reap(proc)
            return None
        except BaseException:
            # Cancelled from above — terminate AND reap so we don't leak a zombie.
            from opendaisugi.claude_code_llm import _terminate_and_reap

            await _terminate_and_reap(proc)
            raise

        if proc.returncode != 0:
            _log.info(
                "ClaudeCodeTier1Provider: exit=%d stderr=%r — declining",
                proc.returncode,
                stderr_bytes[:200].decode("utf-8", "replace"),
            )
            return None

        stdout = stdout_bytes.decode("utf-8", "replace").strip()
        # Best-effort: isolate the first JSON object in output.
        start = stdout.find("{")
        end = stdout.rfind("}")
        if start == -1 or end <= start:
            _log.info("ClaudeCodeTier1Provider: no JSON object in stdout — declining")
            return None
        try:
            payload = json.loads(stdout[start : end + 1])
            return Envelope.model_validate(payload)
        except Exception as exc:
            _log.info("ClaudeCodeTier1Provider: JSON/Envelope parse failed: %s — declining", exc)
            return None
