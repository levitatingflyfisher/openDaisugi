"""Build a VLA-driven plan: a sequence of skills, each delegated to the
policy. The plan structure (skills + ordering) is human-authored or
distilled from past pathways; the per-action stream inside each skill
is whatever the VLA emits at 30Hz."""
from __future__ import annotations

from opendaisugi.models import ActionPlan, VLAStep


def build_plan(skills: list[dict]) -> ActionPlan:
    """Each entry: ``{"task": "...", "target_pose": (x, y, z), "max_actions": int}``.

    Sequential composition: each VLAStep depends_on the previous one's
    completion, so the supervisor verifies + rolls out one skill at a time.
    Per-skill receipts capture the VLA's action sequence summary.
    """
    steps: list = []
    prev_id: str | None = None
    for i, sk in enumerate(skills):
        s = VLAStep(
            id=f"s{i}",
            task=sk["task"],
            target_pose=tuple(sk["target_pose"]),
            max_actions=sk.get("max_actions", 30),
            timeout_s=sk.get("timeout_s", 5.0),
            depends_on=[prev_id] if prev_id else [],
        )
        steps.append(s)
        prev_id = s.id
    return ActionPlan(
        source="pi-vla-kit",
        task=f"VLA-driven {len(skills)} skill sequence",
        steps=steps,
    )
