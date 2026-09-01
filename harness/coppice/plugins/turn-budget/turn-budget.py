#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Turn budget: pause a pane that runs past budget turns.

A turn is one move into working. Past budget turns the policy sends
ctrl-c to the pane and leaves a note. The count starts again, so the next
budget turns run once the operator prompts the pane to continue.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "_lib"))

from floor_client import FloorClient, FloorError, config  # noqa: E402


def main() -> int:
    budget = int(config().get("budget", 40))
    try:
        client = FloorClient()
        client.hello()
        client.subscribe(["state"])
        last: dict[str, str] = {}
        turns: dict[str, int] = {}
        while True:
            try:
                ev = client.next_event()
                if ev is None or ev.get("event") != "state" or not ev.get("pane"):
                    continue
                pane, st = ev["pane"], ev.get("state")
                was, last[pane] = last.get(pane), st
                if st != "working" or was == "working":
                    continue
                turns[pane] = turns.get(pane, 0) + 1
                if turns[pane] <= budget:
                    continue
                turns[pane] = 0
                try:
                    client.call("pane.send_keys", pane=pane, keys=["ctrl+c"])
                except FloorError as e:
                    print(f"turn-budget: {e}", file=sys.stderr)
                    continue
                client.note(f"{pane} paused: past {budget} turns. Prompt it to continue.")
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"turn-budget: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("turn-budget: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"turn-budget: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
