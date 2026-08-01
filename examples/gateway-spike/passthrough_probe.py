#!/usr/bin/env python3
"""Transport probe for the openDaisugi token-saving gateway (throwaway diagnostic).

Before building the gateway, confirm the ONE unverified fact the whole design rests
on: that your harness (Claude Code) actually works when ``ANTHROPIC_BASE_URL`` points
at a local process — and capture what really crosses the wire.

This is a byte-faithful *logging passthrough*. It forwards every request to the real
Anthropic API unchanged, streams the response straight back chunk-for-chunk (so SSE
stays live), and writes a **credential-redacted** record of each exchange to
``./gateway-spike-log/`` — real fixtures to build the gateway's tests against.

It answers three questions the design depends on:
  1. Does base-URL redirection work for your auth at all?
  2. Is your Claude Code sending an OAuth ``Authorization: Bearer`` (subscription)
     or an ``x-api-key`` (API key)? — this changes the gateway's forwarding design.
  3. Does streaming survive a passthrough?

Run it:
    python examples/gateway-spike/passthrough_probe.py     # listens on 127.0.0.1:8787

Then, in the shell where you launch Claude Code:
    export ANTHROPIC_BASE_URL=http://127.0.0.1:8787
    claude          # do one small real task, then exit

Watch this probe's console. Per request it prints: which auth header arrived
(name + scheme + length — never the value), the path, whether streaming was
requested, and whether the upstream call and stream-through succeeded.

Nothing here ships in the library. Delete ``./gateway-spike-log/`` when done — it
holds redacted request/response bodies, not secrets, but it is still your traffic.
"""

from __future__ import annotations

import http.client
import http.server
import json
import os
import time
from urllib.parse import urlsplit

UPSTREAM = os.environ.get("PROBE_UPSTREAM", "https://api.anthropic.com")
LISTEN_HOST = os.environ.get("PROBE_HOST", "127.0.0.1")
LISTEN_PORT = int(os.environ.get("PROBE_PORT", "8787"))
LOG_DIR = os.environ.get("PROBE_LOG_DIR", "gateway-spike-log")

_up = urlsplit(UPSTREAM)
_UP_HOST = _up.hostname
_UP_PORT = _up.port or (443 if _up.scheme == "https" else 80)
_UP_HTTPS = _up.scheme == "https"

# Header names whose *values* must never touch disk or the console.
_SECRET_HEADERS = {"authorization", "x-api-key", "cookie", "anthropic-auth"}


def _redact(name: str, value: str) -> str:
    if name.lower() == "authorization":
        scheme = value.split(" ", 1)[0] if " " in value else "(no-scheme)"
        return f"{scheme} <redacted {len(value)} chars>"
    return f"<redacted {len(value)} chars>"


def _describe_auth(headers) -> str:
    found = [
        f"{name}: {_redact(name, headers[name])}"
        for name in headers
        if name.lower() in _SECRET_HEADERS
    ]
    return "; ".join(found) if found else "!! NO AUTH HEADER SEEN !!"


os.makedirs(LOG_DIR, exist_ok=True)


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def _proxy(self) -> None:
        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b""

        auth = _describe_auth(self.headers)
        wants_stream = b'"stream":true' in body.replace(b" ", b"")
        print(f"\n--> {self.command} {self.path}")
        print(f"    auth: {auth}")
        print(f"    stream requested: {wants_stream}   body: {len(body)} bytes")
        self._dump_request(body, auth)

        conn_cls = http.client.HTTPSConnection if _UP_HTTPS else http.client.HTTPConnection
        try:
            conn = conn_cls(_UP_HOST, _UP_PORT, timeout=600)
            fwd = {k: v for k, v in self.headers.items() if k.lower() != "host"}
            conn.request(self.command, self.path, body=body, headers=fwd)
            resp = conn.getresponse()
        except Exception as exc:  # the real gateway fails open here; the probe reports
            print(f"    !! upstream error: {exc}")
            self.send_error(502, f"probe upstream error: {exc}")
            return

        ctype = resp.getheader("content-type")
        print(f"    <-- {resp.status} {resp.reason}   content-type: {ctype}")

        # Close-delimit the response so we can stream an unknown-length SSE body
        # through faithfully without re-chunking it ourselves.
        self.close_connection = True
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() in ("transfer-encoding", "connection", "content-length"):
                continue
            self.send_header(k, v)
        self.send_header("Connection", "close")
        self.end_headers()

        chunks = 0
        try:
            while True:
                chunk = resp.read(2048)
                if not chunk:
                    break
                self.wfile.write(chunk)
                self.wfile.flush()
                chunks += 1
        except Exception as exc:
            print(f"    !! stream-through error after {chunks} chunks: {exc}")
            return
        print(f"    streamed {chunks} chunk(s) OK")

    do_POST = _proxy
    do_GET = _proxy

    def _dump_request(self, body: bytes, auth: str) -> None:
        ts = int(time.time() * 1000)
        rec: dict = {
            "path": self.path,
            "method": self.command,
            "auth_summary": auth,
            "headers": {
                k: (_redact(k, v) if k.lower() in _SECRET_HEADERS else v)
                for k, v in self.headers.items()
            },
        }
        try:
            rec["body"] = json.loads(body) if body else None
        except Exception:
            rec["body_raw_len"] = len(body)
        with open(os.path.join(LOG_DIR, f"req-{ts}.json"), "w") as fh:
            json.dump(rec, fh, indent=2)

    def log_message(self, *_a) -> None:  # silence default per-request logging
        pass


def main() -> None:
    print("openDaisugi gateway transport probe")
    print(f"  listening: http://{LISTEN_HOST}:{LISTEN_PORT}")
    print(f"  upstream:  {UPSTREAM}")
    print(f"  logging (redacted) to: ./{LOG_DIR}/")
    print(
        f"\nPoint your harness at it:  "
        f"export ANTHROPIC_BASE_URL=http://{LISTEN_HOST}:{LISTEN_PORT}\n"
    )
    http.server.ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), Handler).serve_forever()


if __name__ == "__main__":
    main()
