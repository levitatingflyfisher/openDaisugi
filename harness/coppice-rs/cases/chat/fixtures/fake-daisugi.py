#!/usr/bin/env python3
"""A stand-in for daisugi in the foreman's chat cases. It never calls a
model and never runs a worker.

- `agent-profiles bundle --fd 3`: writes $FAKE_BUNDLE_JSON, or else the file
  $FAKE_BUNDLE_FILE names, to descriptor 3, as the real one writes the operator's profile table.
- `tree root ENVELOPE --session S --data-dir D --root R [--tokens N]
  [--turns N]`: appends its argv to $FAKE_TREE_LOG and exits 0.
- `tree run ... -`: with $FAKE_TREE_START set, first appends "started TASK
  URL" to that file, URL being the base_url that the profile bundle on
  `--keys-fd` gives the run's profile. A line "URL<TAB>TEXT" in the file
  $FAKE_FAIL_FILE makes a run on that URL print TEXT on stderr and exit 1, after
  $FAKE_FAIL_DELAY seconds.
  With $FAKE_TREE_GRANDCHILD set it starts a `sleep` in a session of its
  own (as sprig runs under tree run) and kills it when it ends.
  Then it waits until the file $FAKE_TREE_GO/<task> exists (at
  most 120 s), so the case reads the turn's reply before the worker's
  report, then appends its argv and its stdin to $FAKE_TREE_LOG. A task
  "ran" (or "ran" and more characters) writes a session file
  R/sessions/s-ran.jsonl and answers status ran;
  "refuse" answers status refused with a reason (exit 1); "ask" answers
  status asked with an ask id (exit 3); anything else exits 2 with a line on
  stderr.

Anything else exits 2.
"""

import json
import os
import subprocess
import sys
import time


def log(text):
    with open(os.environ["FAKE_TREE_LOG"], "a", encoding="utf-8") as f:
        f.write(text + "\n")


def main():
    a = sys.argv[1:]
    if a == ["agent-profiles", "bundle", "--fd", "3"]:
        if os.environ.get("FAKE_BUNDLE_JSON"):
            data = os.environ["FAKE_BUNDLE_JSON"]
        else:
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
        url = None
        if "--keys-fd" in a:
            with os.fdopen(int(a[a.index("--keys-fd") + 1]), "rb") as kf:
                table = json.loads(kf.read() or b"{}")
            prof = next((x.split("=", 1)[1] for x in a if x.startswith("--profile=")), "")
            url = (table.get(prof) or {}).get("base_url")
        # $FAKE_TREE_GRANDCHILD set: a process in a session of its own, as
        # sprig is under tree run. A yield must stop it too (FM-R-107).
        if os.environ.get("FAKE_TREE_GRANDCHILD"):
            gc = subprocess.Popen(["sleep", "120"], start_new_session=True)
            import atexit

            atexit.register(gc.kill)
        start_log = os.environ.get("FAKE_TREE_START")
        if start_log:
            with open(start_log, "a", encoding="utf-8") as sl:
                sl.write("started " + req["task"] + " " + str(url) + "\n")
        fail_file = os.environ.get("FAKE_FAIL_FILE")
        if url and fail_file and os.path.exists(fail_file):
            with open(fail_file, encoding="utf-8") as ff:
                for row in ff:
                    furl, _, err = row.rstrip("\n").partition("\t")
                    if furl == url:
                        time.sleep(float(os.environ.get("FAKE_FAIL_DELAY", "0")))
                        print(err, file=sys.stderr)
                        return 1
        # The worker answers once the case has seen the turn's reply: it
        # waits for $FAKE_TREE_GO/<task> (at most 120 s).
        go = os.path.join(os.environ.get("FAKE_TREE_GO", "/nonexistent"), req["task"])
        for _ in range(1200):
            if os.path.exists(go):
                break
            time.sleep(0.1)
        log("run " + " ".join(a[2:]) + " " + json.dumps(req, sort_keys=True))
        root = a[a.index("--root") + 1]
        task = req["task"]
        if task.startswith("ran"):
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
