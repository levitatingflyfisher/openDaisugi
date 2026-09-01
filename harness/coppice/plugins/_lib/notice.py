# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""The words a notifier sends, and the one ntfy post they share.

The words match the built-in Go publisher: the title names the pane, the
body names what the ask wants and what the gate said.
"""

from __future__ import annotations

import os
import urllib.request

from floor_client import FloorClient, FloorError


def ascii_header(s: str, limit: int) -> str:
    """Printable ASCII on one line, cut to limit with three dots."""
    out = []
    last_space = False
    for ch in s:
        if ch < " " or ch > "~":
            ch = " "
        if ch == " ":
            if last_space:
                continue
            last_space = True
        else:
            last_space = False
        out.append(ch)
    text = "".join(out).strip()
    if len(text) <= limit:
        return text
    if limit <= 3:
        return text[: max(limit, 0)]
    return text[: limit - 3].strip() + "..."


def gate_says(gate: dict | None) -> str:
    if gate and gate.get("verdict") == "allow":
        return "Gate says yes."
    if gate and gate.get("rule") is not None:
        return f"Gate says no, rule {gate['rule']}."
    return "Gate says no."


def title(label: str) -> str:
    return f"{label} needs you."


def body(ev: dict) -> str:
    ask = ev.get("ask") or {}
    if ask.get("summary"):
        return f"Wants {ask['summary']}. {gate_says(ask.get('gate'))}"
    if ev.get("detail"):
        return ev["detail"]
    return "blocked, and the gate gave no detail"


def label_of(client: FloorClient, pane: str) -> str:
    """The pane's label, or its id when it has none or the list is refused."""
    try:
        rows = client.call("pane.list").get("panes", [])
    except FloorError:
        return pane
    for row in rows:
        if row.get("id") == pane:
            return row.get("label") or pane
    return pane


class BlockedEdges:
    """Tells the moment a pane becomes blocked from a repeat of it.

    A blocked event that carries held waits on a foreman first, so it counts
    as not blocked. The edge comes when the hold ends and the same ask goes
    out with no held.
    """

    def __init__(self) -> None:
        self.last: dict[str, str] = {}

    def is_new(self, ev: dict) -> bool:
        pane, st = ev.get("pane"), ev.get("state")
        if not pane:
            return False
        if st == "blocked" and ev.get("held"):
            st = "held"
        was, self.last[pane] = self.last.get(pane), st
        return st == "blocked" and was != "blocked"


def post_ntfy(cfg: dict, ev: dict, label: str, actions: str = "") -> None:
    """Post one message to the ntfy topic cfg names."""
    base = str(cfg.get("url", "")).rstrip("/")
    url = f"{base}/{cfg['topic']}"
    req = urllib.request.Request(url, data=ascii_header(body(ev), 500).encode(), method="POST")
    req.add_header("Title", ascii_header(title(label), 100))
    click_base = str(cfg.get("click_base", "")).rstrip("/")
    if click_base:
        req.add_header("Click", ascii_header(f"{click_base}/#/pane/{ev['pane']}", 2000))
    if actions:
        req.add_header("Actions", ascii_header(actions, 4000))
    token_env = cfg.get("token_env", "")
    if token_env and os.environ.get(token_env):
        req.add_header("Authorization", "Bearer " + os.environ[token_env])
    with urllib.request.urlopen(req, timeout=10) as resp:
        resp.read()
