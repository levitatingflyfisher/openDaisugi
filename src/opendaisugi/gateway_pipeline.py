"""The gateway pipeline — the composition the async proxy drives, kept pure and socketless.

The proxy has exactly two moments where it needs the routing brain: once *before* it opens
the upstream call (decide the model, rewrite the outbound body) and once *after* the response
comes back (turn the model's usage report into a recorded saving). :class:`Gateway` is those
two moments and nothing else — :meth:`prepare` and :meth:`finish`. It holds the config that
ties routing, the meter, and the turn journal together; the transport around it lives in the
ASGI layer.

Kept in its own module so the dependency graph stays a line, not a cycle:
``gateway`` (routing + meter) ← ``gateway_journal`` (records + store) ← this.
"""

from __future__ import annotations

import logging
import time
from dataclasses import dataclass, field
from typing import TYPE_CHECKING

from opendaisugi.gateway import (
    _PRICES_PER_MTOK,
    RouteDecision,
    TurnSaving,
    _latest_user_text,
    _new_user_text,
    conversation_key,
    measure_turn,
    route_turn,
)
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord, record_turn
from opendaisugi.routing import _DEFAULT_CHEAP_MODEL

if TYPE_CHECKING:
    from opendaisugi.gateway_answers import AnswerStore

_log = logging.getLogger("opendaisugi.gateway_pipeline")


@dataclass(frozen=True)
class PreparedTurn:
    """What :meth:`Gateway.prepare` hands the proxy: the decision, the body to actually send,
    the governing task text (for the record's readable label), and this turn's own new ask
    (for the repeat signature) — both carried so :meth:`Gateway.finish` need not re-parse."""

    decision: RouteDecision
    outbound_body: dict
    task: str
    ask: str


@dataclass
class Gateway:
    """Ties routing, the meter, and the turn journal into the proxy's two touchpoints."""

    cheap_model: str = _DEFAULT_CHEAP_MODEL
    journal: GatewayJournal | None = None
    prices: dict[str, tuple[float, float]] = field(default_factory=lambda: _PRICES_PER_MTOK)
    # Opt-in, default-off: persisting raw response text is a privacy choice (see
    # gateway_answers.py's module docstring). A bare Gateway() never touches an answer store.
    answer_store: "AnswerStore | None" = None
    capture_answers: bool = False
    # ADR-0015: a qualified local model (llamafile/Ollama via the same base_url
    # convention `daisugi setup` wires). When set, easy turns take the local rung
    # ahead of any cloud downgrade — zero quota, no cache economics to forfeit.
    local_model: str | None = None
    # Per-conversation sticky memory: conversation_key -> last routed model.
    # Bounded FIFO so a long-lived proxy can't grow it without limit.
    _session_models: dict = field(default_factory=dict, repr=False)

    _MAX_SESSIONS = 4096

    def __post_init__(self) -> None:
        # A local model prices at zero — it spends no provider quota and no
        # dollars. Tokens-saved stays the headline (the full turn is kept off
        # the frontier pool); the dollar multiplier is left conservative.
        if self.local_model and self.local_model not in self.prices:
            self.prices = {**self.prices, self.local_model: (0.0, 0.0)}

    def prepare(self, body: dict) -> PreparedTurn:
        """Decide the model for one turn and produce the body to forward.

        On a downgrade the model is swapped in a shallow copy — the caller's ``body`` is left
        exactly as received, so if anything downstream fails the proxy can still forward the
        original untouched (fail-open loses savings, never the turn).
        """
        task = _latest_user_text(body)
        ask = _new_user_text(body)
        key = conversation_key(body)
        decision = route_turn(
            body,
            cheap_model=self.cheap_model,
            local_model=self.local_model,
            sticky_model=self._session_models.get(key),
        )
        if len(self._session_models) >= self._MAX_SESSIONS and key not in self._session_models:
            self._session_models.pop(next(iter(self._session_models)))
        self._session_models[key] = decision.model
        outbound_body = dict(body)
        if decision.downgraded:
            outbound_body["model"] = decision.model
        return PreparedTurn(decision=decision, outbound_body=outbound_body, task=task, ask=ask)

    def finish(
        self,
        prepared: PreparedTurn,
        usage: dict,
        *,
        answer_text: str | None = None,
        now: float | None = None,
    ) -> tuple[TurnSaving, GatewayTurnRecord]:
        """Measure the saving from the response's usage and record the turn.

        Measuring never depends on persistence: the saving is returned whether or not a
        journal is configured, so a proxy with journalling off still reports its multiplier.

        ``answer_text`` is captured into ``self.answer_store`` — for later freshness-gated
        reuse via ``gateway_answers.recall_answer`` — only when ALL of these hold:
        ``capture_answers`` is on, an ``answer_store`` is configured, ``answer_text`` is
        non-empty, and ``prepared.ask`` is a real human ask (never a tool-loop continuation,
        which has nothing to stand alone as a repeatable question). Capture is best-effort:
        any failure is swallowed so it can never break the turn, mirroring the ASGI layer's
        ``_record`` fail-open discipline.
        """
        saving = measure_turn(prepared.decision, usage, prices=self.prices)
        record = record_turn(prepared.decision, saving, task=prepared.task, ask=prepared.ask)
        if self.journal is not None:
            self.journal.append(record)
        if (
            self.capture_answers
            and self.answer_store is not None
            and answer_text
            and prepared.ask.strip()
        ):
            try:
                from opendaisugi.gateway_answers import capture_answer

                capture_answer(
                    self.answer_store,
                    task=prepared.task,
                    answer=answer_text,
                    created_at=now if now is not None else time.time(),
                    ground_hash=None,  # the gateway doesn't know which files this relied on
                )
            except Exception as exc:  # pragma: no cover - defensive
                _log.warning("gateway answer capture failed (turn still served): %s", exc)
        return saving, record
