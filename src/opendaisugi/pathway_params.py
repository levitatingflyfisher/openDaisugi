"""Cluster-diff parameter discovery for distilled pathways (ADR-0008, Phase B3).

Given the concrete plans of a cluster's successful traces, find the typed *data
holes* — fields that vary across members while their capability head stays fixed.
An empty result means a frozen pathway (no safe hole). The same-shape (identical
step-type sequence) plus capability-head gate keeps holes to data slots only, so
a differing program / directory / host refuses parameterization rather than
producing a free ``{command}`` slot.
"""

from __future__ import annotations

import os
import shlex
from urllib.parse import urlsplit

from opendaisugi.dag import topological_order
from opendaisugi.models import ActionPlan, StepBase
from opendaisugi.pathway import PathwayParameter

# Step type → the single capability field B3 parameterizes.
#
# `shell` is deliberately EXCLUDED (ADR-0008 safety review): a shell head pins
# only argv[0], and verify does not check a command's file operands against the
# envelope's file_read/file_write globs — so a typed `grep <pat> /dev/null` could
# be bound to `grep <pat> /etc/shadow` (head `grep`, no metachars, verify passes),
# widening the reachable files. A search pattern can't be reliably told from a
# bare filename in a shell string, so shell pathways stay FROZEN (still 0-token
# reuse). Typed binding is limited to fields whose head pins the LOCATION: a
# file path's directory, a URL's host.
_CAP_FIELD: dict[str, str] = {
    "file_read": "path",
    "file_write": "path",
    "network": "url",
}


def _capability_head(step_type: str, value: str) -> str | None:
    """The invariant part of a capability field: the program / directory / host.

    Returns None when the value can't be parsed into a head, which the caller
    treats as a head mismatch (refuse) — fail-closed for parameterization.
    """
    try:
        if step_type == "shell":
            tokens = shlex.split(value)
            return tokens[0] if tokens else None
        if step_type in ("file_read", "file_write"):
            return os.path.dirname(value) or None
        if step_type == "network":
            parts = urlsplit(value)
            return f"{parts.scheme}://{parts.netloc}" if parts.netloc else None
    except Exception:
        return None
    return None


def diff_plans_for_parameters(plans: list[ActionPlan]) -> list[PathwayParameter]:
    """Diff a cluster's plans into typed data-holes, or [] for a frozen pathway.

    Requires ≥2 plans sharing one structure signature. For each capability field
    that varies across members, the field becomes a parameter only if every
    member's capability head is identical; a single differing head refuses the
    whole cluster (returns []), keeping it frozen rather than unsafe.
    """
    if len(plans) < 2:
        return []
    ordered: list[list[StepBase]] = [topological_order(p) for p in plans]
    signatures = {"→".join(s.type for s in steps) for steps in ordered}
    if len(signatures) != 1:
        return []

    params: list[PathwayParameter] = []
    for i in range(len(ordered[0])):
        step0 = ordered[0][i]
        field = _CAP_FIELD.get(step0.type)
        if field is None:
            continue
        values = [getattr(steps[i], field, None) for steps in ordered]
        if any(v is None for v in values):
            continue
        if len(set(values)) == 1:
            continue  # constant across the cluster → frozen, not a hole
        heads = {_capability_head(step0.type, v) for v in values}
        if len(heads) != 1 or None in heads:
            return []  # differing (or unparseable) capability head → refuse
        params.append(
            PathwayParameter(
                name=f"{step0.id}.{field}",
                step_index=i,
                step_id=step0.id,
                field=field,
                head=heads.pop(),
                observed=sorted(set(values)),
            )
        )
    return params


def rekey_to_template(
    params: list[PathwayParameter], template: ActionPlan
) -> list[PathwayParameter]:
    """Re-express diffed params against a (generalized) template by position.

    The diff runs on concrete cluster plans; the pathway stores an LLM-generalized
    template whose step ids differ. Each param's ``step_index`` locates its step in
    the template's topological order. If the template's shape doesn't line up
    (fewer steps, or a different capability field at that position), returns [] —
    the pathway stays frozen rather than carrying a hole it can't safely bind.
    """
    if not params:
        return []
    steps = topological_order(template)
    out: list[PathwayParameter] = []
    for p in params:
        if p.step_index >= len(steps):
            return []
        tstep = steps[p.step_index]
        if _CAP_FIELD.get(tstep.type) != p.field:
            return []
        out.append(p.model_copy(update={"step_id": tstep.id}))
    return out


def apply_bindings(
    template: ActionPlan,
    params: list["PathwayParameter"],
    values: dict[str, str],
) -> ActionPlan | None:
    """Fill a template's holes with bound values on a deep copy (ADR-0008, B4).

    Returns the bound plan, or None if any hole is unbound or a value would change
    its capability head — the *data-slots-only* rule enforced at bind time: a
    binding may change data, never the program / directory / host. (The bound plan
    is then re-verified against the caller's envelope by ``bind_parameters``, so
    safety rests on two independent checks, not on the LLM's goodwill.)
    """
    plan = template.model_copy(deep=True)
    steps = topological_order(plan)
    for p in params:
        if p.name not in values or p.step_index >= len(steps):
            return None
        value = values[p.name]
        step = steps[p.step_index]
        if _capability_head(step.type, value) != p.head:
            return None
        setattr(step, p.field, value)
    return plan


__all__ = ["apply_bindings", "diff_plans_for_parameters", "rekey_to_template"]
