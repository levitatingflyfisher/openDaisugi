"""Promotion: one line makes a pane the foreman of the floor.

The line tells the pane to run ``coppice skill foreman``, which prints the
page the coppice binary carries. The pane needs no path to a file, so the
line works on any machine that has coppice. The Go floor sends the same
line: ``harness/coppice/internal/tui/prompt.go`` holds it as PromotionLine.

The line goes only to a pane that is idle after it drew something. A
harness that opens on a question of its own is blocked or busy, and an
Enter there would answer the question for the operator, so it gets nothing.
"""

from __future__ import annotations

import time

from opendaisugi.exceptions import OpenDaisugiError
from opendaisugi.floor.backend import PaneBackend, PaneRef

PROMOTION = "Run: coppice skill foreman. Follow it. You are the foreman of this floor. Say ready."

# The detail coppice gives a pane that has drawn nothing yet. Such a pane
# reads idle, but its harness has not started.
_NOT_STARTED = "no output yet"


class PromotionNotSent(OpenDaisugiError):
    """The pane did not go idle in time, so the promotion line was not sent."""


def promote(backend: PaneBackend, pane: PaneRef, *, timeout_s: float = 30.0) -> str:
    """Wait for pane to go idle, then send the promotion line with Enter,
    and return the line. Raise PromotionNotSent, having sent nothing, when
    the pane is not idle within timeout_s."""
    from opendaisugi.floor.registry import wait_for_state

    deadline = time.monotonic() + timeout_s
    while True:
        left = deadline - time.monotonic()
        ev = None
        if left > 0:
            ev = wait_for_state(backend, pane, until="idle", timeout_s=left, poll_s=min(0.5, left))
        if ev is not None and ev.detail != _NOT_STARTED:
            break
        if time.monotonic() >= deadline:
            raise PromotionNotSent(
                f"pane {pane.id} did not go idle in {timeout_s:g} s, so nothing was sent. "
                f"Answer it in the pane, then send: {PROMOTION}"
            )
        time.sleep(0.2)
    backend.send_text(pane, PROMOTION, enter=True)
    return PROMOTION
