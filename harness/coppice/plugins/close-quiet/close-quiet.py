#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Close quiet: close a pane that stays idle for idle_minutes.

The clock is the ts of the state events, moved on by the time that passed
since the last one arrived. An event with no ts, or one older than the
clock, reads as now. pane.close ends the pane's process and keeps
its worktree. A note says which pane closed and why.
"""

from __future__ import annotations

import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "_lib"))

from floor_client import FloorClient, FloorError, config  # noqa: E402

# CHECK_SECONDS is how often the policy looks again with no event.
CHECK_SECONDS = 60.0


def main() -> int:
    minutes = float(config().get("idle_minutes", 60))
    limit = minutes * 60
    try:
        client = FloorClient()
        client.hello()
        client.subscribe(["state"])
        idle_since: dict[str, float] = {}
        last_ts = 0.0
        seen_at = time.monotonic()
        while True:
            try:
                ev = client.next_event(CHECK_SECONDS)
                if ev is not None and ev.get("event") == "state" and ev.get("pane"):
                    pane, st = ev["pane"], ev.get("state")
                    clock = last_ts + (time.monotonic() - seen_at)
                    try:
                        ts = float(ev.get("ts"))
                    except (TypeError, ValueError):
                        ts = 0.0
                    # A ts that is missing or older than the clock reads as
                    # now, so a bad ts never makes a pane look long idle.
                    if ts <= 0 or ts < last_ts:
                        ts = clock
                    last_ts, seen_at = ts, time.monotonic()
                    if st == "idle":
                        idle_since.setdefault(pane, ts)
                    else:
                        idle_since.pop(pane, None)
                now = last_ts + (time.monotonic() - seen_at)
                for pane, since in sorted(idle_since.items()):
                    if now - since < limit:
                        continue
                    del idle_since[pane]
                    try:
                        client.call("pane.close", pane=pane)
                    except FloorError as e:
                        print(f"close-quiet: {e}", file=sys.stderr)
                        continue
                    client.note(
                        f"closed {pane} after {minutes:g} minutes idle. Its worktree stays."
                    )
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"close-quiet: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("close-quiet: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"close-quiet: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
