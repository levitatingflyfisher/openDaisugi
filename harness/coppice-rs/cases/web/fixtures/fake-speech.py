#!/usr/bin/python3
"""A fake speech server for the cases: POST /v1/audio/speech on the address
it is given. It reads the JSON body and answers by its "input":

- "whoami": 200 with audio that is the text "auth=<the Authorization
  header> model=<model> voice=<voice> format=<response_format>".
- "error": 404 with a text body.
- "slow": waits 14 s, then answers as for any other input.
- "redirect": 302 to another address, which must not be followed.
- "huge": 200 with 2 MiB and 10 bytes of audio.
- "empty": 200 with no body.
- "chunked": 200 in chunks, with no Content-Length.
- anything else: 200 with the audio "ID3 fake audio".
"""

import json
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

ADDR = sys.argv[1]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def reply(self, code: int, body: bytes, ctype: str) -> None:
        self.send_response(code)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Connection", "close")
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        n = int(self.headers.get("Content-Length") or 0)
        try:
            body = json.loads(self.rfile.read(n) or b"{}")
        except ValueError:
            body = {}
        if self.path != "/v1/audio/speech":
            self.reply(404, b"no such path\n", "text/plain")
            return
        text = body.get("input", "")
        if text == "error":
            self.reply(404, b"no such voice\n", "text/plain")
            return
        if text == "redirect":
            self.send_response(302)
            self.send_header("Location", "http://127.0.0.1:1/elsewhere")
            self.send_header("Content-Length", "0")
            self.send_header("Connection", "close")
            self.end_headers()
            return
        if text == "slow":
            time.sleep(14)
        if text == "huge":
            self.reply(200, b"\0" * ((2 << 20) + 10), "audio/mpeg")
            return
        if text == "empty":
            self.reply(200, b"", "audio/mpeg")
            return
        if text == "chunked":
            self.send_response(200)
            self.send_header("Content-Type", "audio/mpeg")
            self.send_header("Transfer-Encoding", "chunked")
            self.send_header("Connection", "close")
            self.end_headers()
            for part in (b"ID3 ", b"fake ", b"audio"):
                self.wfile.write(b"%x\r\n%s\r\n" % (len(part), part))
            self.wfile.write(b"0\r\n\r\n")
            return
        if text == "whoami":
            out = "auth=%s model=%s voice=%s format=%s" % (
                self.headers.get("Authorization", ""),
                body.get("model", ""),
                body.get("voice", ""),
                body.get("response_format", ""),
            )
            self.reply(200, out.encode(), "audio/mpeg")
            return
        self.reply(200, b"ID3 fake audio", "audio/mpeg")

    def log_message(self, *args: object) -> None:
        pass


host, port = ADDR.rsplit(":", 1)
HTTPServer.allow_reuse_address = True
from socketserver import ThreadingMixIn


class Threaded(ThreadingMixIn, HTTPServer):
    daemon_threads = True


Threaded((host, int(port)), Handler).serve_forever()
