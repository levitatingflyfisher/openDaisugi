"""Pick the pane backend, and the two drivers every client shares.

Master spec 3.2 gives one protocol and three implementations. auto takes the
first available backend in the master's order. An explicit name that is not
available raises FloorNotAvailable. The message names the command that
installs it. Running the operator's agent somewhere they did not choose is
not helpful, even when a different backend happens to be up.

prompt_pane and wait_for_state live here rather than in each backend because
both the CLI and the floor screen need identical behaviour. The difference
between backends is one optional method.
"""

from __future__ import annotations

import time
from dataclasses import dataclass
from pathlib import Path

from opendaisugi.exceptions import FloorNotAvailable, OpenDaisugiError
from opendaisugi.floor import PaneBackend, PaneRef, PaneStateEvent, effective_state

BACKEND_ORDER: tuple[str, ...] = ("coppice", "herdr", "tmux")

_FIX = {
    # The build command here must name the same command hostfacts.py's own
    # coppice fact does. tests/floor/test_coppice_backend.py binds the two.
    "coppice": "build it: cd harness/coppice && mkdir -p build && "
    "go build -o build/coppice ./cmd/coppice, then run `coppice server start`",
    "herdr": "install herdr from herdr.dev, then run `herdr session list --json`",
    "tmux": "install tmux 3.2 or newer with your package manager",
}


@dataclass(frozen=True)
class BackendStatus:
    name: str
    available: bool
    why_not: str
    fix: str


def build_backend(name: str, config, *, autostart: bool = False) -> PaneBackend:
    """Construct one backend. Imports lazily so a missing host costs nothing.

    autostart reaches the coppice backend only. It is False for every probe,
    so `daisugi coppice backends` stays a diagnostic. Asking a question must
    never start a daemon as a side effect.
    """
    if name == "coppice":
        from opendaisugi.floor.coppice_backend import CoppiceBackend

        sock = Path(config.floor.coppice_socket) if config.floor.coppice_socket else None
        return CoppiceBackend(
            sock_path=sock, data_dir=config.data_dir / "coppice", autostart=autostart
        )
    if name == "herdr":
        from opendaisugi.floor.gate_states import gate_states_reader
        from opendaisugi.floor.herdr_backend import HerdrBackend

        return HerdrBackend(gate_states=gate_states_reader(config.data_dir))
    if name == "tmux":
        from opendaisugi.floor.gate_states import gate_states_reader
        from opendaisugi.floor.tmux_backend import TmuxBackend

        return TmuxBackend(
            socket=getattr(config.floor, "tmux_socket", None),
            gate_states=gate_states_reader(config.data_dir),
        )
    raise FloorNotAvailable(
        f"no pane backend named {name!r}. Choose one of: {', '.join(BACKEND_ORDER)}."
    )


def _probe(
    name: str, config, *, autostart: bool = False
) -> tuple[PaneBackend | None, BackendStatus]:
    try:
        backend = build_backend(name, config, autostart=autostart)
        ok = bool(backend.available())
    except Exception as exc:  # noqa: BLE001 - available() must never take the floor down
        return None, BackendStatus(name, False, str(exc) or exc.__class__.__name__, _FIX[name])
    if ok:
        return backend, BackendStatus(name, True, "", _FIX[name])
    return None, BackendStatus(name, False, f"{name} did not answer", _FIX[name])


def backend_statuses(config) -> list[BackendStatus]:
    """One row per backend: name, available, why not, and the command that fixes it.

    Never autostarts. A diagnostic that changes the machine is not a diagnostic.
    """
    return [_probe(name, config, autostart=False)[1] for name in BACKEND_ORDER]


def pick_backend(config, *, name: str | None = None, autostart: bool = False) -> PaneBackend:
    """Pick the backend to drive. auto scans in order. A name is taken literally.

    autostart lets the coppice backend run `coppice server start` once, and
    only once, when its socket is absent. Callers that ask for coppice pass
    it: `daisugi coppice <verb> --backend coppice` and any `coppice spawn`.
    Callers that merely look do not.
    """
    chosen = name or getattr(config.floor, "backend", "auto")
    if chosen != "auto":
        if chosen not in BACKEND_ORDER:
            raise FloorNotAvailable(
                f"no pane backend named {chosen!r}. Choose one of: {', '.join(BACKEND_ORDER)}."
            )
        backend, status = _probe(chosen, config, autostart=autostart)
        if backend is None:
            raise FloorNotAvailable(f"{chosen} is not available: {status.why_not}. {status.fix}.")
        return backend
    reasons: list[str] = []
    for candidate in BACKEND_ORDER:
        # auto never starts a server. The operator did not name coppice, so
        # the floor picks what is already running rather than launching a
        # daemon.
        backend, status = _probe(candidate, config, autostart=False)
        if backend is not None:
            return backend
        reasons.append(f"{candidate}: {status.why_not}")
    raise FloorNotAvailable(
        "no pane backend is available. "
        + "; ".join(reasons)
        + ". Start one: `coppice server start`, or install tmux."
    )


def prompt_pane(
    backend: PaneBackend,
    ref: PaneRef,
    text: str,
    *,
    kind: str | None = None,
    wait: bool = False,
    timeout_s: float = 60.0,
) -> str:
    """Deliver a prompt. Returns prompted for a real agent prompt, typed for typed text.

    Decided by the pane's own kind, not by whether the backend happens to
    have a prompt method: a headless pane takes agent.prompt and reports
    prompted; anything else, pty included, takes typed text and reports
    typed. coppice's own server has no adapter for a pty pane and falls
    back to typing the text in regardless, so calling backend.prompt for
    one would still type, under the wrong word.

    kind, when the caller already has it, skips a lookup. Omitted, this
    reads it from backend.list() itself, matching ref.id, the same way
    master §3.2 says every other read here degrades: a backend that
    cannot answer that lookup right now gets typed, the safe answer,
    rather than an error out of a function whose whole job is delivering
    text. A pane not found in a list that did answer defaults to typed
    too, for a pane this floor can no longer see.

    wait, when the caller asks for it and the backend has a prompt
    method, is honoured through that same method, the mechanism that
    already knows how to watch a pane settle server-side. Only the
    reported word changes for a pty pane, from prompted to typed; the
    call underneath is the one a headless pane already makes. wait=False,
    or a backend with no prompt method at all, tmux's never does, keeps
    going straight to send_text with no wait attempted, exactly as
    before this fix: watching a floor-side state that a plain pty pane
    may never report would turn a dropped wait into a full-timeout
    stall instead, a worse failure than the one this closes.
    """
    if kind is None:
        try:
            info = next((i for i in backend.list() if i.ref.id == ref.id), None)
        except OpenDaisugiError:
            info = None
        kind = info.kind if info is not None else "pty"
    prompt = getattr(backend, "prompt", None)
    if kind == "headless" and callable(prompt):
        prompt(ref, text, wait=wait, timeout_s=timeout_s)
        return "prompted"
    if wait and callable(prompt):
        prompt(ref, text, wait=wait, timeout_s=timeout_s)
        return "typed"
    backend.send_text(ref, text, enter=True)
    return "typed"


def wait_for_state(
    backend: PaneBackend,
    ref: PaneRef,
    *,
    until: str,
    timeout_s: float,
    poll_s: float = 0.5,
) -> PaneStateEvent | None:
    """Block until the pane reaches until. Returns None on timeout, never a made-up event.

    A pane that was seen alive at least once and then leaves the roster is
    done. The process is gone. That event is tagged source process because
    master spec 3.1 forbids a manifest saying done. Absence is never enough
    on its own: list_proven, when the backend has one, tells a failed call
    apart from a genuinely empty answer, and a poll that never reached the
    host must not read as the pane having exited. A backend with no
    list_proven is read through list() and treated as fully proven, the
    same as every poll always was before list_proven existed.

    The stored event is read through effective_state before it is compared,
    the same expiry merge() itself applies when a fresh event arrives. A
    poller only ever has the last stored event, never a fresh one to merge
    against, so a gate blocked hold whose ask has expired must read as
    working here too. Otherwise a crashed gate process would leave `until=
    "blocked"` satisfied forever.
    """
    list_proven = getattr(backend, "list_proven", None)
    deadline = time.monotonic() + timeout_s
    seen = False
    while True:
        if callable(list_proven):
            proven, raw_infos = list_proven()
        else:
            proven, raw_infos = True, backend.list()
        infos = {i.ref.id: i for i in raw_infos}
        info = infos.get(ref.id)
        if info is None:
            if proven and seen and until in ("done", "any"):
                return PaneStateEvent(
                    session_id="",
                    harness="unknown",
                    state="done",
                    source="process",
                    ts=time.time(),
                    pane=ref.id,
                    detail="pane left the backend's list",
                )
        else:
            seen = True
            current = effective_state(info.state, now=time.time())
            if current is not None and (until == "any" or current.state == until):
                return current
        if time.monotonic() >= deadline:
            return None
        time.sleep(poll_s)
