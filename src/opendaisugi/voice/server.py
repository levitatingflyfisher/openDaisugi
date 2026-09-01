"""The stdlib HTTP server for the voice bridge. Answers GET /health, POST
/transcribe, and POST /deliver.

http.server, not an ASGI stack. This answers three endpoints on one local
surface. It does not proxy a wire protocol, so an async web stack would add
weight with no benefit.

Binds 127.0.0.1 by default. The token file lives at config.data_dir /
coppice / web / token. That is the file the coppice web server writes for
its own bearer token. Pass --token-file to point at a different file.

Every POST needs a bearer token, loopback included. Any local process, or
any page open in a browser on this box, can reach 127.0.0.1, so loopback
gets no special treatment. A request needs the Authorization header Bearer,
then the matching token, compared in constant time. Three bad tokens from
the same address ban that address for one minute, even if the next attempt
carries the right token. Building the server raises RuntimeError instead of
starting, on any address, when the token file is missing or unreadable,
checked once before the socket is bound and once after, against the address
actually bound.

GET /health is unauthenticated liveness only. It answers ok and carries no
session data and no pane data.

As defense in depth, /transcribe and /deliver also refuse a request that
carries an Origin header, or a Sec-Fetch-Site value other than same-origin
or none. A browser can still send some cross-origin requests with no
preflight, so this closes that path even when a bearer token has leaked.
/deliver also refuses a request whose Content-Type does not start with
application/json.

A request to /deliver with mode send is refused, before any pane backend is
built, unless the pane already holds a direct-send grant. The refusal names
the exact command that arms the pane. Mode preview never builds a pane
backend at all. The one arm decision is made inside deliver() itself, so a
concurrent arm change cannot land between a check here and a second check
there. /deliver also caps the request body and the text field, each
answering 413 with a sentence.
"""

from __future__ import annotations

import hmac
import ipaddress
import json
import logging
import ssl
import threading
import time
import wave
from collections.abc import Callable
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import TYPE_CHECKING
from urllib.parse import parse_qs, urlsplit

from opendaisugi.config import Config
from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor.backend import PaneRef
from opendaisugi.floor.registry import pick_backend, prompt_pane
from opendaisugi.voice import deliver as deliver_mod
from opendaisugi.voice.audio import AudioFormatUnsupported, to_wav_16k_mono, wav_duration_s
from opendaisugi.voice.cleanup import clean_transcript
from opendaisugi.voice.engines import EngineUnavailable, pick_engine

if TYPE_CHECKING:
    from opendaisugi.floor.backend import PaneBackend

_log = logging.getLogger("opendaisugi.voice.server")

MAX_AUDIO_SECONDS = 60.0
MAX_CONTENT_LENGTH_BYTES = 30_000_000  # a wire size ceiling, checked before any decode
MAX_DELIVER_BYTES = 65536  # a wire size ceiling for /deliver, checked before any read
MAX_DELIVER_TEXT_CHARS = 20000  # a ceiling on the parsed text field itself
_BAN_WINDOW_S = 60.0
_BAN_THRESHOLD = 3


def default_token_file(config: Config) -> Path:
    """The bearer token file the coppice web server writes for this data directory."""
    return config.data_dir / "coppice" / "web" / "token"


def default_armed_dir(config: Config) -> Path:
    """Where arm grants live for this data directory, when none is given."""
    return config.data_dir / "voice" / "armed"


def _read_token(token_file: Path) -> str | None:
    try:
        return token_file.read_text(encoding="utf-8").strip() or None
    except OSError:
        return None


def _content_length(headers) -> int | None:
    """The declared Content-Length, or None when it is missing, not a number, or negative."""
    raw = headers.get("Content-Length", "0")
    try:
        value = int(raw)
    except ValueError:
        return None
    return value if value >= 0 else None


def _is_loopback(address: str) -> bool:
    """True for the literal name localhost, and for any address in 127.0.0.0/8 or ::1."""
    if address == "localhost":
        return True
    try:
        return ipaddress.ip_address(address).is_loopback
    except ValueError:
        return False


def _extract_multipart_audio(body: bytes, content_type: str) -> tuple[bytes, str]:
    """Pull the first file part out of a multipart/form-data body.

    This does not use the cgi module. It was deprecated in Python 3.12 and
    removed in 3.13. A browser upload from MediaRecorder carries exactly one
    part, so splitting on the boundary is enough. Returns that part's raw
    bytes and its own Content-Type header value.
    """
    marker = "boundary="
    idx = content_type.find(marker)
    if idx == -1:
        raise ValueError("multipart request is missing a boundary")
    boundary = content_type[idx + len(marker) :].strip().strip('"').encode()
    for part in body.split(b"--" + boundary):
        if b"Content-Disposition" not in part:
            continue
        header_end = part.find(b"\r\n\r\n")
        if header_end == -1:
            continue
        headers = part[:header_end].decode("latin-1")
        payload = part[header_end + 4 :]
        if payload.endswith(b"\r\n"):
            payload = payload[:-2]
        part_type = "application/octet-stream"
        for line in headers.splitlines():
            if line.lower().startswith("content-type:"):
                part_type = line.split(":", 1)[1].strip()
        return payload, part_type
    raise ValueError("multipart request has no file part")


class VoiceRequestHandler(BaseHTTPRequestHandler):
    server_version = "opendaisugi-voice/1"

    # A stalled or slow-trickling body cannot park a handler thread forever.
    # http.server applies this to the whole connection, request line through
    # body, and turns an expired read into a closed connection, not a crash.
    timeout = 30.0

    def log_message(self, fmt: str, *args) -> None:  # route through logging, not stderr
        _log.info("%s - %s", self.client_address[0], fmt % args)

    def _authorized(self) -> bool:
        server: VoiceServer = self.server  # type: ignore[assignment]
        token = _read_token(server.token_file)
        header = self.headers.get("Authorization", "")
        supplied = header[len("Bearer ") :].strip() if header.startswith("Bearer ") else ""
        ok = token is not None and hmac.compare_digest(supplied, token)
        if not ok:
            server.record_auth_failure(self.client_address[0])
        return ok

    def _cross_origin_refusal(self) -> dict | None:
        """None when this request looks like it was built to call this server
        directly. A payload ready for _send_json otherwise.

        An Origin header, or a Sec-Fetch-Site value other than same-origin
        or none, means a browser sent this on a page's behalf. No first
        party caller, on this box or another, sends either header.
        """
        if "Origin" in self.headers:
            return {
                "error": "cross_origin_refused",
                "message": "This server does not accept requests carrying an Origin header. "
                "Call it directly, not from a browser page.",
            }
        fetch_site = self.headers.get("Sec-Fetch-Site")
        if fetch_site is not None and fetch_site not in ("same-origin", "none"):
            return {
                "error": "cross_origin_refused",
                "message": "This server does not accept cross-site requests. "
                "Call it directly, not from a browser page.",
            }
        return None

    def _send_json(self, status: int, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self) -> None:
        if urlsplit(self.path).path == "/health":
            self._send_json(200, {"ok": True})
            return
        self._send_json(
            404,
            {
                "error": "not_found",
                "message": "This server answers /health, /transcribe, and /deliver.",
            },
        )

    def do_POST(self) -> None:
        server: VoiceServer = self.server  # type: ignore[assignment]
        parsed = urlsplit(self.path)
        if "Transfer-Encoding" in self.headers and "Content-Length" not in self.headers:
            self._send_json(
                411,
                {
                    "error": "chunked_not_supported",
                    "message": "Send a Content-Length header. Chunked transfer encoding is not read.",
                },
            )
            return
        if server.is_banned(self.client_address[0]):
            self._send_json(
                429,
                {
                    "error": "too_many_failed_tokens",
                    "message": f"Too many failed tokens from this address. "
                    f"Wait {int(_BAN_WINDOW_S)} seconds and try again.",
                },
            )
            return
        if not self._authorized():
            self._send_json(
                401,
                {
                    "error": "unauthorized",
                    "message": "This request needs a bearer token. Run coppice web token "
                    "on the box that runs this server.",
                },
            )
            return
        if parsed.path not in ("/transcribe", "/deliver"):
            self._send_json(
                404,
                {
                    "error": "not_found",
                    "message": "This server answers /health, /transcribe, and /deliver.",
                },
            )
            return
        refusal = self._cross_origin_refusal()
        if refusal is not None:
            self._send_json(403, refusal)
            return
        try:
            if parsed.path == "/transcribe":
                want_cleanup = parse_qs(parsed.query).get("cleanup", ["0"])[0] == "1"
                self._handle_transcribe(server, want_cleanup=want_cleanup)
            else:
                self._handle_deliver(server)
        except Exception:
            _log.exception("unhandled error answering POST %s", parsed.path)
            self._send_json(
                500,
                {
                    "error": "internal_error",
                    "message": "Something went wrong handling this request. Check the "
                    "server log, then try again.",
                },
            )

    def _handle_transcribe(self, server: "VoiceServer", *, want_cleanup: bool) -> None:
        length = _content_length(self.headers)
        if length is None:
            self._send_json(
                400,
                {
                    "error": "bad_content_length",
                    "message": "Send a valid Content-Length header with the request.",
                },
            )
            return
        if length > MAX_CONTENT_LENGTH_BYTES:
            self._send_json(
                413,
                {
                    "error": "payload_too_large",
                    "message": f"The request body must be {MAX_CONTENT_LENGTH_BYTES} "
                    "bytes or smaller.",
                },
            )
            return
        body = self.rfile.read(length)
        content_type = self.headers.get("Content-Type", "application/octet-stream")
        try:
            if content_type.startswith("multipart/form-data"):
                audio_bytes, inner_type = _extract_multipart_audio(body, content_type)
            else:
                audio_bytes, inner_type = body, content_type
            wav_bytes = to_wav_16k_mono(audio_bytes, inner_type)
        except (AudioFormatUnsupported, ValueError, wave.Error, RuntimeError) as exc:
            _log.warning("could not decode an uploaded clip: %s", exc)
            self._send_json(
                400,
                {
                    "error": "bad_audio",
                    "message": "This audio could not be decoded. Send WAV, WebM, Ogg, MP3, or M4A.",
                },
            )
            return
        duration_s = wav_duration_s(wav_bytes)
        if duration_s > MAX_AUDIO_SECONDS:
            self._send_json(
                413,
                {
                    "error": "audio_too_long",
                    "max_seconds": MAX_AUDIO_SECONDS,
                    "message": f"The clip is longer than {MAX_AUDIO_SECONDS:.0f} seconds. "
                    "Send a shorter clip.",
                },
            )
            return
        try:
            transcript = server.engine.transcribe(wav_bytes)
        except EngineUnavailable as exc:
            self._send_json(503, {"error": "engine_unavailable", "message": str(exc)})
            return
        text = transcript.text
        cleaned = False
        reason: str | None = None
        if want_cleanup:
            result = clean_transcript(text, config=server.config)
            text = result.text
            cleaned = result.cleaned
            reason = result.reason
        payload = {
            "text": text,
            "raw_text": transcript.text,
            "duration_s": transcript.duration_s,
            "rtf": transcript.rtf,
            "engine": server.engine.name,
            "cleaned": cleaned,
        }
        if reason is not None:
            payload["reason"] = reason
        self._send_json(200, payload)

    def _handle_deliver(self, server: "VoiceServer") -> None:
        content_type = self.headers.get("Content-Type", "")
        if not content_type.startswith("application/json"):
            self._send_json(
                400,
                {
                    "error": "bad_content_type",
                    "message": "Send Content-Type: application/json.",
                },
            )
            return
        length = _content_length(self.headers)
        if length is None:
            self._send_json(
                400,
                {
                    "error": "bad_content_length",
                    "message": "Send a valid Content-Length header with the request.",
                },
            )
            return
        if length > MAX_DELIVER_BYTES:
            self._send_json(
                413,
                {
                    "error": "payload_too_large",
                    "message": f"The request body must be {MAX_DELIVER_BYTES} bytes or smaller.",
                },
            )
            return
        try:
            payload = json.loads(self.rfile.read(length) or b"{}")
        except json.JSONDecodeError:
            self._send_json(
                400,
                {
                    "error": "bad_json",
                    "message": "Send a JSON object with pane, text, and mode.",
                },
            )
            return
        pane_id = payload.get("pane")
        text = payload.get("text", "")
        mode = payload.get("mode", "preview")
        backend_name = payload.get("backend")
        if not isinstance(pane_id, str) or not pane_id:
            self._send_json(
                400, {"error": "bad_request", "message": "pane must be a non-empty string."}
            )
            return
        if not isinstance(text, str) or not text:
            self._send_json(
                400, {"error": "bad_request", "message": "text must be a non-empty string."}
            )
            return
        if len(text) > MAX_DELIVER_TEXT_CHARS:
            self._send_json(
                413,
                {
                    "error": "text_too_long",
                    "message": f"text must be {MAX_DELIVER_TEXT_CHARS} characters or fewer.",
                },
            )
            return
        if backend_name is not None and not isinstance(backend_name, str):
            self._send_json(400, {"error": "bad_request", "message": "backend must be a string."})
            return
        if mode not in ("preview", "send"):
            self._send_json(
                400, {"error": "bad_request", "message": "mode must be preview or send."}
            )
            return
        # The pane's backend name is set from the payload, or left unknown.
        # It cannot default to a real backend's own name here: building one
        # is deliver()'s job, done only once it has made the one arm
        # decision, never ahead of that decision just to read a name off it.
        # prompt_pane dispatches on backend.list() and ref.id, so it never
        # reads ref.backend at all; the literal unknown never reaches a
        # decision.
        pane = PaneRef(backend=backend_name or "unknown", id=pane_id)
        try:
            result = deliver_mod.deliver(
                pane,
                text,
                server.backend_factory,
                mode=mode,
                armed_dir=server.armed_dir,
                send=server.send,
            )
        except FloorNotAvailable as exc:
            self._send_json(503, {"error": "no_pane_backend", "message": str(exc)})
            return
        status = 200 if result.delivered != "refused" else 403
        self._send_json(status, {"delivered": result.delivered, "reason": result.reason})


class VoiceServer(ThreadingHTTPServer):
    def __init__(
        self,
        address: tuple[str, int],
        *,
        config: Config,
        engine,
        token_file: Path,
        armed_dir: Path,
        backend_factory: Callable[[], "PaneBackend"],
        send: Callable[..., str] | None = None,
        tls_cert: Path | None = None,
        tls_key: Path | None = None,
    ) -> None:
        super().__init__(address, VoiceRequestHandler)
        self.config = config
        self.engine = engine
        self.token_file = token_file
        self.armed_dir = armed_dir
        self.backend_factory = backend_factory
        self.send = send if send is not None else prompt_pane
        self._auth_failures: dict[str, list[float]] = {}
        self._auth_lock = threading.Lock()
        if tls_cert is not None or tls_key is not None:
            if tls_cert is None or tls_key is None:
                # super().__init__ above already bound and listened on the
                # socket. Closing it here means a half-given pair leaves
                # nothing open behind it.
                self.server_close()
                raise ValueError("pass both --tls-cert and --tls-key, or pass neither")
            ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            ctx.load_cert_chain(certfile=str(tls_cert), keyfile=str(tls_key))
            self.socket = ctx.wrap_socket(self.socket, server_side=True)

    def _prune_expired_addresses(self, now: float) -> None:
        """Drop every address whose whole window has expired, not only the caller's own.

        Must run with self._auth_lock held. An address that fails once and is
        never seen again would otherwise sit in the ledger forever.
        """
        for address in list(self._auth_failures):
            hits = [t for t in self._auth_failures[address] if now - t < _BAN_WINDOW_S]
            if hits:
                self._auth_failures[address] = hits
            else:
                del self._auth_failures[address]

    def record_auth_failure(self, address: str) -> None:
        now = time.time()
        with self._auth_lock:
            self._prune_expired_addresses(now)
            self._auth_failures.setdefault(address, []).append(now)

    def is_banned(self, address: str) -> bool:
        now = time.time()
        with self._auth_lock:
            self._prune_expired_addresses(now)
            return len(self._auth_failures.get(address, [])) >= _BAN_THRESHOLD


def build_server(
    host: str,
    port: int,
    *,
    config: Config,
    engine,
    token_file: Path,
    armed_dir: Path | None = None,
    backend_factory: Callable[[], "PaneBackend"],
    send: Callable[..., str] | None = None,
    tls_cert: Path | None = None,
    tls_key: Path | None = None,
) -> VoiceServer:
    """Build, but do not start, a VoiceServer.

    Refuses to build a server on any address, loopback included, with no
    readable token file. Every POST needs a bearer token now, so a server
    that could never check one would only ever answer 401. Checks the host
    string first, then checks again against the address the socket actually
    bound, since a name can resolve to a different address than it names.
    armed_dir defaults to default_armed_dir(config) when left unset. send is
    the callable /deliver uses to reach a pane in send mode. Leave it unset
    to use registry.prompt_pane, the real one.
    """

    def _refuse(address: str) -> None:
        raise RuntimeError(
            f"refusing to listen on {address} without a token. {token_file} is missing "
            f"or unreadable. Run coppice web token first, or pass --token-file."
        )

    if _read_token(token_file) is None:
        _refuse(host)
    resolved_armed_dir = armed_dir if armed_dir is not None else default_armed_dir(config)
    server = VoiceServer(
        (host, port),
        config=config,
        engine=engine,
        token_file=token_file,
        armed_dir=resolved_armed_dir,
        backend_factory=backend_factory,
        send=send,
        tls_cert=tls_cert,
        tls_key=tls_key,
    )
    bound_address = server.server_address[0]
    if _read_token(token_file) is None:
        server.server_close()
        _refuse(bound_address)
    return server


def serve(
    *,
    host: str = "127.0.0.1",
    port: int = 7477,
    config: Config,
    token_file: Path | None = None,
    armed_dir: Path | None = None,
    tls_cert: Path | None = None,
    tls_key: Path | None = None,
    on_bound: Callable[[], None] | None = None,
) -> None:
    """Build the real engine and the real backend, then serve forever.

    Blocks until the process stops. pick_engine can raise EngineUnavailable
    when the configured engine's package or model files are missing, or
    UnknownEngine when voice_engine names something other than
    faster-whisper or parakeet. Building the server can raise OSError, for
    example a port already in use, or ValueError, for example a half-given
    TLS pair. All of these propagate to the caller, uncaught, so the command
    line can turn each into its own exit code.

    on_bound, when given, runs once the socket is bound and before this
    blocks in serve_forever. A caller prints its own listening line there,
    so nothing is printed that claims the server is listening before the
    bind actually succeeded.
    """
    engine = pick_engine(config)
    resolved_token_file = token_file if token_file is not None else default_token_file(config)
    server = build_server(
        host,
        port,
        config=config,
        engine=engine,
        token_file=resolved_token_file,
        armed_dir=armed_dir,
        backend_factory=lambda: pick_backend(config),
        tls_cert=tls_cert,
        tls_key=tls_key,
    )
    if on_bound is not None:
        on_bound()
    server.serve_forever()
