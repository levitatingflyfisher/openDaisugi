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


# ---------------------------------------------------------------------------
# Do-nothing salvage (the gradual-automation inversion).
#
# ADR-0008's typed holes cover data-only variance. What they refuse — a shell
# command that varies at all (shell is excluded from typing), a path whose
# directory moves, a URL whose host changes — used to freeze the cluster on the
# representative's concrete step, replaying an action most members never took.
# The honest template for such a position is a *delegated leaf*: an AgenticStep
# prompted with the observed variants, executed under the same envelope through
# the call-time gate (ADR-0007), so delegation widens nothing. Invariant steps
# stay concrete (zero-token replay); typed holes stay typed; only the genuinely
# varying-in-kind positions cost LLM tokens at reuse — and the Gardener can
# promote a leaf back to concrete once its variance collapses.
# ---------------------------------------------------------------------------

# Host tools an AgenticStep leaf gets, by the step type it replaces. Each maps
# to a capability the cluster's envelope already grants (the step it replaces
# exercised it), so the delegated leaf verifies wherever the original did.
_LEAF_TOOLS: dict[str, list[str]] = {
    "shell": ["Bash"],
    "file_read": ["Read", "Glob", "Grep"],
    "file_write": ["Read", "Write", "Edit"],
    "network": ["WebFetch"],
}

_LEAF_VALUE_FIELD: dict[str, str] = {
    "shell": "command",
    "file_read": "path",
    "file_write": "path",
    "network": "url",
}

_MAX_LEAF_VARIANTS = 5


def plan_divergence(plans: list[ActionPlan]) -> tuple[list[int], list[PathwayParameter]]:
    """Split a same-shape cluster into divergent positions and typed holes.

    Returns ``(divergent_positions, parameters)``:

    - a position is **divergent** when its concrete values vary in a way typed
      binding refuses — any variance on a ``shell`` step (never typeable, per
      ADR-0008), or a differing/unparseable capability head on a
      file/network step;
    - a position is a **typed hole** exactly as in
      :func:`diff_plans_for_parameters` (same head, varying value);
    - everything else stays concrete.

    Refuses outright (``([], [])``) when the plans don't share one structure
    signature, when fewer than 2 plans exist, or when *every* position would be
    divergent — a cluster with no invariant left is not a pathway.
    """
    if len(plans) < 2:
        return [], []
    ordered: list[list[StepBase]] = [topological_order(p) for p in plans]
    signatures = {"→".join(s.type for s in steps) for steps in ordered}
    if len(signatures) != 1:
        return [], []

    divergent: list[int] = []
    params: list[PathwayParameter] = []
    for i in range(len(ordered[0])):
        step0 = ordered[0][i]
        value_field = _LEAF_VALUE_FIELD.get(step0.type)
        if value_field is None:
            continue
        values = [getattr(steps[i], value_field, None) for steps in ordered]
        if any(v is None for v in values) or len(set(values)) == 1:
            continue
        if step0.type == "shell":
            divergent.append(i)
            continue
        heads = {_capability_head(step0.type, v) for v in values}
        if len(heads) != 1 or None in heads:
            divergent.append(i)
            continue
        params.append(
            PathwayParameter(
                name=f"{step0.id}.{value_field}",
                step_index=i,
                step_id=step0.id,
                field=value_field,
                head=heads.pop(),
                observed=sorted(set(values)),
            )
        )
    if divergent and len(divergent) >= len(ordered[0]):
        return [], []
    return divergent, params


def build_delegated_template(
    representative: ActionPlan,
    divergent: list[int],
    plans: list[ActionPlan],
    *,
    workspace: str = ".",
) -> ActionPlan:
    """The mixed do-nothing template: representative plan with each divergent
    position replaced by an :class:`AgenticStep` leaf.

    The leaf keeps the original step's id and dependencies (the graph shape is
    the pathway's identity), carries the observed variants in its prompt so the
    executing agent knows what "this step" has meant before, and gets only the
    host tools mapped from the step type it replaced — capabilities the
    cluster's envelope already granted.
    """
    from opendaisugi.models import AgenticStep

    ordered = topological_order(representative)
    all_ordered = [topological_order(p) for p in plans]
    steps: list[StepBase] = []
    for i, step in enumerate(ordered):
        if i not in divergent:
            steps.append(step)
            continue
        value_field = _LEAF_VALUE_FIELD[step.type]
        variants = sorted({str(getattr(s[i], value_field, "")) for s in all_ordered if i < len(s)})
        shown = variants[:_MAX_LEAF_VARIANTS]
        more = f" (+{len(variants) - len(shown)} more)" if len(variants) > len(shown) else ""
        prompt = (
            f"Perform this step of the task. In past successful runs it was one of: "
            f"{'; '.join(shown)}{more}. Choose and execute the right equivalent for "
            f"the current task; stay within the granted tools and permissions."
        )
        steps.append(
            AgenticStep(
                id=step.id,
                depends_on=list(step.depends_on),
                prompt=prompt,
                workspace=workspace,
                tools=list(_LEAF_TOOLS[step.type]),
            )
        )
    return ActionPlan(source="distiller-salvage", task=representative.task, steps=steps)
