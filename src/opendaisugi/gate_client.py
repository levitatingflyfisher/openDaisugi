"""The hook's entry: ask the resident gate, else run the gate in-process.

Stdlib only until the fallback, so the hot path is ~40 ms of Python startup
plus one socket round trip. Every failure of the server path (no socket,
refused, timeout, malformed reply, or a socket that isn't a private one we
trust) falls back to the real gate. There is no path from a broken or rogue
server to an allow.
"""

from __future__ import annotations

import base64
import json
import os
import socket
import stat
import sys
from collections.abc import Mapping
from pathlib import Path

SOCK_NAME = "gate.sock"
_DEFAULT_ROOT = Path.home() / ".opendaisugi" / "gate"
_SERVER_TIMEOUT_S = 5.0  # well under any host hook timeout, so the fallback still fits


def _root_from_argv(argv: list[str]) -> Path:
    for i, a in enumerate(argv):
        if a == "--root" and i + 1 < len(argv):
            return Path(argv[i + 1])
        if a.startswith("--root="):
            return Path(a.split("=", 1)[1])
    return _DEFAULT_ROOT


def _socket_is_trustworthy(sock_path: Path) -> bool:
    """A private (0600, owned by us, a real socket — never a symlink target
    we didn't verify) file at ``sock_path``.

    ``Path.exists()`` follows symlinks, so it is not used here: a rogue
    process could otherwise plant a symlink to a socket it controls and have
    the client trust it. ``os.lstat`` inspects the path entry itself.
    """
    try:
        st = os.lstat(sock_path)
    except OSError:
        return False
    if not stat.S_ISSOCK(st.st_mode):
        return False
    if st.st_uid != os.getuid():
        return False
    if stat.S_IMODE(st.st_mode) != 0o600:
        return False
    return True


def caller_pane_fields(env: Mapping[str, str]) -> dict[str, str]:
    """This caller's own coppice/Herdr pane identity, read from ITS OWN
    environment. This process runs as the actual hook subprocess inside
    whatever pane started it, so its environment IS the caller's identity.
    Carried as explicit request fields so the resident gate, a separate
    long-lived process with no pane environment of its own (gate_server.py's
    serve() drops COPPICE_SOCK/COPPICE_PANE/HERDR_PANE_ID/HERDR_PANE at
    start), can still report for THIS caller rather than for itself or for
    nobody. Empty when this caller has no pane identity to report: a bare
    COPPICE_PANE with no matching COPPICE_SOCK, or neither, sends nothing
    for coppice; report_state fails closed on an incomplete pair either way,
    this just avoids sending one.
    """
    fields: dict[str, str] = {}
    sock = env.get("COPPICE_SOCK")
    pane = env.get("COPPICE_PANE")
    if sock and pane:
        fields["coppice_sock"] = sock
        fields["coppice_pane"] = pane
    herdr_pane = env.get("HERDR_PANE_ID") or env.get("HERDR_PANE")
    if herdr_pane:
        fields["herdr_pane"] = herdr_pane
    # The coppice data directory this pane's server keeps its secrets in.
    # The resident gate guards it as it guards the default one. Only an
    # absolute path is sent, since coppice always sets one.
    data_dir = env.get("COPPICE_DATA_DIR")
    if data_dir and os.path.isabs(data_dir):
        fields["coppice_data_dir"] = data_dir
    return fields


def ask_server(
    sock_path: Path,
    argv: list[str],
    raw: bytes,
    *,
    timeout_s: float = _SERVER_TIMEOUT_S,
    caller_pane: Mapping[str, str] | None = None,
) -> dict | None:
    """One round trip. None on any failure; the caller then runs the gate itself.

    ``caller_pane`` (see ``caller_pane_fields``) rides on the request as
    extra top-level fields, read by gate_server.py's handler and forwarded
    to whatever report this call triggers server-side. Omitted, or given as
    an empty mapping, and the request carries none, same as before this
    existed.
    """
    if not _socket_is_trustworthy(sock_path):
        return None
    try:
        with socket.socket(socket.AF_UNIX, socket.SOCK_STREAM) as s:
            s.settimeout(timeout_s)
            s.connect(str(sock_path))
            req: dict[str, object] = {
                "v": 1,
                "argv": list(argv),
                "stdin_b64": base64.b64encode(raw).decode(),
            }
            if caller_pane:
                req.update(caller_pane)
            s.sendall(json.dumps(req).encode() + b"\n")
            buf = b""
            while not buf.endswith(b"\n"):
                chunk = s.recv(65536)
                if not chunk:
                    return None
                buf += chunk
        reply = json.loads(buf)
        if not isinstance(reply, dict) or reply.get("v") != 1:
            return None
        if not all(k in reply for k in ("stdout", "stderr", "exit_code")):
            return None
        if not isinstance(reply["exit_code"], int):
            return None
        return reply
    except (OSError, ValueError):
        return None


def _has_ask_flag(argv: list[str]) -> bool:
    return any(a == "--ask" or a.startswith("--ask=") for a in argv)


def main(argv: list[str] | None = None) -> int:
    argv = list(sys.argv[1:] if argv is None else argv)
    try:
        raw = sys.stdin.buffer.read()
    except Exception:  # noqa: BLE001
        raw = b""
    # --ask can legitimately block for up to --ask-timeout seconds waiting on
    # a present operator, so it always runs straight in-process — never
    # through the resident server. Going through the server first is a
    # double loss here: the round trip buys nothing when the request is
    # about to take up to --ask-timeout seconds anyway; _SERVER_TIMEOUT_S
    # (well under any NORMAL hook timeout, but far short of an ask) would
    # abandon the server's in-flight ask and pay import + a FRESH ask cycle
    # on top, which can bust the host's own outer hook timeout — and that
    # outer timeout fails OPEN; and the abandoned server thread would keep
    # polling the OLD nonce while this fallback's post_ask mints a new one,
    # so the operator's one real answer can be consumed and destroyed by the
    # stale server-side wait as a mismatch (a lost answer, not a security
    # hole — ask.py still denies — but a real usability break). One process,
    # one ask cycle, one nonce.
    reply = (
        None
        if _has_ask_flag(argv)
        else ask_server(
            _root_from_argv(argv) / SOCK_NAME,
            argv,
            raw,
            caller_pane=caller_pane_fields(os.environ),
        )
    )
    if reply is None:
        from opendaisugi.gate import run_argv  # the slow, correct path

        out = run_argv(argv, raw)
        reply = {"stdout": out.stdout, "stderr": out.stderr, "exit_code": out.exit_code}
    try:
        if reply["stdout"]:
            print(reply["stdout"])
        if reply["stderr"]:
            print(reply["stderr"], file=sys.stderr)
    except Exception:  # noqa: BLE001 — a broken stdout must not un-deny
        pass
    return int(reply["exit_code"])


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
