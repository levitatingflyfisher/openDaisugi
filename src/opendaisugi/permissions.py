"""Shared Permission algebra.

The canonical home for operations over ``Permission`` objects. Lives
outside ``distiller`` so the Gardener and any future modules can reuse
the intersection logic without pulling distillation's transitive deps.
"""

from __future__ import annotations

from opendaisugi.models import Permission


def intersect_permissions(perms: list[Permission]) -> Permission:
    """Return the tightest Permission covering all inputs.

    - Boolean flags: logical AND (only True if all are True).
    - List fields (shell_allowlist, file_read, file_write, network_hosts):
      set intersection, sorted for determinism.
    - Int ceilings (max_execution_time_s, max_output_size_mb): minimum,
      so the merged permission is no more permissive than the tightest input.

    Raises ValueError on empty input.
    """
    if not perms:
        raise ValueError("intersect_permissions requires at least one Permission")

    if len(perms) == 1:
        return perms[0].model_copy(deep=True)

    # Fields we know about on Permission. Mirror the model definition —
    # if Permission gains new fields this helper must be updated.
    bool_fields = ("shell", "network")
    list_fields = ("shell_allowlist", "file_read", "file_write", "network_hosts")
    int_min_fields = ("max_execution_time_s", "max_output_size_mb")

    merged: dict = {}
    for f in bool_fields:
        merged[f] = all(getattr(p, f, False) for p in perms)
    for f in list_fields:
        sets = [set(getattr(p, f, []) or []) for p in perms]
        merged[f] = sorted(set.intersection(*sets)) if sets else []
    for f in int_min_fields:
        values = [getattr(p, f) for p in perms if getattr(p, f, None) is not None]
        if values:
            merged[f] = min(values)

    return Permission(**merged)
