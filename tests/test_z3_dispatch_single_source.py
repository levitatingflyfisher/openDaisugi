"""v0.27.0 fixup — z3_checks robotics dispatch must derive from the single
RECOGNIZED_OPAQUE_TYPES constant. Otherwise a 5th recognized type added to
_invariant_types.py gets the strict-reject carve-out for free but has no handler
here, silently leaving that invariant unchecked.
"""
from __future__ import annotations

from opendaisugi._invariant_types import RECOGNIZED_OPAQUE_TYPES
from opendaisugi.z3_checks import _INVARIANT_HANDLERS


def test_dispatch_set_equals_recognized_opaque_types():
    assert set(_INVARIANT_HANDLERS) == RECOGNIZED_OPAQUE_TYPES
