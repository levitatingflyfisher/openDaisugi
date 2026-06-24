"""Step types for the dish-wash kit (v0.23 worked example).

The kit invents five motion-domain primitives that compose into a
'plate-wash' sub-DAG. A 'dish-wash' plan is N plate-wash sub-DAGs
chained sequentially. This tests two things at once:

1. Bet-test (a) — does the LLM/author invent useful domain DSLs *outside*
   software? Email and council are software-flavored; ApproachDish,
   LocateRim, BeginScrub stress the substrate's domain-agnosticism.
2. Pathway-as-step composition — plate-wash is a reusable 5-step
   sub-DAG; dish-wash composes it N times via depends_on. v0.22 doesn't
   ship a SubPlanStep type but the depends_on DAG already handles
   sequential composition cleanly.
"""
from __future__ import annotations

from typing import Literal

from opendaisugi.models import StepBase, step_type, Postcondition


@step_type
class ApproachDish(StepBase):
    """Move the end-effector above a target dish in the stack."""
    type: Literal["approach_dish"] = "approach_dish"
    dish_index: int
    approach_height_m: float = 0.05
    duration_s: float = 1.5
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="end_effector_xyz",
    )


@step_type
class LocateRim(StepBase):
    """Use vision to find the dish rim and refine the end-effector pose."""
    type: Literal["locate_rim"] = "locate_rim"
    dish_index: int
    pose_tolerance_mm: float = 2.0
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="rim_pose_error_mm",
    )


@step_type
class BeginScrub(StepBase):
    """Start the scrubbing motion (oscillating sponge contact along rim)."""
    type: Literal["begin_scrub"] = "begin_scrub"
    dish_index: int
    duration_s: float = 8.0
    contact_force_n: float = 4.0
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="scrub_complete",
    )


@step_type
class RinseWithHose(StepBase):
    """Activate the hose nozzle and rinse the dish under flowing water."""
    type: Literal["rinse_with_hose"] = "rinse_with_hose"
    dish_index: int
    duration_s: float = 3.0
    flow_rate_lps: float = 0.15
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="rinse_volume_ml",
    )


@step_type
class ReturnToDock(StepBase):
    """Move the end-effector back to the home / dock position.

    Required as the terminal step of every plate-wash sub-DAG so that
    the next plate-wash sequence starts from a known pose. The envelope
    invariant 'forall_steps: every plate-wash ends with ReturnToDock'
    encodes this contract — verifier rejects any plan that drops it.
    """
    type: Literal["return_to_dock"] = "return_to_dock"
    dish_index: int
    duration_s: float = 1.0
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="end_effector_xyz",
    )
