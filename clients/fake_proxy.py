"""A fake HTTP proxy on 127.0.0.1 for the proxy cases.

It records what each client sends it and never connects to the host a
client names. For every request it reads:

- the request line (a CONNECT line, or an absolute-form request line);
- for a CONNECT, every header, sorted; for any other request, only `host`
  and `proxy-authorization`, since the fake upstream records the rest;
- for a CONNECT it answered 200, what the client sent next: `tls` and the
  server name (SNI) of a TLS ClientHello, or the first line of anything
  else.

A byte stream that is not HTTP (a TLS ClientHello sent to a proxy URL of
the https scheme) is recorded as `{NOT HTTP}`, with the SNI when it is a
ClientHello.

Modes:

- `forward` (the default): an absolute-form request goes on in origin
  form to the loopback port its URL names, or to the fake upstream when
  its host is not a loopback address, and the answer comes back as sent. A CONNECT is answered 200 and the tunnel is read but not forwarded,
  so a TLS handshake through it fails on every side.
- `refuse`: a CONNECT is answered 403 Forbidden and closed. Other
  requests are forwarded as in `forward`.
- `deny`: every request is answered 407 Proxy Authentication Required.
"""

from __future__ import annotations

import socket
import threading
from typing import Any

MAX_HEAD = 65536


def _read_head(sock: socket.socket, buf: bytearray) -> bytes | None:
    """The bytes up to and including the blank line, or None at EOF or on
    a stream that is not HTTP. Bytes after the head stay in buf."""
    while True:
        i = buf.find(b"\r\n\r\n")
        if i >= 0:
            head = bytes(buf[: i + 4])
            del buf[: i + 4]
            return head
        if buf and not buf[:1].isalpha():
            return None
        if len(buf) > MAX_HEAD:
            return None
        try:
            chunk = sock.recv(65536)
        except OSError:
            return None
        if not chunk:
            return None
        buf += chunk


def client_hello_sni(data: bytes) -> str | None:
    """The server name in a TLS ClientHello, "" when it has none, or None
    when data is not a ClientHello."""
    try:
        if data[0] != 0x16 or data[5] != 0x01:
            return None
        p = 9 + 2 + 32  # record header, handshake header, version, random
        p += 1 + data[p]  # session id
        p += 2 + int.from_bytes(data[p : p + 2], "big")  # cipher suites
        p += 1 + data[p]  # compression methods
        end = p + 2 + int.from_bytes(data[p : p + 2], "big")
        p += 2
        while p + 4 <= end:
            kind = int.from_bytes(data[p : p + 2], "big")
            size = int.from_bytes(data[p + 2 : p + 4], "big")
            body = data[p + 4 : p + 4 + size]
            if kind == 0:
                # server_name_list: length, then (type, length, name)
                n = int.from_bytes(body[3:5], "big")
                return body[5 : 5 + n].decode("ascii", "replace")
            p += 4 + size
        return ""
    except IndexError:
        return None


def _recv_some(sock: socket.socket, buf: bytearray, want: int, limit: float = 5.0) -> None:
    sock.settimeout(limit)
    try:
        while len(buf) < want:
            chunk = sock.recv(65536)
            if not chunk:
                return
            buf += chunk
    except OSError:
        return


def _what_followed(sock: socket.socket, buf: bytearray) -> str:
    """What a client sent into a tunnel: `tls <sni>` for a ClientHello,
    else its first line."""
    _recv_some(sock, buf, 5)
    if buf[:1] == b"\x16" and len(buf) >= 5:
        _recv_some(sock, buf, 5 + int.from_bytes(buf[3:5], "big"))
        sni = client_hello_sni(bytes(buf))
        return "tls" if sni is None else f"tls {sni}" if sni else "tls"
    if not buf:
        return "{NOTHING}"
    return bytes(buf).split(b"\r\n", 1)[0].decode("latin-1")


class FakeProxy:
    def __init__(
        self, log: list[dict[str, Any]], upstream_port: int | None = None, mode: str = "forward"
    ):
        self.log = log
        self.upstream_port = upstream_port
        self.mode = mode
        self.lock = threading.Lock()
        self.srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
        self.srv.bind(("127.0.0.1", 0))
        self.srv.listen(64)
        self.port = self.srv.getsockname()[1]
        self.stop = False
        self.thread = threading.Thread(target=self._serve, daemon=True)

    def __enter__(self) -> FakeProxy:
        self.thread.start()
        return self

    def __exit__(self, *a: Any) -> None:
        self.stop = True
        try:
            socket.create_connection(("127.0.0.1", self.port), timeout=1).close()
        except OSError:
            pass
        self.srv.close()

    def _note(self, entry: dict[str, Any]) -> None:
        with self.lock:
            self.log.append(entry)

    def _serve(self) -> None:
        while not self.stop:
            try:
                conn, _ = self.srv.accept()
            except OSError:
                return
            if self.stop:
                conn.close()
                return
            threading.Thread(target=self._handle, args=(conn,), daemon=True).start()

    def _handle(self, conn: socket.socket) -> None:
        conn.settimeout(30)
        buf = bytearray()
        try:
            while True:
                head = _read_head(conn, buf)
                if head is None:
                    if buf:
                        _recv_some(conn, buf, 5)
                        _recv_some(
                            conn, buf, 5 + int.from_bytes(buf[3:5], "big") if len(buf) >= 5 else 5
                        )
                        sni = client_hello_sni(bytes(buf))
                        entry: dict[str, Any] = {"line": "{NOT HTTP}"}
                        if sni is not None:
                            entry["tls"] = sni
                        self._note(entry)
                    return
                if not self._one(conn, head, buf):
                    return
        finally:
            try:
                conn.close()
            except OSError:
                pass

    def _one(self, conn: socket.socket, head: bytes, buf: bytearray) -> bool:
        """Serve one request; False when the connection is done."""
        lines = head.decode("latin-1").split("\r\n")
        line = lines[0]
        headers: list[tuple[str, str]] = []
        for ln in lines[1:]:
            if not ln:
                continue
            k, _, v = ln.partition(":")
            headers.append((k.strip(), v.strip()))
        method = line.split(" ", 1)[0]
        entry: dict[str, Any] = {"line": line}
        if method == "CONNECT":
            entry["headers"] = sorted([k.lower(), v] for k, v in headers)
        else:
            entry["headers"] = sorted(
                [k.lower(), v] for k, v in headers if k.lower() in ("host", "proxy-authorization")
            )
        if self.mode == "deny":
            self._note(entry)
            # Read the body first: closing on unread bytes resets the
            # connection, and a client may lose the answer to the reset.
            length = next((int(v) for k, v in headers if k.lower() == "content-length"), 0)
            _recv_some(conn, buf, length, 30)
            conn.sendall(
                b"HTTP/1.1 407 Proxy Authentication Required\r\ncontent-length: 0\r\n"
                b"connection: close\r\n\r\n"
            )
            return False
        if method == "CONNECT":
            if self.mode == "refuse":
                self._note(entry)
                conn.sendall(
                    b"HTTP/1.1 403 Forbidden\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
                )
                return False
            conn.sendall(b"HTTP/1.1 200 Connection established\r\n\r\n")
            entry["then"] = _what_followed(conn, buf)
            self._note(entry)
            return False
        self._note(entry)
        return self._forward(conn, line, headers, buf)

    def _forward(
        self, conn: socket.socket, line: str, headers: list[tuple[str, str]], buf: bytearray
    ) -> bool:
        method, _, rest = line.partition(" ")
        target, _, version = rest.rpartition(" ")
        dest = self.upstream_port
        if "://" in target:
            after = target.split("://", 1)[1]
            slash = after.find("/")
            authority = after[:slash] if slash >= 0 else after
            target = after[slash:] if slash >= 0 else "/"
            host, _, port = authority.rpartition(":")
            if host in ("127.0.0.1", "localhost") and port.isdigit():
                dest = int(port)
        length = 0
        chunked = False
        out = [f"{method} {target} {version}"]
        for k, v in headers:
            low = k.lower()
            if low == "content-length":
                length = int(v)
            if low == "transfer-encoding" and "chunked" in v.lower():
                chunked = True
            if low in ("proxy-authorization", "proxy-connection", "connection", "keep-alive"):
                continue
            out.append(f"{k}: {v}")
        out.append("connection: close")
        body = bytearray()
        if chunked:
            while True:
                i = buf.find(b"\r\n")
                while i < 0:
                    chunk = conn.recv(65536)
                    if not chunk:
                        return False
                    buf += chunk
                    i = buf.find(b"\r\n")
                n = int(bytes(buf[:i]).split(b";")[0] or b"0", 16)
                _recv_some(conn, buf, i + 2 + n + 2, 30)
                body += buf[: i + 2 + n + 2]
                del buf[: i + 2 + n + 2]
                if n == 0:
                    break
        else:
            _recv_some(conn, buf, length, 30)
            body += buf[:length]
            del buf[:length]
        if dest is None:
            conn.sendall(
                b"HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
            )
            return False
        try:
            up = socket.create_connection(("127.0.0.1", dest), timeout=60)
        except OSError:
            conn.sendall(
                b"HTTP/1.1 502 Bad Gateway\r\ncontent-length: 0\r\nconnection: close\r\n\r\n"
            )
            return False
        try:
            up.sendall(("\r\n".join(out) + "\r\n\r\n").encode("latin-1") + bytes(body))
            conn.settimeout(None)
            first = bytearray()
            head_sent = False
            while True:
                chunk = up.recv(65536)
                if not chunk:
                    break
                if head_sent:
                    conn.sendall(chunk)
                    continue
                first += chunk
                i = first.find(b"\r\n\r\n")
                if i < 0:
                    continue
                # The proxy closes after each answer and says so.
                head = bytes(first[:i]).decode("latin-1").split("\r\n")
                head = [h for h in head if h.split(":", 1)[0].strip().lower() != "connection"]
                conn.sendall(
                    ("\r\n".join(head + ["connection: close"]) + "\r\n\r\n").encode("latin-1")
                    + bytes(first[i + 4 :])
                )
                head_sent = True
            if not head_sent and first:
                conn.sendall(bytes(first))
        finally:
            up.close()
        return False
