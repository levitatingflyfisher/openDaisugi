"""Envelope inheritance verification.

Pure function: child must be a *tightening* of parent. The generator (Task 5)
calls this when a parent envelope is supplied. Multi-level inheritance is out
of scope for v0.1.2 — child.parent_envelope MUST be set to parent.id, and
parent.parent_envelope MUST be None.
"""

from __future__ import annotations

from pydantic import BaseModel

from opendaisugi.models import Envelope, Violation

_STAGE = "inheritance"


class EnvelopeInheritanceError(Exception):
    """Raised when a child envelope relaxes any parent constraint."""

    def __init__(self, violations: list[Violation]) -> None:
        self.violations = violations
        msgs = "; ".join(v.message for v in violations)
        super().__init__(f"inheritance violations: {msgs}")


def verify_inheritance(child: Envelope, parent: Envelope) -> list[Violation]:
    """Return all tightening violations. Empty list = child is a valid tightening.

    Rules (per v0.1.2 spec "Inheritance design" table):
      - list[str] glob/allowlist fields: set(child) ⊆ set(parent)
      - bool fields: child <= parent (False is tighter)
      - int budget fields: child <= parent
      - network_hosts: if parent is empty (= any host) child may be anything;
        otherwise set(child) ⊆ set(parent) AND child must be non-empty
      - invariants / postconditions: parent's set ⊆ child's set (child may add,
        may not remove)
      - parent.parent_envelope MUST be None (depth-1 only)
    """
    violations: list[Violation] = []

    if parent.parent_envelope is not None:
        violations.append(
            Violation(
                stage=_STAGE,
                message=(
                    "parent envelope has its own parent_envelope; "
                    "v0.1.2 supports only depth-1 inheritance"
                ),
            )
        )

    _check_set_subset(
        violations, "file_read",
        child.permissions.file_read, parent.permissions.file_read,
    )
    _check_set_subset(
        violations, "file_write",
        child.permissions.file_write, parent.permissions.file_write,
    )
    _check_bool_le(
        violations, "network",
        child.permissions.network, parent.permissions.network,
    )
    _check_network_hosts(
        violations,
        child.permissions.network_hosts, parent.permissions.network_hosts,
    )
    _check_bool_le(
        violations, "shell",
        child.permissions.shell, parent.permissions.shell,
    )
    _check_set_subset(
        violations, "shell_allowlist",
        child.permissions.shell_allowlist, parent.permissions.shell_allowlist,
    )
    _check_int_le(
        violations, "max_execution_time_s",
        child.permissions.max_execution_time_s,
        parent.permissions.max_execution_time_s,
    )
    _check_int_le(
        violations, "max_output_size_mb",
        child.permissions.max_output_size_mb,
        parent.permissions.max_output_size_mb,
    )
    _check_set_subset(
        violations, "mcp_allowlist",
        child.permissions.mcp_allowlist, parent.permissions.mcp_allowlist,
    )
    # Robotics capabilities (workspace_bounds/velocity/torque/joint/obstacles) —
    # reuse the fail-closed subsumption check: the child must not exceed OR leave
    # undeclared any physical bound the parent constrains. (v0.1.2's field-by-field
    # inheritance predates v0.8 robotics, so these were silently unchecked.)
    from opendaisugi.subsumption import _robot_capability_violation
    robot_reason = _robot_capability_violation(parent.permissions, child.permissions)
    if robot_reason is not None:
        violations.append(Violation(
            stage=_STAGE, message=f"robot capability relaxed: {robot_reason}",
        ))
    # Stakes may be tightened (escalated) but never downgraded — downgrading
    # physical→low re-enables probabilistic primitives the parent locked out.
    _stakes_rank = {"low": 0, "medium": 1, "high": 2, "physical": 3}
    if _stakes_rank.get(child.stakes, 0) < _stakes_rank.get(parent.stakes, 0):
        violations.append(Violation(
            stage=_STAGE,
            message=f"stakes: child '{child.stakes}' downgrades parent '{parent.stakes}'",
        ))
    _check_superset(violations, "invariants", child.invariants, parent.invariants)
    _check_superset(
        violations, "postconditions", child.postconditions, parent.postconditions,
    )
    return violations


def _check_set_subset(
    violations: list[Violation],
    field: str,
    child_vals: list[str],
    parent_vals: list[str],
) -> None:
    parent_set = set(parent_vals)
    extras = sorted(set(child_vals) - parent_set)
    if extras:
        for extra in extras:
            violations.append(
                Violation(
                    stage=_STAGE,
                    message=(
                        f"{field}: child glob {extra!r} not in parent's "
                        f"allowed set {sorted(parent_set)!r}"
                    ),
                )
            )


def _check_bool_le(
    violations: list[Violation],
    field: str,
    child_val: bool,
    parent_val: bool,
) -> None:
    if child_val and not parent_val:
        violations.append(
            Violation(
                stage=_STAGE,
                message=f"{field}: child=True relaxes parent=False",
            )
        )


def _check_int_le(
    violations: list[Violation],
    field: str,
    child_val: int,
    parent_val: int,
) -> None:
    if child_val > parent_val:
        violations.append(
            Violation(
                stage=_STAGE,
                message=f"{field}: child={child_val} exceeds parent={parent_val}",
            )
        )


def _check_network_hosts(
    violations: list[Violation],
    child_hosts: list[str],
    parent_hosts: list[str],
) -> None:
    # Parent empty = any host allowed → child may be anything.
    if not parent_hosts:
        return
    # Parent non-empty: child empty means "any" = relaxation.
    if not child_hosts:
        violations.append(
            Violation(
                stage=_STAGE,
                message=(
                    f"network_hosts: child is empty (means any host) but "
                    f"parent restricts to {sorted(parent_hosts)!r}"
                ),
            )
        )
        return
    # Both non-empty: strict string-set subset.
    parent_set = set(parent_hosts)
    extras = sorted(set(child_hosts) - parent_set)
    for extra in extras:
        violations.append(
            Violation(
                stage=_STAGE,
                message=(
                    f"network_hosts: child host {extra!r} not in parent's "
                    f"allowed set {sorted(parent_set)!r}"
                ),
            )
        )


def _check_superset(
    violations: list[Violation],
    field: str,
    child_items: list[BaseModel],
    parent_items: list[BaseModel],
) -> None:
    """Assert set(parent) ⊆ set(child). Child may add, may not remove."""
    child_keys = {m.model_dump_json() for m in child_items}
    parent_keys = {m.model_dump_json() for m in parent_items}
    missing = parent_keys - child_keys
    if missing:
        violations.append(
            Violation(
                stage=_STAGE,
                message=(
                    f"{field}: child is missing parent {field} "
                    f"{frozenset(missing)}"
                ),
            )
        )
