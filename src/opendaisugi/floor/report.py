"""report_state(): the one function every gate/hook/extension call site
uses to tell a listening floor (coppice or Herdr) about a state change.

Implemented in the layer (opendaisugi._state_report, stdlib only, no floor
import — spec-01's layer-purity rule) and re-exported here for floor-side
and extension callers that don't need the layer-internal validator too.
"""

from __future__ import annotations

from opendaisugi._state_report import report_state

__all__ = ["report_state"]
