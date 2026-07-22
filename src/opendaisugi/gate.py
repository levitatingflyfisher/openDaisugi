"""Call-time tool gate — the enforce-mode counterpart of the passive hook.

Where :mod:`opendaisugi.hook` observes a host harness's tool calls and fails
open (correct for capture, wrong for protection), this module takes each
intercepted call, synthesizes it into a one-step plan, and proves it inside
the session's registered envelope *before it runs* (ADR-0007). The two share
a seam, not a failure policy:

- **enforce** mode is fail-closed: unknown tool, unparseable input, internal
  exception, or a slow verifier all DENY. The gate owns an inner timeout that
  itself denies, because every known host's *outer* hook timeout fails open.
- **shadow** mode (the default) observes: every call is evaluated and the
  would-have-denied verdict recorded, but the host always gets its allow
  contract. Shadow mode is observation, not protection.

The full :func:`opendaisugi.verify.verify` pipeline runs per call — not the
``verify_step`` hot path — so plan-level strict-mode checks are never skipped
at the boundary, and strictness is resolved from the envelope's stakes
(``strict=None``), never relaxed at the gate.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass, field
from typing import Any

from opendaisugi.hook import _payload_to_record, _records_to_steps
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.verify import verify

_DEFAULT_VERIFY_TIMEOUT_S = 10.0


@dataclass
class GateDecision:
    """The gate's verdict on one intercepted tool call.

    ``allow`` is what the host is told in the current mode; ``would_deny``
    is what enforce mode would have done — in shadow mode the pair diverges
    by design, and the shadow report is built from ``would_deny``.
    """

    allow: bool
    would_deny: bool
    reason: str
    mode: str
    tool_name: str | None = None
    step_type: str | None = None
    detail: str = ""
    elapsed_ms: float = 0.0


def _deny(mode: str, reason: str, *, tool_name: str | None = None,
          step_type: str | None = None, detail: str = "",
          t0: float) -> GateDecision:
    return GateDecision(
        allow=(mode == "shadow"),
        would_deny=True,
        reason=reason,
        mode=mode,
        tool_name=tool_name,
        step_type=step_type,
        detail=detail,
        elapsed_ms=(time.monotonic() - t0) * 1000,
    )


def _verify_with_timeout(plan: ActionPlan, envelope: Envelope,
                         timeout_s: float):
    """Run verify() in a worker thread with an inner deny-on-timeout.

    Returns the VerificationResult, or raises TimeoutError when the verifier
    outlives the budget. The worker is a daemon thread — a hung Z3 query
    cannot pin the gate process open past its own deadline.
    """
    box: list[Any] = []
    err: list[BaseException] = []

    def _run() -> None:
        try:
            box.append(verify(plan, envelope, strict=None))
        except BaseException as exc:  # noqa: BLE001 — re-raised on the caller side
            err.append(exc)

    worker = threading.Thread(target=_run, daemon=True)
    worker.start()
    worker.join(timeout_s)
    if worker.is_alive():
        raise TimeoutError(
            f"verifier exceeded the gate's inner time budget ({timeout_s}s)"
        )
    if err:
        raise err[0]
    return box[0]


def evaluate_record(record: dict[str, Any], envelope: Envelope, *,
                    mode: str = "shadow",
                    verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                    ) -> GateDecision:
    """Decide one already-normalized capture record against an envelope.

    Deny-by-default: every failure path inside this function resolves to a
    deny decision, never an exception to the caller.
    """
    t0 = time.monotonic()
    tool_name = record.get("tool_name")
    step_type = record.get("step_type")
    detail = str(
        record.get("command") or record.get("path") or record.get("url") or ""
    )
    try:
        steps = _records_to_steps([record])
        if not steps:
            return _deny(mode, f"could not synthesize a step for tool {tool_name!r}",
                         tool_name=tool_name, step_type=step_type, detail=detail, t0=t0)
        plan = ActionPlan(source="call-time-gate", task=envelope.task, steps=steps)
        result = _verify_with_timeout(plan, envelope, verify_timeout_s)
        if result.ok:
            return GateDecision(
                allow=True, would_deny=False, reason="verified in envelope",
                mode=mode, tool_name=tool_name, step_type=step_type,
                detail=detail, elapsed_ms=(time.monotonic() - t0) * 1000,
            )
        summary = "; ".join(
            f"{v.stage}: {v.message}" for v in result.violations
        ) or "verification failed"
        return _deny(mode, summary, tool_name=tool_name, step_type=step_type,
                     detail=detail, t0=t0)
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}",
                     tool_name=tool_name, step_type=step_type, detail=detail, t0=t0)


def evaluate_call(payload: Any, envelope: Envelope, *,
                  mode: str = "shadow",
                  verify_timeout_s: float = _DEFAULT_VERIFY_TIMEOUT_S,
                  ) -> GateDecision:
    """Decide one raw hook payload against an envelope. Deny-by-default.

    Never raises: malformed payloads, unknown tools, verifier errors, and
    verifier timeouts all come back as deny decisions (allowed-but-flagged
    in shadow mode).
    """
    t0 = time.monotonic()
    try:
        if not isinstance(payload, dict):
            return _deny(mode, "hook payload is not a JSON object", t0=t0)
        tool_name = (
            payload.get("tool_name") or payload.get("tool") or payload.get("name")
        )
        if not tool_name:
            return _deny(mode, "no tool name in hook payload", t0=t0)
        record = _payload_to_record(payload)
        if record is None:
            return _deny(
                mode,
                f"unrecognized tool {tool_name!r} — not in the gate's "
                "classification map, denied by default",
                tool_name=str(tool_name), t0=t0,
            )
        return evaluate_record(
            record, envelope, mode=mode, verify_timeout_s=verify_timeout_s,
        )
    except Exception as exc:  # noqa: BLE001 — fail-closed: any error denies
        return _deny(mode, f"gate internal error (denied fail-closed): {exc}", t0=t0)
