"""litellm + instructor glue for opendaisugi.

All LLM access flows through ``get_instructor_client``. Keeping this a
single factory means tests have exactly one seam to monkeypatch, and the
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
from typing import TYPE_CHECKING, Union

from opendaisugi.exceptions import EnvelopeGenerationError

if TYPE_CHECKING:
    import instructor

_KEY_RE = re.compile(r"(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})")


def _redact_keys(msg: str) -> str:
    """Replace API-key-shaped tokens with a redacted version."""
    return _KEY_RE.sub(r"\1...\2", msg)


def translate_llm_error(exc: BaseException) -> EnvelopeGenerationError:
    """Normalize any upstream LLM exception into EnvelopeGenerationError.

    Passes through existing EnvelopeGenerationError untouched. Callers are
    expected to re-raise using ``raise translate_llm_error(e) from e`` so
    Python sets ``__cause__`` via the ``from`` clause — this function does
    not set ``__cause__`` itself.

    API keys matching the ``sk-`` token pattern are redacted before the
    message is stored, so keys cannot leak through logged exception strings.
    """
    if isinstance(exc, EnvelopeGenerationError):
        return exc
    return EnvelopeGenerationError(_redact_keys(str(exc) or exc.__class__.__name__))


def resolve_backend(backend: str | None = None) -> str:
    """Return the active LLM backend name.

    Priority: explicit ``backend=`` argument → ``OPENDAISUGI_LLM_BACKEND``
    env var → ``"litellm"``. Canonical source across the package — callers
    outside this module (``llm_check``, transcript parsers, anything else
    that needs to branch on backend) import this so the env var name and
    default live in one place.
    """
    return backend or os.environ.get("OPENDAISUGI_LLM_BACKEND", "litellm")


def get_instructor_client(
    model: str, *, backend: str | None = None,
) -> Union[instructor.AsyncInstructor, "object"]:
    """Return an instructor-compatible client.

    Backend selection (priority order):
      1. explicit ``backend=`` argument
      2. ``OPENDAISUGI_LLM_BACKEND`` env var
      3. default ``"litellm"``

    - ``"litellm"`` returns ``instructor.from_litellm(acompletion, mode=JSON)``
      (v0.11.x behavior, unchanged).
    - ``"claude-code"`` returns a :class:`ClaudeCodeInstructorClient` that
      routes every call through a local ``claude -p`` subprocess. No API
      key required.

    ``model`` is passed through to the underlying backend at call time,
    not here — this factory just wires up the client object.
    """
    resolved = resolve_backend(backend)
    if resolved == "claude-code":
        from opendaisugi.claude_code_llm import ClaudeCodeInstructorClient
        return ClaudeCodeInstructorClient()
    del model  # accepted for API symmetry; instructor/litellm use it at call time
    # Imported lazily: this is the ~2.4s import chain, and it must not load when
    # the package is imported for the capture hook (which fires per tool call).
    import instructor
    from litellm import acompletion
    return instructor.from_litellm(acompletion, mode=instructor.Mode.JSON)
