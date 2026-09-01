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
from dataclasses import dataclass, field, replace
from typing import TYPE_CHECKING

from opendaisugi.gateway import (
    _PRICES_PER_MTOK,
    RouteDecision,
    TurnSaving,
    _latest_user_text,
    _new_user_text,
    _strictly_cheaper,
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


ROUTER_MODES = ("rules", "external", "off")
EXTERNAL_TIER = "tier-switchyard"
OFF_TIER = "tier-off"


@dataclass(frozen=True)
class ExternalRouterConfig:
    """What the gateway must know to meter an outside chooser honestly.

    ``route_id`` is the model string that selects the Switchyard route.
    ``capable_target`` and ``efficient_target`` are the real model ids the
    route chooses between. The ASGI layer compares the target the response
    names against these ids, never against the harness's own request, so a
    turn counts as a saving only when the efficient target served it.
    ``prices`` adds per-target prices to the meter's table.
    """

    route_id: str
    capable_target: str
    efficient_target: str
    prices: dict[str, tuple[float, float]] = field(default_factory=dict)


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
    # convention `daisugi tiers setup` wires). When set, easy turns take the local rung
    # ahead of any cloud downgrade — zero quota, no cache economics to forfeit.
    local_model: str | None = None
    # "rules" routes each turn with route_turn below. "external" sends the
    # route id of `external` and lets that chooser pick; it never rewrites a
    # turn onto a local or cheap model. "off" forwards each turn unchanged.
    router_mode: str = "rules"
    external: ExternalRouterConfig | None = None
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
        if self.router_mode not in ROUTER_MODES:
            raise ValueError(
                f"router_mode must be one of {', '.join(ROUTER_MODES)}, not {self.router_mode!r}"
            )
        if self.router_mode == "external":
            if self.external is None:
                raise ValueError("router_mode 'external' needs an ExternalRouterConfig")
            self.prices = {**self.prices, **self.external.prices}

    def prepare(self, body: dict) -> PreparedTurn:
        """Decide the model for one turn and produce the body to forward.

        On a downgrade the model is swapped in a shallow copy — the caller's ``body`` is left
        exactly as received, so if anything downstream fails the proxy can still forward the
        original untouched (fail-open loses savings, never the turn).

        External and off modes skip route_turn and the sticky table. No turn
        is marked downgraded here, so the proxy never retries one. The ASGI
        layer books an external turn once the response names its target.
        """
        if self.router_mode == "external":
            return self._prepare_external(body)
        if self.router_mode == "off":
            return self._prepare_off(body)
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

    def _prepare_external(self, body: dict) -> PreparedTurn:
        if self.external is None:
            raise ValueError("router_mode 'external' needs an ExternalRouterConfig")
        requested = body.get("model", "")
        route_id = self.external.route_id
        decision = RouteDecision(
            tier=EXTERNAL_TIER,
            model=route_id,
            requested_model=requested if isinstance(requested, str) else "",
            difficulty=0.0,
            downgraded=False,
            reason=f"the external router picks the model; sent as route {route_id!r}",
        )
        outbound_body = dict(body)
        outbound_body["model"] = route_id
        return PreparedTurn(
            decision=decision,
            outbound_body=outbound_body,
            task=_latest_user_text(body),
            ask=_new_user_text(body),
        )

    def _prepare_off(self, body: dict) -> PreparedTurn:
        requested = body.get("model", "")
        requested = requested if isinstance(requested, str) else ""
        decision = RouteDecision(
            tier=OFF_TIER,
            model=requested,
            requested_model=requested,
            difficulty=0.0,
            downgraded=False,
            reason="routing is off; the turn goes unchanged and is only metered",
        )
        return PreparedTurn(
            decision=decision,
            outbound_body=dict(body),
            task=_latest_user_text(body),
            ask=_new_user_text(body),
        )

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
        decision = prepared.decision
        if decision.downgraded and not _strictly_cheaper(
            decision.model, decision.requested_model, self.prices
        ):
            # The routing stands, but the price table shows no saving: the
            # same model under another id, or a model with no price.
            decision = replace(
                decision,
                downgraded=False,
                reason=f"{decision.reason}; the price table shows no saving, so none is booked",
            )
        saving = measure_turn(decision, usage, prices=self.prices)
        record = record_turn(decision, saving, task=prepared.task, ask=prepared.ask)
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
