"""Decomposer — prompt → verified typed-step DAG (v0.32).

The forward-looking counterpart to envelope generation: where ``generate_envelope``
asks an LLM for the *safety envelope* of a task, ``decompose`` asks an LLM for the
*plan* — a DAG of typed steps (task / skill / mcp / shell / file / network) with
dependency edges. The library already defined the target shape (``ActionPlan`` +
``@step_type``) but wrote none of the authoring logic; this is that logic.

Every decomposition is verified before it is returned — the thesis is "verify
plans authored by LLMs at runtime", and a plan is the thing being verified:

1. **Structural** — ``check_dag`` proves no cycles and no dangling dependencies.
   Non-negotiable: ``topological_order`` (used by the Supervisor) would crash on
   a cyclic plan.
2. **Policy (optional)** — when an ``envelope`` is supplied, ``verify`` proves the
   whole plan is admissible; an out-of-policy decomposition raises rather than
   flowing into execution. The Supervisor re-verifies each step at run time too;
   this is the earlier, cheaper gate that also lets the caller retry.

The instructor client is injectable (``client=``) so this is unit-testable
without a live model, mirroring ``envelope.py``.
"""

from __future__ import annotations

import logging
from typing import Any, Literal

from pydantic import BaseModel, Field, ValidationError

from opendaisugi import llm as _llm
from opendaisugi.dag import check_dag
from opendaisugi.models import ActionPlan, Envelope, StepBase, coerce_step
from opendaisugi.verify import verify

_log = logging.getLogger("opendaisugi.decomposer")

_DEFAULT_MODEL = "anthropic/claude-sonnet-4-20250514"

DECOMPOSER_SYSTEM_PROMPT = """\
You are a planning decomposer. Given a task, break it into the smallest useful
sequence of typed steps and return them as a DAG.

Step types:
- "task": a natural-language subtask for an LLM to reason about. Field: prompt.
  Use this for analysis, drafting, summarizing, deciding — anything that is
  thinking rather than acting.
- "skill": invoke a reusable named skill/pathway. Field: skill_id (+ optional
  skill_input). Use when a distilled capability already covers the sub-goal.
- "mcp": call an external tool over MCP. Fields: server, tool (+ optional
  arguments).
- "shell": run one shell command (no pipes/;/&&). Field: command.
- "file_read"/"file_write": read/write one path. Fields: path (+ content).
- "network": one HTTP GET. Field: url.

Rules:
- Give every step a short unique id.
- Use depends_on (list of step ids) to encode ordering; independent steps may
  have no dependencies and will run in parallel-eligible order.
- Prefer "task" steps for reasoning and keep each step focused on one thing.
- Do not invent shell pipelines or chained commands; emit separate steps.
"""


class DecompositionError(Exception):
    """The decomposed plan failed structural or policy verification."""

    def __init__(self, message: str, *, plan: "ActionPlan | None" = None) -> None:
        super().__init__(message)
        self.plan = plan


class DecomposedStep(BaseModel):
    """LLM-facing flat step schema. Converted to a concrete StepBase subclass.

    All type-specific fields are optional here so a single schema covers every
    step kind; ``_to_step`` keeps only the fields relevant to ``type`` and lets
    the concrete Pydantic model enforce which are actually required.
    """

    id: str
    type: Literal["task", "skill", "mcp", "shell", "file_read", "file_write", "network"]
    depends_on: list[str] = Field(default_factory=list)
    # task
    prompt: str | None = None
    # skill
    skill_id: str | None = None
    skill_input: dict[str, Any] | None = None
    # mcp
    server: str | None = None
    tool: str | None = None
    arguments: dict[str, Any] | None = None
    # shell
    command: str | None = None
    # file
    path: str | None = None
    content: str | None = None
    # network
    url: str | None = None


class DecomposedPlan(BaseModel):
    steps: list[DecomposedStep]


_TYPE_FIELDS = (
    "prompt", "skill_id", "skill_input", "server", "tool", "arguments",
    "command", "path", "content", "url",
)


def _to_step(s: DecomposedStep) -> StepBase:
    payload: dict[str, Any] = {"type": s.type, "id": s.id, "depends_on": s.depends_on}
    for field in _TYPE_FIELDS:
        value = getattr(s, field, None)
        if value is not None:
            payload[field] = value
    try:
        step = coerce_step(payload)
    except ValidationError as e:
        raise DecompositionError(
            f"step {s.id!r} (type {s.type!r}) is missing required fields: {e}"
        ) from e
    if not isinstance(step, StepBase):
        raise DecompositionError(f"unknown step type {s.type!r} for step {s.id!r}")
    return step


async def decompose(
    prompt: str,
    *,
    model: str = _DEFAULT_MODEL,
    client: Any | None = None,
    backend: str | None = None,
    max_retries: int = 2,
    envelope: Envelope | None = None,
    z3_timeout_ms: int = 500,
) -> ActionPlan:
    """Decompose ``prompt`` into a verified :class:`ActionPlan`.

    Raises :class:`DecompositionError` if the LLM output can't be assembled into
    a structurally valid DAG, or (when ``envelope`` is given) if the plan is not
    admissible under it. The exception carries the assembled ``plan`` (when one
    was built) so a caller can inspect or retry.
    """
    if client is None:
        client = _llm.get_instructor_client(model=model, backend=backend)
    try:
        decomposed: DecomposedPlan = await client.chat.completions.create(
            model=model,
            max_retries=max_retries,
            response_model=DecomposedPlan,
            messages=[
                {"role": "system", "content": DECOMPOSER_SYSTEM_PROMPT},
                {"role": "user", "content": prompt},
            ],
        )
    except Exception as e:  # noqa: BLE001 — normalize at the boundary
        raise DecompositionError(f"decomposition LLM call failed: {_llm.translate_llm_error(e)}") from e

    if not decomposed.steps:
        raise DecompositionError("decomposition produced no steps")

    steps = [_to_step(s) for s in decomposed.steps]
    plan = ActionPlan(source="decomposer", task=prompt, steps=steps)

    dag_violations = check_dag(plan)
    if dag_violations:
        raise DecompositionError(
            f"decomposed plan is not a valid DAG: {dag_violations[0].message}",
            plan=plan,
        )

    if envelope is not None:
        result = verify(plan, envelope, z3_timeout_ms=z3_timeout_ms)
        if not result.ok:
            first = result.violations[0]
            raise DecompositionError(
                f"decomposed plan failed verify against envelope (out of policy): "
                f"[{first.stage}] {first.message}",
                plan=plan,
            )

    _log.info(
        "decompose.ok",
        extra={"steps": len(plan.steps), "gated_by_envelope": envelope is not None},
    )
    return plan


__all__ = [
    "DECOMPOSER_SYSTEM_PROMPT",
    "DecomposedPlan",
    "DecomposedStep",
    "DecompositionError",
    "decompose",
]
