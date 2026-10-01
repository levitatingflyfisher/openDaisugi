#!/usr/bin/env python3
"""A stand-in for `opencode serve`, with no model.

It serves the part of OpenCode's HTTP API the coppice adapter calls, on
127.0.0.1 and the port the kernel picks, with HTTP Basic auth from
OPENCODE_SERVER_USERNAME and OPENCODE_SERVER_PASSWORD, and prints the
listen line the real server prints. Every request it takes is appended to
COPPICE_OPENCODE_ECHO when that is set.

Sessions are named ses_fake1, ses_fake2, ... in order. A session id that
starts with ses_keep exists too, so a resume of it succeeds; any other id
the server did not make does not exist.

A prompt plays one short turn on the event stream, by its text:

- "replay": the captured stream in session_lifecycle.sse (next to this
  file), with its session id made this session's.
- "perm": a permission prompt, per_1, for two bash patterns. A reply of
  once finishes the turn; a reject ends it idle.
- "question": a question, que_1. A reply finishes the turn with the
  answer; a reject ends it idle.
- "fail": the prompt is refused with 500.
- anything else: busy, the text "you said: TEXT", one tool call reported
  twice, then idle.
"""

from __future__ import annotations

import base64
import itertools
import json
import os
import queue
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

HERE = Path(__file__).resolve().parent
CAPTURE = HERE / "session_lifecycle.sse"
CAPTURED_SESSION = "ses_f3107821dffeLaGMxGh2BNticC"

USER = os.environ.get("OPENCODE_SERVER_USERNAME", "opencode")
PASSWORD = os.environ.get("OPENCODE_SERVER_PASSWORD", "")
AUTH = "Basic " + base64.b64encode(f"{USER}:{PASSWORD}".encode()).decode()
ECHO = os.environ.get("COPPICE_OPENCODE_ECHO", "")

counter = itertools.count(1)
sessions: set[str] = set()
streams: list[queue.Queue[str | None]] = []
lock = threading.Lock()


def publish(ev: dict) -> None:
    """Sends one bus event to every open event stream."""
    line = "data: " + json.dumps(ev, separators=(",", ":"))
    with lock:
        for q in streams:
            q.put(line)


def status(sid: str, kind: str) -> None:
    publish({"type": "session.status", "properties": {"sessionID": sid, "status": {"type": kind}}})


def text(sid: str, words: str) -> None:
    publish(
        {
            "type": "message.part.updated",
            "properties": {
                "sessionID": sid,
                "part": {"type": "text", "text": words, "time": {"start": 1, "end": 2}},
            },
        }
    )


def turn(sid: str, words: str) -> None:
    """The turn a prompt with this text plays."""
    if words == "replay":
        for raw in CAPTURE.read_text(encoding="utf-8").splitlines():
            if raw.startswith("data:"):
                with lock:
                    for q in streams:
                        q.put(raw.replace(CAPTURED_SESSION, sid))
        return
    status(sid, "busy")
    if words == "perm":
        publish(
            {
                "type": "permission.asked",
                "properties": {
                    "id": "per_1",
                    "sessionID": sid,
                    "permission": "bash",
                    "patterns": ["rm -rf build", "ls"],
                },
            }
        )
        return
    if words == "question":
        publish(
            {
                "type": "question.asked",
                "properties": {
                    "id": "que_1",
                    "sessionID": sid,
                    "questions": [{"question": "Which branch?"}],
                },
            }
        )
        return
    text(sid, f"you said: {words}")
    part = {
        "type": "tool",
        "callID": "call_1",
        "tool": "bash",
        "state": {"status": "running", "input": {"command": "ls", "z": 1}},
    }
    for _ in range(2):
        publish({"type": "message.part.updated", "properties": {"sessionID": sid, "part": part}})
    status(sid, "idle")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.0"

    def log_message(self, format: str, *args: object) -> None:  # noqa: A002
        pass

    def reply(self, code: int, body: object = None) -> None:
        data = b"" if body is None else json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def body(self) -> dict:
        n = int(self.headers.get("Content-Length") or 0)
        raw = self.rfile.read(n) if n else b""
        if ECHO:
            with lock, open(ECHO, "a", encoding="utf-8") as f:
                f.write(f"{self.command} {self.path} {raw.decode()}\n")
        return json.loads(raw) if raw else {}

    def authed(self) -> bool:
        if self.headers.get("Authorization") == AUTH:
            return True
        self.send_response(401)
        self.send_header("WWW-Authenticate", 'Basic realm="Secure Area"')
        self.send_header("Content-Length", "0")
        self.end_headers()
        return False

    def do_GET(self) -> None:  # noqa: N802
        if not self.authed():
            return
        if self.path == "/global/health":
            self.reply(200, {"healthy": True, "version": "fake"})
        elif self.path == "/event":
            self.stream()
        elif self.path.startswith("/session/"):
            sid = self.path.removeprefix("/session/")
            known = sid in sessions or sid.startswith("ses_keep")
            self.reply(200 if known else 404, {"id": sid} if known else {"name": "NotFound"})
        else:
            self.reply(404, {"name": "NotFound"})

    def stream(self) -> None:
        q: queue.Queue[str | None] = queue.Queue()
        with lock:
            streams.append(q)
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        connected = {"type": "server.connected", "properties": {}}
        self.wfile.write(b"data: " + json.dumps(connected).encode() + b"\n\n")
        self.wfile.flush()
        while True:
            line = q.get()
            if line is None:
                return
            try:
                self.wfile.write(line.encode() + b"\n\n")
                self.wfile.flush()
            except OSError:
                return

    def do_POST(self) -> None:  # noqa: N802
        if not self.authed():
            return
        body = self.body()
        parts = self.path.strip("/").split("/")
        if parts == ["session"]:
            sid = f"ses_fake{next(counter)}"
            sessions.add(sid)
            self.reply(200, {"id": sid, "title": body.get("title", "")})
        elif len(parts) == 3 and parts[0] == "session" and parts[2] == "prompt_async":
            words = "".join(p.get("text", "") for p in body.get("parts", []))
            if words == "fail":
                self.send_response(500)
                self.send_header("Content-Length", "4")
                self.end_headers()
                self.wfile.write(b"boom")
                return
            self.send_response(204)
            self.end_headers()
            threading.Thread(target=turn, args=(parts[1], words), daemon=True).start()
        elif len(parts) == 3 and parts[0] == "permission" and parts[2] == "reply":
            self.reply(200, True)
            sid = next(iter(sorted(sessions)), "")
            publish(
                {
                    "type": "permission.replied",
                    "properties": {"sessionID": sid, "requestID": parts[1], "reply": body["reply"]},
                }
            )
            if body["reply"] == "once":
                text(sid, "permission granted")
            status(sid, "idle")
        elif len(parts) == 3 and parts[0] == "question" and parts[2] in ("reply", "reject"):
            self.reply(200, True)
            sid = next(iter(sorted(sessions)), "")
            kind = "question.replied" if parts[2] == "reply" else "question.rejected"
            publish({"type": kind, "properties": {"sessionID": sid, "requestID": parts[1]}})
            if parts[2] == "reply":
                text(sid, "answer: " + body["answers"][0][0])
            status(sid, "idle")
        else:
            self.reply(404, {"name": "NotFound"})


def main() -> None:
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    srv.daemon_threads = True
    port = srv.server_address[1]
    print(f"opencode server listening on http://127.0.0.1:{port}", flush=True)
    srv.serve_forever()


if __name__ == "__main__":
    sys.exit(main())
