"""A generic, resumable, sha256-verified download utility.

The int8 matcher of ADR-0021 is the first caller. Nothing here is specific
to int8. The server is a loopback fixture this file starts itself.
"""

from __future__ import annotations

import hashlib
import http.server
import threading

import pytest

from opendaisugi._model_fetch import FetchVerificationError, fetch, sha256_file

BLOB = b"x" * 500_000 + b"y" * 12345
SHA = hashlib.sha256(BLOB).hexdigest()


class _RangeHandler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass  # keep test output clean

    def do_GET(self):
        rng = self.headers.get("Range")
        if rng:
            start = int(rng.split("=")[1].split("-")[0])
            body = BLOB[start:]
            self.send_response(206)
            self.send_header("Content-Range", f"bytes {start}-{len(BLOB) - 1}/{len(BLOB)}")
        else:
            body = BLOB
            self.send_response(200)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class _ShortBodyHandler(_RangeHandler):
    """A plain GET promises the full length, sends 100,000 bytes, and closes.
    A Range GET serves the remainder in full, so a second call can finish."""

    def do_GET(self):
        if self.headers.get("Range"):
            return super().do_GET()
        self.send_response(200)
        self.send_header("Content-Length", str(len(BLOB)))
        self.end_headers()
        self.wfile.write(BLOB[:100_000])


class _DyingChunkedHandler(http.server.BaseHTTPRequestHandler):
    """A chunked body that announces a chunk and closes before it ends. The
    client sees http.client.IncompleteRead, which is not an OSError."""

    def log_message(self, *a):
        pass

    def do_GET(self):
        self.send_response(200)
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        self.wfile.write(b"5000\r\n" + b"x" * 100)


def _serve(handler):
    httpd = http.server.HTTPServer(("127.0.0.1", 0), handler)
    t = threading.Thread(target=httpd.serve_forever, daemon=True)
    t.start()
    return httpd


@pytest.fixture
def server():
    httpd = _serve(_RangeHandler)
    yield f"http://127.0.0.1:{httpd.server_port}/blob"
    httpd.shutdown()


@pytest.fixture
def short_server():
    httpd = _serve(_ShortBodyHandler)
    yield f"http://127.0.0.1:{httpd.server_port}/blob"
    httpd.shutdown()


@pytest.fixture
def dying_server():
    httpd = _serve(_DyingChunkedHandler)
    yield f"http://127.0.0.1:{httpd.server_port}/blob"
    httpd.shutdown()


def test_sha256_file_matches_hashlib(tmp_path):
    p = tmp_path / "f.bin"
    p.write_bytes(BLOB)
    assert sha256_file(p) == SHA


def test_fetch_downloads_and_verifies(tmp_path, server):
    dest = tmp_path / "model.bin"
    out = fetch(server, SHA, dest, size=len(BLOB))
    assert out == dest
    assert dest.read_bytes() == BLOB
    assert not (tmp_path / "model.bin.part").exists()


def test_fetch_short_circuits_when_already_valid(tmp_path, server, monkeypatch):
    dest = tmp_path / "model.bin"
    dest.write_bytes(BLOB)
    calls = []
    import urllib.request

    orig = urllib.request.urlopen

    def spy(*a, **k):
        calls.append(1)
        return orig(*a, **k)

    monkeypatch.setattr(urllib.request, "urlopen", spy)
    fetch(server, SHA, dest, size=len(BLOB))
    assert calls == []  # no network call at all


def test_fetch_resumes_partial_download(tmp_path, server):
    dest = tmp_path / "model.bin"
    part = tmp_path / "model.bin.part"
    part.write_bytes(BLOB[:500_000])
    fetch(server, SHA, dest, size=len(BLOB))
    assert dest.read_bytes() == BLOB
    assert not part.exists()


def test_fetch_deletes_and_raises_on_hash_mismatch(tmp_path, server):
    with pytest.raises(FetchVerificationError):
        fetch(server, "0" * 64, tmp_path / "model.bin", size=len(BLOB))
    assert not (tmp_path / "model.bin").exists()
    assert not (tmp_path / "model.bin.part").exists()  # the next call re-fetches clean


def test_fetch_unreachable_host_raises_oserror(tmp_path):
    with pytest.raises(OSError):
        fetch("http://127.0.0.1:1/nope", SHA, tmp_path / "model.bin", size=1)


def test_fetch_short_transfer_keeps_part_and_resumes_next_call(tmp_path, short_server):
    dest = tmp_path / "model.bin"
    part = tmp_path / "model.bin.part"
    with pytest.raises(OSError, match="stopped at 100000 of 512345 bytes") as info:
        fetch(short_server, SHA, dest, size=len(BLOB))
    assert not isinstance(info.value, FetchVerificationError)
    assert part.stat().st_size == 100_000  # kept, so the next call resumes
    assert not dest.exists()
    fetch(short_server, SHA, dest, size=len(BLOB))
    assert dest.read_bytes() == BLOB
    assert not part.exists()


def test_fetch_dying_chunked_body_raises_oserror(tmp_path, dying_server):
    dest = tmp_path / "model.bin"
    with pytest.raises(OSError) as info:
        fetch(dying_server, SHA, dest, size=len(BLOB))
    assert not isinstance(info.value, FetchVerificationError)
    assert not dest.exists()
