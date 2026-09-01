from __future__ import annotations

import io
import json
import urllib.error
import wave

import pytest

from opendaisugi.config import Config
from opendaisugi.voice import ptt
from opendaisugi.voice.ptt import main_loop, record_and_send, run_ptt
from opendaisugi.voice.server import default_token_file

from .conftest import FakeClient, FakeStream


class _FakeResponse:
    """A stand-in for the context manager urllib.request.urlopen returns."""

    def __init__(self, body: bytes) -> None:
        self._body = body

    def __enter__(self) -> "_FakeResponse":
        return self

    def __exit__(self, *exc_info) -> bool:
        return False

    def read(self) -> bytes:
        return self._body


def test_record_and_send_packages_frames_as_a_16k_mono_wav():
    client = FakeClient()
    text, result = record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    assert text == "hello world"
    assert result == {"delivered": "preview"}
    wav_bytes = client.transcribe_calls[0]
    assert wav_bytes[:4] == b"RIFF"
    with wave.open(io.BytesIO(wav_bytes), "rb") as w:
        assert w.getframerate() == 16000
        assert w.getnchannels() == 1
    assert client.deliver_calls == [("w1:p1", "hello world", "preview")]


def test_record_and_send_prints_the_raw_text_when_it_differs(capsys):
    client = FakeClient()
    client.transcribe = lambda wav_bytes: {"text": "hello world", "raw_text": "hello wurld"}
    record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    out = capsys.readouterr().out
    assert "hello wurld" in out
    assert "hello world" in out


def test_record_and_send_prints_nothing_extra_when_raw_text_matches_text(capsys):
    client = FakeClient(text="hello world")
    record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    out = capsys.readouterr().out
    assert out.count("hello world") == 1


def test_record_and_send_skips_deliver_when_no_speech_was_heard(capsys):
    client = FakeClient(text="")
    text, result = record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    assert text == ""
    assert client.deliver_calls == []
    assert result["delivered"] == "skipped"
    out = capsys.readouterr().out
    assert "Record for longer before tapping space again." in out


def test_record_and_send_prints_the_reason_when_delivery_is_refused(capsys):
    client = FakeClient()
    client.deliver = lambda pane, text, *, mode: {
        "delivered": "refused",
        "reason": "w1:p1 is not armed. Run daisugi voice arm w1:p1 --for 30m.",
    }
    record_and_send([b"\x00\x00" * 100], pane="w1:p1", client=client)
    out = capsys.readouterr().out
    assert "is not armed" in out


def test_run_ptt_posts_once_per_press_release_cycle():
    client = FakeClient()
    stream = FakeStream([b"\x00\x01" * 800])
    results = run_ptt("w1:p1", client=client, keys=[" ", " ", "q"], open_stream=lambda: stream)
    assert len(client.transcribe_calls) == 1
    assert len(client.deliver_calls) == 1
    assert stream.started and stream.stopped
    assert results == [("hello world", {"delivered": "preview"})]


def test_run_ptt_handles_two_press_release_cycles():
    client = FakeClient()
    streams = [FakeStream([b"\x00\x01" * 400]), FakeStream([b"\x00\x02" * 400])]
    it = iter(streams)
    results = run_ptt(
        "w1:p1", client=client, keys=[" ", " ", " ", " ", "q"], open_stream=lambda: next(it)
    )
    assert len(client.transcribe_calls) == 2
    assert len(results) == 2
    assert all(s.started and s.stopped for s in streams)


def test_run_ptt_quits_immediately_on_q_without_recording():
    client = FakeClient()
    results = run_ptt("w1:p1", client=client, keys=["q"], open_stream=lambda: FakeStream([]))
    assert results == []
    assert client.transcribe_calls == []


def test_run_ptt_prints_the_message_and_continues_after_a_server_error(capsys):
    class FailingClient(FakeClient):
        def transcribe(self, wav_bytes: bytes) -> dict:
            raise ptt.VoiceServerError("The voice server answered 503. Install faster-whisper.")

    client = FailingClient()
    stream = FakeStream([b"\x00\x01" * 800])
    results = run_ptt("w1:p1", client=client, keys=[" ", " ", "q"], open_stream=lambda: stream)
    assert results == []
    out = capsys.readouterr().out.strip().splitlines()
    assert out == ["The voice server answered 503. Install faster-whisper."]


def test_main_loop_reads_from_a_fake_non_tty_stdin_and_quits(monkeypatch):
    calls: list[tuple[str, str, str]] = []

    class FixedClient(FakeClient):
        def deliver(self, pane, text, *, mode):
            calls.append((pane, text, mode))
            return {"delivered": "preview"}

    monkeypatch.setattr(
        "opendaisugi.voice.ptt.HttpVoiceClient", lambda url, **kwargs: FixedClient()
    )
    monkeypatch.setattr(
        "opendaisugi.voice.ptt._default_stream", lambda: FakeStream([b"\x00\x00" * 100])
    )
    fake_stdin = io.StringIO("  q")  # not a real tty, so main_loop must skip termios entirely
    main_loop("w1:p1", server_url="http://127.0.0.1:7477", stdin=fake_stdin)
    assert calls == [("w1:p1", "hello world", "preview")]


def test_main_loop_prints_the_status_line_with_no_banned_punctuation(monkeypatch):
    monkeypatch.setattr(ptt, "HttpVoiceClient", lambda url, **kwargs: FakeClient())
    monkeypatch.setattr(ptt, "_default_stream", lambda: FakeStream([b"\x00\x00" * 100]))
    lines: list[str] = []
    fake_stdin = io.StringIO("q")
    main_loop("w1:p1", server_url="http://127.0.0.1:7477", stdin=fake_stdin, print_fn=lines.append)
    assert lines[0] == (
        "Push to talk on w1:p1 through http://127.0.0.1:7477. "
        "Tap space to start recording. Tap it again to stop and send. q to quit."
    )
    for banned in ("—", "->", "(", ")"):
        assert banned not in lines[0]


def test_http_voice_client_sends_bearer_token_on_loopback_too(tmp_path, monkeypatch):
    config = Config(data_dir=tmp_path)
    token_file = default_token_file(config)
    token_file.parent.mkdir(parents=True)
    token_file.write_text("secret-token\n")

    captured = {}

    def fake_urlopen(req, timeout=None):
        captured["req"] = req
        return _FakeResponse(b'{"text": "hi", "raw_text": "hi"}')

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = ptt.HttpVoiceClient("http://127.0.0.1:7477", config=config)
    client.transcribe(b"RIFF....")
    assert captured["req"].get_header("Authorization") == "Bearer secret-token"


def test_http_voice_client_sends_bearer_token_off_loopback(tmp_path, monkeypatch):
    config = Config(data_dir=tmp_path)
    token_file = default_token_file(config)
    token_file.parent.mkdir(parents=True)
    token_file.write_text("secret-token\n")

    captured = {}

    def fake_urlopen(req, timeout=None):
        captured["req"] = req
        return _FakeResponse(b'{"text": "hi", "raw_text": "hi"}')

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = ptt.HttpVoiceClient("http://100.64.0.5:7477", config=config)
    client.transcribe(b"RIFF....")
    assert captured["req"].get_header("Authorization") == "Bearer secret-token"


def test_http_voice_client_raises_when_no_token_file_is_readable_off_loopback(tmp_path):
    config = Config(data_dir=tmp_path)
    token_file = default_token_file(config)
    with pytest.raises(ptt.VoiceServerError, match="coppice web token") as exc_info:
        ptt.HttpVoiceClient("http://100.64.0.5:7477", config=config)
    assert str(token_file) in str(exc_info.value)


def test_http_voice_client_raises_when_no_token_file_is_readable_on_loopback_too(tmp_path):
    config = Config(data_dir=tmp_path)
    token_file = default_token_file(config)
    with pytest.raises(ptt.VoiceServerError, match="coppice web token") as exc_info:
        ptt.HttpVoiceClient("http://127.0.0.1:7477", config=config)
    assert str(token_file) in str(exc_info.value)


def _loopback_client(tmp_path, server_url: str = "http://127.0.0.1:7477") -> "ptt.HttpVoiceClient":
    """A client built against a config whose token file already exists.

    Every HttpVoiceClient reads a token file now, loopback included, so
    every test that builds one needs a config with a real token file
    behind it, not the operator's own default config.
    """
    config = Config(data_dir=tmp_path)
    token_file = default_token_file(config)
    token_file.parent.mkdir(parents=True)
    token_file.write_text("s3cr3t")
    return ptt.HttpVoiceClient(server_url, config=config)


def test_http_voice_client_transcribe_raises_with_the_servers_message(tmp_path, monkeypatch):
    body = json.dumps(
        {"error": "bad_audio", "message": "Send WAV, WebM, Ogg, MP3, or M4A."}
    ).encode()

    def fake_urlopen(req, timeout=None):
        raise urllib.error.HTTPError(req.full_url, 400, "Bad Request", None, io.BytesIO(body))

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = _loopback_client(tmp_path)
    with pytest.raises(ptt.VoiceServerError, match="Send WAV"):
        client.transcribe(b"not audio")


def test_http_voice_client_deliver_returns_a_refused_body_instead_of_raising(tmp_path, monkeypatch):
    body = json.dumps(
        {"delivered": "refused", "reason": "w1:p1 is not armed. Run daisugi voice arm."}
    ).encode()

    def fake_urlopen(req, timeout=None):
        raise urllib.error.HTTPError(req.full_url, 403, "Forbidden", None, io.BytesIO(body))

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = _loopback_client(tmp_path)
    result = client.deliver("w1:p1", "hello", mode="preview")
    assert result["delivered"] == "refused"
    assert "not armed" in result["reason"]


def test_http_voice_client_deliver_raises_on_a_bad_request(tmp_path, monkeypatch):
    body = json.dumps(
        {"error": "bad_request", "message": "text must be a non-empty string."}
    ).encode()

    def fake_urlopen(req, timeout=None):
        raise urllib.error.HTTPError(req.full_url, 400, "Bad Request", None, io.BytesIO(body))

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = _loopback_client(tmp_path)
    with pytest.raises(ptt.VoiceServerError, match="non-empty"):
        client.deliver("w1:p1", "", mode="preview")


def test_http_voice_client_raises_a_plain_message_when_the_server_is_unreachable(
    tmp_path, monkeypatch
):
    def fake_urlopen(req, timeout=None):
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = _loopback_client(tmp_path)
    with pytest.raises(ptt.VoiceServerError, match="daisugi voice serve"):
        client.transcribe(b"RIFF....")


def test_http_voice_client_deliver_names_daisugi_voice_serve_when_unreachable(
    tmp_path, monkeypatch
):
    def fake_urlopen(req, timeout=None):
        raise urllib.error.URLError("connection refused")

    monkeypatch.setattr(ptt.urllib_request, "urlopen", fake_urlopen)
    client = _loopback_client(tmp_path)
    with pytest.raises(ptt.VoiceServerError, match="daisugi voice serve"):
        client.deliver("w1:p1", "hello", mode="preview")


def test_unreachable_names_the_server_and_the_serve_command():
    exc = ptt._unreachable("http://127.0.0.1:7477")
    assert "http://127.0.0.1:7477" in str(exc)
    assert "daisugi voice serve" in str(exc)


def test_record_and_send_posts_nothing_when_no_audio_was_captured(capsys):
    client = FakeClient()
    text, result = record_and_send([], pane="w1:p1", client=client)
    assert text == ""
    assert client.transcribe_calls == []
    assert client.deliver_calls == []
    assert result == {"delivered": "skipped", "reason": "no audio was captured"}
    out = capsys.readouterr().out
    assert "Record for longer before tapping space again." in out


def test_run_ptt_posts_nothing_when_a_release_captured_no_audio():
    client = FakeClient()
    stream = FakeStream([])
    results = run_ptt("w1:p1", client=client, keys=[" ", " ", "q"], open_stream=lambda: stream)
    assert client.transcribe_calls == []
    assert client.deliver_calls == []
    assert stream.started and stream.stopped
    assert results == [("", {"delivered": "skipped", "reason": "no audio was captured"})]


def test_run_ptt_stops_the_stream_and_discards_the_clip_on_q_mid_recording():
    client = FakeClient()
    stream = FakeStream([b"\x00\x01" * 800])
    results = run_ptt("w1:p1", client=client, keys=[" ", "q"], open_stream=lambda: stream)
    assert results == []
    assert stream.started and stream.stopped
    assert client.transcribe_calls == []


def test_main_loop_prints_one_line_and_returns_when_no_token_file_is_readable(
    tmp_path, monkeypatch
):
    import opendaisugi.config as config_mod

    monkeypatch.setattr(config_mod, "load_config", lambda: Config(data_dir=tmp_path))
    stream_opened = []
    monkeypatch.setattr(ptt, "_default_stream", lambda: stream_opened.append(1))
    lines: list[str] = []
    fake_stdin = io.StringIO("q")
    main_loop("w1:p1", server_url="http://100.64.0.5:7477", stdin=fake_stdin, print_fn=lines.append)
    assert len(lines) == 1
    assert "coppice web token" in lines[0]
    assert stream_opened == []


class _FakeRawStream:
    """Mimics sounddevice.RawInputStream's read_available, read, start, and stop."""

    def __init__(self, total_frames: int) -> None:
        self.available = total_frames
        self.started = False
        self.stopped = False

    @property
    def read_available(self) -> int:
        return self.available

    def start(self) -> None:
        self.started = True

    def stop(self) -> None:
        self.stopped = True

    def read(self, frames: int) -> tuple[bytes, bool]:
        frames = min(frames, self.available)
        self.available -= frames
        return b"\x00\x00" * frames, False


def test_sounddevice_stream_adapter_captures_the_whole_press():
    raw = _FakeRawStream(total_frames=5000)
    adapter = ptt._SoundDeviceStream(raw)
    adapter.start()
    assert raw.started
    total_frames = 0
    more = True
    while more:
        chunk, more = adapter.read(1600)
        total_frames += len(chunk) // 2
    adapter.stop()
    assert raw.stopped
    assert total_frames == 5000


def test_sounddevice_stream_adapter_reads_nothing_when_nothing_is_buffered():
    raw = _FakeRawStream(total_frames=0)
    adapter = ptt._SoundDeviceStream(raw)
    chunk, more = adapter.read(1600)
    assert chunk == b""
    assert more is False


class _RefillingFakeRawStream:
    """Mimics a sounddevice stream that keeps filling its buffer between reads,
    so read_available reports a fresh chunk again right after the first read
    drains what was there before."""

    def __init__(self, first_chunk: int, refill: int) -> None:
        self._available = first_chunk
        self._refill = refill
        self.read_calls = 0

    @property
    def read_available(self) -> int:
        return self._available

    def start(self) -> None:
        pass

    def stop(self) -> None:
        pass

    def read(self, frames: int) -> tuple[bytes, bool]:
        self.read_calls += 1
        frames = min(frames, self._available)
        self._available = self._refill if self.read_calls == 1 else 0
        return b"\x00\x00" * frames, False


def test_sounddevice_stream_adapter_drains_twice_when_more_audio_arrives_between_reads():
    raw = _RefillingFakeRawStream(first_chunk=1600, refill=1600)
    adapter = ptt._SoundDeviceStream(raw)

    first_chunk, more = adapter.read(1600)
    assert more is True
    assert len(first_chunk) == 1600 * 2

    frames = [first_chunk]
    while more:
        chunk, more = adapter.read(1600)
        frames.append(chunk)

    assert raw.read_calls == 2
    assert sum(len(c) for c in frames) == 1600 * 2 * 2


def test_ptt_module_imports_with_sounddevice_hidden_and_default_stream_names_the_extra(monkeypatch):
    import importlib
    import sys

    import opendaisugi.voice as voice_pkg

    monkeypatch.setitem(sys.modules, "sounddevice", None)
    monkeypatch.delitem(sys.modules, "opendaisugi.voice.ptt", raising=False)
    # The fresh import rebinds the package attribute; restore it at teardown so
    # later tests that patch the original module still reach the code the CLI runs.
    monkeypatch.setattr(voice_pkg, "ptt", voice_pkg.ptt, raising=False)
    fresh_ptt = importlib.import_module("opendaisugi.voice.ptt")
    with pytest.raises(RuntimeError, match=r"opendaisugi\[voice\]"):
        fresh_ptt._default_stream()
