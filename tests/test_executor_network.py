"""NetworkExecutor — stdlib urllib, GET-only, no redirects."""

from __future__ import annotations

import threading
import time
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from opendaisugi.executor import NetworkExecutor
from opendaisugi.models import NetworkStep, ShellStep


class _Handler(BaseHTTPRequestHandler):
    # Class-level counter shared across requests within a test — the server
    # instance is recreated per-test via the fixture.
    hit_counts: dict[str, int] = {}

    def log_message(self, *args, **kwargs):
        pass  # silence stderr

    def do_GET(self):
        self.hit_counts[self.path] = self.hit_counts.get(self.path, 0) + 1
        if self.path == "/ok":
            body = b"hello world"
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/big":
            body = b"A" * 1000
            self.send_response(200)
            self.send_header("Content-Type", "text/plain")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/redirect":
            self.send_response(302)
            self.send_header("Location", "/other")
            self.send_header("Content-Length", "0")
            self.end_headers()
        elif self.path == "/other":
            body = b"should-not-be-fetched"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/notfound":
            body = b"missing"
            self.send_response(404)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.path == "/slow":
            time.sleep(2)
            body = b"eventually"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(400)
            self.send_header("Content-Length", "0")
            self.end_headers()


@pytest.fixture
def server():
    _Handler.hit_counts = {}
    httpd = HTTPServer(("127.0.0.1", 0), _Handler)
    port = httpd.server_address[1]
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    try:
        yield f"http://127.0.0.1:{port}", _Handler
    finally:
        httpd.shutdown()
        httpd.server_close()


def test_happy_get_2xx(server):
    base, _ = server
    step = NetworkStep(id="s", url=f"{base}/ok")

    result = NetworkExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 0
    assert result.stdout == "hello world"
    assert result.timed_out is False
    assert result.duration_ms > 0


def test_3xx_not_followed(server):
    base, handler = server
    step = NetworkStep(id="s", url=f"{base}/redirect")

    result = NetworkExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 1
    assert "302" in result.stdout
    # Crucial: the redirect target must never have been fetched.
    assert handler.hit_counts.get("/other", 0) == 0


def test_4xx_returns_rc1(server):
    base, _ = server
    step = NetworkStep(id="s", url=f"{base}/notfound")

    result = NetworkExecutor().run(step, timeout_s=5, max_output_bytes=1024)

    assert result.rc == 1


def test_truncation_at_max_output_bytes(server):
    base, _ = server
    step = NetworkStep(id="s", url=f"{base}/big")

    result = NetworkExecutor().run(step, timeout_s=5, max_output_bytes=100)

    assert result.rc == 0
    assert result.stdout.startswith("A" * 100)
    assert "truncated" in result.stdout


def test_timeout_sets_timed_out_true(server):
    base, _ = server
    step = NetworkStep(id="s", url=f"{base}/slow")

    result = NetworkExecutor().run(step, timeout_s=1, max_output_bytes=1024)

    assert result.rc == 2
    assert result.timed_out is True


def test_rejects_non_network_step():
    step = ShellStep(id="s", command="echo hi")

    with pytest.raises(TypeError):
        NetworkExecutor().run(step, timeout_s=5, max_output_bytes=1024)


def test_url_error_returns_rc2():
    # Port 1 is reserved and almost certainly not listening — connection refused.
    step = NetworkStep(id="s", url="http://127.0.0.1:1/")

    result = NetworkExecutor().run(step, timeout_s=2, max_output_bytes=1024)

    assert result.rc == 2
    assert result.timed_out is False


# --------------------- URL scheme guard (SGCM review EB-1) ---------------------

def test_network_executor_refuses_non_http_schemes(tmp_path):
    from opendaisugi.executor import NetworkExecutor
    from opendaisugi.models import NetworkStep
    secret = tmp_path / "secret.txt"
    secret.write_text("TOP SECRET")
    exe = NetworkExecutor()
    for url in [f"file://{secret}", "ftp://example.com/x", "data:text/plain,hi"]:
        r = exe.run(NetworkStep(id="s", url=url), timeout_s=2, max_output_bytes=1024)
        assert r.rc != 0
        assert "TOP SECRET" not in r.stdout


def test_network_executor_enforces_wall_clock_deadline(monkeypatch):
    # EB-4: a slow-drip server (a byte before each socket timeout) must not hold
    # the executor past the wall-clock deadline.
    import time as _t

    from opendaisugi.executor import NetworkExecutor
    from opendaisugi.models import NetworkStep

    class _SlowResp:
        def read(self, want):
            _t.sleep(0.25)
            return b"x"  # 1 byte, never EOF → infinite drip
        def __enter__(self): return self
        def __exit__(self, *a): return False

    import urllib.request
    monkeypatch.setattr(urllib.request.OpenerDirector, "open", lambda self, req, timeout=None: _SlowResp())
    exe = NetworkExecutor()
    r = exe.run(NetworkStep(id="s", url="https://slow.example.com/"), timeout_s=1, max_output_bytes=10_000_000)
    assert r.timed_out is True
    assert r.rc == 2
