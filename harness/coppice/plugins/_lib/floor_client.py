# /// script
# requires-python = ">=3.10"
# dependencies = []
# ///
"""A small client for the coppice socket, for plugin policies.

Stdlib only. A policy reads its socket from COPPICE_SOCKET, its id from
COPPICE_PLUGIN, and its settings from COPPICE_PLUGIN_CONFIG, all of which
the server's runner sets. Every request is one JSON line. Every reply
names the request id. Event lines carry no id and wait in a queue until
the policy reads them.
"""

from __future__ import annotations

import json
import os
import socket
from collections import deque
from collections.abc import Iterator


class FloorError(Exception):
    """The server refused a request."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message


def config() -> dict:
    """The plugin's settings: the manifest's config with the operator's table over it."""
    raw = os.environ.get("COPPICE_PLUGIN_CONFIG", "") or "{}"
    try:
        value = json.loads(raw)
    except ValueError:
        return {}
    return value if isinstance(value, dict) else {}


class FloorClient:
    """One connection to the coppice socket."""

    def __init__(self, path: str | None = None, plugin: str | None = None) -> None:
        path = path or os.environ.get("COPPICE_SOCKET") or os.environ.get("COPPICE_SOCK", "")
        if not path:
            raise FloorError("no_socket", "COPPICE_SOCKET is not set.")
        self.plugin = plugin or os.environ.get("COPPICE_PLUGIN", "")
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(path)
        self.buf = b""
        self.events: deque[dict] = deque()
        self.seq = 0
        self.closed = False

    def close(self) -> None:
        self.sock.close()

    def _line(self, timeout: float | None) -> dict | None:
        """Read one line. None on a timeout. Raises EOFError when the server goes."""
        while b"\n" not in self.buf:
            self.sock.settimeout(timeout)
            try:
                chunk = self.sock.recv(65536)
            except TimeoutError:
                return None
            if not chunk:
                self.closed = True
                raise EOFError("the coppice server closed the connection")
            self.buf += chunk
        line, self.buf = self.buf.split(b"\n", 1)
        if not line.strip():
            return {}
        return json.loads(line)

    def call(self, cmd: str, **fields) -> dict:
        """Send one request and return its result. A refusal raises FloorError."""
        self.seq += 1
        rid = f"p{self.seq}"
        msg = {"id": rid, "cmd": cmd, **fields}
        self.sock.sendall(json.dumps(msg).encode() + b"\n")
        while True:
            line = self._line(None)
            if not line:
                continue
            if line.get("id") != rid:
                if "event" in line:
                    self.events.append(line)
                continue
            if not line.get("ok"):
                err = line.get("error") or {}
                raise FloorError(str(err.get("code", "")), str(err.get("message", "")))
            result = line.get("result")
            return result if isinstance(result, dict) else {}

    def hello(self) -> dict:
        """Name this connection as its plugin."""
        return self.call("hello", role="plugin", plugin=self.plugin)

    def subscribe(self, kinds: list[str]) -> dict:
        return self.call("events.subscribe", kinds=kinds, panes="*")

    def next_event(self, timeout: float | None = None) -> dict | None:
        """The next event, or None when timeout passes first."""
        if self.events:
            return self.events.popleft()
        while True:
            line = self._line(timeout)
            if line is None:
                return None
            if "event" in line:
                return line

    def iter_events(self) -> Iterator[dict]:
        while True:
            yield self.next_event()

    def note(self, text: str) -> None:
        """Leave a note on the floor. The server puts the plugin id in front."""
        self.call("floor.note", text=text)
