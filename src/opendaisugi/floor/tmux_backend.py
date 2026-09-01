"""tmux as a pane backend: the substrate the operator already has.

tmux has no agent state, so this backend earns its state twice over. Every poll
merges what it learns into a per-pane cache with `opendaisugi.floor.merge`, the
same precedence rule the rest of the floor uses: master §5.1 says a gate fact
outranks a screen guess, and a terminal `done` absorbs everything after it.

A rule that matches nothing, and a rule that matches but sets
`skip_state_update`, both mean "no event this tick": the pane keeps whatever a
real source last said. The coppice Go server treats the two identically.
`server/detect.go` reads `if !r.Matched || r.Skip { continue }`, and its own
README says why: "No match means no event, and the pane keeps whatever a
real source last told us." Master spec 3.1 forbids a manifest ever asserting
`done`, so a pane that has left `list-panes` is `done` from the *process*,
not a manifest.

tmux owns the layout, so `resize` is a no-op. Splits, zoom, and window
arrangement belong to the operator's own tmux config, not to us.

Probed against tmux 3.7b:

* `new-window` needs a session; from a cold server we `new-session -d` instead.
* `-e K=V` needs tmux 3.2. Older tmux gets `env K=V -- cmd` wrapped around the argv.
* `kill-pane` on a gone pane exits 1. The message depends on how gone: "can't
  find pane: %N" when the server survives, "no server running on <path>" when
  a server ran here and has since shut down, "error connecting to <path> (No
  such file or directory)" when no server has ever run on this socket at
  all. All three are success.
* `display-message -p -t <gone>` exits 0 with empty output, so existence is
  decided by membership in `list-panes -a`, never by that command.
* `capture-pane -p -J` returns the pane's whole visible screen with wrapped
  lines rejoined into logical ones. There is no capture limit below the pane
  height: a manifest rule's own `region` is what narrows the text, not us.
"""

from __future__ import annotations

import re
import shlex
import shutil
import subprocess
import threading
import time
from collections.abc import Callable, Iterator, Sequence
from pathlib import Path
from typing import Literal

from opendaisugi.floor import Frame, PaneInfo, PaneRef, PaneStateEvent, merge
from opendaisugi.floor.manifests import DEFAULT_MANIFEST_DIRS, classify, load_manifests

_SESSION = "coppice"
_DEFAULT_COLS = 120
_DEFAULT_ROWS = 40
_RECENT_LINES = 200
_POLL_S = 0.5
_TIMEOUT_S = 5.0
_READ_SOURCES = ("visible", "recent", "detection")

# Master spec 3.2 names our key set. This maps it to tmux's own key names.
KEY_MAP: dict[str, str] = {
    "enter": "Enter",
    "esc": "Escape",
    "escape": "Escape",
    "tab": "Tab",
    "backspace": "BSpace",
    "space": "Space",
    "up": "Up",
    "down": "Down",
    "left": "Left",
    "right": "Right",
    "ctrl+a": "C-a",
    "ctrl+b": "C-b",
    "ctrl+c": "C-c",
    "ctrl+d": "C-d",
    "ctrl+r": "C-r",
    "f1": "F1",
    "f2": "F2",
    "f3": "F3",
    "f4": "F4",
}

_VERSION_RE = re.compile(r"(\d+)\.(\d+)")
_LIST_FORMAT = (
    "#{pane_id}\t#{window_name}\t#{pane_current_path}\t"
    "#{pane_current_command}\t#{pane_start_command}\t#{pane_dead}\t#{pane_title}"
)
_GONE_ERRORS = ("can't find pane", "no server running", "error connecting to")


def _unquote_start_command(text: str) -> str:
    """tmux 3.7b returns `pane_start_command` WITH surrounding double quotes,
    when the pane was started from one joined command string.

    Probed: a pane started as `sh -c 'echo READY'` comes back as
    `"sh -c 'echo READY'"`, so a bare `shlex.split` yields one element holding
    the whole string, and the roster would show that blob as the harness.
    """
    stripped = text.strip()
    if len(stripped) >= 2 and stripped[0] == stripped[-1] and stripped[0] in "\"'":
        return stripped[1:-1]
    return stripped


def parse_tmux_version(text: str) -> tuple[int, int]:
    """``"tmux 3.7b"`` -> ``(3, 7)``. An unparsable spelling is ``(0, 0)``, not a crash."""
    match = _VERSION_RE.search(text or "")
    if not match:
        return (0, 0)
    return (int(match.group(1)), int(match.group(2)))


def supports_env_flag(version: tuple[int, int]) -> bool:
    """``new-session -e K=V`` arrived in tmux 3.2."""
    return version >= (3, 2)


class TmuxBackend:
    """Drive panes through the tmux CLI. Stdlib subprocess only."""

    name = "tmux"
    yields_frames = False

    def __init__(
        self,
        socket: str | None = None,
        *,
        manifest_dirs: Sequence[Path] | None = None,
        gate_states: Callable[[], dict[str, PaneStateEvent]] | None = None,
    ) -> None:
        self._socket = socket
        self._manifest_dirs = tuple(manifest_dirs or DEFAULT_MANIFEST_DIRS)
        self._gate_states = gate_states or (lambda: {})
        self._manifests: dict | None = None
        self._version: tuple[int, int] | None = None
        # The backend's own idea of each pane's current state, kept up to
        # date by merge() rather than recomputed from scratch every poll.
        # tmux has no notion of this on its own.
        self._current: dict[str, PaneStateEvent] = {}
        # The ts of the last gate event actually merged in for each pane, so
        # a gate_states() callback that keeps returning the same snapshot
        # does not get re-applied every poll. See _state_for.
        self._gate_seen: dict[str, float] = {}
        # subscribe() polls forever until this is set. close_connection()
        # is the only way to set it from outside the loop.
        self._stopped = False

    # --- process plumbing -------------------------------------------------
    def _argv(self, *args: str) -> list[str]:
        exe = shutil.which("tmux")
        if exe is None:
            raise FileNotFoundError("tmux is not on PATH")
        base = [exe]
        if self._socket:
            base += ["-L", self._socket]
        return base + list(args)

    def _run(self, *args: str, check: bool = True) -> subprocess.CompletedProcess[str]:
        """Run one tmux command. A timeout, or tmux itself vanishing from
        PATH, is treated exactly like a nonzero exit: `check=False`
        degrades to a failed result, `check=True` raises.
        """
        try:
            argv = self._argv(*args)
            proc = subprocess.run(
                argv, capture_output=True, text=True, timeout=_TIMEOUT_S, check=False
            )
        except subprocess.TimeoutExpired as exc:
            if check:
                raise RuntimeError(
                    f"tmux {' '.join(args[:2])}: timed out after {_TIMEOUT_S}s"
                ) from exc
            proc = subprocess.CompletedProcess(
                list(args), 1, stdout="", stderr=f"tmux timed out after {_TIMEOUT_S}s"
            )
        except (OSError, subprocess.SubprocessError) as exc:
            if check:
                raise
            proc = subprocess.CompletedProcess(list(args), 1, stdout="", stderr=str(exc))
        if check and proc.returncode != 0:
            raise RuntimeError(f"tmux {' '.join(args[:2])}: {proc.stderr.strip()}")
        return proc

    def _version_tuple(self) -> tuple[int, int]:
        if self._version is None:
            try:
                self._version = parse_tmux_version(self._run("-V", check=False).stdout)
            except (OSError, FileNotFoundError, subprocess.SubprocessError):
                self._version = (0, 0)
        return self._version

    # --- PaneBackend ------------------------------------------------------
    def available(self) -> bool:
        """tmux on PATH. Never raises. Master spec 3.2 requires that.

        Deliberately NOT gated on the version: `tmux master` and other unparsable
        spellings parse to (0, 0), and refusing a working tmux over a version
        string would skip the contract suite on a perfectly good box. The
        version tuple decides `-e` versus an `env` wrapper, nothing else.
        """
        try:
            return shutil.which("tmux") is not None
        except Exception:  # noqa: BLE001
            return False

    def _has_session(self) -> bool:
        return self._run("has-session", "-t", _SESSION, check=False).returncode == 0

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
        """A new window running ``cmd``. tmux has no headless kind; every pane is a pty.

        ``harness`` is part of the §3.2 protocol so one call site drives all three
        backends. tmux has no adapter registry, so it is recorded nowhere and ignored.
        """
        argv = list(cmd)
        env_args: list[str] = []
        if env:
            if supports_env_flag(self._version_tuple()):
                for key, value in env.items():
                    env_args += ["-e", f"{key}={value}"]
            else:
                argv = ["env", *[f"{k}={v}" for k, v in env.items()], *argv]
        command = shlex.join(argv)
        common = ["-d", "-c", str(cwd), "-P", "-F", "#{pane_id}", "-n", label or "pane", *env_args]
        if self._has_session():
            out = self._run("new-window", *common, command).stdout
        else:
            out = self._run(
                "new-session",
                "-s",
                _SESSION,
                "-x",
                str(_DEFAULT_COLS),
                "-y",
                str(_DEFAULT_ROWS),
                *common,
                command,
            ).stdout
        pane_id = out.strip().splitlines()[-1].strip()
        return PaneRef(backend=self.name, id=pane_id)

    def list(self) -> list[PaneInfo]:
        _, infos = self._list_raw()
        return infos

    def list_proven(self) -> tuple[bool, list[PaneInfo]]:
        """Whether list-panes truly answered, and what it said. See _list_raw."""
        return self._list_raw()

    def _list_raw(self) -> tuple[bool, list[PaneInfo]]:
        """The pane list right now, and whether that list is a proven fact.

        A clean list-panes, or one of the known no-server wordings on a
        failed one, both mean there really are zero panes: proven, not an
        unknown. Anything else, a timeout or an unrecognised failure,
        means the call itself failed and the empty list it returns proves
        nothing; `_poll` reads the first element, the proven flag, before
        it trusts the second as a real roster, so it never mistakes "the
        call failed" for "every pane exited". `list()` itself keeps
        degrading to an empty roster either way, matching master 3.2.
        """
        proc = self._run("list-panes", "-a", "-F", _LIST_FORMAT, check=False)
        if proc.returncode != 0:
            proven = any(text in proc.stderr for text in _GONE_ERRORS)
            if proven:
                # No server means no panes, not an error, and nothing
                # left to remember a cached state for. An unproven
                # failure leaves the cache alone: it might still be true.
                self._current.clear()
                self._gate_seen.clear()
            return proven, []
        gate = self._gate_states()
        now = time.time()
        out: list[PaneInfo] = []
        alive_ids: set[str] = set()
        for line in proc.stdout.splitlines():
            parts = line.split("\t")
            if len(parts) < 7:
                continue
            pane_id, window, cwd, current, start, dead, title = parts[:7]
            alive_ids.add(pane_id)
            ref = PaneRef(backend=self.name, id=pane_id)
            start = _unquote_start_command(start)
            try:
                argv = shlex.split(start) if start else [current]
            except ValueError:
                argv = [start or current]  # an unbalanced quote is a name, not a crash
            is_dead = dead.strip() == "1"
            out.append(
                PaneInfo(
                    ref=ref,
                    label=window,
                    cwd=cwd,
                    cmd=argv,
                    # `kind` carries "dead" so a caller sees a pane the
                    # operator's own tmux config kept on screen as finished
                    # too. The operator's tmux.conf may set remain-on-exit
                    # on. Every live tmux pane is a pty.
                    kind="dead" if is_dead else "pty",
                    state=None if is_dead else self._state_for(ref, current, title, gate, now),
                )
            )
        for stale_id in set(self._current) - alive_ids:
            self._current.pop(stale_id, None)
            self._gate_seen.pop(stale_id, None)
        return True, out

    def send_text(self, pane: PaneRef, text: str, *, enter: bool = True) -> None:
        self._run("send-keys", "-t", pane.id, "-l", text)
        if enter:
            self._run("send-keys", "-t", pane.id, "Enter")

    def send_keys(self, pane: PaneRef, keys: list[str]) -> None:
        for key in keys:
            mapped = KEY_MAP.get(key.lower())
            if mapped is not None:
                self._run("send-keys", "-t", pane.id, mapped)
            elif len(key) == 1:
                self._run("send-keys", "-t", pane.id, "-l", key)
            else:
                raise ValueError(
                    f"unknown key {key!r}. Known keys: {', '.join(sorted(KEY_MAP))}, "
                    f"or one printable character."
                )

    def read(
        self, pane: PaneRef, *, source: Literal["visible", "recent", "detection"] = "visible"
    ) -> str:
        if source not in _READ_SOURCES:
            raise ValueError(
                f"tmux has no read source {source!r}. Known sources: {', '.join(_READ_SOURCES)}."
            )
        if source == "recent":
            args = ["capture-pane", "-p", "-S", f"-{_RECENT_LINES}", "-t", pane.id]
        elif source == "detection":
            args = ["capture-pane", "-p", "-J", "-t", pane.id]
        else:
            args = ["capture-pane", "-p", "-t", pane.id]
        proc = self._run(*args, check=False)
        if proc.returncode != 0:
            return ""
        return proc.stdout

    def resize(self, pane: PaneRef, cols: int, rows: int) -> None:
        """A no-op: tmux owns the layout, and fighting it would move the operator's panes."""
        return None

    def close(self, pane: PaneRef) -> None:
        """Kill the pane. A pane that already exited is already closed, not an error.

        tmux itself being gone from PATH is checked first and separately:
        that is the shape master 3.2 says close() may never raise for, and
        it answers every call the same way, kill-pane included, so nothing
        past this point could tell it apart from a genuine failure.

        Past that, tmux reports a gone pane several different ways: "can't
        find pane" when the server survives, "no server running" when the
        pane's own window was the server's last one and the whole server
        has already shut down, "error connecting to" when no server has
        ever run on this socket. A pane racing its own process exit can
        hit others still, "no current target" and "server exited
        unexpectedly" both measured here, and the wording is not a fixed
        set to enumerate. A message _GONE_ERRORS does not know falls back
        to a fresh list-panes, checked the same two ways: a clean answer,
        exit 0, that omits the pane, or the same _GONE_ERRORS wording,
        this time from list-panes' own stderr. Anything else, a timeout or
        some other failure of list-panes itself, proves nothing and still
        raises: a client/server version mismatch fails both calls the same
        way, empty stdout included, and that empty stdout is not the same
        fact as a clean list that omits the pane.
        """
        if shutil.which("tmux") is None:
            return None
        proc = self._run("kill-pane", "-t", pane.id, check=False)
        if proc.returncode == 0:
            return
        if any(text in proc.stderr for text in _GONE_ERRORS):
            return
        confirm = self._run("list-panes", "-a", "-F", "#{pane_id}", check=False)
        if confirm.returncode == 0 and pane.id not in confirm.stdout.split():
            return
        if any(text in confirm.stderr for text in _GONE_ERRORS):
            return
        raise RuntimeError(f"tmux kill-pane: {proc.stderr.strip()}")

    def report_state(self, pane: PaneRef, ev: PaneStateEvent) -> None:
        """tmux holds no state of its own. The gate's own event store is the sink."""
        return None

    def _poll(
        self, seen: set[str], done_sent: set[str], last: dict[str, str]
    ) -> tuple[bool, list[PaneStateEvent]]:
        """Whether this poll cycle actually reached tmux, and its events.
        Mutates `seen`, `done_sent`, and `last` in place.

        `seen` tracks pane IDs still considered alive, not their states.
        Tracking states instead would mean a pane that never matched a
        manifest is never in the map, so its exit is never announced. That
        is every pane on a box with no manifests installed.

        `done` on tmux is a PROCESS fact: the pane left `list-panes`, or
        tmux flagged it `pane_dead`. It fires the first poll that observes
        either fact, even for a pane already gone or already `pane_dead`
        the very first time this method ever sees it, since `newly_done`
        below never requires prior membership in `seen`. `done_sent` guards
        the other direction: a pane the operator's own tmux config keeps on
        screen with `remain-on-exit on` stays `pane_dead` in `list-panes`
        forever, and must be reported done exactly once, not on every poll
        that still observes it.

        Split out of `subscribe` so a test can drive many poll cycles
        directly, with no real sleep between them.

        An unproven `list-panes` failure, a timeout or an unrecognised
        wording, returns no events at all rather than reading the empty
        roster `_list_raw` hands back as proof every pane exited. A pane
        that is genuinely still alive must survive a poll that could not
        reach the host, not be reported done because of it. The proven
        flag this returns is `subscribe`'s own `ready` signal: returned
        here, on the stack, rather than kept on the instance, so two
        generators over one backend can never read each other's answer.
        """
        polled_ok, infos = self._list_raw()
        if not polled_ok:
            return False, []
        alive = {i.ref.id for i in infos}
        dead_now = {i.ref.id for i in infos if i.kind == "dead"}
        events: list[PaneStateEvent] = []
        newly_done = ((seen - alive) | dead_now) - done_sent
        for pane_id in sorted(newly_done):
            done_sent.add(pane_id)
            seen.discard(pane_id)
            last.pop(pane_id, None)
            events.append(
                PaneStateEvent(
                    session_id="",
                    harness="unknown",
                    state="done",
                    source="process",
                    ts=time.time(),
                    pane=pane_id,
                    detail="the pane's process exited",
                )
            )
        seen |= alive - dead_now
        done_sent &= alive  # gone from list-panes for good needs no guard any more
        for info in infos:
            if info.state is None:
                continue
            if last.get(info.ref.id) != info.state.state:
                last[info.ref.id] = info.state.state
                events.append(info.state)
        return True, events

    def subscribe(
        self, *, ready: threading.Event | None = None
    ) -> Iterator[PaneStateEvent | Frame]:
        """Poll every 500 ms. Yield `done` from the process, and state changes from state.

        Master §3.2: tmux yields no frames. Only the coppice backend does.
        close_connection() stops the loop; cleared at the top here, so a
        second call to subscribe() after a previous one was stopped starts
        polling again rather than exiting at once.

        ready, when given, is set once the first poll has actually reached
        tmux, not merely been attempted: a caller waiting on it knows the
        subscription has a real baseline, not just a running thread. The
        event is set from inside this generator, so it only fires while a
        caller is iterating it. Cleared at the top, the same as _stopped,
        so a caller that reuses one Event across two subscribe() calls
        never reads a leftover set flag as a fresh baseline.
        """
        self._stopped = False
        if ready is not None:
            ready.clear()
        seen: set[str] = set()
        done_sent: set[str] = set()
        last: dict[str, str] = {}
        signaled = False
        while not self._stopped:
            polled_ok, events = self._poll(seen, done_sent, last)
            yield from events
            if ready is not None and not signaled and polled_ok:
                ready.set()
                signaled = True
            time.sleep(_POLL_S)

    def close_connection(self) -> None:
        """Stop the poll loop subscribe() runs. Nothing else is held open.

        The loop notices within one poll interval of this call, plus
        however long whatever tmux call is already in flight takes to
        return: worst case _TIMEOUT_S (5.0s) plus _POLL_S (0.5s), not a
        flat one poll interval.
        """
        self._stopped = True

    # --- state --------------------------------------------------------
    def _state_for(
        self,
        ref: PaneRef,
        current_command: str,
        title: str,
        gate: dict[str, PaneStateEvent],
        now: float,
    ) -> PaneStateEvent | None:
        """The pane's current state: the last fact merge() decided, updated with
        whatever new evidence this poll has.

        A gate fact is merged in once per event, keyed by its own timestamp, so
        a gate_states() callback that keeps returning the same already-resolved
        snapshot does not get re-applied and reassert a stale hold. A manifest
        match is merged in only when a rule actually won and did not ask to
        stay silent. Master §5.1 says so; see the module docstring too. A
        bare no-match and a winning skip_state_update rule both leave the
        cached state exactly as it was.
        """
        prior = self._current.get(ref.id)
        gate_event = gate.get(ref.id)
        if gate_event is not None and gate_event.ts > self._gate_seen.get(ref.id, 0.0):
            self._gate_seen[ref.id] = gate_event.ts
            prior = merge(prior, gate_event, now=now)
        if self._manifests is None:
            self._manifests = load_manifests(self._manifest_dirs)
        if self._manifests:
            match = classify(
                self.read(ref, source="detection"),
                title=title,
                osc_progress=None,
                agent_hint=current_command or None,
                manifests=self._manifests,
            )
            if match.rule is not None and not match.skip:
                event = PaneStateEvent(
                    session_id="",
                    harness=current_command or "unknown",
                    state=match.state,
                    source="manifest",
                    ts=now,
                    pane=ref.id,
                    detail=f"rule={match.rule} region={match.region}",
                )
                prior = merge(prior, event, now=now)
        self._current[ref.id] = prior
        return prior
