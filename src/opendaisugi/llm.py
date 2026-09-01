"""The model-call factory and the rules around it.

All structured model access flows through ``get_model_client``. One factory
means tests have one place to monkeypatch, and the backend switch lives in one
place: ``OPENDAISUGI_LLM_BACKEND=claude-code`` (or ``backend="claude-code"``)
routes every call through a local ``claude -p`` subprocess; any other value
(``api`` is the name) routes through our own HTTP client,
``opendaisugi.llm_client``. The old name ``litellm`` is refused.
"""

from __future__ import annotations

import os
import re
import shutil
from collections.abc import Mapping
from typing import Any

from opendaisugi.exceptions import EnvelopeGenerationError, LLMNotConfigured

_KEY_RE = re.compile(r"(sk-[a-zA-Z0-9_-]{4})[a-zA-Z0-9_-]{8,}([a-zA-Z0-9_-]{4})")


# The API backend was named ``litellm`` while litellm carried it. litellm is
# gone, so the old name is refused, never read as ``api``.
RENAMED_BACKEND = "litellm"
RENAMED_BACKEND_TEXT = "The LLM backend 'litellm' is now named 'api'. Use 'api'."


def _redact_keys(msg: str) -> str:
    """Replace API-key-shaped tokens with a redacted version."""
    return _KEY_RE.sub(r"\1...\2", msg)


def translate_llm_error(exc: BaseException) -> EnvelopeGenerationError:
    """Normalize any model-call exception into EnvelopeGenerationError.

    Passes through existing EnvelopeGenerationError untouched (the model
    client's own errors are already one clean line). Callers are expected to
    re-raise using ``raise translate_llm_error(e) from e`` so Python sets
    ``__cause__`` via the ``from`` clause; this function does not set
    ``__cause__`` itself.

    API keys matching the ``sk-`` token pattern are redacted before the
    message is stored, so keys cannot leak through logged exception strings.
    """
    if isinstance(exc, EnvelopeGenerationError):
        return exc
    message = str(exc) or exc.__class__.__name__
    return EnvelopeGenerationError(_redact_keys(message))


def resolve_backend(backend: str | None = None) -> str:
    """Return the active LLM backend name.

    Priority: the explicit ``backend=`` argument, then the
    ``OPENDAISUGI_LLM_BACKEND`` env var, then an ``llm_backend`` the config
    file sets, then auto-detection. Canonical source across the package.
    Callers outside this module (``llm_check``, transcript parsers, anything
    else that needs to branch on backend) import this so the env var name
    and resolution rule live in one place.

    The config rung sits below the env var on purpose: the env var is this
    process's own explicit setting, and a user-writable file must not
    override it. It sits above auto-detection because otherwise the swap
    knob writes a field nothing reads. It is read per call, so a change to
    config.yaml takes effect with no restart.

    Auto-detection (when nothing is configured) picks a backend that
    RUNS on this machine, instead of the API path that dies without a key: a
    configured Anthropic key means the ``"api"`` path is intended;
    otherwise, if the local ``claude`` CLI is present, use the keyless
    ``"claude-code"`` backend; failing both, fall back to ``"api"`` so its
    human-readable missing-key error can fire.

    The old name ``litellm``, from any rung and with any surrounding
    whitespace, raises LLMNotConfigured with RENAMED_BACKEND_TEXT.
    """
    resolved = backend or os.environ.get("OPENDAISUGI_LLM_BACKEND")
    if not resolved:
        try:
            from opendaisugi import DEFAULT_DATA_DIR
            from opendaisugi.config import configured_backend

            resolved = configured_backend(DEFAULT_DATA_DIR / "config.yaml")
        except Exception:  # noqa: BLE001 - a broken config must not break backend choice
            resolved = None
    resolved = resolved or _auto_backend()
    if resolved.strip() == RENAMED_BACKEND:
        raise LLMNotConfigured(RENAMED_BACKEND_TEXT)
    return resolved


def _auto_backend(env: "Mapping[str, str] | None" = None, *, which=None) -> str:
    """Pick a working backend when nothing names one.

    ``env`` defaults to the process env and ``which`` to ``shutil.which``,
    both resolved at call time, so a caller that isolates its environment can
    pass its own and a monkeypatched ``shutil.which`` still reaches the default.
    """
    env = os.environ if env is None else env
    which = shutil.which if which is None else which
    if env.get("ANTHROPIC_API_KEY") or env.get("ANTHROPIC_AUTH_TOKEN"):
        return "api"
    if which("claude"):
        return "claude-code"
    return "api"


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
    the API backend; other providers surface their own errors.

    ``which`` defaults to ``shutil.which`` resolved at call time (not bound at
    def time), so ``monkeypatch.setattr("shutil.which", ...)`` reaches the
    default path — not just an explicitly-passed ``which=``.
    """
    which = shutil.which if which is None else which
    env = os.environ if env is None else env
    resolved = resolve_backend(backend)
    if resolved == "api":
        anthropic = model is None or model.startswith(_ANTHROPIC_PREFIXES)
        has_key = bool(env.get("ANTHROPIC_API_KEY") or env.get("ANTHROPIC_AUTH_TOKEN"))
        if anthropic and not has_key:
            raise LLMNotConfigured(
                "Tried to call the Anthropic API.\n"
                "No ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN is set.\n"
                "Set a key, or run with --llm claude-code to use the local claude CLI."
            )
    elif resolved == "claude-code":
        if which("claude") is None:
            raise LLMNotConfigured(
                "Tried to use the claude-code backend.\n"
                "No `claude` command is on PATH.\n"
                "Install Claude Code, or set ANTHROPIC_API_KEY and run with --llm api."
            )
    return resolved


def get_model_client(
    model: str,
    *,
    backend: str | None = None,
) -> Any:
    """Return the client for the resolved backend.

    Backend selection, in priority order:
      1. explicit ``backend=`` argument
      2. ``OPENDAISUGI_LLM_BACKEND`` env var
      3. ``llm_backend`` in config.yaml, read per call
      4. auto-detect, ``_auto_backend``

    - ``"claude-code"`` returns a :class:`ClaudeCodeInstructorClient` that
      routes every call through a local ``claude -p`` subprocess. No API
      key required.
    - anything else returns :class:`opendaisugi.llm_client.ModelClient`,
      our own HTTP client (Anthropic Messages or OpenAI-compatible chat
      completions, chosen by the model name at call time).

    Both expose ``.chat.completions.create(model=..., messages=...,
    response_model=..., max_retries=...)``. ``model`` is checked here
    (preflight) and passed again at call time.
    """
    resolved = preflight(backend, model=model)
    if resolved == "claude-code":
        from opendaisugi.claude_code_llm import ClaudeCodeInstructorClient

        return ClaudeCodeInstructorClient()
    from opendaisugi.llm_client import ModelClient

    return ModelClient()
