"""Envelope for the PI-VLA kit (v0.26+).

stakes='physical' triggers two existing v0.19 guarantees:
- Refuses delegation: a VLAStep can't carry preferred_model='haiku'
  (the VLA itself isn't LLM-delegation; the LLM-author would be).
- Blocks llm_check postconditions: physical stakes use sound primitives only.

The envelope also declares an end_effector_in_workspace invariant
that gates VLAStep.target_pose against the allowed bounds — same
v0.8-shipped Z3 check that runs on CartesianMoveStep.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission


def build_envelope() -> Envelope:
    return Envelope(
        generated_by="pi-vla-kit",
        task="VLA-driven multi-skill manipulation under physical stakes",
        permissions=Permission(
            shell=False,
            file_read=[], file_write=[],
            network=False,
            max_execution_time_s=120,
            max_output_size_mb=4,
            # Workspace AABB the VLA's target_pose must stay inside.
            # 2-DOF arm reaches modestly; production deployments tighten
            # this to the actual robot's safe envelope.
            workspace_bounds=((-0.4, -0.4, -0.1), (0.4, 0.6, 0.1)),
        ),
        stakes="physical",
        invariants=[
            Invariant(
                type="end_effector_in_workspace",
                description=(
                    "Every VLAStep's target_pose lies within the declared "
                    "workspace bounds. The Z3 check (z3_checks.py:117) "
                    "applies to VLAStep targets the same way it applies "
                    "to CartesianMoveStep — a learned policy can't be "
                    "asked to drive into a forbidden region."
                ),
                enforce=True,
                expr=None,  # v0.8 dedicated handler in z3_checks.py
            ),
        ],
    )
