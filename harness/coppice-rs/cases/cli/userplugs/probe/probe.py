"""A policy for the compare cases: it runs verbs as a plugin and notes
each answer, then finishes."""

import json
import os
import socket

s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
s.connect(os.environ["COPPICE_SOCKET"])
f = s.makefile("rwb")
n = 0


def call(cmd, **params):
    global n
    n += 1
    f.write((json.dumps({"id": str(n), "cmd": cmd, **params}) + "\n").encode())
    f.flush()
    while True:
        line = f.readline()
        if not line:
            raise SystemExit(3)
        r = json.loads(line)
        if r.get("id") == str(n):
            return r


def note(text):
    call("floor.note", text=text)


def said(r):
    if r.get("ok"):
        return "ok " + json.dumps(r.get("result", {}), sort_keys=True)[:60]
    return r["error"]["code"] + ": " + r["error"]["message"]


env = os.environ
note("id " + env.get("COPPICE_PLUGIN", ""))
note("config " + env.get("COPPICE_PLUGIN_CONFIG", ""))
note("sock " + str(env.get("COPPICE_SOCK") == env.get("COPPICE_SOCKET")))
note("pane " + repr(env.get("COPPICE_PANE")))
note("cwd " + os.path.basename(os.getcwd()))
event = {
    "v": 1,
    "ts": 1.0,
    "session_id": "s",
    "harness": "x",
    "pane": "w1:p1",
    "state": "idle",
    "source": "gate",
    "detail": "",
}
for cmd, params in [
    ("pane.list", {}),
    ("task.list", {}),
    ("server.status", {}),
    ("agent.allow", {"pane": "w1:p1", "ask": "a"}),
    ("agent.deny", {"pane": "w1:p1", "ask": "a"}),
    ("pane.report_state", {"pane": "w1:p1", "event": event}),
    ("pane.report_child", {"pane": "w1:p1"}),
    ("events.subscribe", {"kinds": ["state"]}),
    ("events.subscribe", {"kinds": ["note"]}),
    ("events.subscribe", {}),
    ("events.pause", {}),
    ("hello", {"role": "plugin", "plugin": "other"}),
    ("hello", {"role": "plugin", "plugin": "probe"}),
    ("hello", {"role": "pane", "plugin": "probe"}),
]:
    note(cmd + " " + said(call(cmd, **params)))
note("done")
