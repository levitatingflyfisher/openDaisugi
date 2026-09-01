#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Speak asks: say the label and the ask when a pane becomes blocked.

It posts {"text": ...} as JSON to speak_url from [plugin.notify-voice] in
coppice.toml. The voice bridge has no speak endpoint of its own, so
speak_url points at whatever local speaker the operator runs. With no
speak_url the words go to the floor as a note, which says voice is not
set up.
"""

from __future__ import annotations

import json
import sys
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "_lib"))

from floor_client import FloorClient, FloorError, config  # noqa: E402
from notice import BlockedEdges, label_of, title  # noqa: E402

NOT_SET_UP = "Voice is not set up. Set speak_url in [plugin.notify-voice] in coppice.toml."


def words(ev: dict, label: str) -> str:
    ask = ev.get("ask") or {}
    text = title(label)
    if ask.get("summary"):
        text += f" Wants {ask['summary']}."
    return text


def speak(url: str, text: str) -> None:
    req = urllib.request.Request(url, data=json.dumps({"text": text}).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(req, timeout=10) as resp:
        resp.read()


def main() -> int:
    url = str(config().get("speak_url", ""))
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
                text = words(ev, label_of(client, ev["pane"]))
                if not url:
                    client.note(f"{text} {NOT_SET_UP}")
                    continue
                try:
                    speak(url, text)
                except OSError as e:
                    print(
                        f"notify-voice: the speak call failed: {type(e).__name__}", file=sys.stderr
                    )
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"notify-voice: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("notify-voice: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"notify-voice: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
