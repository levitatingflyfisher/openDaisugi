#!/usr/bin/env python3
"""Stands in for a claude foreman, for the chat compare cases. From the
pane's own process it writes an invented Claude transcript at PATH, with
one owner line and the reply "The fake foreman is ready.", draws a line,
and names the transcript in a pane.report_state that says idle. Then each
line typed into it is one turn: it reports working, records the line in
the transcript as the owner's, prints "LOGGED <line>", and reports idle.

    chat-foreman.py PATH [--hold FILE]

With --hold, a turn other than the page line records its line only once
FILE exists, and removes FILE, so a case says when each turn starts: a
line typed while the turn before it holds stays unrecorded, as a sentence
queued behind a long turn does. In PATH, {pane} is the pane's id, ':' as
'-'. The first record's working directory is padded with spaces to a
fixed width, so each message id is the same on every side. No model and
no network are involved.
"""

import json
import os
import socket
import sys
import time

PAGE = "Run: coppice skill foreman."
CWD_WIDTH = 300


def stamp() -> str:
    t = time.time()
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(t)) + ".%03dZ" % int((t % 1) * 1000)


def main() -> None:
    pane = os.environ["COPPICE_PANE"]
    args = sys.argv[1:]
    path = args[0].replace("{pane}", pane.replace(":", "-"))
    hold = args[args.index("--hold") + 1] if "--hold" in args else ""
    os.makedirs(os.path.dirname(path), exist_ok=True)
    n = 0

    def say(role: str, text: str, first: bool = False) -> None:
        nonlocal n
        n += 1
        if role == "user":
            head = '{"type":"user","uuid":"chat-u%d","timestamp":"%s"' % (n, stamp())
            if first:
                cwd = '"cwd":' + json.dumps(os.getcwd())
                head += "," + cwd + " " * max(0, CWD_WIDTH - len(cwd))
            line = head + ',"message":{"role":"user","content":%s}}' % json.dumps(text)
        else:
            line = (
                '{"type":"assistant","uuid":"chat-a%d","timestamp":"%s","message":{"id":"msg_chat_%d",'
                '"model":"claude-fake","role":"assistant","content":[{"type":"text","text":%s}]}}'
            ) % (n, stamp(), n, json.dumps(text))
        with open(path, "a", encoding="utf-8") as f:
            f.write(line + "\n")

    def report(state: str, detail: str) -> None:
        ev = {
            "v": 1,
            "ts": time.time(),
            "session_id": "s",
            "harness_session_id": "fake-session",
            "harness": "claude-code",
            "pane": pane,
            "state": state,
            "source": "gate",
            "detail": detail,
            "transcript_path": path,
        }
        s = socket.socket(socket.AF_UNIX)
        s.connect(os.environ["COPPICE_SOCK"])
        s.sendall(
            (
                json.dumps({"id": "r", "cmd": "pane.report_state", "pane": pane, "event": ev})
                + "\n"
            ).encode()
        )
        buf = b""
        while b"\n" not in buf:
            chunk = s.recv(65536)
            if not chunk:
                break
            buf += chunk
        s.close()

    say("user", "The fake foreman starts.", first=True)
    say("assistant", "The fake foreman is ready.")
    sys.stdout.write("foreman ready\r\n")
    sys.stdout.flush()
    time.sleep(0.2)
    report("idle", "Stop")
    while True:
        line = sys.stdin.readline()
        if not line:
            return
        line = line.rstrip("\r\n")
        if not line:
            continue
        report("working", "UserPromptSubmit")
        if hold and not line.startswith(PAGE):
            while not os.path.exists(hold):
                time.sleep(0.05)
            os.remove(hold)
        say("user", line)
        sys.stdout.write("LOGGED " + line + "\r\n")
        sys.stdout.flush()
        report("idle", "Stop")


main()
