"""Laptop push-to-talk: tap space to start recording, tap it again to stop and send.

This reads keys only while its own terminal has focus, in raw mode. It does
not install a global hotkey. sounddevice is imported lazily, only when a
press actually starts recording. Importing it at module load time can raise
OSError on a box with no audio device, and this module's own tests must not
depend on a working audio device.

The core logic, run_ptt, takes its keypresses and its audio stream as
parameters, so it runs fully under test with no terminal and no microphone.
main_loop is the thin wrapper that supplies the real terminal and the real
stream.
"""

from __future__ import annotations

import io
import json
import wave
from typing import TYPE_CHECKING, Callable, Iterable, Protocol
from urllib import error as urllib_error
from urllib import request as urllib_request

if TYPE_CHECKING:
    from opendaisugi.config import Config


class RecordStream(Protocol):
    def start(self) -> None: ...
    def stop(self) -> None: ...
    def read(self, frames: int) -> tuple[bytes, bool]:
        """Return the next chunk and whether more is available."""
        ...


class VoiceClient(Protocol):
    def transcribe(self, wav_bytes: bytes) -> dict: ...
    def deliver(self, pane: str, text: str, *, mode: str) -> dict: ...


class VoiceServerError(RuntimeError):
    """The voice server answered a request with an error, or could not be reached.

    The message is ready to show a person as one line. It never needs a
    caller to unpack a status code or a JSON body first.
    """


def _read_token_file(token_file) -> str | None:
    try:
        return token_file.read_text(encoding="utf-8").strip() or None
    except OSError:
        return None


def _read_error_payload(exc: urllib_error.HTTPError) -> dict | None:
    """Parse the server's JSON error body, or None when there is none to read."""
    try:
        payload = json.loads(exc.read())
    except (ValueError, OSError):
        return None
    return payload if isinstance(payload, dict) else None


def _error_message(payload: dict | None, exc: urllib_error.HTTPError) -> str:
    fallback = f"The voice server answered {exc.code}."
    if payload is None:
        return fallback
    return str(payload.get("message") or payload.get("error") or fallback)


def _unreachable(server_url: str) -> VoiceServerError:
    return VoiceServerError(
        f"Could not reach the voice server at {server_url}. Run daisugi voice serve first."
    )


class HttpVoiceClient:
    """stdlib urllib client for the voice server's /transcribe and /deliver.

    Always reads the token file opendaisugi.voice.server.default_token_file
    names for the given config, and always sends it as an Authorization
    Bearer header, loopback included. config defaults to
    opendaisugi.config.load_config(). Raises VoiceServerError at
    construction when that token file cannot be read, so a caller never
    sends a doomed unauthenticated request.
    """

    def __init__(
        self,
        server_url: str,
        *,
        timeout_s: float = 30.0,
        config: "Config | None" = None,
    ) -> None:
        self.server_url = server_url.rstrip("/")
        self.timeout_s = timeout_s
        from opendaisugi.config import load_config
        from opendaisugi.voice.server import default_token_file

        resolved_config = config if config is not None else load_config()
        token_file = default_token_file(resolved_config)
        token = _read_token_file(token_file)
        if token is None:
            raise VoiceServerError(
                f"No token file at {token_file}. Run coppice web token on the box, "
                "or pass --token-file."
            )
        self._token = token

    def _headers(self, content_type: str) -> dict[str, str]:
        return {"Content-Type": content_type, "Authorization": f"Bearer {self._token}"}

    def transcribe(self, wav_bytes: bytes) -> dict:
        req = urllib_request.Request(
            f"{self.server_url}/transcribe",
            data=wav_bytes,
            method="POST",
            headers=self._headers("audio/wav"),
        )
        try:
            with urllib_request.urlopen(req, timeout=self.timeout_s) as resp:
                return json.loads(resp.read())
        except urllib_error.HTTPError as exc:
            raise VoiceServerError(_error_message(_read_error_payload(exc), exc)) from exc
        except urllib_error.URLError as exc:
            raise _unreachable(self.server_url) from exc

    def deliver(self, pane: str, text: str, *, mode: str) -> dict:
        body = json.dumps({"pane": pane, "text": text, "mode": mode}).encode()
        req = urllib_request.Request(
            f"{self.server_url}/deliver",
            data=body,
            method="POST",
            headers=self._headers("application/json"),
        )
        try:
            with urllib_request.urlopen(req, timeout=self.timeout_s) as resp:
                return json.loads(resp.read())
        except urllib_error.HTTPError as exc:
            payload = _read_error_payload(exc)
            if payload is not None and "delivered" in payload:
                # A refused delivery, sent with status 403, is a normal answer.
                return payload
            raise VoiceServerError(_error_message(payload, exc)) from exc
        except urllib_error.URLError as exc:
            raise _unreachable(self.server_url) from exc


def record_and_send(
    frames: list[bytes],
    *,
    pane: str,
    client: "VoiceClient",
    sample_rate: int = 16000,
    print_fn: Callable[[str], None] = print,
) -> tuple[str, dict]:
    """Package recorded 16-bit mono PCM frames as a WAV, transcribe, and deliver.

    Delivery always runs in preview mode. Arming a pane for direct send is a
    separate step, run ahead of time. Prints the text, and the raw text too
    when cleanup changed it. No frames, or empty text, skips the network
    call it has nothing to send for. Returns the text and the delivery
    result.
    """
    if not frames:
        print_fn("No audio was captured. Record for longer before tapping space again.")
        return "", {"delivered": "skipped", "reason": "no audio was captured"}
    buf = io.BytesIO()
    with wave.open(buf, "wb") as w:
        w.setnchannels(1)
        w.setsampwidth(2)
        w.setframerate(sample_rate)
        w.writeframes(b"".join(frames))
    result = client.transcribe(buf.getvalue())
    text = result.get("text", "")
    raw_text = result.get("raw_text", text)
    if raw_text != text:
        print_fn(f"Heard: {raw_text}")
    if not text:
        print_fn("No speech was heard. Record for longer before tapping space again.")
        return text, {"delivered": "skipped", "reason": "no speech was heard"}
    print_fn(text)
    deliver_result = client.deliver(pane, text, mode="preview")
    line = str(deliver_result.get("delivered", "?"))
    reason = deliver_result.get("reason")
    if reason:
        line = f"{line}. {reason}"
    print_fn(line)
    return text, deliver_result


class _SoundDeviceStream:
    """Adapt a real sounddevice input stream to the RecordStream protocol.

    sounddevice's own read(frames) blocks until frames samples exist, and
    its second return value means an overflow happened, not that more audio
    is waiting. This adapter reads only what the stream has already
    buffered, using read_available, so one held key captures the whole
    press instead of one fixed-size block.
    """

    def __init__(self, stream) -> None:
        self._stream = stream

    def start(self) -> None:
        self._stream.start()

    def stop(self) -> None:
        self._stream.stop()

    def read(self, frames: int) -> tuple[bytes, bool]:
        available = self._stream.read_available
        if available <= 0:
            return b"", False
        data, _overflowed = self._stream.read(available)
        more = self._stream.read_available >= frames
        return bytes(data), more


def _default_stream() -> "RecordStream":
    """Open the real microphone stream.

    Imported lazily so a headless box, or a --help invocation, never touches
    an audio device. Raises a plain sentence naming the voice extra when
    sounddevice is not installed.
    """
    try:
        import sounddevice as sd
    except ImportError as exc:
        raise RuntimeError(
            "sounddevice is not installed. Install it with: pip install 'opendaisugi[voice]'"
        ) from exc
    return _SoundDeviceStream(sd.RawInputStream(samplerate=16000, channels=1, dtype="int16"))


def run_ptt(
    pane: str,
    *,
    client: "VoiceClient",
    keys: Iterable[str],
    open_stream: Callable[[], "RecordStream"] | None = None,
    chunk_frames: int = 1600,
    print_fn: Callable[[str], None] = print,
) -> list[tuple[str, dict]]:
    """Drive one push-to-talk session from a sequence of keypresses.

    Space starts a recording, then stops it. q quits, stopping and
    discarding an in-progress recording first. Returns the text and delivery
    result pairs produced, one per full press and release cycle. A cycle
    that raises VoiceServerError prints the message and moves on, so one bad
    clip does not end the session.
    """
    open_stream = open_stream or _default_stream
    results: list[tuple[str, dict]] = []
    recording = False
    stream: "RecordStream | None" = None
    for ch in keys:
        if ch == "q":
            if recording and stream is not None:
                stream.stop()
            break
        if ch == " " and not recording:
            recording = True
            frames = []
            stream = open_stream()
            stream.start()
        elif ch == " " and recording:
            recording = False
            more = True
            while more:
                chunk, more = stream.read(chunk_frames)
                if chunk:
                    frames.append(chunk)
            stream.stop()
            try:
                text, result = record_and_send(frames, pane=pane, client=client, print_fn=print_fn)
            except VoiceServerError as exc:
                print_fn(str(exc))
                continue
            results.append((text, result))
    return results


def main_loop(
    pane: str,
    *,
    server_url: str,
    stdin=None,
    print_fn: Callable[[str], None] = print,
    config: "Config | None" = None,
) -> None:
    """Run one real interactive push-to-talk session.

    Uses raw terminal mode on a real tty, and reads plain characters
    otherwise, for example from a pipe in a test. All decision logic runs in
    run_ptt. Prints one line and returns, without opening a stream, when the
    client cannot be built, for example a missing token file.

    config is forwarded to HttpVoiceClient, which always reads it to
    resolve the bearer token file under the right data directory. Left
    unset, HttpVoiceClient reads the default config itself.
    """
    import sys

    stdin = stdin if stdin is not None else sys.stdin
    try:
        client = HttpVoiceClient(server_url, config=config)
    except VoiceServerError as exc:
        print_fn(str(exc))
        return
    print_fn(
        f"Push to talk on {pane} through {server_url}. "
        "Tap space to start recording. Tap it again to stop and send. q to quit."
    )
    is_tty = bool(getattr(stdin, "isatty", lambda: False)())
    old_settings = None
    if is_tty:
        import termios
        import tty

        fd = stdin.fileno()
        old_settings = termios.tcgetattr(fd)
        tty.setraw(fd)
    try:

        def _keys():
            while True:
                ch = stdin.read(1)
                if not ch:
                    return
                yield ch

        run_ptt(pane, client=client, keys=_keys(), print_fn=print_fn)
    finally:
        if is_tty and old_settings is not None:
            import termios

            termios.tcsetattr(stdin.fileno(), termios.TCSADRAIN, old_settings)
