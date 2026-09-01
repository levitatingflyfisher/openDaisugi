#!/usr/bin/env python3
# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""Merge on green: ask a done pane to merge once its PR checks pass.

When a pane's state becomes done, and the pane belongs to a task with a
worktree, and that worktree has an open pull request, this policy runs
gh pr checks there. When the checks pass it prompts the pane with
"checks are green, merge". While checks are pending it asks again every
poll_seconds, up to max_polls times. It never merges on its own and never
answers a gate ask. The pane's own gate decides what the merge may do.
"""

from __future__ import annotations

import json
import subprocess
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "_lib"))

from floor_client import FloorClient, FloorError, config  # noqa: E402

PROMPT = "checks are green, merge"
# gh pr checks exits 0 when every check passed and 8 while some are pending.
PENDING = 8


def worktree_of(client: FloorClient, pane: str) -> str:
    for task in client.call("task.list").get("tasks", []):
        if pane in (task.get("panes") or []) and task.get("worktree"):
            return task["worktree"]
    return ""


def open_pr(worktree: str) -> bool:
    try:
        out = subprocess.run(
            ["gh", "pr", "view", "--json", "state,number"],
            cwd=worktree,
            capture_output=True,
            text=True,
            timeout=60,
        )
    except (OSError, subprocess.TimeoutExpired):
        return False
    if out.returncode != 0:
        return False
    try:
        return json.loads(out.stdout).get("state") == "OPEN"
    except ValueError:
        return False


def checks(worktree: str) -> int:
    try:
        return subprocess.run(
            ["gh", "pr", "checks"], cwd=worktree, capture_output=True, timeout=120
        ).returncode
    except (OSError, subprocess.TimeoutExpired):
        return 1


def main() -> int:
    cfg = config()
    poll = float(cfg.get("poll_seconds", 60))
    max_polls = int(cfg.get("max_polls", 30))
    try:
        client = FloorClient()
        client.hello()
        client.subscribe(["state"])
        last: dict[str, str] = {}
        # pending maps a pane to its worktree, the polls left, and when to ask next.
        pending: dict[str, tuple[str, int, float]] = {}

        def check(pane: str, worktree: str, left: int) -> None:
            code = checks(worktree)
            if code == 0:
                client.call("agent.prompt", pane=pane, text=PROMPT)
                client.note(f"{pane}: checks are green. Asked it to merge.")
            elif code == PENDING and left > 1:
                pending[pane] = (worktree, left - 1, time.monotonic() + poll)
            elif code == PENDING:
                client.note(f"{pane}: checks are still pending. Stopped waiting.")
            else:
                client.note(f"{pane}: checks did not pass. Not asking it to merge.")

        while True:
            try:
                now = time.monotonic()
                for pane, (wt, left, due) in list(pending.items()):
                    if now >= due:
                        del pending[pane]
                        check(pane, wt, left)
                wait = None
                if pending:
                    wait = max(0.0, min(due for _, _, due in pending.values()) - time.monotonic())
                ev = client.next_event(wait)
                if ev is None or ev.get("event") != "state":
                    continue
                pane, st = ev.get("pane"), ev.get("state")
                if not pane:
                    continue
                was, last[pane] = last.get(pane), st
                if st != "done":
                    pending.pop(pane, None)
                    continue
                if was == "done" or pane in pending:
                    continue
                worktree = worktree_of(client, pane)
                if not worktree or not open_pr(worktree):
                    continue
                check(pane, worktree, max_polls)
            except FloorError as e:
                # One refused request, such as a prompt to a pane that went
                # blocked, is logged and the policy goes on.
                print(f"merge-on-green: {e}", file=sys.stderr)
    except EOFError:
        # A lost connection is a failure, so the runner starts it again.
        # The server drops a client that falls far behind on events.
        print("merge-on-green: the server closed the connection.", file=sys.stderr)
        return 1
    except FloorError as e:
        # A refusal at the start, such as a plugin that is not enabled.
        print(f"merge-on-green: {e}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
