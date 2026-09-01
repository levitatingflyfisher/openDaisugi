#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Notify through ntfy: post a message when a pane becomes blocked.

This is the reference notifier for plugin authors. The built-in Go
publisher does the same job from web.json. Set one of the two, not both,
or each block posts twice. This one reads only its own table,
[plugin.notify-ntfy] in coppice.toml: url, topic, and the optional
token_env and click_base. With no url or topic it has nothing to do, and
it finishes at once.
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
            "notify-ntfy: not set up. Set url and topic in [plugin.notify-ntfy] in coppice.toml.",
            file=sys.stderr,
        )
        return 0
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
                try:
                    post_ntfy(cfg, ev, label)
                except OSError as e:
                    # The URL carries the topic, which is the credential, so
                    # only the kind of failure is printed.
                    print(f"notify-ntfy: the post failed: {type(e).__name__}", file=sys.stderr)
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"notify-ntfy: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("notify-ntfy: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"notify-ntfy: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
