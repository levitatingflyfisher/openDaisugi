"""Build dish-wash ActionPlans from plate-wash sub-DAGs."""
from __future__ import annotations

from step_types import (
    ApproachDish,
    BeginScrub,
    LocateRim,
    ReturnToDock,
    RinseWithHose,
)

from opendaisugi.models import ActionPlan


def _plate_wash_steps(dish_index: int, base_id: int) -> list:
    """5-step sub-DAG for washing a single plate.

    ApproachDish → LocateRim → BeginScrub → RinseWithHose → ReturnToDock.
    Each step depends on the previous; the sub-DAG is a sequential chain.
    Returns steps with ids of the form ``s{base_id+i}`` so the caller can
    chain multiple plate-washes via depends_on on the LAST step.
    """
    a = ApproachDish(id=f"s{base_id+0}", dish_index=dish_index)
    b = LocateRim(id=f"s{base_id+1}", dish_index=dish_index, depends_on=[a.id])
    c = BeginScrub(id=f"s{base_id+2}", dish_index=dish_index, depends_on=[b.id])
    d = RinseWithHose(id=f"s{base_id+3}", dish_index=dish_index, depends_on=[c.id])
    e = ReturnToDock(id=f"s{base_id+4}", dish_index=dish_index, depends_on=[d.id])
    return [a, b, c, d, e]


def build_plan(num_dishes: int) -> ActionPlan:
    """Compose a dish-wash plan as N plate-wash sub-DAGs in series.

    The next plate-wash starts only after the previous one's ReturnToDock —
    this ordering is guaranteed by adding the previous ReturnToDock's id
    to the next ApproachDish's depends_on. The whole plan is one DAG.
    """
    steps: list = []
    prev_terminal_id: str | None = None
    for dish_idx in range(num_dishes):
        sub = _plate_wash_steps(dish_idx, base_id=len(steps))
        if prev_terminal_id is not None:
            # Chain this sub-DAG onto the previous one's ReturnToDock.
            sub[0] = sub[0].model_copy(
                update={"depends_on": [prev_terminal_id]},
            )
        steps.extend(sub)
        prev_terminal_id = sub[-1].id  # the ReturnToDock id
    return ActionPlan(
        source="dish-wash-kit",
        task=f"Wash {num_dishes} plate(s)",
        steps=steps,
    )
