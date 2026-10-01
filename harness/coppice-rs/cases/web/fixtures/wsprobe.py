"""A websocket client a pane runs for the web cases. It opens /ws with
the token it is given, asks hello and then agent.allow of an ask that
does not exist, and writes each reply's ok and error code to the file it
is given, then sleeps."""

import base64
import json
import os
import socket
import struct
import time


def frame(text: str) -> bytes:
    data = text.encode()
    key = os.urandom(4)
    head = bytes([0x81, 0x80 | len(data)])
    return head + key + bytes(b ^ key[i % 4] for i, b in enumerate(data))


def read_frame(f) -> bytes:
    b = f.read(2)
    n = b[1] & 0x7F
    if n == 126:
        n = struct.unpack(">H", f.read(2))[0]
    elif n == 127:
        n = struct.unpack(">Q", f.read(8))[0]
    return f.read(n)


def main(addr: str, token: str, out_path: str) -> None:
    host, _, port = addr.rpartition(":")
    s = socket.create_connection((host, int(port)))
    key = base64.b64encode(os.urandom(16)).decode()
    s.sendall(
        (
            f"GET /ws HTTP/1.1\r\nHost: {addr}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
            f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n"
            f"Sec-WebSocket-Protocol: daisugi.v1, daisugi.bearer.{token}\r\n\r\n"
        ).encode()
    )
    f = s.makefile("rb")
    out = [f.readline().decode().strip()]
    while f.readline() not in (b"\r\n", b""):
        pass
    for req in (
        {"id": "1", "cmd": "hello"},
        {"id": "2", "cmd": "agent.allow", "pane": "w1:p1", "tool_use_id": "nope"},
    ):
        s.sendall(frame(json.dumps(req)))
        while True:
            msg = json.loads(read_frame(f))
            if "id" in msg:
                break
        err = (msg.get("error") or {}).get("code", "")
        role = (msg.get("result") or {}).get("role", "")
        out.append(f"reply {msg['id']} {msg['ok']} {err} {role}")
    with open(out_path, "w", encoding="utf-8") as o:
        o.write("\n".join(out) + "\n")
    time.sleep(600)
