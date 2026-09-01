"""The floor's Python contracts (master spec §3): PaneStateEvent,
PaneBackend, and the value types every later sub-project (coppice, the
Herdr/tmux backends, the pi/OpenCode adapters) builds on.

Not a layer module — the layer's gate.py and hook.py may not import this
package (spec-01). The reverse (this package depending on opendaisugi's
layer modules, e.g. opendaisugi._state_report) is fine.
"""

from __future__ import annotations

from opendaisugi.floor.backend import Frame, PaneBackend, PaneInfo, PaneRef, TaskInfo
from opendaisugi.floor.events import (
    ERROR_CODES,
    SOURCES,
    STATES,
    Ask,
    PaneStateEvent,
    effective_state,
    merge,
)

__all__ = [
    "ERROR_CODES",
    "SOURCES",
    "STATES",
    "Ask",
    "Frame",
    "PaneBackend",
    "PaneInfo",
    "PaneRef",
    "PaneStateEvent",
    "TaskInfo",
    "effective_state",
    "merge",
]
