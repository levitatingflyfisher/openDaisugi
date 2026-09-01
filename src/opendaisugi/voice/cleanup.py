"""The optional cleanup pass: one fixed-prompt local-model call, journaled like any other turn.

Cleanup polishes a raw transcript through one local model call before the text
reaches a pane. It is off by default and never touches a paid model unless
the operator names one in config.

`clean_transcript` returns a `CleanupResult` with four distinct outcomes.
Cleanup off, or no model configured: the raw transcript, `cleaned=False`,
no reason, nothing journaled. A usable correction that journals cleanly:
the corrected text, `cleaned=True`, no reason. A transport error, or a
correction with no usable text: the raw transcript, `cleaned=False`, a reason
sentence naming what to do next, nothing journaled. A usable correction that
cannot be journaled, for example an unwritable data directory: the corrected
text, `cleaned=False`, a reason sentence naming what to do next. A local
model can be unreachable, can answer with nothing useful, or the journal
itself can be unwritable; none of these cases should turn a spoken word
into a lost one.

A successful call is recorded through the same `record_turn` path the
gateway uses for a proxied turn. The transcript is the task and the ask, the
same way the gateway journals any other turn's human text. Two different
transcripts get two different signatures, so the garden can still see a real
repeat when the same phrase is cleaned up twice.

The tier is always `tier1-local` and the decision is never a downgrade,
because a cleanup call has no requested model to route away from. It is
always local, by design.

Pricing is set explicitly to zero dollars for the configured model. Without
that override, the gateway's fallback price table would book real dollars
against a local call and quietly pull down the blended multiplier reported
for every other turn. `Gateway.__post_init__` in gateway_pipeline.py does the
same override for a configured local model.

The transport is `litellm.completion()`. Litellm is already a dependency of
opendaisugi. This mirrors the local-endpoint call shape used by
`LiteLLMTier1Provider` in tier1.py.
"""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path
from typing import Protocol

from opendaisugi.config import Config
from opendaisugi.gateway import RouteDecision, measure_turn
from opendaisugi.gateway_journal import GatewayJournal, record_turn
from opendaisugi.routing import estimate_difficulty

CLEANUP_PROMPT_V1 = (
    "Fix punctuation and obvious transcription errors. Keep the meaning and "
    "the words. Return only the corrected text."
)

_NO_TEXT_REASON = "The cleanup model returned no text. Check the model, then try again."
_UNREACHABLE_REASON = (
    "The cleanup model failed to respond. Check that it is running, then try again."
)
_JOURNAL_UNWRITABLE_REASON = (
    "The transcript could not be journaled. Check the data directory, then try again."
)


@dataclass(frozen=True)
class CleanupResult:
    """What one cleanup call produced.

    `cleaned` is True only for a usable correction that was also journaled.
    `reason` is set on any fault: a transport error, a correction with no
    usable text, or a correction that could not be journaled, and names
    what to do next. `text` is the corrected text whenever the model
    produced one, journaled or not; it is the raw transcript only when
    cleanup never ran or the model itself failed.
    """

    text: str
    cleaned: bool
    reason: str | None = None


class CleanupTransport(Protocol):
    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        """Run one blocking completion call.

        Returns the corrected text and a usage dict carrying input_tokens and
        output_tokens. A backend that does not report usage returns zeros for
        both.
        """
        ...


class LiteLLMCleanupTransport:
    """The real transport. One `litellm.completion()` call.

    A `base_url` means a local OpenAI-compatible endpoint such as Ollama,
    llamafile, or a running gateway. A bare model name is prefixed with
    `openai/` in that case, the same auto-prefix `LiteLLMTier1Provider` uses,
    since litellm needs a provider prefix to route to `base_url` instead of a
    cloud provider.
    """

    def __init__(self, *, base_url: str | None = None, timeout_s: float = 30.0) -> None:
        self.base_url = base_url
        self.timeout_s = timeout_s

    def complete(self, *, model: str, system: str, user: str) -> tuple[str, dict[str, int]]:
        import litellm

        call_model = model
        if self.base_url is not None and "/" not in call_model:
            call_model = f"openai/{call_model}"
        kwargs: dict = {}
        if self.base_url is not None:
            kwargs["base_url"] = self.base_url
        resp = litellm.completion(
            model=call_model,
            messages=[
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            timeout=self.timeout_s,
            **kwargs,
        )
        text = resp.choices[0].message.content or ""
        usage = getattr(resp, "usage", None) or {}
        if isinstance(usage, dict):
            input_tokens = int(usage.get("prompt_tokens", 0) or 0)
            output_tokens = int(usage.get("completion_tokens", 0) or 0)
        else:
            input_tokens = int(getattr(usage, "prompt_tokens", 0) or 0)
            output_tokens = int(getattr(usage, "completion_tokens", 0) or 0)
        return text, {"input_tokens": input_tokens, "output_tokens": output_tokens}


def _default_journal(config: Config) -> GatewayJournal:
    return GatewayJournal(path=Path(config.data_dir) / "gateway" / "turns.jsonl")


def _journal_cleanup(
    *, model: str, text: str, usage: dict[str, int], journal: GatewayJournal
) -> None:
    decision = RouteDecision(
        tier="tier1-local",
        model=model,
        requested_model=model,
        difficulty=estimate_difficulty(text),
        downgraded=False,
        reason="voice cleanup runs on a fixed local model. It is never routed.",
    )
    saving = measure_turn(decision, usage, prices={model: (0.0, 0.0)})
    record = record_turn(decision, saving, task=text, ask=text)
    journal.append(record)


def clean_transcript(
    text: str,
    *,
    config: Config,
    transport: CleanupTransport | None = None,
    journal: GatewayJournal | None = None,
) -> CleanupResult:
    """Polish a transcript through one fixed-prompt local-model call.

    Returns the raw transcript, unjournaled, when cleanup is off or when no
    model is configured. Cleanup never touches a paid model unless
    `config.voice_cleanup_model` names one, and it never falls back to the
    cloud on its own.

    A transport error, or a correction with no usable text, also returns the
    raw transcript, unjournaled, with a reason naming what to do next. A
    usable correction that cannot be journaled, for example an unwritable
    data directory, still returns the corrected text, with a reason naming
    what to do next. Only a usable correction that journals cleanly reports
    cleaned=True.
    """
    if not config.voice_cleanup or not config.voice_cleanup_model:
        return CleanupResult(text=text, cleaned=False, reason=None)
    model = config.voice_cleanup_model
    transport = transport or LiteLLMCleanupTransport(base_url=config.voice_cleanup_base_url)
    try:
        corrected, usage = transport.complete(model=model, system=CLEANUP_PROMPT_V1, user=text)
    except Exception:
        return CleanupResult(text=text, cleaned=False, reason=_UNREACHABLE_REASON)
    cleaned = corrected.strip()
    if not cleaned:
        return CleanupResult(text=text, cleaned=False, reason=_NO_TEXT_REASON)
    try:
        _journal_cleanup(
            model=model, text=text, usage=usage, journal=journal or _default_journal(config)
        )
    except OSError:
        return CleanupResult(text=cleaned, cleaned=False, reason=_JOURNAL_UNWRITABLE_REASON)
    return CleanupResult(text=cleaned, cleaned=True, reason=None)
