"""litellm + instructor glue for opendaisugi.

All LLM access flows through ``get_instructor_client``. Keeping this a
single factory means tests have exactly one place to monkeypatch, and the
v0.12.0 backend switch lives in one place: set
``OPENDAISUGI_LLM_BACKEND=claude-code`` (or pass ``backend="claude-code"``)
and every instructor call site routes through a local ``claude -p``
subprocess instead of an API key. Default is ``"litellm"`` — v0.11.x
behavior is preserved.

Why instructor over raw litellm: instructor handles JSON-mode extraction,
Pydantic validation, and retry-on-invalid-output in one place. The
alternative (parse + validate manually) duplicates work instructor does
well.
"""

from __future__ import annotations

import os
import re
import shutil
from collections.abc import Mapping
from typing import TYPE_CHECKING, Union

from opendaisugi.exceptions import EnvelopeGenerationError, LLMNotConfigured

if TYPE_CHECKING:
    import instructor

_KEY_RE = re.compile(r"(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})")


def _redact_keys(msg: str) -> str:
    """Replace API-key-shaped tokens with a redacted version."""
    return _KEY_RE.sub(r"\1...\2", msg)


def _flatten_retry(exc: BaseException) -> str | None:
    """One line for an instructor retry exception, or None if ``exc`` is not one.

    instructor renders ``failed_attempts`` as a multi-line XML template. The
    operator needs the first attempt's cause and the count, nothing else.
    """
    attempts = getattr(exc, "failed_attempts", None)
    if not attempts:
        return None
    inner = getattr(attempts[0], "exception", None)
    if inner is None:
        return None
    lines = [ln for ln in str(inner).strip().splitlines() if ln.strip()]
    first = lines[0].strip() if lines else inner.__class__.__name__
    return f"{inner.__class__.__name__}: {first} ({len(attempts)} attempts)"


def translate_llm_error(exc: BaseException) -> EnvelopeGenerationError:
    """Normalize any upstream LLM exception into EnvelopeGenerationError.

    Passes through existing EnvelopeGenerationError untouched. Callers are
    expected to re-raise using ``raise translate_llm_error(e) from e`` so
    Python sets ``__cause__`` via the ``from`` clause — this function does
    not set ``__cause__`` itself.

    API keys matching the ``sk-`` token pattern are redacted before the
    message is stored, so keys cannot leak through logged exception strings.
    An instructor retry exception's multi-line XML ``failed_attempts`` dump
    is flattened to one line: the first attempt's cause and the count.
    """
    if isinstance(exc, EnvelopeGenerationError):
        return exc
    flat = _flatten_retry(exc)
    message = flat or (str(exc) or exc.__class__.__name__)
    return EnvelopeGenerationError(_redact_keys(message))


def resolve_backend(backend: str | None = None) -> str:
    """Return the active LLM backend name.

    Priority: explicit ``backend=`` argument → ``OPENDAISUGI_LLM_BACKEND``
    env var → auto-detection. Canonical source across the package — callers
    outside this module (``llm_check``, transcript parsers, anything else
    that needs to branch on backend) import this so the env var name and
    resolution rule live in one place.

    Auto-detection (when nothing is configured) picks a backend that actually
    RUNS on this machine, instead of the API path that dies without a key: a
    configured Anthropic key means the ``"litellm"`` API path is intended;
    otherwise, if the local ``claude`` CLI is present, use the keyless
    ``"claude-code"`` backend; failing both, fall back to ``"litellm"`` so its
    (human-readable) missing-key error can fire.
    """
    return backend or os.environ.get("OPENDAISUGI_LLM_BACKEND") or _auto_backend()


def _auto_backend() -> str:
    """Pick a working backend when neither an argument nor the env var is set."""
    if os.environ.get("ANTHROPIC_API_KEY") or os.environ.get("ANTHROPIC_AUTH_TOKEN"):
        return "litellm"
    if shutil.which("claude"):
        return "claude-code"
    return "litellm"


_ANTHROPIC_PREFIXES = ("anthropic/", "claude")


def preflight(
    backend: str | None = None,
    *,
    model: str | None = None,
    env: "Mapping[str, str] | None" = None,
    which=None,
) -> str:
    """Check the resolved backend can run here; raise LLMNotConfigured if not.

    Runs before any client is built so a missing key or binary is one plain
    error, never a retried network call. Only Anthropic models are checked on
    the litellm path; other providers surface their own errors.

    ``which`` defaults to ``shutil.which`` resolved at call time (not bound at
    def time), so ``monkeypatch.setattr("shutil.which", ...)`` reaches the
    default path — not just an explicitly-passed ``which=``.
    """
    which = shutil.which if which is None else which
    env = os.environ if env is None else env
    resolved = resolve_backend(backend)
    if resolved == "litellm":
        anthropic = model is None or model.startswith(_ANTHROPIC_PREFIXES)
        has_key = bool(env.get("ANTHROPIC_API_KEY") or env.get("ANTHROPIC_AUTH_TOKEN"))
        if anthropic and not has_key:
            raise LLMNotConfigured(
                "Tried to call the Anthropic API through litellm.\n"
                "No ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n"
                "Set a key, or run with --llm claude-code to use the local claude CLI."
            )
    elif resolved == "claude-code":
        if which("claude") is None:
            raise LLMNotConfigured(
                "Tried to use the claude-code backend.\n"
                "No `claude` command is on PATH.\n"
                "Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm litellm."
            )
    return resolved


def get_instructor_client(
    model: str,
    *,
    backend: str | None = None,
) -> Union[instructor.AsyncInstructor, "object"]:
    """Return an instructor-compatible client.

    Backend selection (priority order):
      1. explicit ``backend=`` argument
      2. ``OPENDAISUGI_LLM_BACKEND`` env var
      3. auto-detect (``_auto_backend``)

    - ``"litellm"`` returns ``instructor.from_litellm(acompletion, mode=JSON)``
      (v0.11.x behavior, unchanged).
    - ``"claude-code"`` returns a :class:`ClaudeCodeInstructorClient` that
      routes every call through a local ``claude -p`` subprocess. No API
      key required.

    ``model`` is passed through to the underlying backend at call time,
    not here — this factory just wires up the client object.
    """
    resolved = preflight(backend, model=model)
    if resolved == "claude-code":
        from opendaisugi.claude_code_llm import ClaudeCodeInstructorClient

        return ClaudeCodeInstructorClient()
    del model  # accepted for API symmetry; instructor/litellm use it at call time
    # Imported lazily: this is the ~2.4s import chain, and it must not load when
    # the package is imported for the capture hook (which fires per tool call).
    import instructor
    from litellm import acompletion

    return instructor.from_litellm(acompletion, mode=instructor.Mode.JSON)
