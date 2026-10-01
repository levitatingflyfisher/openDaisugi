#!/usr/bin/env python3
"""Stands in for a gate hook inside a pane, for the compare cases: from the
pane's own process it sends one pane.report_state that names a transcript,
prints the reply (ok, the refusal of its transcript, or "refused" and
the refusal of the whole report), then waits or turns into cat. The report
names the harness session fake-session, so pane.resume can resume it.

    report.py [--resume ID] PATH [--copy SRC [--cwd DIR]] [--link LINK DIR]
              [--when FILE] [--cat | --exit]

With --exit the process ends just after its report, so the pane ends and
can be resumed; a leading --resume ID, which pane.resume puts there, is
skipped.

In every argument {pane} is the pane's id, ':' as '-', so each pane names
its own new file. With --copy, SRC is first copied to PATH. With --when
it reports only once FILE exists, so a case says when. With --link it
first makes LINK a symlink to the directory DIR. With --cat the
process then echoes what it reads, as the fake foreman does, after turning
bracketed paste on. The transcripts it names are invented fixtures.
"""

import json
import os
import re
import shutil
import socket
import sys
import time


def main() -> None:
    pane = os.environ["COPPICE_PANE"]
    args = [a.replace("{pane}", pane.replace(":", "-")) for a in sys.argv[1:]]
    if args[:1] == ["--resume"]:
        # A pane.resume puts the harness's resume_args first.
        args = args[2:]
    path = args[0]
    if "--when" in args:
        when = args[args.index("--when") + 1]
        while not os.path.exists(when):
            time.sleep(0.05)
    if "--link" in args:
        i = args.index("--link")
        os.symlink(args[i + 2], args[i + 1])
    if "--copy" in args:
        src = args[args.index("--copy") + 1]
        os.makedirs(os.path.dirname(path), exist_ok=True)
        shutil.copyfile(src, path)
        # The fixture's first record names "/fake", padded with spaces: put
        # this pane's own directory there (or --cwd's), keeping each line's
        # length, so every message keeps its id.
        cwd = args[args.index("--cwd") + 1] if "--cwd" in args else os.getcwd()
        text = open(path, encoding="utf-8").read()
        m = re.search(r'"cwd":"/fake" *', text)
        if m:
            new = '"cwd":' + json.dumps(cwd)
            new += " " * (len(m.group(0)) - len(new))
            text = text[: m.start()] + new + text[m.end() :]
            open(path, "w", encoding="utf-8").write(text)
    ev = {
        "v": 1,
        "ts": 1,
        "session_id": "s",
        "harness_session_id": "fake-session",
        "harness": "claude-code",
        "pane": pane,
        "state": "idle",
        "source": "gate",
        "detail": "Stop",
        "transcript_path": path,
    }
    s = socket.socket(socket.AF_UNIX)
    s.connect(os.environ["COPPICE_SOCK"])
    s.sendall(
        (
            json.dumps({"id": "r", "cmd": "pane.report_state", "pane": pane, "event": ev}) + "\n"
        ).encode()
    )
    buf = b""
    while b"\n" not in buf:
        chunk = s.recv(65536)
        if not chunk:
            break
        buf += chunk
    r = json.loads(buf.split(b"\n")[0] or b"{}")
    if r.get("ok"):
        word = (r.get("result") or {}).get("transcript") or "ok"
    else:
        word = "refused " + (r.get("error") or {}).get("message", "no reply")
    print("REPORT " + word, flush=True)
    if "--exit" in args:
        time.sleep(0.5)
        return
    if "--cat" in args:
        sys.stdout.write("\033[?2004hforeman ready\r\n")
        sys.stdout.flush()
        os.execvp("cat", ["cat"])
    time.sleep(600)


main()
