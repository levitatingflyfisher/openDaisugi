#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Lock screen card: an ntfy card with a Look button.

The card opens the pane on the floor page, where the operator answers
with their own token. It carries no Deny button. A Deny button needs a
token that can deny one ask and nothing else, and only the server mints
those. The built-in publisher, set in web.json, is the one that carries
Deny. Settings are [plugin.notify-lockscreen] in coppice.toml: url, topic,
click_base, and the optional token_env.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "_lib"))

from floor_client import FloorClient, FloorError, config  # noqa: E402
from notice import BlockedEdges, label_of, post_ntfy  # noqa: E402


def main() -> int:
    cfg = config()
    if not cfg.get("url") or not cfg.get("topic"):
        print(
            "notify-lockscreen: not set up. Set url and topic in [plugin.notify-lockscreen].",
            file=sys.stderr,
        )
        return 0
    click_base = str(cfg.get("click_base", "")).rstrip("/")
    try:
        client = FloorClient()
        client.hello()
        client.subscribe(["state"])
        edges = BlockedEdges()
        while True:
            try:
                ev = client.next_event()
                if ev is None or ev.get("event") != "state" or not edges.is_new(ev):
                    continue
                label = label_of(client, ev["pane"])
                actions = f"view, Look, {click_base}/#/pane/{ev['pane']}" if click_base else ""
                try:
                    post_ntfy(cfg, ev, label, actions)
                except OSError as e:
                    print(
                        f"notify-lockscreen: the post failed: {type(e).__name__}", file=sys.stderr
                    )
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"notify-lockscreen: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("notify-lockscreen: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"notify-lockscreen: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
