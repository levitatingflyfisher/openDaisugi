#!/usr/bin/python3
"""A fake voice server for the web cases: POST /transcribe on the address
it is given. It checks the bearer against the token file it is given and
answers by the body it gets:

- "silence": 422 with an error code and a message.
- "plain": 500 with a text/plain body.
- "slow": waits 2 s, then answers as for any other body.
- anything else: 200 with the text, numbers with trailing zeros, and the
  query and Content-Type it saw.
"""

import json
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

ADDR, TOKEN = sys.argv[1], sys.argv[2]


class Handler(BaseHTTPRequestHandler):
    def reply(self, code: int, body: bytes, ctype: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode("utf-8", "replace")
        want = "Bearer " + open(TOKEN, encoding="utf-8").read().strip()
        if self.headers.get("Authorization") != want:
            self.reply(
                401, b'{"error":"unauthorized","message":"Bad voice token."}', "application/json"
            )
            return
        if body == "silence":
            self.reply(
                422,
                b'{"error":"no_speech","message":"No speech found.","at":1.50}',
                "application/json",
            )
            return
        if body == "plain":
            self.reply(500, b"plain failure\n", "text/plain")
            return
        if body == "slow":
            time.sleep(2)
        seen = {"path": self.path, "ctype": self.headers.get("Content-Type", "")}
        out = '{"text":"heard %d bytes","duration_s":1.50,"rtf":0.250,"seen":%s}' % (
            n,
            json.dumps(seen),
        )
        self.reply(200, out.encode(), "application/json; charset=utf-8")

    def log_message(self, *args: object) -> None:
        pass


host, _, port = ADDR.rpartition(":")
HTTPServer((host, int(port)), Handler).serve_forever()
