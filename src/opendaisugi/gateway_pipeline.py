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
import threading
import time
from dataclasses import dataclass, field, replace
from types import SimpleNamespace
from typing import TYPE_CHECKING, Any

from opendaisugi.gateway import (
    _PRICES_PER_MTOK,
    STICKY_PREFIX_THRESHOLD_TOKENS,
    RouteDecision,
    TurnSaving,
    _latest_user_text,
    _new_user_text,
    _strictly_cheaper,
    conversation_key,
    estimate_prefix_tokens,
    measure_turn,
    route_turn,
)
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord, record_turn
from opendaisugi.routing import _DEFAULT_CHEAP_MODEL, estimate_difficulty

if TYPE_CHECKING:
    from opendaisugi.gateway_answers import AnswerStore
    from opendaisugi.route_knn import KnnRouter

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
    # When prepare() began, on the monotonic clock. finish() books the time
    # from here to the end of the turn as the record's elapsed_ms.
    started: float | None = None
    # The coppice pane this turn came from and its floor, or empty. The
    # proxy takes them from the request path; finish() stamps the record.
    pane: str = ""
    floor: str = ""
    # knn mode only: the task vector and the embedder that made it, and the
    # conversation key. finish() books them. A turn with no vector is not booked.
    knn_vec: Any = None
    knn_embedder: str = ""
    conversation: str = ""


NOT_ASKED = "not asked"
ROUTER_MODES = ("rules", "external", "off", "knn", "model")
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
    # "knn" routes by the outcomes of similar past turns (route_knn). It needs a KnnRouter.
    knn: "KnnRouter | None" = None
    # "model" asks this model to pick among the allowed names; it needs a KnnRouter too,
    # which books the turns. grader_model grades finished turns (any booking mode).
    router_model: str | None = None
    grader_model: str | None = None
    # Per-conversation sticky memory: conversation_key -> last routed model.
    # Bounded FIFO so a long-lived proxy can't grow it without limit.
    _session_models: dict = field(default_factory=dict, repr=False)
    _lock: Any = field(default_factory=threading.Lock, repr=False)

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
        if self.router_mode == "knn" and self.knn is None:
            raise ValueError("router_mode 'knn' needs a KnnRouter")
        if self.router_mode == "model" and (self.knn is None or not self.router_model):
            raise ValueError("router_mode 'model' needs a KnnRouter and a router_model")

    def pick_request(self, body: dict) -> tuple[bytes, list[str]] | None:
        """The request that asks the router model to pick, and the allowed names.

        None when the turn has no new text (a tool-loop continuation), names no model, or
        has no cheaper rung to pick: the conversation's last model or the rules router
        decides then.
        """
        from opendaisugi.route_knn import candidate_models
        from opendaisugi.route_model import picker_body

        task = _latest_user_text(body)
        requested = body.get("model", "")
        if not _new_user_text(body).strip() or not task.strip():
            return None
        if not isinstance(requested, str) or not requested:
            return None
        cands = candidate_models(
            requested,
            cheap_model=self.cheap_model,
            local_model=self.local_model,
            prices=self.prices,
        )
        if len(cands) < 2:
            return None
        return picker_body(self.router_model or "", task, cands), cands

    def prepare(
        self, body: dict, choice: tuple[str | None, str | None] | None = None
    ) -> PreparedTurn:
        """Decide the model for one turn and produce the body to forward.

        On a downgrade the model is swapped in a shallow copy — the caller's ``body`` is left
        exactly as received, so if anything downstream fails the proxy can still forward the
        original untouched (fail-open loses savings, never the turn).

        External and off modes skip route_turn and the sticky table. No turn
        is marked downgraded here, so the proxy never retries one. The ASGI
        layer books an external turn once the response names its target.
        """
        started = time.monotonic()
        return replace(self._prepare(body, choice), started=started)

    def _prepare(
        self, body: dict, choice: tuple[str | None, str | None] | None = None
    ) -> PreparedTurn:
        if self.router_mode == "external":
            return self._prepare_external(body)
        if self.router_mode == "off":
            return self._prepare_off(body)
        if self.router_mode == "knn":
            return self._prepare_knn(body)
        if self.router_mode == "model":
            return self._prepare_knn(body, choice if choice is not None else (None, NOT_ASKED))
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

    def _remember(self, key: str, model: str) -> None:
        with self._lock:  # knn turns are prepared on worker threads
            if len(self._session_models) >= self._MAX_SESSIONS and key not in self._session_models:
                self._session_models.pop(next(iter(self._session_models)))
            self._session_models[key] = model

    def _prepare_knn(
        self, body: dict, choice: tuple[str | None, str | None] | None = None
    ) -> PreparedTurn:
        """Route by the outcomes of similar past turns. See :mod:`opendaisugi.route_knn`.

        With too few known neighbours, or no vector for the task, the rules router
        decides and the reason says why. The conservative core of route_turn stays:
        no signal means no downgrade, and knn never picks a model above the requested one.
        """
        from opendaisugi.route_knn import candidate_models, decide

        assert self.knn is not None
        task = _latest_user_text(body)
        ask = _new_user_text(body)
        key = conversation_key(body)
        sticky = self._session_models.get(key)
        requested = body.get("model", "")

        def rules() -> RouteDecision:
            return route_turn(
                body,
                cheap_model=self.cheap_model,
                local_model=self.local_model,
                sticky_model=sticky,
            )

        def fallback(prefix: str) -> RouteDecision:
            d = rules()
            return replace(d, reason=f"{prefix}; rules router: {d.reason}")

        vec = None
        identity = ""
        if not task.strip():
            decision = rules()
        elif not isinstance(requested, str) or not requested:
            decision = fallback("knn: the request names no model")
        else:
            embed_failed = False
            # In model mode the vector only books the turn, and a continuation is not booked.
            if choice is None or ask.strip():
                try:
                    vec, identity = self.knn.embed(task)
                except Exception as exc:
                    _log.warning("knn: embedding failed: %s", type(exc).__name__)
                    embed_failed = True
            if choice is not None:
                return self._prepare_model(body, choice, task, ask, key, sticky, vec, identity)
            if embed_failed:
                decision = fallback("knn: could not embed the task")
            else:
                try:
                    rows = self.knn.store.recent(identity)
                except Exception as exc:
                    _log.warning("knn: outcome table unreadable, using the rules router: %s", exc)
                    rows = []
                kd = decide(
                    vec,
                    rows,
                    requested=requested,
                    candidates=candidate_models(
                        requested,
                        cheap_model=self.cheap_model,
                        local_model=self.local_model,
                        prices=self.prices,
                    ),
                    prices=self.prices,
                    threshold=self.knn.threshold,
                )
                if kd.fallback:
                    decision = fallback(kd.reason)
                else:
                    decision = self._knn_decision(kd, body, sticky, task)
        self._remember(key, decision.model)
        outbound_body = dict(body)
        if decision.downgraded:
            outbound_body["model"] = decision.model
        return PreparedTurn(
            decision=decision,
            outbound_body=outbound_body,
            task=task,
            ask=ask,
            knn_vec=vec,
            knn_embedder=identity,
            conversation=key,
        )

    def _prepare_model(self, body, choice, task, ask, key, sticky, vec, identity) -> PreparedTurn:
        """The model router: the router model picked ``choice[0]``, or failed with ``choice[1]``."""
        from opendaisugi.route_knn import candidate_models

        requested = body.get("model", "")
        picked, why = choice
        cands = candidate_models(
            requested,
            cheap_model=self.cheap_model,
            local_model=self.local_model,
            prices=self.prices,
        )
        how = f"model: {picked} picked by {self.router_model} from {', '.join(cands)}"
        if picked is None and why == NOT_ASKED and not ask.strip() and sticky in cands:
            # A tool-loop continuation stays on the conversation's last model.
            picked, how = sticky, f"model: {sticky} kept from the last turn of this conversation"
        elif picked is not None and picked not in cands:
            picked, why = None, "the router model named a model that is not allowed"
        if picked is None:
            d = route_turn(
                body,
                cheap_model=self.cheap_model,
                local_model=self.local_model,
                sticky_model=sticky,
            )
            decision = replace(d, reason=f"model: {why}; rules router: {d.reason}")
        else:
            decision = self._knn_decision(
                SimpleNamespace(model=picked, reason=how), body, sticky, task
            )
        self._remember(key, decision.model)
        outbound_body = dict(body)
        if decision.downgraded:
            outbound_body["model"] = decision.model
        return PreparedTurn(
            decision=decision,
            outbound_body=outbound_body,
            task=task,
            ask=ask,
            knn_vec=vec,
            knn_embedder=identity,
            conversation=key,
        )

    def _knn_decision(self, kd, body: dict, sticky: str | None, task: str) -> RouteDecision:
        """A RouteDecision from a knn choice, with ADR-0015 stickiness on top."""
        requested = body.get("model", "")
        difficulty = estimate_difficulty(task)
        model = kd.model
        if model == requested:
            return RouteDecision(
                tier="tier2-frontier",
                model=requested,
                requested_model=requested,
                difficulty=difficulty,
                downgraded=False,
                reason=kd.reason,
            )
        if model != self.local_model and sticky != model:
            prefix = estimate_prefix_tokens(body)
            if prefix >= STICKY_PREFIX_THRESHOLD_TOKENS:
                return RouteDecision(
                    tier="tier2-frontier",
                    model=requested,
                    requested_model=requested,
                    difficulty=difficulty,
                    downgraded=False,
                    reason=(
                        f"{kd.reason}; sticky: ~{prefix} prefix tokens cached against the "
                        f"requested model, so it stays"
                    ),
                )
        return RouteDecision(
            tier="tier1-local" if model == self.local_model else "tier1-cheap",
            model=model,
            requested_model=requested,
            difficulty=difficulty,
            downgraded=True,
            reason=kd.reason,
        )

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

    def _book_knn(self, prepared, decision, saving, elapsed, failed: bool, now) -> str | None:
        """Book one knn turn in the outcome table. Best-effort: it never breaks the turn."""
        if self.knn is None or prepared.knn_vec is None or not prepared.ask.strip():
            return None
        try:
            return self.knn.record(
                conversation=prepared.conversation,
                task=prepared.task,
                vec=prepared.knn_vec,
                embedder=prepared.knn_embedder,
                model=decision.model,
                cost=saving.actual.dollars,
                latency_ms=elapsed,
                failed=failed,
                now=now,
            )
        except Exception as exc:
            _log.warning("knn: could not book the turn (turn still served): %s", exc)
            return None

    def finish(
        self,
        prepared: PreparedTurn,
        usage: dict,
        *,
        answer_text: str | None = None,
        now: float | None = None,
        failed: bool = False,
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
        elapsed = (
            None
            if prepared.started is None
            else round((time.monotonic() - prepared.started) * 1000, 3)
        )
        turn_id = None
        reason = None
        if self.router_mode in ("knn", "model"):
            reason = decision.reason
            turn_id = self._book_knn(prepared, decision, saving, elapsed, failed, now)
        record = record_turn(
            decision,
            saving,
            task=prepared.task,
            ask=prepared.ask,
            elapsed_ms=elapsed,
            pane=prepared.pane or None,
            floor=prepared.floor or None,
            turn_id=turn_id,
            reason=reason,
        )
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
