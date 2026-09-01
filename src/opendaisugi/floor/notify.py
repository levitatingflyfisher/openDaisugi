"""Run the operator's own command when a pane's merged state turns blocked.

Master spec section 5.6: push goes through a self-hosted ntfy or not at all.
This module holds to that rule. It is not a notification service and not a
relay this project picked. It runs one command the operator configured, with
a small JSON payload on stdin, and nothing else. If the operator sets no
command, nothing runs and nothing leaves the box.

The caller must pass the merged state, not a raw per-source event. merge()
and effective_state() in opendaisugi.floor.events compute that state. A
blocked event whose ask deadline has passed reads as working once
effective_state() has run on it. Notifier never treats that as blocked, so a
stale hold never notifies.

Example config (~/.opendaisugi/config.yaml)::

    floor:
      notify_cmd: ntfy publish --title "agent blocked" https://ntfy.example.net/daisugi

The payload carries only the pane id, the harness, the state and the detail.
The event's ask never reaches the command. An ask can carry a tool name and a
summary of what it wants to run. That is not data to hand an arbitrary
command the operator may have pointed at a third party.

A failing or slow command never blocks the floor. The floor's own state stays
correct either way. Only the side effect of telling the operator's phone can
fail open: a failed attempt is logged once and skipped, and notify() returns
False. It never raises.
"""

from __future__ import annotations

import json
import logging
import os
import shlex
import signal
import subprocess
import time
from collections.abc import Callable
from contextlib import suppress

from opendaisugi.floor.events import PaneStateEvent

EXAMPLE_CMD = 'ntfy publish --title "agent blocked" https://ntfy.example.net/daisugi'

logger = logging.getLogger(__name__)


class Notifier:
    """Runs one configured command on a blocked state, debounced per pane."""

    def __init__(
        self,
        cmd: str | None,
        *,
        debounce_s: float = 5.0,
        timeout_s: float = 5.0,
        clock: Callable[[], float] = time.monotonic,
    ) -> None:
        self.cmd = (cmd or "").strip() or None
        self.debounce_s = debounce_s
        self.timeout_s = timeout_s
        self._clock = clock
        self._last: dict[str, float] = {}

    def notify(self, ev: PaneStateEvent) -> bool:
        """Run the command for ev. Return True when it ran.

        ev must already carry the merged, effective state. Only state ==
        "blocked" runs the command. A second event for the same pane inside
        debounce_s is skipped. Never raises.
        """
        if self.cmd is None:
            return False
        if ev.state != "blocked":
            return False
        key = ev.pane or ev.session_id
        now = self._clock()
        last = self._last.get(key)
        if last is not None and now - last < self.debounce_s:
            return False
        try:
            argv = shlex.split(self.cmd)
        except ValueError:
            logger.warning("notify_cmd could not be parsed: %r", self.cmd)
            return False
        if not argv:
            return False
        self._last[key] = now
        return self._run(argv, ev)

    def _run(self, argv: list[str], ev: PaneStateEvent) -> bool:
        payload = json.dumps(
            {"pane": ev.pane, "harness": ev.harness, "state": ev.state, "detail": ev.detail}
        )
        try:
            proc = subprocess.Popen(
                argv,
                stdin=subprocess.PIPE,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                text=True,
                start_new_session=True,
            )
        except OSError as exc:
            logger.warning("notify_cmd %r could not start: %s", self.cmd, exc)
            return False
        try:
            proc.communicate(input=payload, timeout=self.timeout_s)
        except subprocess.TimeoutExpired:
            self._kill(proc)
            logger.warning("notify_cmd %r timed out after %.1fs", self.cmd, self.timeout_s)
            return False
        except (OSError, subprocess.SubprocessError) as exc:
            self._kill(proc)
            logger.warning("notify_cmd %r failed: %s", self.cmd, exc)
            return False
        if proc.returncode != 0:
            logger.warning("notify_cmd %r exited %d", self.cmd, proc.returncode)
        return True

    def _kill(self, proc: subprocess.Popen) -> None:
        """Kill the whole process group started for proc.

        stdout and stderr are DEVNULL, not pipes, so the wait below cannot
        deadlock on a full pipe buffer.
        """
        with suppress(OSError, ProcessLookupError):
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        with suppress(subprocess.TimeoutExpired):
            proc.wait(timeout=1.0)
