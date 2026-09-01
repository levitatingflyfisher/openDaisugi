from __future__ import annotations

import email.message
import http.client
import inspect
import json
import threading
from contextlib import contextmanager

import pytest

from opendaisugi.config import Config
from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor.registry import prompt_pane
from opendaisugi.voice import server as voice_server
from opendaisugi.voice.cleanup import CleanupResult
from opendaisugi.voice.deliver import arm
from opendaisugi.voice.engines import EngineUnavailable, Transcript, UnknownEngine

from .conftest import FakeBackend, RecordingSend, make_wav


@contextmanager
def _started(server: voice_server.VoiceServer):
    """Run server.serve_forever() on a daemon thread for the life of the block."""
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield server
    finally:
        server.shutdown()
        thread.join(timeout=5)


class FakeEngine:
    name = "fake"

    def __init__(self, text: str = "hello from the fixture") -> None:
        self.text = text
        self.calls: list[bytes] = []

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        self.calls.append(wav_16k_mono)
        return Transcript(text=self.text, segments=[], duration_s=1.0, rtf=0.1)


class UnavailableEngine:
    name = "fake"

    def transcribe(self, wav_16k_mono: bytes, *, language: str | None = None) -> Transcript:
        raise EngineUnavailable("the fake engine is offline")


def _wav_bytes(seconds: float = 1.0, sr: int = 16000) -> bytes:
    return make_wav(sr=sr, n_samples=int(sr * seconds))


@pytest.fixture
def running_server(tmp_path):
    engine = FakeEngine()
    backend = FakeBackend()
    send = RecordingSend()
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=config,
        engine=engine,
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: backend,
        send=send,
    )
    with _started(server):
        yield server, engine, backend, send, token_file, tmp_path


def _conn(server) -> http.client.HTTPConnection:
    host, port = server.server_address[:2]
    return http.client.HTTPConnection(host, port, timeout=5)


def _token(token_file) -> str:
    return token_file.read_text().strip()


def _auth(token_file) -> dict:
    """The Authorization header for token_file's own token.

    Every POST needs a bearer token now, loopback included, so almost every
    test in this file merges this into its own headers dict.
    """
    return {"Authorization": f"Bearer {_token(token_file)}"}


def test_health_returns_ok_without_a_token(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.request("GET", "/health")
    resp = conn.getresponse()
    assert resp.status == 200
    assert json.loads(resp.read()) == {"ok": True}


def test_get_of_an_unknown_path_is_404_with_a_message(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.request("GET", "/unknown")
    resp = conn.getresponse()
    assert resp.status == 404
    payload = json.loads(resp.read())
    assert payload["error"] == "not_found"
    assert payload["message"] == "This server answers /health, /transcribe, and /deliver."


def test_post_of_an_unknown_path_is_404_with_a_message(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/unknown",
        body=b"{}",
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 404
    payload = json.loads(resp.read())
    assert payload["error"] == "not_found"
    assert payload["message"] == "This server answers /health, /transcribe, and /deliver."


def test_transcribe_on_loopback_with_no_token_is_401(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.request("POST", "/transcribe", body=_wav_bytes(), headers={"Content-Type": "audio/wav"})
    resp = conn.getresponse()
    assert resp.status == 401
    payload = json.loads(resp.read())
    assert payload["error"] == "unauthorized"
    assert "coppice web token" in payload["message"]


def test_transcribe_with_a_bad_token_is_401(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
    )
    resp = conn.getresponse()
    assert resp.status == 401
    payload = json.loads(resp.read())
    assert payload["error"] == "unauthorized"


def test_transcribe_on_loopback_with_the_right_token_succeeds(running_server):
    server, engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert payload["raw_text"] == "hello from the fixture"
    assert payload["engine"] == "fake"
    assert payload["cleaned"] is False
    assert "reason" not in payload
    assert len(engine.calls) == 1


def test_three_bad_tokens_ban_the_address_for_a_minute(running_server):
    server, *_ = running_server
    conn = _conn(server)
    for _ in range(3):
        conn.request(
            "POST",
            "/transcribe",
            body=_wav_bytes(),
            headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
        )
        conn.getresponse().read()
        conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", "Authorization": "Bearer wrong"},
    )
    resp = conn.getresponse()
    assert resp.status == 429
    payload = json.loads(resp.read())
    assert payload["error"] == "too_many_failed_tokens"
    assert "60 seconds" in payload["message"]


def test_transcribe_accepts_a_multipart_form_upload(running_server):
    server, engine, _backend, _send, token_file, _tmp = running_server
    boundary = "----daisugiTestBoundary"
    wav_bytes = _wav_bytes()
    body = (
        (
            f"--{boundary}\r\n"
            f'Content-Disposition: form-data; name="audio"; filename="clip.wav"\r\n'
            f"Content-Type: audio/wav\r\n\r\n"
        ).encode()
        + wav_bytes
        + f"\r\n--{boundary}--\r\n".encode()
    )
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=body,
        headers={"Content-Type": f"multipart/form-data; boundary={boundary}", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert payload["raw_text"] == "hello from the fixture"
    assert engine.calls[-1][:4] == b"RIFF"


def test_audio_over_the_duration_ceiling_is_413(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    oversized = _wav_bytes(seconds=61.0)
    conn.request(
        "POST",
        "/transcribe",
        body=oversized,
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 413
    payload = json.loads(resp.read())
    assert payload["error"] == "audio_too_long"
    assert "60 seconds" in payload["message"]


def test_oversized_content_length_is_413_before_any_body_is_read(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    fake_length = voice_server.MAX_CONTENT_LENGTH_BYTES + 1
    conn.putrequest("POST", "/transcribe")
    conn.putheader("Content-Type", "audio/wav")
    conn.putheader("Authorization", f"Bearer {_token(token_file)}")
    conn.putheader("Content-Length", str(fake_length))
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 413
    payload = json.loads(resp.read())
    assert payload["error"] == "payload_too_large"
    assert str(voice_server.MAX_CONTENT_LENGTH_BYTES) in payload["message"]


def test_bad_audio_is_400(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=b"not audio data at all",
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_audio"
    assert payload["message"] == (
        "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A."
    )


def test_truncated_wav_is_400_not_a_dropped_connection(running_server):
    # A RIFF/WAVE header with no fmt or data chunk behind it makes Python's
    # wave module raise wave.Error, not AudioFormatUnsupported or ValueError.
    # wave.Error is not a ValueError, so without it in the except tuple this
    # exception walks out of the handler and closes the connection with no
    # answer.
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    truncated = b"RIFF" + (0).to_bytes(4, "little") + b"WAVE"
    conn.request(
        "POST",
        "/transcribe",
        body=truncated,
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_audio"


def test_engine_unavailable_is_503(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=UnavailableEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
        send=RecordingSend(),
    )
    with _started(server):
        conn = _conn(server)
        conn.request(
            "POST",
            "/transcribe",
            body=_wav_bytes(),
            headers={"Content-Type": "audio/wav", **_auth(token_file)},
        )
        resp = conn.getresponse()
        assert resp.status == 503
        payload = json.loads(resp.read())
        assert payload["error"] == "engine_unavailable"


def test_transcribe_with_a_malformed_content_length_is_400(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.putrequest("POST", "/transcribe")
    conn.putheader("Content-Type", "audio/wav")
    conn.putheader("Authorization", f"Bearer {_token(token_file)}")
    conn.putheader("Content-Length", "not-a-number")
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_content_length"
    assert "Content-Length" in payload["message"]


def test_deliver_with_a_malformed_content_length_is_400(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.putrequest("POST", "/deliver")
    conn.putheader("Content-Type", "application/json")
    conn.putheader("Authorization", f"Bearer {_token(token_file)}")
    conn.putheader("Content-Length", "not-a-number")
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_content_length"
    assert "Content-Length" in payload["message"]


def test_deliver_with_malformed_json_is_400(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/deliver",
        body=b"not json at all",
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_json"
    assert "JSON" in payload["message"]


def _server_with_counting_backend_factory(tmp_path, *, raise_floor_not_available: bool):
    calls: list[int] = []

    def _factory() -> FakeBackend:
        calls.append(1)
        if raise_floor_not_available:
            raise FloorNotAvailable("no pane backend is available")
        return FakeBackend()

    send = RecordingSend()
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=_factory,
        send=send,
    )
    return server, calls, send, token_file


def test_deliver_preview_never_builds_a_pane_backend(tmp_path):
    server, calls, send, token_file = _server_with_counting_backend_factory(
        tmp_path, raise_floor_not_available=True
    )
    with _started(server):
        conn = _conn(server)
        body = json.dumps({"pane": "p1", "text": "hi", "mode": "preview"}).encode()
        conn.request(
            "POST",
            "/deliver",
            body=body,
            headers={"Content-Type": "application/json", **_auth(token_file)},
        )
        resp = conn.getresponse()
        assert resp.status == 200
        payload = json.loads(resp.read())
        assert payload["delivered"] == "preview"
        assert calls == []
        assert send.calls == []


def test_deliver_unarmed_send_never_builds_a_pane_backend(tmp_path):
    server, calls, send, token_file = _server_with_counting_backend_factory(
        tmp_path, raise_floor_not_available=True
    )
    with _started(server):
        conn = _conn(server)
        body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
        conn.request(
            "POST",
            "/deliver",
            body=body,
            headers={"Content-Type": "application/json", **_auth(token_file)},
        )
        resp = conn.getresponse()
        assert resp.status == 403
        payload = json.loads(resp.read())
        assert payload["delivered"] == "refused"
        assert "daisugi voice arm" in payload["reason"]
        assert calls == []
        assert send.calls == []


def test_deliver_armed_send_with_no_pane_backend_is_503(tmp_path):
    server, calls, send, token_file = _server_with_counting_backend_factory(
        tmp_path, raise_floor_not_available=True
    )
    with _started(server):
        arm("p1", minutes=30, armed_dir=tmp_path / "armed")
        conn = _conn(server)
        body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
        conn.request(
            "POST",
            "/deliver",
            body=body,
            headers={"Content-Type": "application/json", **_auth(token_file)},
        )
        resp = conn.getresponse()
        assert resp.status == 503
        payload = json.loads(resp.read())
        assert payload["error"] == "no_pane_backend"
        assert len(calls) == 1
        assert send.calls == []


def test_transcribe_with_cleanup_off_reports_uncleaned_with_no_reason(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe?cleanup=1",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert payload["cleaned"] is False
    assert "reason" not in payload


def test_transcribe_with_cleanup_success_reports_the_cleaned_text(running_server, monkeypatch):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    monkeypatch.setattr(
        voice_server,
        "clean_transcript",
        lambda text, *, config: CleanupResult(text="a cleaned sentence.", cleaned=True),
    )
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe?cleanup=1",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "a cleaned sentence."
    assert payload["raw_text"] == "hello from the fixture"
    assert payload["cleaned"] is True
    assert "reason" not in payload


def test_transcribe_with_cleanup_failure_reports_the_raw_text_and_a_reason(
    running_server, monkeypatch
):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    monkeypatch.setattr(
        voice_server,
        "clean_transcript",
        lambda text, *, config: CleanupResult(
            text=text, cleaned=False, reason="the cleanup model failed to respond"
        ),
    )
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe?cleanup=1",
        body=_wav_bytes(),
        headers={"Content-Type": "audio/wav", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["text"] == "hello from the fixture"
    assert payload["cleaned"] is False
    assert payload["reason"] == "the cleanup model failed to respond"


def test_deliver_send_on_an_unarmed_pane_is_refused_with_the_arm_command(running_server):
    server, _engine, _backend, send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["delivered"] == "refused"
    assert "daisugi voice arm" in payload["reason"]
    assert send.calls == []


def test_deliver_send_on_an_armed_pane_reaches_the_backend(running_server):
    server, _engine, backend, send, token_file, tmp = running_server
    arm("p1", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "sent"
    assert len(send.calls) == 1
    sent_backend, sent_pane, sent_text = send.calls[0]
    assert sent_backend is backend
    assert sent_pane.backend == "unknown"
    assert sent_pane.id == "p1"
    assert sent_text == "hi"


def test_deliver_send_defaults_the_pane_backend_name_to_unknown_when_omitted(running_server):
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("p2", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p2", "text": "hi", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    resp.read()
    assert send.calls[-1][1].backend == "unknown"


def test_deliver_send_uses_an_explicit_backend_name_when_given(running_server):
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("p3", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p3", "text": "hi", "mode": "send", "backend": "coppice"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    resp.read()
    assert send.calls[-1][1].backend == "coppice"


def test_deliver_preview_never_calls_send(running_server):
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("p4", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p4", "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "preview"
    assert send.calls == []


def test_deliver_rejects_a_request_with_no_pane(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_refuses_an_origin_header_even_with_a_valid_token(running_server):
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("p1", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "rm -rf /", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={
            "Content-Type": "application/json",
            "Origin": "https://example.com",
            **_auth(token_file),
        },
    )
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["error"] == "cross_origin_refused"
    assert send.calls == []


def test_deliver_refuses_a_cross_site_sec_fetch_site(running_server):
    server, *_, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={
            "Content-Type": "application/json",
            "Sec-Fetch-Site": "cross-site",
            **_auth(token_file),
        },
    )
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["error"] == "cross_origin_refused"


def test_deliver_accepts_a_same_origin_sec_fetch_site(running_server):
    server, *_, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={
            "Content-Type": "application/json",
            "Sec-Fetch-Site": "same-origin",
            **_auth(token_file),
        },
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "preview"


def test_deliver_accepts_a_sec_fetch_site_of_none(running_server):
    # none is what a request typed straight into a browser's own address bar
    # carries, and what most non-browser HTTP clients send by carrying no
    # Sec-Fetch-Site header at all when a proxy adds one anyway.
    server, *_, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={
            "Content-Type": "application/json",
            "Sec-Fetch-Site": "none",
            **_auth(token_file),
        },
    )
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "preview"


def test_deliver_refuses_a_non_json_content_type_even_to_an_armed_pane(running_server):
    # A text/plain body with a valid token to an armed pane must not reach
    # send(), even though a browser can send this cross-origin with no
    # preflight.
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("p1", minutes=30, armed_dir=tmp / "armed")
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "rm -rf /", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "text/plain", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_content_type"
    assert send.calls == []


def test_transcribe_refuses_an_origin_header_even_with_a_valid_token(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.request(
        "POST",
        "/transcribe",
        body=_wav_bytes(),
        headers={
            "Content-Type": "audio/wav",
            "Origin": "https://example.com",
            **_auth(token_file),
        },
    )
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["error"] == "cross_origin_refused"


def test_deliver_with_an_unknown_mode_is_400(running_server):
    server, _engine, _backend, send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p", "text": "t", "mode": "nonsense"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"
    assert payload["message"] == "mode must be preview or send."
    assert send.calls == []


def test_build_server_refuses_off_loopback_without_a_readable_token_file(tmp_path):
    with pytest.raises(RuntimeError, match="refusing to listen"):
        voice_server.build_server(
            "0.0.0.0",
            0,
            config=Config(data_dir=tmp_path),
            engine=FakeEngine(),
            token_file=tmp_path / "no-such-token",
            armed_dir=tmp_path / "armed",
            backend_factory=lambda: FakeBackend(),
        )


def test_build_server_refuses_on_loopback_without_a_readable_token_file(tmp_path):
    # Every POST needs a bearer token now, loopback included, so the server
    # must not be allowed to start on 127.0.0.1 with no token to check
    # against either.
    with pytest.raises(RuntimeError, match="refusing to listen") as exc_info:
        voice_server.build_server(
            "127.0.0.1",
            0,
            config=Config(data_dir=tmp_path),
            engine=FakeEngine(),
            token_file=tmp_path / "no-such-token",
            armed_dir=tmp_path / "armed",
            backend_factory=lambda: FakeBackend(),
        )
    assert "coppice web token" in str(exc_info.value)
    assert "--token-file" in str(exc_info.value)


def test_build_server_accepts_off_loopback_with_a_readable_token_file(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "0.0.0.0",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    assert not voice_server._is_loopback(server.server_address[0])
    server.server_close()


def test_build_server_accepts_on_loopback_with_a_readable_token_file(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    assert voice_server._is_loopback(server.server_address[0])
    server.server_close()


def test_build_server_defaults_send_to_registry_prompt_pane(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    try:
        assert server.send is prompt_pane
    finally:
        server.server_close()


def test_default_token_file_is_under_the_config_data_dir(tmp_path):
    config = Config(data_dir=tmp_path)
    assert voice_server.default_token_file(config) == tmp_path / "coppice" / "web" / "token"


def test_serve_lets_an_unknown_engine_name_propagate(tmp_path, monkeypatch):
    def _raise(_config):
        raise UnknownEngine("unknown voice_engine")

    monkeypatch.setattr(voice_server, "pick_engine", _raise)
    with pytest.raises(UnknownEngine):
        voice_server.serve(config=Config(data_dir=tmp_path), port=0)


def test_serve_lets_an_unavailable_engine_propagate(tmp_path, monkeypatch):
    def _raise(_config):
        raise EngineUnavailable("faster-whisper is not installed")

    monkeypatch.setattr(voice_server, "pick_engine", _raise)
    with pytest.raises(EngineUnavailable):
        voice_server.serve(config=Config(data_dir=tmp_path), port=0)


def test_token_comparison_uses_hmac_compare_digest():
    source = inspect.getsource(voice_server.VoiceRequestHandler._authorized)
    assert "hmac.compare_digest" in source
    assert "supplied == token" not in source
    assert "token == supplied" not in source


def test_a_spoofed_x_forwarded_for_header_grants_nothing(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    try:
        handler = voice_server.VoiceRequestHandler.__new__(voice_server.VoiceRequestHandler)
        handler.client_address = ("203.0.113.5", 1234)
        handler.headers = email.message.Message()
        handler.headers["X-Forwarded-For"] = "127.0.0.1"
        handler.server = server
        assert handler._authorized() is False
    finally:
        server.server_close()


def test_negative_content_length_is_400(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    conn.putrequest("POST", "/transcribe")
    conn.putheader("Content-Type", "audio/wav")
    conn.putheader("Authorization", f"Bearer {_token(token_file)}")
    conn.putheader("Content-Length", "-1")
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_content_length"


def test_handler_has_a_read_timeout_so_a_stalled_body_cannot_park_a_thread():
    assert isinstance(voice_server.VoiceRequestHandler.timeout, (int, float))
    assert voice_server.VoiceRequestHandler.timeout > 0


def test_chunked_transfer_encoding_without_content_length_is_411(running_server):
    server, *_ = running_server
    conn = _conn(server)
    conn.putrequest("POST", "/transcribe")
    conn.putheader("Content-Type", "audio/wav")
    conn.putheader("Transfer-Encoding", "chunked")
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 411
    payload = json.loads(resp.read())
    assert payload["error"] == "chunked_not_supported"


def test_ban_ledger_prunes_every_expired_address_not_only_the_caller(tmp_path, monkeypatch):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    try:
        now = [1000.0]
        monkeypatch.setattr(voice_server.time, "time", lambda: now[0])
        server.record_auth_failure("10.0.0.1")
        server.record_auth_failure("10.0.0.1")
        now[0] += voice_server._BAN_WINDOW_S + 1
        server.record_auth_failure("10.0.0.2")
        assert "10.0.0.1" not in server._auth_failures
        assert "10.0.0.2" in server._auth_failures
    finally:
        server.server_close()


def test_ban_ledger_records_every_concurrent_failure_with_no_lost_updates(tmp_path):
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=Config(data_dir=tmp_path),
        engine=FakeEngine(),
        token_file=token_file,
        armed_dir=tmp_path / "armed",
        backend_factory=lambda: FakeBackend(),
    )
    try:
        address = "198.51.100.7"
        iterations_per_thread = 100
        thread_count = 20

        def _hammer() -> None:
            for _ in range(iterations_per_thread):
                server.record_auth_failure(address)

        threads = [threading.Thread(target=_hammer) for _ in range(thread_count)]
        for t in threads:
            t.start()
        for t in threads:
            t.join(timeout=10)
        assert len(server._auth_failures[address]) == iterations_per_thread * thread_count
    finally:
        server.server_close()


def test_build_server_rechecks_the_token_file_after_binding(tmp_path, monkeypatch):
    # build_server reads the token file once before the socket is bound and
    # once after, so a token that stops being readable between the two
    # reads is still caught rather than a stale first answer being trusted.
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    real_read_token = voice_server._read_token
    calls: list[int] = []

    def _flaky_read_token(path):
        calls.append(1)
        return real_read_token(path) if len(calls) == 1 else None

    monkeypatch.setattr(voice_server, "_read_token", _flaky_read_token)
    with pytest.raises(RuntimeError, match="refusing to listen"):
        voice_server.build_server(
            "127.0.0.1",
            0,
            config=Config(data_dir=tmp_path),
            engine=FakeEngine(),
            token_file=token_file,
            armed_dir=tmp_path / "armed",
            backend_factory=lambda: FakeBackend(),
        )
    assert len(calls) == 2


def test_deliver_rejects_a_non_string_pane(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": ["p1"], "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_rejects_an_empty_pane(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "", "text": "hi", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_rejects_a_non_string_text(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": 5, "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_rejects_an_empty_text(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "", "mode": "preview"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_rejects_a_non_string_backend(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "preview", "backend": 5}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 400
    payload = json.loads(resp.read())
    assert payload["error"] == "bad_request"


def test_deliver_send_makes_only_one_is_armed_decision_so_no_none_backend_reaches_send(
    running_server, monkeypatch
):
    server, _engine, backend, send, token_file, _tmp = running_server
    answers = iter([False, True])
    monkeypatch.setattr("opendaisugi.voice.deliver.is_armed", lambda *a, **k: next(answers))
    body = json.dumps({"pane": "p1", "text": "hi", "mode": "send"}).encode()
    headers = {"Content-Type": "application/json", **_auth(token_file)}

    conn = _conn(server)
    conn.request("POST", "/deliver", body=body, headers=headers)
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["delivered"] == "refused"
    assert send.calls == []

    conn = _conn(server)
    conn.request("POST", "/deliver", body=body, headers=headers)
    resp = conn.getresponse()
    assert resp.status == 200
    payload = json.loads(resp.read())
    assert payload["delivered"] == "sent"
    assert len(send.calls) == 1
    sent_backend, _sent_pane, _sent_text = send.calls[0]
    assert sent_backend is backend
    assert sent_backend is not None


def test_deliver_send_with_a_300_character_pane_id_is_refused_not_a_500(running_server):
    # A pane id long enough to overflow a file name must still answer the
    # normal unarmed refusal, not an exception that kills the connection.
    # armed_dir must already exist on disk for this to reproduce: arm a
    # different, short pane first so the directory is there.
    server, _engine, _backend, send, token_file, tmp = running_server
    arm("w1:p1", minutes=30, armed_dir=tmp / "armed")
    long_pane = "p" * 300
    conn = _conn(server)
    body = json.dumps({"pane": long_pane, "text": "hi", "mode": "send"}).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 403
    payload = json.loads(resp.read())
    assert payload["delivered"] == "refused"
    assert f"daisugi voice arm {long_pane}" in payload["reason"]
    assert send.calls == []


def test_deliver_wire_size_ceiling_is_413_before_any_body_is_read(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    fake_length = voice_server.MAX_DELIVER_BYTES + 1
    conn.putrequest("POST", "/deliver")
    conn.putheader("Content-Type", "application/json")
    conn.putheader("Authorization", f"Bearer {_token(token_file)}")
    conn.putheader("Content-Length", str(fake_length))
    conn.endheaders()
    resp = conn.getresponse()
    assert resp.status == 413
    payload = json.loads(resp.read())
    assert payload["error"] == "payload_too_large"


def test_deliver_text_ceiling_is_413(running_server):
    server, _engine, _backend, _send, token_file, _tmp = running_server
    conn = _conn(server)
    body = json.dumps(
        {"pane": "p1", "text": "x" * (voice_server.MAX_DELIVER_TEXT_CHARS + 1), "mode": "preview"}
    ).encode()
    conn.request(
        "POST",
        "/deliver",
        body=body,
        headers={"Content-Type": "application/json", **_auth(token_file)},
    )
    resp = conn.getresponse()
    assert resp.status == 413
    payload = json.loads(resp.read())
    assert payload["error"] == "text_too_long"


def test_default_armed_dir_is_under_the_config_data_dir(tmp_path):
    config = Config(data_dir=tmp_path)
    assert voice_server.default_armed_dir(config) == tmp_path / "voice" / "armed"


def test_build_server_resolves_a_default_armed_dir_when_none_is_given(tmp_path):
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    server = voice_server.build_server(
        "127.0.0.1",
        0,
        config=config,
        engine=FakeEngine(),
        token_file=token_file,
        backend_factory=lambda: FakeBackend(),
    )
    try:
        assert server.armed_dir == voice_server.default_armed_dir(config)
    finally:
        server.server_close()


def test_half_given_tls_pair_closes_the_bound_socket_before_raising(tmp_path, monkeypatch):
    # super().__init__ already binds and listens before the half-given-pair
    # check runs. Spying on server_close proves the fix actually closes that
    # socket rather than leaking it on the refusal path.
    calls: list[voice_server.VoiceServer] = []
    real_close = voice_server.VoiceServer.server_close

    def _spy(self):
        calls.append(self)
        return real_close(self)

    monkeypatch.setattr(voice_server.VoiceServer, "server_close", _spy)
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")
    with pytest.raises(ValueError, match="pass both --tls-cert and --tls-key"):
        voice_server.build_server(
            "127.0.0.1",
            0,
            config=config,
            engine=FakeEngine(),
            token_file=token_file,
            backend_factory=lambda: FakeBackend(),
            tls_key=tmp_path / "key.pem",
        )
    assert len(calls) == 1


def test_serve_end_to_end_answers_health(tmp_path, monkeypatch):
    monkeypatch.setattr(voice_server, "pick_engine", lambda config: FakeEngine())
    monkeypatch.setattr(voice_server, "pick_backend", lambda config: FakeBackend())
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")

    real_build_server = voice_server.build_server
    captured: list[voice_server.VoiceServer] = []
    ready = threading.Event()

    def _capturing_build_server(*args, **kwargs):
        server = real_build_server(*args, **kwargs)
        captured.append(server)
        ready.set()
        return server

    monkeypatch.setattr(voice_server, "build_server", _capturing_build_server)

    thread = threading.Thread(
        target=voice_server.serve,
        kwargs={"config": config, "port": 0, "token_file": token_file},
        daemon=True,
    )
    thread.start()
    try:
        assert ready.wait(timeout=5)
        server = captured[0]
        conn = _conn(server)
        conn.request("GET", "/health")
        resp = conn.getresponse()
        assert resp.status == 200
        assert json.loads(resp.read()) == {"ok": True}
    finally:
        captured[0].shutdown()
        thread.join(timeout=5)


def test_serve_calls_on_bound_once_the_socket_is_bound_and_before_it_blocks(tmp_path, monkeypatch):
    monkeypatch.setattr(voice_server, "pick_engine", lambda config: FakeEngine())
    monkeypatch.setattr(voice_server, "pick_backend", lambda config: FakeBackend())
    config = Config(data_dir=tmp_path)
    token_file = tmp_path / "token"
    token_file.write_text("s3cr3t")

    real_build_server = voice_server.build_server
    captured: list[voice_server.VoiceServer] = []
    events: list[str] = []

    def _capturing_build_server(*args, **kwargs):
        server = real_build_server(*args, **kwargs)
        captured.append(server)
        events.append("bound")
        return server

    monkeypatch.setattr(voice_server, "build_server", _capturing_build_server)
    ready = threading.Event()

    def _on_bound():
        events.append("on_bound")
        ready.set()

    thread = threading.Thread(
        target=voice_server.serve,
        kwargs={"config": config, "port": 0, "token_file": token_file, "on_bound": _on_bound},
        daemon=True,
    )
    thread.start()
    try:
        assert ready.wait(timeout=5)
        assert events == ["bound", "on_bound"]
    finally:
        captured[0].shutdown()
        thread.join(timeout=5)
