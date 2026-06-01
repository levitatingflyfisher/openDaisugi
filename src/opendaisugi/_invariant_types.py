"""Authoritative set of invariant types that are legitimately ``expr``-less.

These are discharged by a dedicated symbolic/numerical handler in
``z3_checks.check_plan_invariants`` rather than the predicate algebra, so the
strict-mode opaque-invariant reject (verify.py / subsumption.py) must NOT flag
them. Single source of truth — keep in sync with the handlers in
``z3_checks.py`` (currently dispatched at z3_checks.py:326-332).
"""
from __future__ import annotations

RECOGNIZED_OPAQUE_TYPES = frozenset({
    "end_effector_in_workspace",
    "joint_limits_respected",
    "velocity_bounded",
    "no_obstacle_penetration",
})

# v0.28.3: opaque postcondition types discharged at Stage 2 by
# ``stage2._OPAQUE_POSTCONDITION_HANDLERS``. Stage 1 strict-mode must not
# reject these as "no verifiable expr" — they ARE verifiable, just at the
# post-execution gate. Keep in sync with stage2._OPAQUE_POSTCONDITION_HANDLERS.
RECOGNIZED_STAGE2_POSTCONDITION_TYPES = frozenset({
    "exit_code",
    "file_exists",
    "file_size_range",
})
