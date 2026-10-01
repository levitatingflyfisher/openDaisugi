#!/usr/bin/python3
"""A fake ntfy server for the web cases. It listens on the address it is
given and writes each request it gets to the log file it is given, one
JSON object per line: the method, the path, the headers ntfy reads and
the body. A topic named "fail" is answered 500."""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer

ADDR, LOG = sys.argv[1], sys.argv[2]
KEEP = ("Title", "Click", "Actions", "Authorization", "Content-Type")


class Handler(BaseHTTPRequestHandler):
    def do_POST(self) -> None:
        n = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(n).decode("utf-8", "replace")
        row = {"method": "POST", "path": self.path, "body": body}
        row["headers"] = {k: self.headers[k] for k in KEEP if k in self.headers}
        with open(LOG, "a", encoding="utf-8") as f:
            f.write(json.dumps(row, sort_keys=True) + "\n")
        code = 500 if self.path.endswith("/fail") else 200
        self.send_response(code)
        self.send_header("Content-Length", "2")
        self.end_headers()
        self.wfile.write(b"{}")

    def log_message(self, *args: object) -> None:
        pass


host, _, port = ADDR.rpartition(":")
HTTPServer((host, int(port)), Handler).serve_forever()
