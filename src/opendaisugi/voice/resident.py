"""The resident speech engine: one child process that loads its model once
and transcribes clip after clip. This module is the one definition of the
protocol between the voice server and that child. The C side is
clients/native/resident.h; the Go and Rust servers follow this text.

The protocol (version daisugi-voice-1):

- The server starts the child with the engine's own arguments. The child
  loads its model. Then it writes one line on stdout, the ready line, a
  JSON object whose "ready" is "daisugi-voice-1". Other keys are allowed
  and ignored (load_ms, say).
- A child that cannot load writes one line {"error": "WHY"} and exits 3.
- Each request is a frame on the child's stdin: four bytes, the length N
  of the clip as an unsigned big-endian integer, then N bytes, a 16 kHz
  mono 16-bit PCM WAV. N is from 1 to MAX_FRAME_BYTES.
- Each reply is one line on stdout, a JSON object: {"text": "..."} for a
  transcript, or {"error": "WHY"} for a clip the child could not
  transcribe. After either reply the child waits for the next frame.
  Other keys are allowed and ignored (decode_ms, say).
- End of file on stdin, at a frame boundary, means exit 0. A length of 0
  or over MAX_FRAME_BYTES, or end of file inside a frame, means the
  framing is lost: the child exits 2.
- stderr is free text: the library's log and the reason for a failure.
  The server keeps its last lines and names the last one in a message.

What the server does (all three servers do the same):

- It starts the child before it binds, and waits for the ready line, up
  to the load timeout. A child that does not get ready stops the start
  (EngineUnavailable, exit 3 from voice serve).
- It sends one clip at a time. Clips that arrive together wait in turn.
- A child that dies after it was ready is started again at once, in the
  background. While a child loads, a clip is refused with EngineLoading
  (503, engine_loading).
- A child that dies inside a clip, a reply that is not one JSON object
  with a string "text" or "error", and a clip with no reply within the
  clip timeout (which covers writing the frame too): the server kills the child, starts a new one, and refuses
  that clip with EngineUnavailable (503).
- A child that fails to load again is not started again at once, so a
  broken engine never runs in a loop. The next clip is refused with the
  reason, and starts it again.
- When the server stops, it closes the child's stdin, waits a short
  time, and kills it. The C wrappers also ask the kernel to end them
  when the server dies.
"""

from __future__ import annotations

import queue
import struct
import subprocess
import threading
import time
from collections import deque
from collections.abc import Callable

from opendaisugi.childline import exit_reason, parse_line
from opendaisugi.voice.audio import wav_duration_s

PROTOCOL = "daisugi-voice-1"
MAX_FRAME_BYTES = 64 * 1024 * 1024
LOAD_TIMEOUT_S = 300.0
CLIP_TIMEOUT_S = 120.0
STOP_GRACE_S = 2.0
STDERR_LINES = 20

LOADING = "The speech engine is loading its model. Try again in a moment."
AGAIN = "It is starting again. Try again in a moment."


def frame(wav: bytes) -> bytes:
    """One request frame: the clip's length, big-endian, then the clip."""
    return struct.pack(">I", len(wav)) + wav


def is_ready(obj: dict | None) -> bool:
    return obj is not None and obj.get("ready") == PROTOCOL


def reply_kind(obj: dict | None) -> str | None:
    """'text' or 'error' for a reply line the server reads, else None."""
    if obj is None:
        return None
    if isinstance(obj.get("text"), str):
        return "text"
    if isinstance(obj.get("error"), str):
        return "error"
    return None


class _Child:
    """One run of the engine program, with a thread on each output pipe."""

    def __init__(self, argv: list[str], env: dict[str, str] | None) -> None:
        self.proc = subprocess.Popen(
            argv,
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
        )
        self.lines: queue.Queue[bytes | None] = queue.Queue()
        self.tail: deque[str] = deque(maxlen=STDERR_LINES)
        self.ready = False
        self._err = threading.Thread(target=self._read_err, daemon=True)
        self._err.start()

    def start_reader(self, on_exit: Callable[[_Child], None]) -> None:
        threading.Thread(target=self._read_out, args=(on_exit,), daemon=True).start()

    def _read_err(self) -> None:
        assert self.proc.stderr is not None
        for raw in self.proc.stderr:
            line = raw.decode("utf-8", errors="replace").rstrip("\n")
            if line.strip():
                self.tail.append(line)

    def _read_out(self, on_exit: Callable[[_Child], None]) -> None:
        assert self.proc.stdout is not None
        for raw in self.proc.stdout:
            self.lines.put(raw)
        self.proc.wait()
        self._err.join()
        self.lines.put(None)
        on_exit(self)

    def reason(self) -> str:
        """Why the child ended. Call it only once the child has ended."""
        self.proc.wait()
        self._err.join()
        return exit_reason(self.proc.returncode, list(self.tail))

    def kill(self) -> None:
        if self.proc.poll() is None:
            self.proc.kill()

    def send(self, data: bytes) -> None:
        """Write one frame. A child that has ended is found by the reply read."""
        try:
            assert self.proc.stdin is not None
            self.proc.stdin.write(data)
            self.proc.stdin.flush()
        except (BrokenPipeError, OSError, ValueError):
            pass

    def close_stdin(self) -> None:
        try:
            if self.proc.stdin is not None:
                self.proc.stdin.close()
        except OSError:
            pass


class ResidentEngine:
    """An engine program run as one resident child (see the module text).

    ``unavailable`` and ``loading`` are the exception classes to raise, so
    this module needs nothing from engines.py. ``binary`` names the program
    in messages.
    """

    def __init__(
        self,
        name: str,
        argv: list[str],
        *,
        binary: str,
        unavailable: type[Exception],
        loading: type[Exception],
        env: dict[str, str] | None = None,
        load_timeout_s: float = LOAD_TIMEOUT_S,
        clip_timeout_s: float = CLIP_TIMEOUT_S,
    ) -> None:
        self.name = name
        self.argv = argv
        self.binary = binary
        self._unavailable = unavailable
        self._loading = loading
        self._env = env
        self.load_timeout_s = load_timeout_s
        self.clip_timeout_s = clip_timeout_s
        self._lock = threading.Lock()  # guards the state and the child
        self._clip = threading.Lock()  # one clip at a time
        self._state = "new"  # new, loading, ready, failed, stopped
        self._child: _Child | None = None
        self._why = ""
        self._loaded = threading.Condition(self._lock)

    # -- starting -----------------------------------------------------------

    def _load(self, child: _Child) -> str | None:
        """Wait for the child's ready line. None when it is ready, else why not."""
        try:
            raw = child.lines.get(timeout=self.load_timeout_s)
        except queue.Empty:
            child.kill()
            return f"it did not load its model in {self.load_timeout_s:g} seconds"
        if raw is None:
            return child.reason()
        obj = parse_line(raw)
        if is_ready(obj):
            return None
        child.kill()
        if obj is not None and isinstance(obj.get("error"), str):
            return obj["error"].strip()[:200]
        return f"its first line was not the {PROTOCOL} ready line"

    def _spawn(self) -> _Child:
        child = _Child(self.argv, self._env)
        child.start_reader(self._exited)
        return child

    def start(self) -> None:
        """Start the child and wait until it is ready. Raises ``unavailable``."""
        try:
            child = self._spawn()
        except OSError as exc:
            raise self._unavailable(f"{self.binary} did not start: {exc.strerror or exc}") from exc
        with self._lock:
            self._child = child
            self._state = "loading"
        why = self._load(child)
        with self._lock:
            if why is None:
                child.ready = True
                self._state = "ready"
            else:
                self._state = "stopped"
                self._child = None
            self._loaded.notify_all()
        if why is not None:
            raise self._unavailable(f"{self.binary} did not start: {why}")

    def _restart(self, old: _Child | None) -> None:
        """Start a new child in place of ``old``, loading in the background.
        Does nothing when ``old`` is no longer the current child."""
        with self._lock:
            if self._state == "stopped" or self._child is not old:
                return
            if old is not None:
                old.kill()
            try:
                child = self._spawn()
            except OSError as exc:
                self._child = None
                self._state = "failed"
                self._why = f"{self.binary} did not start: {exc.strerror or exc}"
                self._loaded.notify_all()
                return
            self._child = child
            self._state = "loading"
        threading.Thread(target=self._finish_load, args=(child,), daemon=True).start()

    def _finish_load(self, child: _Child) -> None:
        why = self._load(child)
        with self._lock:
            if self._child is not child or self._state == "stopped":
                return
            if why is None:
                child.ready = True
                self._state = "ready"
            else:
                child.kill()
                self._child = None
                self._state = "failed"
                self._why = why
            self._loaded.notify_all()

    def _exited(self, child: _Child) -> None:
        """The reader thread saw the child end. Restart one that was ready."""
        with self._lock:
            restart = self._child is child and self._state == "ready" and child.ready
        if restart:
            self._restart(child)

    def wait_ready(self, timeout: float) -> str:
        """Wait while a child loads. Returns the state then (for tests)."""
        end = time.monotonic() + timeout
        with self._lock:
            while self._state == "loading":
                left = end - time.monotonic()
                if left <= 0:
                    break
                self._loaded.wait(left)
            return self._state

    # -- clips --------------------------------------------------------------

    def _lost(self, child: _Child, what: str) -> Exception:
        self._restart(child)
        return self._unavailable(f"The speech engine {what}. {AGAIN}")

    def transcribe_text(self, wav: bytes) -> str:
        """The child's text for one clip. Raises ``loading``, ``unavailable``
        (for a child that is lost or will not start) or RuntimeError (for a
        clip the child refused)."""
        with self._clip:
            with self._lock:
                state, child, why = self._state, self._child, self._why
            if state == "loading":
                raise self._loading(LOADING)
            if state == "failed":
                self._restart(None)
                raise self._unavailable(
                    f"The speech engine did not start again: {why}. The next clip tries again."
                )
            if state != "ready" or child is None:
                raise self._unavailable("The speech engine has stopped.")
            # The clip timeout covers the write too: a child that stops
            # reading its stdin would block a clip bigger than the pipe.
            deadline = time.monotonic() + self.clip_timeout_s
            writer = threading.Thread(target=child.send, args=(frame(wav),), daemon=True)
            writer.start()
            writer.join(self.clip_timeout_s)
            try:
                if writer.is_alive():
                    raise queue.Empty
                raw = child.lines.get(timeout=max(0.0, deadline - time.monotonic()))
            except queue.Empty:
                raise self._lost(
                    child, f"did not answer in {self.clip_timeout_s:g} seconds"
                ) from None
            if raw is None:
                raise self._lost(child, f"stopped during a clip ({child.reason()})")
            obj = parse_line(raw)
            kind = reply_kind(obj)
            if kind is None:
                raise self._lost(child, "gave a reply that is not one JSON object with text")
            assert obj is not None
            if kind == "error":
                raise RuntimeError(f"{self.binary}: {obj['error'].strip()[:200]}")
            return obj["text"]

    def transcribe(self, wav: bytes) -> tuple[str, float, float]:
        """The text, the clip's seconds, and the wall time of the reply."""
        duration_s = wav_duration_s(wav)
        start = time.monotonic()
        text = self.transcribe_text(wav)
        return text, duration_s, time.monotonic() - start

    # -- stopping -----------------------------------------------------------

    def stop(self) -> None:
        """Close the child's stdin, wait a little, then kill it."""
        with self._lock:
            child = self._child
            self._state = "stopped"
            self._child = None
            self._loaded.notify_all()
        if child is None:
            return
        child.close_stdin()
        try:
            child.proc.wait(timeout=STOP_GRACE_S)
        except subprocess.TimeoutExpired:
            child.kill()
            child.proc.wait()

    @property
    def pid(self) -> int | None:
        with self._lock:
            return self._child.proc.pid if self._child is not None else None
