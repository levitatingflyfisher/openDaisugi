"""The native pane backend: a stdlib JSONL client for coppice-server.

Most verbs get one connection per call: `pane.create`, `pane.list`,
`pane.read` and so on. `subscribe()`, `attach()` and `detach()` share one
long-lived connection instead. Stdlib socket and json only, because this
runs in the cockpit, on a phone-facing box, and inside a hook process that
must stay cheap.

`available()` costs one `server.status` round trip with a 300 ms budget.
Whether it may start a server is the CALLER's choice, threaded through
`registry.pick_backend(..., autostart=...)`:

* `backend: auto` and `daisugi coppice backends` pass `autostart=False`.
  Opening a screen, or asking a diagnostic a question, must not spawn a
  daemon.
* `daisugi coppice <verb> --backend coppice` passes `autostart=True`. The
  operator named coppice, so starting it is what they asked for.

Even with `autostart=True` the start is tried once per process and a
failure returns False rather than raising.

Events are validated before they leave here. `PaneStateEvent.from_json`
rejects a `state` or `source` outside the closed enum, in its own
`__post_init__`, the same as every other malformed event. `_validated`
catches that raise and turns it into no event, so a server bug that emits
`state: "ready"` never paints an unknown word where `unknown` belongs; the
pane just shows no state at all.

Frames arrive as they come. Coalescing is the widget's job, not ours.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import stat
import subprocess
import threading
from collections.abc import Iterator, Mapping
from pathlib import Path
from typing import Literal

from opendaisugi.exceptions import OpenDaisugiError
from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent

_PROBE_S = 0.3
_CALL_S = 5.0
_READ_SOURCES = ("visible", "recent", "detection")
# A long-running backend keeps failing attaches or detaches for as long as
# an operator leaves a stale screen open. Capped so _event_errors stays
# small; the oldest entry is dropped to make room for the newest.
_EVENT_ERRORS_MAX = 32

_REPO_ROOT = Path(__file__).resolve().parents[3]
_KEYS_JSON = _REPO_ROOT / "harness" / "coppice" / "testdata" / "keys.json"


def _load_known_keys() -> frozenset[str]:
    """coppice's own named-key vocabulary, straight from the Go source's own
    recorded list. Empty when the file is missing rather than raising:
    importing this module must never fail because a fixture is absent."""
    try:
        return frozenset(json.loads(_KEYS_JSON.read_text()))
    except (OSError, ValueError):
        return frozenset()


KNOWN_KEYS: frozenset[str] = _load_known_keys()


def _validated(raw: dict | None) -> PaneStateEvent | None:
    """A PaneStateEvent built from raw, or None when raw cannot become one.

    `PaneStateEvent.from_json` already rejects an out-of-enum `state` or
    `source`: constructing the dataclass runs `__post_init__`, which raises
    `ValueError` on a word outside `STATES` or `SOURCES`, and `from_json`
    re-raises that. This function's whole job is catching that raise, and
    every other malformed-event raise beside it, and turning it into None.
    A word we do not know is no state, never a painted guess.
    """
    if not raw:
        return None
    try:
        return PaneStateEvent.from_json(json.dumps(raw))
    except (ValueError, KeyError, TypeError):
        return None


def _state_from_pane_row(row: dict) -> PaneStateEvent | None:
    """Reassemble a PaneStateEvent from `pane.list`'s flat state columns.

    `pane.list` never nests an event object. `state`, `source`, `detail`
    and an optional `ask` sit beside the pane's own fields, the shape
    `internal/server/panes.go`'s `handlePaneList` sends. `ts` is the tell
    for whether a stored event exists at all: it and `session_id` are
    ABSENT from the row, not null, when the pane has never reported one.
    Absence means no state here too, never a fabricated receive-time stamp.

    `harness` comes from the pane's own registered harness, `row["harness"]`,
    set at `pane.create` time. `pane.list` does not expose whatever harness
    the stored event itself carried, so a pane whose event reported a
    different harness than the one it was created with shows the pane's own
    value here, not the event's.
    """
    if "ts" not in row:
        return None
    raw: dict[str, object] = {
        "v": 1,
        "session_id": row.get("session_id", ""),
        "harness": row.get("harness") or "",
        "state": row.get("state"),
        "source": row.get("source"),
        "ts": row.get("ts"),
        "detail": row.get("detail", ""),
        "pane": row.get("id"),
    }
    if row.get("ask") is not None:
        raw["ask"] = row["ask"]
    return _validated(raw)


class CoppiceError(OpenDaisugiError):
    """coppice-server answered `ok: false`. ``code`` is one of its closed enum."""

    def __init__(self, code: str, message: str) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code


def default_socket_path(env: Mapping[str, str] | None = None) -> Path:
    """`$XDG_RUNTIME_DIR/coppice/server.sock`, else `~/.opendaisugi/coppice/server.sock`.

    Matches `harness/coppice/internal/server/paths.go` `SocketPath` exactly.
    """
    env = os.environ if env is None else env
    runtime = env.get("XDG_RUNTIME_DIR")
    if runtime:
        return Path(runtime) / "coppice" / "server.sock"
    return Path.home() / ".opendaisugi" / "coppice" / "server.sock"


def default_data_dir(env: Mapping[str, str] | None = None) -> Path:
    """`~/.opendaisugi/coppice`. Never the runtime dir, which the system clears on logout.

    Matches `harness/coppice/internal/server/paths.go` `DataDir` exactly.
    `env` is accepted for symmetry with `default_socket_path` and is unused:
    Go's own `DataDir` reads no environment variable either.
    """
    del env
    return Path.home() / ".opendaisugi" / "coppice"


class CoppiceBackend:
    """Speak the coppice socket API. The only backend that yields frames."""

    name = "coppice"
    yields_frames = True  # only coppice streams frames; herdr and tmux yield text

    def __init__(
        self,
        sock_path: Path | None = None,
        data_dir: Path | None = None,
        *,
        autostart: bool = False,
        timeout_s: float = _PROBE_S,
    ) -> None:
        self.sock_path = Path(sock_path) if sock_path else default_socket_path()
        self.data_dir = Path(data_dir) if data_dir else default_data_dir()
        self._autostart = autostart
        self._probe_s = timeout_s
        self._start_tried = False
        self._seq = 0
        # Guards _seq and _events_generation, the two counters _call,
        # _send_event_cmd and subscribe() all touch from different
        # threads in a running cockpit: the roster poll, the UI thread's
        # attach or detach, and the pump.
        self._lock = threading.Lock()
        self._events_sock: socket.socket | None = None
        self._events_stream = None
        # Bumped every time _events_conn() opens a fresh connection, and
        # every time _close_locked() closes one. subscribe() records the
        # value current when its own connection was opened, so its finally
        # can tell whether that connection is still the live one before
        # closing it.
        self._events_generation = 0
        self._event_errors: dict[str, CoppiceError] = {}

    def _next_id(self) -> str:
        """The next request id. Thread-safe: a plain increment can hand two
        concurrent callers the same id, and a last_error(id) lookup would
        then answer for the wrong request."""
        with self._lock:
            self._seq += 1
            return str(self._seq)

    # --- wire, one connection per call -------------------------------------
    def _is_socket(self) -> bool:
        try:
            st = os.lstat(self.sock_path)
        except OSError:
            return False
        return stat.S_ISSOCK(st.st_mode) and st.st_uid == os.getuid()

    def _connect(self, timeout_s: float | None) -> socket.socket:
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.settimeout(timeout_s)
        sock.connect(str(self.sock_path))
        return sock

    def _call(self, cmd: str, timeout_s: float = _CALL_S, **fields) -> dict:
        """One request, one reply. Raises CoppiceError on `ok: false`, on a
        dropped connection, and on a reply this client cannot parse as
        JSON: every wire failure is the one exception type a caller has to
        catch."""
        request = {"id": self._next_id(), "cmd": cmd, **fields}
        with self._connect(timeout_s) as sock, sock.makefile("rwb") as stream:
            stream.write(json.dumps(request).encode() + b"\n")
            stream.flush()
            line = stream.readline()
        if not line:
            raise CoppiceError("internal", f"coppice closed the connection on {cmd}")
        try:
            reply = json.loads(line)
        except ValueError as exc:
            raise CoppiceError("internal", f"coppice sent a reply that is not JSON: {exc}") from exc
        if not reply.get("ok"):
            err = reply.get("error") or {}
            raise CoppiceError(str(err.get("code", "internal")), str(err.get("message", "")))
        return reply.get("result") or {}

    def _try_start(self) -> None:
        """Start the server once per process, at self.sock_path / self.data_dir.

        coppice's CLI takes `--socket` and `--data-dir` as global flags,
        before the verb; a start with neither would come up on the
        operator's own default paths regardless of what this backend was
        pointed at. COPPICE_NO_AUTOSTART is honoured directly, before the
        binary is even looked up: the real coppice binary refuses too, at
        its own StartBackgroundServer, but checking here as well means a
        caller that set the variable never pays for a fork it already told
        this process not to make.
        """
        if os.environ.get("COPPICE_NO_AUTOSTART"):
            return
        if self._start_tried or shutil.which("coppice") is None:
            return
        self._start_tried = True
        try:
            subprocess.run(
                [
                    "coppice",
                    "--socket",
                    str(self.sock_path),
                    "--data-dir",
                    str(self.data_dir),
                    "server",
                    "start",
                ],
                capture_output=True,
                timeout=10.0,
                check=False,
            )
        except (OSError, subprocess.SubprocessError):
            return

    # --- the shared events connection --------------------------------------
    def _events_conn(self):
        """The one connection `subscribe()`, `attach()` and `detach()` share.

        Opened once and kept until `subscribe()`'s own read loop ends, for
        any reason; the next call after that opens a fresh one, since
        `subscribe()` clears `_events_stream` on its way out. `attach()`
        and `detach()` write their request here and do not wait for a
        matched reply. Nothing else safely could: each request the server
        gets runs on its own goroutine, and the frame pump for an
        already-attached pane runs on yet another, and both write through
        the same connection's encoder with no ordering guarantee between
        them. The encoder's own mutex guarantees one whole line at a time,
        never which line comes first. The real `coppice attach` client
        reads the exact same way: one loop dispatched by `id` versus
        `event`. See `internal/attach/attach.go`'s `case r := <-lines`
        block, lines 568 through 590. It does more than just read that way,
        though: a failed attach ends its session with the error, and every
        other failed reply is written to the operator's screen, at lines
        577 through 587 of the same block. This client's `subscribe()`
        records every `ok: false` reply the same way, keyed by request id,
        for `last_error()` to answer instead of printing it, since nothing
        here owns a screen.

        Opening a new connection bumps `_events_generation`. `subscribe()`
        needs both the stream and the generation it belongs to, read
        under the one lock acquisition so the two can never disagree;
        `_events_conn_with_generation` is that pair, and this method is
        its stream-only half for `attach()`/`detach()`, which have no use
        for the generation.
        """
        return self._events_conn_with_generation()[0]

    def _events_conn_with_generation(self) -> tuple[object, int]:
        """`_events_conn`'s connection, plus the generation it belongs to.

        The lock spans only the connect call itself, a socket connect on
        a local Unix socket, not the request/reply that follows.
        """
        with self._lock:
            if self._events_stream is None:
                self._events_sock = self._connect(None)
                self._events_stream = self._events_sock.makefile("rwb")
                self._events_generation += 1
            return self._events_stream, self._events_generation

    def _send_event_cmd(self, cmd: str, **fields) -> str:
        """Write one request on the shared events connection. Returns its
        id, for a caller that wants to check `last_error(id)` later."""
        stream = self._events_conn()
        request_id = self._next_id()
        request = {"id": request_id, "cmd": cmd, **fields}
        stream.write(json.dumps(request).encode() + b"\n")
        stream.flush()
        return request_id

    # --- PaneBackend ---------------------------------------------------------
    def available(self) -> bool:
        """A socket that answers `server.status` inside the probe budget. Never raises."""
        try:
            if self._is_socket():
                self._call("server.status", timeout_s=self._probe_s)
                return True
        except Exception:  # noqa: BLE001 - available() must never take the floor down
            pass
        if not self._autostart:
            return False
        self._try_start()
        try:
            if self._is_socket():
                self._call("server.status", timeout_s=self._probe_s)
                return True
        except Exception:  # noqa: BLE001
            return False
        return False

    def spawn(
        self,
        *,
        cwd: Path,
        cmd: list[str],
        env: dict[str, str],
        label: str,
        kind: Literal["pty", "headless"] = "pty",
        harness: str | None = None,
    ) -> PaneRef:
        result = self._call(
            "pane.create",
            cwd=str(cwd),
            cmd_argv=list(cmd),
            env=dict(env),
            label=label,
            kind=kind,
            harness=harness,
        )
        return PaneRef(backend=self.name, id=str(result["pane"]))

    def _panes_from_result(self, result: dict) -> list[PaneInfo]:
        out: list[PaneInfo] = []
        for row in result.get("panes", []):
            out.append(
                PaneInfo(
                    ref=PaneRef(self.name, str(row["id"])),
                    label=str(row.get("label", "")),
                    cwd=str(row.get("cwd", "")),
                    cmd=list(row.get("cmd", [])),
                    kind=str(row.get("kind", "pty")),
                    state=_state_from_pane_row(row),
                )
            )
        return out

    def list(self) -> list[PaneInfo]:
        """Every pane the server knows about, or an empty list.

        Master §3.2: only spawn, send_text and send_keys may raise. A
        socket nothing is listening on, whether never created or long
        gone, raises OSError at connect time, before any reply exists to
        wrap into a CoppiceError; that is the honest shape of a host that
        is gone, degraded here to an empty roster rather than a crash.
        """
        try:
            result = self._call("pane.list")
        except OSError:
            return []
        return self._panes_from_result(result)

    def list_proven(self) -> tuple[bool, list[PaneInfo]]:
        """Whether pane.list truly answered, and what it said.

        An OSError at connect time, whether the socket was never created
        or is long gone, means the call itself never happened: unproven,
        not an honestly empty roster.
        """
        try:
            result = self._call("pane.list")
        except OSError:
            return False, []
        return True, self._panes_from_result(result)

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._call("pane.send_text", pane=pane.id, text=text, enter=enter)

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        """Send named or literal keys. coppice's own vocabulary, mirrored
        from testdata/keys.json into KNOWN_KEYS, IS the floor's vocabulary
        here. Unlike tmux there is no translation table, so this passes
        keys straight through and lets the server's own bad_request name
        what it does not know."""
        self._call("pane.send_keys", pane=pane.id, keys=list(keys))

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        """The pane's text from source, or the empty string when the host is gone."""
        if source not in _READ_SOURCES:
            raise ValueError(
                f"coppice has no read source {source!r}. Known sources: {', '.join(_READ_SOURCES)}."
            )
        try:
            return str(self._call("pane.read", pane=pane.id, source=source).get("text", ""))
        except OSError:
            return ""

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        self._call("pane.resize", pane=pane.id, cols=cols, rows=rows)

    def close(self, pane: PaneRef) -> None:
        """Close one pane. Already closed, or the host already gone, both
        mean close() already achieved what it was asked for."""
        try:
            self._call("pane.close", pane=pane.id)
        except CoppiceError as exc:
            if exc.code not in ("no_such_pane", "pane_closed"):
                raise
        except OSError:
            return None

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        self._call("pane.report_state", pane=pane.id, event=json.loads(ev.to_json()))

    def prompt(
        self, pane: PaneRef, text: str, *, wait: bool = False, timeout_s: float = 60.0
    ) -> str:
        """A real agent prompt, not typed keystrokes. Used for headless panes."""
        self._call(
            "agent.prompt",
            pane=pane.id,
            text=text,
            wait=wait,
            timeout_ms=int(timeout_s * 1000),
            timeout_s=max(timeout_s + 1.0, _CALL_S),
        )
        return "sent"

    def attach(self, pane: PaneRef, cols: int, rows: int) -> str:
        """Start this pane's frame pump on the shared events connection.

        `events.subscribe` never delivers a frame for a pane nothing has
        attached. See `internal/server/attach.go`'s own `handleSubscribe`
        comment. Calling `subscribe()` alone, or listing panes, attaches
        nothing: a caller wanting live frames for one pane must call this
        first.

        Returns the request id. The write does not wait for a reply, so a
        failed attach, a pane that already closed for instance, raises
        nothing here; `subscribe()` must be iterated at least once past the
        reply for `last_error(id)` to answer it. The real `coppice attach`
        client treats its own failed attach as fatal, ending the session
        with the error, and writes every other failed reply to the
        operator's screen; this client hands both the same way to whatever
        reads `last_error`, since nothing here owns a screen of its own.
        """
        return self._send_event_cmd("pane.attach", pane=pane.id, cols=cols, rows=rows)

    def detach(self, pane: PaneRef) -> str:
        """Stop this pane's frame pump. State events for other panes, or for
        this one, keep flowing on the shared connection if `subscribe()`
        already asked for them broadly. Returns the request id, the same
        as `attach()`, for `last_error(id)`."""
        return self._send_event_cmd("pane.detach", pane=pane.id)

    def last_error(self, request_id: str) -> CoppiceError | None:
        """The `ok: false` reply `subscribe()`'s loop recorded for
        request_id, or None. Only meaningful once `subscribe()` has been
        iterated past that reply: nothing pumps the events connection on
        its own, so a caller that never iterates `subscribe()` never sees
        one recorded here either."""
        return self._event_errors.get(request_id)

    def _close_locked(self) -> None:
        """The actual close, run only while holding self._lock.

        shutdown() runs first, on the raw socket, before either object is
        closed. A thread abandoned mid-subscribe is blocked reading through
        the buffered stream, holding its internal lock for the length of
        that read; closing the stream from a different thread waits on the
        same lock and never returns. shutdown() ends the blocked read at
        the kernel, releasing that lock, so the close calls that follow,
        from either thread, land on an already-finished read instead of a
        live one. Must call nothing that takes self._lock itself: a plain
        threading.Lock is not reentrant.
        """
        if self._events_sock is not None:
            try:
                self._events_sock.shutdown(socket.SHUT_RDWR)
            except OSError:
                pass
        if self._events_stream is not None:
            try:
                self._events_stream.close()
            except OSError:
                pass
        if self._events_sock is not None:
            try:
                self._events_sock.close()
            except OSError:
                pass
        self._events_sock = None
        self._events_stream = None
        self._events_generation += 1

    def close_connection(self) -> None:
        """Release the shared events connection, if one is open.

        subscribe(), attach() and detach() all write on this one socket.
        subscribe()'s own loop calls this too when it ends, for any
        reason, so the connection never outlives the generator that reads
        it. A caller that only ever used attach() or detach(), and never
        iterated subscribe(), would otherwise leak the file descriptor for
        the life of the backend. Optional member of PaneBackend; a backend
        with no long-lived connection of its own has nothing to release.
        Safe to call with nothing open, and safe to call more than once.

        Runs under self._lock, the same lock _events_conn_with_generation
        takes to open a connection: reading self._events_sock and
        self._events_stream, then acting on what was read, must be one
        critical section, or a connection opened between the read and the
        act would be the one this method actually closes. Bumping the
        generation here too means a subscribe() generator whose own
        finally runs afterward, against a generation this call has since
        moved past, correctly finds a mismatch and leaves whatever is
        open now alone.
        """
        with self._lock:
            self._close_locked()

    def subscribe(self) -> Iterator[PaneStateEvent | Frame]:
        """State and frame events on the shared events connection, forever.

        Sends `events.subscribe` for every pane's state the first time this
        is iterated. `attach()`/`detach()` write on the SAME connection,
        the one `_events_conn` opens; their replies interleave here with
        state and frame events. A reply with no `event` key is this
        subscribe's own ack or an attach/detach reply, never yielded.

        The reply to subscribe's OWN request is different from the rest: an
        `ok: false` answer to it means the whole subscription was refused,
        so this loop raises `CoppiceError` from it directly rather than
        filing it under `_event_errors`, where nothing could ever look it
        up, since a caller has no id to ask for until subscribe() itself
        has already returned one. An `ok: false` answer to any OTHER
        request, an attach or a detach, is recorded into `_event_errors` by
        its own id instead, for `last_error()` to answer. Call this once
        per backend; a second concurrent iterator over the same connection
        would split lines between the two readers.

        A frame payload missing a required field, or carrying one of the
        wrong shape, is no frame: it is dropped and recorded into
        `_event_errors` under this call's own subscribe id, the same map
        an `ok: false` reply uses, so `last_error(subscribe_id)` can still
        answer what went wrong. The stream keeps running past it, the
        same way a malformed state event already does through
        `_validated`.

        When this loop ends, for any reason, this generator's own finally
        releases the connection, PROVIDED nobody has already replaced it:
        `_events_conn` stamps every connection it opens with a generation
        number, and this generator records the one its own connection
        carried. The finally then takes self._lock, compares that number
        against the current one, and closes inside that same critical
        section when they still match, the same lock and the same close
        close_connection() itself now shares. A caller that closed this
        connection and opened a fresh one while this generator was still
        blocked reading the old one bumps that number first; this
        generator's own finally then finds a mismatch and leaves the new
        connection alone, rather than closing a connection that is not
        the one it opened.
        """
        stream, my_generation = self._events_conn_with_generation()
        subscribe_id = self._next_id()
        request = {
            "id": subscribe_id,
            "cmd": "events.subscribe",
            "panes": "*",
            "kinds": ["state", "frame"],
        }
        stream.write(json.dumps(request).encode() + b"\n")
        stream.flush()
        try:
            for line in stream:
                try:
                    payload = json.loads(line)
                except ValueError:
                    continue
                kind = payload.get("event")
                if kind == "frame":
                    try:
                        frame = Frame(
                            pane=str(payload["pane"]),
                            seq=int(payload["seq"]),
                            cols=int(payload["cols"]),
                            rows=int(payload["rows"]),
                            cursor=tuple(payload["cursor"]),
                            rows_changed={int(k): v for k, v in payload["rows_changed"].items()},
                        )
                    except (KeyError, TypeError, ValueError, AttributeError) as exc:
                        self._event_errors[subscribe_id] = CoppiceError(
                            "internal", f"malformed frame: {exc}"
                        )
                        continue  # a malformed frame is no frame
                    yield frame
                elif kind == "state":
                    body = {k: v for k, v in payload.items() if k != "event"}
                    ev = _validated(body)
                    if ev is None:
                        continue  # a malformed or out-of-enum event is no event
                    yield ev
                elif kind is None and payload.get("ok") is False:
                    err = payload.get("error") or {}
                    error = CoppiceError(
                        str(err.get("code", "internal")), str(err.get("message", ""))
                    )
                    request_id = str(payload.get("id", ""))
                    if request_id == subscribe_id:
                        raise error
                    self._event_errors[request_id] = error
                    if len(self._event_errors) > _EVENT_ERRORS_MAX:
                        oldest = next(iter(self._event_errors))
                        del self._event_errors[oldest]
                # else: a successful plain reply, this subscribe's own ack
                # among them, needs no handling.
        finally:
            with self._lock:
                if self._events_generation == my_generation:
                    self._close_locked()
