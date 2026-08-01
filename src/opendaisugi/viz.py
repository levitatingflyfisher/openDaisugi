"""Render an ActionPlan as a self-contained execution-monitor HTML page.

``plan_to_viz_data`` turns a plan + envelope into a JSON-able dict using the same
real machinery the runtime uses — :func:`opendaisugi.verify.verify` for the
allowed/refused verdict per step, :func:`opendaisugi.model_sizer.size_plan` for
per-step model sizing, and :func:`opendaisugi.dag.dependency_levels` for the
parallel "waves". ``render_dag_html`` injects that data into a bundled template
(all CSS/JS inline, no external fetches) so the result opens anywhere.

There is no LLM call and no I/O here — it is a pure view over an existing plan.
"""

from __future__ import annotations

import json
import re
from importlib import resources
from typing import Any

from opendaisugi.dag import dependency_levels
from opendaisugi.model_sizer import DEFAULT_LADDER, ModelLadder, size_plan
from opendaisugi.models import ActionPlan, Envelope
from opendaisugi.verify import verify

_KIND_LABEL: dict[str, str] = {
    "shell": "Shell",
    "file_read": "Read",
    "file_write": "Write",
    "network": "Network",
    "task": "Sub-agent",
    "skill": "Skill",
    "mcp": "MCP",
}

# Steps that run without a model (the sizer sizes every step, but only these
# reasoning kinds actually spend tokens at execution).
_LLM_KINDS = frozenset({"task", "agentic"})

_STEP_RE = re.compile(r"[Ss]tep '([^']+)'")


def _step_label(step: Any) -> str:
    t = step.type
    if t == "shell":
        return step.command or ""
    if t in ("file_read", "file_write"):
        return step.path or ""
    if t == "network":
        return getattr(step, "url", "") or ""
    if t == "task":
        return step.prompt or ""
    if t == "skill":
        return step.skill_id or ""
    if t == "mcp":
        return f"{step.server}/{step.tool}"
    return t


def plan_to_viz_data(
    plan: ActionPlan, envelope: Envelope, *, ladder: ModelLadder = DEFAULT_LADDER
) -> dict:
    """Build the JSON-able view model for ``plan`` verified against ``envelope``."""
    result = verify(plan, envelope)
    sizings = {s.step_id: s for s in size_plan(plan, ladder=ladder)}
    levels = dependency_levels(plan)
    level_of: dict[str, int] = {}
    for i, lvl in enumerate(levels):
        for st in lvl:
            level_of[st.id] = i

    viol_by_step: dict[str, list[dict]] = {}
    for v in result.violations:
        m = _STEP_RE.search(getattr(v, "message", "") or "")
        sid = m.group(1) if m else getattr(v, "step_id", None)
        viol_by_step.setdefault(sid, []).append(
            {"stage": getattr(v, "stage", ""), "message": getattr(v, "message", "")}
        )

    steps: list[dict] = []
    for s in plan.steps:
        sz = sizings.get(s.id)
        vs = viol_by_step.get(s.id, [])
        steps.append(
            {
                "id": s.id,
                "type": s.type,
                "kind": _KIND_LABEL.get(s.type, s.type),
                "label": _step_label(s),
                "depends_on": list(s.depends_on),
                "level": level_of.get(s.id, 0),
                "blocked": bool(vs),
                "violations": vs,
                "model": sz.model if sz else None,
                "tier": sz.tier if sz else None,
                "difficulty": round(sz.difficulty, 2) if sz else None,
                "est_tokens": sz.est_tokens if sz else None,
                "runs_llm": s.type in _LLM_KINDS,
            }
        )

    p = envelope.permissions
    return {
        "task": envelope.task or plan.task,
        "envelope": {
            "shell_allowlist": list(p.shell_allowlist),
            "file_read": list(p.file_read),
            "network": p.network,
            "mcp_allowlist": list(p.mcp_allowlist),
        },
        "ok": result.ok,
        "n_violations": len(result.violations),
        "levels": [[st.id for st in lvl] for lvl in levels],
        "steps": steps,
    }


_TEMPLATE_CACHE: str | None = None


def _template() -> str:
    global _TEMPLATE_CACHE
    if _TEMPLATE_CACHE is None:
        _TEMPLATE_CACHE = (
            resources.files("opendaisugi")
            .joinpath("viz_dag_template.html")
            .read_text(encoding="utf-8")
        )
    return _TEMPLATE_CACHE


def render_dag_html(
    plan: ActionPlan, envelope: Envelope, *, ladder: ModelLadder = DEFAULT_LADDER
) -> str:
    """Render ``plan`` (verified against ``envelope``) to a standalone HTML page."""
    data = plan_to_viz_data(plan, envelope, ladder=ladder)
    # Escape ``</`` so a step label containing ``</script>`` cannot close the
    # inline data block and inject markup.
    payload = json.dumps(data).replace("</", "<\\/")
    return _template().replace("/*__DATA__*/ null", payload)
