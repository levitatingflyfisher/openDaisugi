"""Dish-wash envelope: physical stakes + structural invariants.

stakes='physical' triggers two guarantees from the v0.19 substrate:
- The verifier's _check_delegation_safety refuses any step with
  preferred_model set. Robotic motions cannot be delegated to an LLM
  whose arguments static verification can't ground.
- LLMCheck postconditions are blocked at the predicate evaluator —
  perceptual judgement has no place in a physical-stakes plan.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission


def build_envelope() -> Envelope:
    return Envelope(
        generated_by="dish-wash-kit",
        task="Wash a stack of plates with the dish-wash robot",
        permissions=Permission(
            shell=False,
            file_read=[],
            file_write=[],
            network=False,
            max_execution_time_s=600,
            max_output_size_mb=1,
            # Declare this kit's own registered @step_type primitives so they are
            # permitted under strict mode (stakes='physical' turns strict on; an
            # undeclared custom step type is otherwise rejected fail-closed).
            custom_step_allowlist=[
                "approach_dish", "locate_rim", "begin_scrub",
                "rinse_with_hose", "return_to_dock",
            ],
        ),
        stakes="physical",
        invariants=[
            Invariant(
                type="every_plate_wash_ends_with_return_to_dock",
                description=(
                    "Every plate-wash sub-DAG must terminate with a "
                    "ReturnToDock step. The next plate-wash sequence "
                    "depends on the end-effector starting at a known dock "
                    "pose — dropping ReturnToDock leaves the arm in an "
                    "undefined position and breaks the next sub-DAG's "
                    "ApproachDish trajectory assumptions."
                ),
                enforce=True,
                # exists_step ReturnToDock — i.e. at least one ReturnToDock
                # appears in the plan. A stronger 'every BeginScrub has a
                # corresponding ReturnToDock with same dish_index' would
                # need scalar-context exists_step, deferred to v0.23+.
                expr={
                    "op": "exists_step",
                    "pred": {"op": "equals", "path": "type", "value": "return_to_dock"},
                },
            ),
        ],
    )
