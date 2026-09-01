"""One contract, three backends. A backend that cannot pass it is not a backend.

Master §3.2: the suite skips a backend whose `available()` is false and says
which host is missing. A backend that is present and fails is a suite
failure.

The contract is deliberately small, because it is the part every substrate
must share: spawn something, read what it printed, watch it finish, and
have it gone from the list afterwards. Anything a single backend does
better belongs in that backend's own test file.
"""

from __future__ import annotations

import contextlib
import json
import os
import queue
import re
import shutil
import subprocess
import threading
import time
from pathlib import Path

import pytest

from opendaisugi.floor import Ask, PaneRef, PaneStateEvent, TaskInfo
from opendaisugi.floor.coppice_backend import KNOWN_KEYS, CoppiceBackend, CoppiceError
from opendaisugi.floor.herdr_backend import HerdrBackend
from opendaisugi.floor.tmux_backend import TmuxBackend
from tests.floor import hostfacts
from tests.floor.coppice_sandbox import coppice_binary, coppice_server  # noqa: F401

# Named with this process's own pid so two pytest processes running the
# contract suite at once never kill each other's private server.
TMUX_SOCKET = f"daisugi-test-{os.getpid()}"
_READY_S = 3.0
_DONE_S = 8.0


@pytest.fixture(autouse=True)
def _default_no_autostart(monkeypatch):
    """Every test here keeps COPPICE_NO_AUTOSTART set, the guard
    test_coppice_backend.py also uses. The coppice_server fixture's own
    start and stop are exempt by construction; see coppice_sandbox._run_cli.
    """
    monkeypatch.setenv("COPPICE_NO_AUTOSTART", "1")


def _make(name: str) -> TmuxBackend | HerdrBackend:
    if name == "tmux":
        return TmuxBackend(socket=TMUX_SOCKET)
    if name == "herdr":
        return HerdrBackend()
    raise AssertionError(f"_make does not build {name!r}. coppice comes from the sandbox.")


def _teardown(name: str, made) -> None:
    """Release whatever this backend's own contract fixture opened.

    close_connection() runs for every backend; only coppice has anything to
    release. tmux's own private server is killed here so the next test
    starts from a clean slate; the socket file left behind by that
    server, one per pytest process since the socket name carries this
    process's own pid, is unlinked too, so a long-running box does not
    accumulate one dead file per run. coppice's real server belongs to the
    module-scoped coppice_server fixture, started and stopped once for the
    whole file, not once per test.
    """
    close_connection = getattr(made, "close_connection", None)
    if callable(close_connection):
        close_connection()
    if name == "tmux" and shutil.which("tmux"):
        subprocess.run(
            [shutil.which("tmux"), "-L", TMUX_SOCKET, "kill-server"],
            capture_output=True,
            check=False,
        )
        Path(f"/tmp/tmux-{os.getuid()}/{TMUX_SOCKET}").unlink(missing_ok=True)


@pytest.fixture(params=["coppice", "herdr", "tmux"])
def backend(request):
    name = request.param
    if name == "coppice":
        # Lazily resolved: requesting coppice_server unconditionally would
        # build and start the sandbox server even for the tmux and herdr
        # parameters. Its own skip, when the toolchain is absent, carries
        # the reason.
        server = request.getfixturevalue("coppice_server")
        made = CoppiceBackend(sock_path=server.socket_path, data_dir=server.data_dir, timeout_s=2.0)
        # server start already blocked until the socket answered, so a
        # False here is a defect in the sandbox, not an absent host: fail
        # loudly instead of turning it into a silent skip.
        assert made.available(), "the sandbox server started but does not answer"
    else:
        reason = hostfacts.skip_reason(name)
        if reason:
            pytest.skip(reason)
        made = _make(name)
        if not made.available():
            _teardown(name, made)
            pytest.skip(
                f"{name} is installed but not answering. {hostfacts.host_facts()[name].fix}."
            )
    yield made
    _teardown(name, made)


def _ids(backend) -> set[str]:
    return {i.ref.id for i in backend.list()}


def _subscribe_until(backend, predicate, timeout_s: float, *, on_ready=None):
    """The first event from backend.subscribe() matching predicate, or None.

    Runs subscribe() in a background daemon thread and reads its events off
    a queue with a real timeout, so a generator that yields nothing, since
    every backend's subscribe() loops forever by design, never hangs this
    test.

    on_ready, when given, runs once the subscription actually has a
    baseline, before this function waits for any event. Neither backend
    replays an event from before a subscriber arrives, so a caller
    watching for a short-lived process's own exit passes a callable that
    spawns it there: the subscription exists first, and the process
    cannot end before it does.

    tmux and herdr accept an optional `ready` keyword on subscribe() that
    each sets once its own first poll has actually succeeded, not merely
    been attempted; this function waits on that event, so on_ready runs
    only once the subscription has a real baseline. coppice's subscribe()
    takes no such keyword, since this round's file list keeps
    coppice_backend.py untouched: for coppice this instead waits for the
    pump thread to start, then makes one list() round trip from the main
    thread before calling on_ready. That is not a strict ordering
    guarantee, since list() opens its own connection rather than the
    shared events connection subscribe() uses; in practice the server
    only answers a fresh connection's request after it has already
    accepted the events.subscribe request the pump thread queued first on
    its own connection, which is the ordering this function actually
    needs.

    Closes the backend's shared connection before returning, found or not:
    the pump thread's own read stays blocked on that connection forever
    otherwise, past this function's own return, holding it open against the
    real server for the rest of the test run.

    The whole budget, from this function's own entry to its return, is one
    timeout_s: deadline is taken up front, before either branch below runs,
    so a subscription that spends most of that budget just reaching a
    first baseline is not handed a second full timeout_s for the polling
    loop that follows. For tmux and herdr, a baseline that never arrives
    fails loudly here, naming the backend, rather than falling through to
    the polling loop's own generic failure message. coppice has no ready
    keyword; its own probe call is a plain list(), best-effort, so a
    failure there is swallowed rather than skipping the cleanup below.
    """
    events: queue.Queue = queue.Queue()
    thread_up = threading.Event()
    ready = threading.Event()
    supports_ready = backend.name in ("tmux", "herdr")
    deadline = time.monotonic() + timeout_s

    def pump() -> None:
        thread_up.set()
        try:
            iterator = backend.subscribe(ready=ready) if supports_ready else backend.subscribe()
            for event in iterator:
                events.put(event)
        except Exception as exc:  # noqa: BLE001 - handed to the caller through the queue
            events.put(exc)

    threading.Thread(target=pump, daemon=True).start()
    if supports_ready:
        assert ready.wait(timeout=timeout_s), (
            f"{backend.name}: subscribe() never reached a first successful poll"
        )
    else:
        thread_up.wait(timeout=timeout_s)
        with contextlib.suppress(Exception):
            backend.list()
    if on_ready is not None:
        on_ready()
    found = None
    raised = None
    while time.monotonic() < deadline:
        remaining = max(0.0, deadline - time.monotonic())
        try:
            item = events.get(timeout=min(remaining, 0.5))
        except queue.Empty:
            continue
        if isinstance(item, Exception):
            raised = item
            break
        if predicate(item):
            found = item
            break
    close_connection = getattr(backend, "close_connection", None)
    if callable(close_connection):
        close_connection()
    if raised is not None:
        raise raised
    return found


class _NeverReadyBackend:
    """A tmux-or-herdr-shaped double whose subscribe() never sets ready:
    exercises _subscribe_until's own budget and failure message with no
    real host involved."""

    name = "tmux"

    def __init__(self):
        self._stopped = False

    def subscribe(self, *, ready=None):
        while not self._stopped:
            time.sleep(0.02)
        yield from ()

    def close_connection(self):
        self._stopped = True


def test_subscribe_until_raises_naming_the_backend_when_ready_never_arrives():
    backend = _NeverReadyBackend()
    started = time.monotonic()
    with pytest.raises(AssertionError) as excinfo:
        _subscribe_until(backend, lambda ev: True, 0.3)
    elapsed = time.monotonic() - started
    assert "tmux" in str(excinfo.value)
    assert elapsed < 0.45, (
        f"took {elapsed:.2f}s for a 0.3s budget: the deadline must not restart after the wait"
    )
    backend.close_connection()


class _CoppiceListRaisesOnReadyProbe:
    """A coppice-shaped double whose list() raises: exercises _subscribe_until's
    own cleanup guarantee with no real host involved."""

    name = "coppice"

    def __init__(self):
        self._stopped = False
        self.closed = False

    def list(self):
        raise RuntimeError("the ready probe's own list() call failed")

    def subscribe(self):
        while not self._stopped:
            time.sleep(0.02)
        yield from ()

    def close_connection(self):
        self.closed = True
        self._stopped = True


def test_subscribe_until_still_closes_the_connection_when_the_ready_probe_raises():
    backend = _CoppiceListRaisesOnReadyProbe()
    found = _subscribe_until(backend, lambda ev: True, 0.3)
    assert found is None
    assert backend.closed is True, "a raising list() must not skip close_connection()"


def test_the_backend_names_itself_and_never_raises_in_available(backend):
    assert backend.name in ("coppice", "herdr", "tmux")
    assert isinstance(backend.available(), bool)


def test_spawn_read_finish_close(backend, tmp_path):
    """The whole contract in one pass, so a partial backend cannot half-pass it."""
    ref = backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo READY; sleep 2"],
        env={"COPPICE_PANE": "contract"},
        label="contract",
        kind="pty",
        harness=None,
    )
    assert ref.backend == backend.name and ref.id

    # 1. read visible shows READY within a second of the process printing it
    deadline = time.time() + _READY_S
    seen = ""
    while time.time() < deadline:
        seen = backend.read(ref, source="visible")
        if "READY" in seen:
            break
        time.sleep(0.1)
    assert "READY" in seen, f"{backend.name}: never saw READY, got {seen!r}"

    # 2. the pane is in list() while it runs
    assert ref.id in _ids(backend)

    # 3. the shell exits on its own; the pane reaches done, or leaves the
    #    list, which the contract accepts as done too
    backend.send_text(ref, "exit")
    deadline = time.time() + _DONE_S
    finished = False
    while time.time() < deadline:
        infos = {i.ref.id: i for i in backend.list()}
        info = infos.get(ref.id)
        if info is None:
            finished = True
            break
        if info.state is not None and info.state.state == "done":
            finished = True
            break
        time.sleep(0.2)
    assert finished, f"{backend.name}: the pane never reached done and never left the list"

    # 4. close is safe on a pane that already finished, and safe again on an
    #    already-closed one. tmux and herdr drop a closed pane from list();
    #    coppice's pane.list is a history and keeps the row, so a pane that
    #    stays listed must at least read done, never a live pty.
    backend.close(ref)
    backend.close(ref)
    if ref.id in _ids(backend):
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        assert info.state is not None and info.state.state == "done", (
            f"{backend.name}: a pane still listed after close must read done"
        )


def test_close_stops_a_pane_that_is_still_running(backend, tmp_path):
    """Step 4 of the row above tolerates a pane close() leaves listed, as
    long as it reads done. That tolerance only means something if close()
    is proven to actually stop a pane somewhere. This row closes one that
    is still alive: a no-op close() fails it on every parameter, because
    the pane was running when close() was called.
    """
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="closenow", kind="pty"
    )
    assert ref.id in _ids(backend)
    backend.close(ref)
    deadline = time.time() + _DONE_S
    while time.time() < deadline:
        infos = {i.ref.id: i for i in backend.list()}
        info = infos.get(ref.id)
        if info is None or (info.state is not None and info.state.state == "done"):
            return
        time.sleep(0.2)
    raise AssertionError(f"{backend.name}: close() left the pane running")


def test_read_accepts_all_three_sources(backend, tmp_path):
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "echo READY; sleep 3"], env={}, label="sources", kind="pty"
    )
    try:
        time.sleep(1.0)
        for source in ("visible", "recent", "detection"):
            assert isinstance(backend.read(ref, source=source), str)
    finally:
        backend.close(ref)


def test_send_keys_accepts_the_names_the_floor_binds(backend, tmp_path):
    ref = backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="keys", kind="pty")
    try:
        backend.send_keys(ref, ["ctrl+c"])
    finally:
        backend.close(ref)


def test_close_on_an_unknown_pane_never_raises(backend):
    backend.close(PaneRef(backend.name, "definitely-not-a-pane"))


def test_resize_is_accepted_by_every_backend(backend, tmp_path):
    ref = backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="sz", kind="pty")
    try:
        backend.resize(ref, 100, 30)  # tmux and herdr return None; coppice resizes
    finally:
        backend.close(ref)


def test_a_state_a_backend_reports_carries_a_real_source_and_is_never_a_bare_idle(
    backend, tmp_path
):
    """Master §3.1: `unknown` with no source, never `idle`; a manifest never says done."""
    from opendaisugi.floor.events import SOURCES, STATES

    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="state", kind="pty"
    )
    try:
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        if info.state is None:
            return  # no source is the honest answer; the roster paints `unknown`
        assert info.state.source in SOURCES
        assert info.state.state in STATES
        assert info.state.state != "idle" or info.state.detail, (
            f"{backend.name}: an idle state must at least name why, whatever the source"
        )
        if info.state.source == "manifest":
            assert info.state.state != "done", "a manifest may never say done"
    finally:
        backend.close(ref)


def test_subscribe_reports_a_pane_exit_within_the_budget(backend, tmp_path):
    """`subscribe()` is in the §3.2 protocol and runs on all three, so it is a contract.

    A backend could pass every other test here with `subscribe()` broken,
    which is exactly how the tmux `done` defect got through the first
    review. Neither backend replays an event from before a subscriber
    arrives, so the pane is spawned only once the pump thread is already
    running its subscription; spawning it first, as this row once did,
    races a short-lived process against its own subscription.
    """
    ref_box: dict[str, PaneRef] = {}

    def spawn_it() -> None:
        ref_box["ref"] = backend.spawn(
            cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="exit", kind="pty"
        )
        assert ref_box["ref"].id in _ids(backend), (
            f"{backend.name}: the pane exited before it was seen"
        )

    event = _subscribe_until(
        backend,
        lambda ev: (
            getattr(ev, "pane", None) == ref_box["ref"].id and getattr(ev, "state", "") == "done"
        ),
        _DONE_S,
        on_ready=spawn_it,
    )
    assert event is not None, f"{backend.name}: subscribe() never reported the pane's exit"
    assert event.source == "process", (
        f"{backend.name}: a pane exit must be a process fact, not {event.source}"
    )


def test_report_state_accepts_an_event_and_never_raises(backend, tmp_path):
    """Push authority in, master §3.2. tmux drops it, herdr forwards it, coppice stores it."""
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="report", kind="pty"
    )
    try:
        backend.report_state(
            ref,
            PaneStateEvent(
                session_id="contract",
                harness="shell",
                state="working",
                source="gate",
                ts=time.time(),
                pane=ref.id,
                detail="verdict=allow",
            ),
        )
    finally:
        backend.close(ref)


def test_read_rejects_a_source_that_is_not_in_the_protocol(backend, tmp_path):
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 3"], env={}, label="badsrc", kind="pty"
    )
    try:
        with pytest.raises(ValueError):
            backend.read(ref, source="scrollback")
    finally:
        backend.close(ref)


def test_every_backend_declares_whether_it_yields_frames(backend):
    """Master §3.2: only coppice yields frames. The floor's attach screen depends on knowing.

    A backend declaring True is a promise: FloorScreen.action_attach reads
    yields_frames through getattr and then calls attach and detach. A
    backend that made the promise without the verbs would pass every other
    row here and still crash the attach screen.
    """
    assert isinstance(backend.yields_frames, bool)
    assert backend.yields_frames is (backend.name == "coppice")
    if backend.yields_frames:
        assert callable(getattr(backend, "attach", None)), (
            f"{backend.name} yields frames but has no attach()"
        )
        assert callable(getattr(backend, "detach", None)), (
            f"{backend.name} yields frames but has no detach()"
        )


def _declared_keys(backend) -> set[str]:
    if backend.name == "tmux":
        from opendaisugi.floor.tmux_backend import KEY_MAP

        return set(KEY_MAP)
    if backend.name == "coppice":
        return set(KNOWN_KEYS)
    if backend.name == "herdr":
        return set(backend.verbs.keys)
    raise AssertionError(f"no declared key vocabulary known for {backend.name}")


def test_every_backends_key_vocabulary_is_a_subset_of_coppices_named_keys(backend):
    """One key vocabulary, written once, in Go. A backend cannot bind a key coppice lacks.

    coppice's own declared vocabulary IS KNOWN_KEYS, so the subset check is a
    set against itself for that parameter; it still means something, since
    KNOWN_KEYS is loaded from keys.json and comes back an empty frozenset
    when that file is missing. Asserted directly for coppice, so a missing
    or empty keys.json fails loudly there too.
    """
    if backend.name == "coppice":
        assert KNOWN_KEYS, "keys.json is missing or empty"
    declared = _declared_keys(backend)
    assert declared <= KNOWN_KEYS, (
        f"{backend.name} declares keys coppice does not accept: {declared - KNOWN_KEYS}"
    )


def _dead_backend(name: str, tmp_path, monkeypatch):
    """A fresh backend of the same kind, asked the same question: the
    executable itself is not there. tmux and herdr shell out, so
    `shutil.which` returning None is the shared shape for both; coppice
    never shells out, so it also gets a socket path nothing is listening
    on, the same question asked in its own terms.
    """
    monkeypatch.setattr("shutil.which", lambda exe: None)
    if name == "tmux":
        return TmuxBackend(socket=f"{TMUX_SOCKET}-gone")
    if name == "coppice":
        return CoppiceBackend(sock_path=tmp_path / "gone" / "server.sock", data_dir=tmp_path)
    if name == "herdr":
        return HerdrBackend()
    raise AssertionError(f"no dead-host shape known for {name}")


def test_list_read_and_close_never_raise_when_the_host_is_gone(backend, tmp_path, monkeypatch):
    """Master §3.2: a gone host degrades list, read and close, and raises
    only on spawn, send_text and send_keys. A cockpit reading a roster
    must survive a host that has already gone away, and a caller that
    tries to act on one must still hear about it.
    """
    dead = _dead_backend(backend.name, tmp_path, monkeypatch)
    assert dead.list() == []
    assert dead.read(PaneRef(dead.name, "gone")) == ""
    dead.close(PaneRef(dead.name, "gone"))  # must not raise
    # bare Exception on purpose: tmux, coppice and herdr raise three
    # different types here, and the contract's claim is "it raises", not
    # "it raises this type".
    with pytest.raises(Exception):  # noqa: B017
        dead.spawn(cwd=tmp_path, cmd=["sh", "-c", "true"], env={}, label="x", kind="pty")
    with pytest.raises(Exception):  # noqa: B017
        dead.send_text(PaneRef(dead.name, "gone"), "hi")
    with pytest.raises(Exception):  # noqa: B017
        dead.send_keys(PaneRef(dead.name, "gone"), ["enter"])


def test_report_state_downgrades_an_unattached_operator_claim_to_gate(tmp_path, coppice_server):
    """`internal/server/agents.go`'s handleReportState: only a client
    watching the pane may speak for the operator. report_state opens a
    fresh connection per call, so it is never attached, and the server
    downgrades the claim to gate rather than trusting it.
    """
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="downgrade", kind="pty"
    )
    try:
        backend.report_state(
            ref,
            PaneStateEvent(
                session_id="c1",
                harness="claude-code",
                state="idle",
                source="operator",
                ts=time.time(),
                pane=ref.id,
                detail="I am here",
            ),
        )
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        assert info.state is not None
        assert info.state.source == "gate", "an unattached operator claim must be downgraded"
    finally:
        backend.close(ref)


def test_report_state_of_a_downgraded_gate_blocked_claim_with_no_ask_is_refused(
    tmp_path, coppice_server
):
    """A human operator may say blocked with no ask. The same claim,
    downgraded to gate because nothing is attached, may not: a gate's
    blocked claim always carries the ask it is holding.
    """
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="downgrade-refused", kind="pty"
    )
    try:
        with pytest.raises(CoppiceError) as e:
            backend.report_state(
                ref,
                PaneStateEvent(
                    session_id="c1",
                    harness="claude-code",
                    state="blocked",
                    source="operator",
                    ts=time.time(),
                    pane=ref.id,
                    detail="stuck",
                ),
            )
        assert e.value.code == "bad_request"
    finally:
        backend.close(ref)


def test_a_gate_blocked_claim_with_no_ask_is_refused_before_a_backend_sees_it():
    """coppice refuses this shape over its own wire, through the downgrade
    path above. tmux and herdr have no such wire: PaneStateEvent refuses a
    gate source blocked state with no ask at construction, before either
    backend's report_state ever sees it. No host needed.
    """
    with pytest.raises(ValueError):
        PaneStateEvent(
            session_id="c1",
            harness="shell",
            state="blocked",
            source="gate",
            ts=time.time(),
            pane="p1",
        )


def test_a_whole_number_ask_deadline_round_trips_as_a_float(tmp_path, coppice_server):
    """Go's JSON encoder drops the decimal point on a whole-number float, so
    a deadline of a whole number of seconds comes back over the wire as
    the JSON integer, not a float. list() must still hand back a float.
    The deadline itself is set well into the future: a whole-number
    deadline already in the past reads as expired instead, through the
    server's own EffectiveState, which is a different row entirely.
    """
    deadline = float(int(time.time()) + 3600)
    backend = CoppiceBackend(sock_path=coppice_server.socket_path, data_dir=coppice_server.data_dir)
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="deadline", kind="pty"
    )
    try:
        backend.report_state(
            ref,
            PaneStateEvent(
                session_id="c1",
                harness="claude-code",
                state="blocked",
                source="gate",
                ts=time.time(),
                pane=ref.id,
                detail="needs an answer",
                ask=Ask(id="a1", tool="Bash", summary="rm -rf", deadline=deadline),
            ),
        )
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        assert info.state is not None and info.state.ask is not None
        assert isinstance(info.state.ask.deadline, float)
        assert info.state.ask.deadline == deadline
    finally:
        backend.close(ref)


def test_a_gate_blocked_fact_reaches_the_backend_the_registry_builds(backend, tmp_path):
    """A gate 'blocked' fact must show up as blocked, source gate, on the
    backend an operator actually gets from build_backend.

    coppice already carries this over its own wire, through report_state.
    This proves it once more through the same registry path the other two
    parameters use, for one comparable row across all three. tmux and
    herdr never hear from the gate directly: the fact lives in the
    session tree, and build_backend wires gate_states_reader over that
    store into the backend it constructs.
    """
    deadline = time.time() + 3600
    if backend.name == "coppice":
        ref = backend.spawn(
            cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="gatefact", kind="pty"
        )
        try:
            backend.report_state(
                ref,
                PaneStateEvent(
                    session_id="gate-s1",
                    harness="claude-code",
                    state="blocked",
                    source="gate",
                    ts=time.time(),
                    pane=ref.id,
                    detail="needs an answer",
                    ask=Ask(id="a1", tool="Bash", summary="rm -rf", deadline=deadline),
                ),
            )
            info = next(i for i in backend.list() if i.ref.id == ref.id)
            assert info.state is not None
            assert info.state.state == "blocked" and info.state.source == "gate"
            assert info.state.session_id == "gate-s1"
        finally:
            backend.close(ref)
        return

    from opendaisugi.config import Config, FloorConfig
    from opendaisugi.floor.registry import build_backend
    from opendaisugi.session_tree import SessionTree

    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 300"], env={}, label="gatefact", kind="pty"
    )
    try:
        assert ref.id in _ids(backend), f"{backend.name}: the spawned pane never listed"
        ev = PaneStateEvent(
            session_id="gate-s1",
            harness="claude-code",
            state="blocked",
            source="gate",
            ts=time.time(),
            pane=ref.id,
            detail="needs an answer",
            ask=Ask(id="a1", tool="Bash", summary="rm -rf", deadline=deadline),
        )
        tree = SessionTree.open_or_create(
            tmp_path / "sessions",
            session_id="gate-s1",
            harness="claude-code",
            cwd=str(tmp_path),
        )
        tree.append("state", json.loads(ev.to_json()), clock=lambda: ev.ts)

        cfg = Config(
            data_dir=tmp_path,
            floor=FloorConfig(tmux_socket=TMUX_SOCKET if backend.name == "tmux" else None),
        )
        wired = build_backend(backend.name, cfg)
        try:
            info = next((i for i in wired.list() if i.ref.id == ref.id), None)
            assert info is not None, f"{backend.name}: the wired backend never listed the pane"
            assert info.state is not None
            assert info.state.state == "blocked" and info.state.source == "gate"
            assert info.state.session_id == "gate-s1"
        finally:
            close_connection = getattr(wired, "close_connection", None)
            if callable(close_connection):
                close_connection()
    finally:
        backend.close(ref)


def test_list_tasks_returns_a_list_on_every_backend(backend):
    """Master spec section 3.2: list_tasks never raises. A backend with no
    tasks answers an empty list."""
    tasks = backend.list_tasks()
    assert isinstance(tasks, list)
    for info in tasks:
        assert isinstance(info, TaskInfo)


@pytest.mark.parametrize("name", ["herdr", "tmux"])
def test_herdr_and_tmux_refuse_tasks_naming_themselves(name, tmp_path):
    """Neither Herdr nor tmux has tasks. Every task verb raises
    NotImplementedError that names the backend and the one that works,
    before any binary is asked anything."""
    made = _make(name)
    want = f"{name} has no tasks. Use the coppice backend."
    assert made.list_tasks() == []
    with pytest.raises(NotImplementedError, match=re.escape(want)):
        made.create_task("x")
    with pytest.raises(NotImplementedError, match=re.escape(want)):
        made.close_task("t1")
    with pytest.raises(NotImplementedError, match=re.escape(want)):
        made.spawn(cwd=tmp_path, cmd=["sh"], env={}, label="x", kind="pty", task="t1")
