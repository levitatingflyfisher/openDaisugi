"""Synthesizer — collect step outputs into a final answer (v0.32).

The orchestrator runs a decomposed plan step-by-step; each step leaves an output
on its :class:`~opendaisugi.run_session.StepOutcome`. The synthesizer is the last
stage: it gathers those outputs and composes the single answer the user asked for.

Two modes:
- **LLM synthesis** (default) — one model call that reads the original prompt plus
  every step's output and writes the final answer. Usually the cheapest tier: the
  hard reasoning already happened in the steps; this is assembly.
- **Deterministic fallback** (D5) — a plain, labeled concatenation of the step
  outputs. Used when ``use_llm=False`` (e.g. the token budget is spent), when no
  LLM client is available, or when the LLM call fails. The synthesizer therefore
  *always* returns something rather than erroring at the finish line.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import Any

from pydantic import BaseModel

from opendaisugi import llm as _llm

_log = logging.getLogger("opendaisugi.synthesizer")

_DEFAULT_MODEL = "claude-haiku-4-5"

SYNTHESIZER_SYSTEM_PROMPT = """\
You are a synthesizer. You are given the user's original request and the outputs
of the steps that were run to fulfill it. Write the single, complete final answer
the user asked for, drawing only on the step outputs. Do not mention the steps,
the plan, or that you are synthesizing — just give the answer.
"""


@dataclass(frozen=True)
class StepOutput:
    """One step's contribution to the final answer."""

    step_id: str
    kind: str
    status: str
    output: str


@dataclass(frozen=True)
class SynthesisResult:
    answer: str
    used_llm: bool
    outputs: list[StepOutput]


class _Answer(BaseModel):
    answer: str


def collect_outputs(session, plan) -> list[StepOutput]:
    """Pair each step outcome with its step kind, preserving plan order.

    Failed/aborted steps are kept (marked by ``status``), not dropped — the
    synthesizer decides what to do with a partial result rather than silently
    losing it.
    """
    kind_by_id = {s.id: getattr(s, "type", "?") for s in plan.steps}
    order = {s.id: i for i, s in enumerate(plan.steps)}
    outs = [
        StepOutput(
            step_id=o.step_id,
            kind=kind_by_id.get(o.step_id, "?"),
            status=o.status,
            output=o.stdout or "",
        )
        for o in session.steps
    ]
    outs.sort(key=lambda o: order.get(o.step_id, len(order)))
    return outs


def _deterministic_answer(prompt: str, outputs: list[StepOutput]) -> str:
    """A labeled concatenation of successful step outputs — the always-works path."""
    succeeded = [o for o in outputs if o.status == "succeeded" and o.output.strip()]
    if not succeeded:
        return f"No step produced output for: {prompt}"
    lines = [f"Results for: {prompt}", ""]
    for o in succeeded:
        lines.append(f"[{o.step_id} · {o.kind}]")
        lines.append(o.output.strip())
        lines.append("")
    return "\n".join(lines).rstrip()


def _render_steps_for_llm(outputs: list[StepOutput]) -> str:
    blocks = []
    for o in outputs:
        blocks.append(f"### step {o.step_id} ({o.kind}, {o.status})\n{o.output.strip()}")
    return "\n\n".join(blocks)


async def synthesize(
    prompt: str,
    session,
    plan,
    *,
    model: str = _DEFAULT_MODEL,
    client: Any | None = None,
    backend: str | None = None,
    use_llm: bool = True,
) -> SynthesisResult:
    """Compose the final answer from ``session``'s step outputs.

    Never raises for LLM reasons — on any failure it returns the deterministic
    fallback so the orchestration always yields an answer.
    """
    outputs = collect_outputs(session, plan)

    def _fallback() -> SynthesisResult:
        return SynthesisResult(_deterministic_answer(prompt, outputs), used_llm=False, outputs=outputs)

    if not use_llm:
        return _fallback()

    if client is None:
        try:
            client = _llm.get_instructor_client(model=model, backend=backend)
        except Exception as e:  # noqa: BLE001
            _log.info("synthesize.no_client", extra={"error": str(e)})
            return _fallback()

    user_content = (
        f"Original request:\n{prompt}\n\n"
        f"Step outputs:\n{_render_steps_for_llm(outputs)}"
    )
    try:
        resp = await client.chat.completions.create(
            model=model,
            response_model=_Answer,
            messages=[
                {"role": "system", "content": SYNTHESIZER_SYSTEM_PROMPT},
                {"role": "user", "content": user_content},
            ],
        )
    except Exception as e:  # noqa: BLE001 — synthesis must not be the thing that fails
        _log.info("synthesize.llm_failed_fallback", extra={"error": str(e)})
        return _fallback()

    answer = (getattr(resp, "answer", "") or "").strip()
    if not answer:
        return _fallback()
    return SynthesisResult(answer=answer, used_llm=True, outputs=outputs)


__all__ = [
    "SYNTHESIZER_SYSTEM_PROMPT",
    "StepOutput",
    "SynthesisResult",
    "collect_outputs",
    "synthesize",
]
