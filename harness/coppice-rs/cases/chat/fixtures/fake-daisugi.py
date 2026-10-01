#!/usr/bin/env python3
"""A stand-in for daisugi in the foreman's chat cases. It never calls a
model and never runs a worker.

- `agent-profiles bundle --fd 3`: writes the file $FAKE_BUNDLE_FILE names to
  descriptor 3, as the real one writes the operator's profile table.
- `tree root ENVELOPE --session S --data-dir D --root R [--tokens N]
  [--turns N]`: appends its argv to $FAKE_TREE_LOG and exits 0.
- `tree run ... -`: waits until the file $FAKE_TREE_GO/<task> exists (at
  most 30 s), so the case reads the turn's reply before the worker's
  report, then appends its argv and its stdin to $FAKE_TREE_LOG. A task
  "ran" writes a session file R/sessions/s-ran.jsonl and answers status ran;
  "refuse" answers status refused with a reason (exit 1); "ask" answers
  status asked with an ask id (exit 3); anything else exits 2 with a line on
  stderr.

Anything else exits 2.
"""

import json
import os
import sys
import time


def log(text):
    with open(os.environ["FAKE_TREE_LOG"], "a", encoding="utf-8") as f:
        f.write(text + "\n")


def main():
    a = sys.argv[1:]
    if a == ["agent-profiles", "bundle", "--fd", "3"]:
        with open(os.environ["FAKE_BUNDLE_FILE"], encoding="utf-8") as f:
            data = f.read()
        with os.fdopen(3, "w", encoding="utf-8") as out:
            out.write(data)
        return 0
    if a[:2] == ["tree", "root"]:
        log("root " + " ".join(a[2:]))
        return 0
    if a[:2] == ["tree", "run"]:
        req = json.loads(sys.stdin.read())
        # The worker answers once the case has seen the turn's reply: it
        # waits for $FAKE_TREE_GO/<task> (at most 30 s).
        go = os.path.join(os.environ.get("FAKE_TREE_GO", "/nonexistent"), req["task"])
        for _ in range(300):
            if os.path.exists(go):
                break
            time.sleep(0.1)
        log("run " + " ".join(a[2:]) + " " + json.dumps(req, sort_keys=True))
        root = a[a.index("--root") + 1]
        task = req["task"]
        if task == "ran":
            os.makedirs(os.path.join(root, "sessions"), exist_ok=True)
            with open(os.path.join(root, "sessions", "s-ran.jsonl"), "w", encoding="utf-8") as f:
                f.write('{"type":"session","id":"s-ran"}\n{"type":"assistant","text":"all done"}\n')
            print(json.dumps({"status": "ran", "session": "s-ran", "answer": "all done"}))
            return 0
        if task == "refuse":
            print(
                json.dumps(
                    {"status": "refused", "session": "s-ref", "reasons": ["no-net", "no-push"]}
                )
            )
            return 1
        if task == "ask":
            print(
                json.dumps(
                    {
                        "status": "asked",
                        "session": "s-ask",
                        "ask_id": "a-1",
                        "reasons": ["needs a look"],
                    }
                )
            )
            return 3
        print("the tree run broke", file=sys.stderr)
        return 2
    print("fake daisugi: unknown command " + " ".join(a), file=sys.stderr)
    return 2


sys.exit(main())
