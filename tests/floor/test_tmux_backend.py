"""tmux as a pane backend. Every live test runs on a private server, `-L daisugi-test`,
torn down after.

The pure functions run everywhere. The live tests skip with a named reason when
tmux is absent. `done` on tmux is inferred from a pane leaving `list-panes`, so it
is tagged `source: "process"` -- master §3.1 forbids a manifest saying done.

tmux holds no state of its own, so the backend keeps its own last-known state per
pane and merges new facts into it with `opendaisugi.floor.merge`. A rule that
matches nothing, and a rule that matches but sets `skip_state_update`, both mean
"no event this tick": the pane keeps whatever a real source last said. Several
tests below prove that directly, without a live pane, by calling the private
`_state_for` helper with a monkeypatched `read`.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import threading
import time
from pathlib import Path

import pytest

from opendaisugi.floor import Ask, PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.tmux_backend import (
    KEY_MAP,
    TmuxBackend,
    parse_tmux_version,
    supports_env_flag,
)
from tests.floor import hostfacts
from tests.floor.conftest import write_manifest

# Named with this process's own pid so two pytest processes running this
# file at once never kill each other's private server.
SOCKET = f"daisugi-test-{os.getpid()}"


@pytest.fixture
def tmux_backend():
    reason = hostfacts.skip_reason("tmux")
    if reason:
        pytest.skip(reason)
    backend = TmuxBackend(socket=SOCKET)
    yield backend
    subprocess.run(
        [shutil.which("tmux"), "-L", SOCKET, "kill-server"], capture_output=True, check=False
    )
    # One socket file per pytest process, since SOCKET carries this
    # process's own pid; unlinked here so a long-running box does not
    # accumulate one dead file per run.
    Path(f"/tmp/tmux-{os.getuid()}/{SOCKET}").unlink(missing_ok=True)


@pytest.mark.parametrize(
    "text,want",
    [
        ("tmux 3.2", (3, 2)),
        ("tmux 3.1c", (3, 1)),
        ("tmux 3.7b", (3, 7)),
        ("tmux next-3.8", (3, 8)),
        ("tmux 2.9a", (2, 9)),
        ("tmux master", (0, 0)),
        ("", (0, 0)),
    ],
)
def test_version_parser_survives_every_real_tmux_spelling(text, want):
    assert parse_tmux_version(text) == want


def test_env_flag_needs_three_two():
    assert supports_env_flag((3, 2)) is True
    assert supports_env_flag((3, 7)) is True
    assert supports_env_flag((3, 1)) is False
    assert supports_env_flag((0, 0)) is False


def test_key_map_covers_the_names_the_floor_binds():
    for name in ("enter", "ctrl+c", "esc", "tab", "f1"):
        assert name in KEY_MAP
    assert KEY_MAP["enter"] == "Enter"
    assert KEY_MAP["ctrl+c"] == "C-c"
    assert KEY_MAP["esc"] == "Escape"


def test_available_is_false_without_tmux(monkeypatch):
    monkeypatch.setattr("shutil.which", lambda name: None)
    assert TmuxBackend(socket=SOCKET).available() is False


def test_spawn_from_a_cold_server_then_read_and_close(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo READY; sleep 5"],
        env={"COPPICE_PANE": "t1"},
        label="probe",
        kind="pty",
    )
    assert ref.backend == "tmux" and ref.id.startswith("%")
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "READY" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "READY" in tmux_backend.read(ref, source="visible")
    assert ref.id in {i.ref.id for i in tmux_backend.list()}
    tmux_backend.close(ref)
    assert ref.id not in {i.ref.id for i in tmux_backend.list()}


def test_the_label_is_the_window_name(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="auth fix", kind="pty"
    )
    info = next(i for i in tmux_backend.list() if i.ref.id == ref.id)
    assert info.label == "auth fix"
    assert info.cwd == str(tmp_path)


def test_env_reaches_the_pane(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "echo P=$COPPICE_PANE; sleep 5"],
        env={"COPPICE_PANE": "w1p9"},
        label="envtest",
        kind="pty",
    )
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "P=w1p9" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "P=w1p9" in tmux_backend.read(ref, source="visible")


def test_send_text_types_and_presses_enter(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["bash", "--norc", "-i"], env={}, label="keys", kind="pty"
    )
    time.sleep(0.8)
    tmux_backend.send_text(ref, "echo hello-literal")
    deadline = time.time() + 3.0
    while time.time() < deadline:
        if "hello-literal" in tmux_backend.read(ref, source="visible"):
            break
        time.sleep(0.1)
    assert "hello-literal" in tmux_backend.read(ref, source="visible")


def test_detection_source_is_the_whole_unwrapped_screen(tmux_backend, tmp_path):
    """The bottom-12-lines cap is gone. `detection` hands back the pane's whole
    visible screen, unnarrowed -- a manifest's own `region` is what narrows it,
    not a fixed window underneath every rule.
    """
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "for i in $(seq 1 40); do echo line$i; done; sleep 5"],
        env={},
        label="detect",
        kind="pty",
    )
    time.sleep(1.0)
    tail = tmux_backend.read(ref, source="detection")
    lines = tail.splitlines()
    assert len(lines) > 12, "the old 12-line cap must not still be applied"
    assert "line40" in tail
    assert "line30" in tail, "a 12-line cap would have already dropped this line"


def test_close_is_idempotent_on_a_pane_that_already_exited(tmux_backend, tmp_path):
    """tmux exits 1 on a gone pane -- 'can't find pane' when the server survives,
    'no server running' when the pane's own window was the server's last one.
    Both are success, not a failure.
    """
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "true"], env={}, label="short", kind="pty"
    )
    time.sleep(1.0)
    tmux_backend.close(ref)  # must not raise
    tmux_backend.close(ref)


def test_a_finished_pane_reports_done_from_the_process_not_a_manifest(tmux_backend, tmp_path):
    """`done` is a process fact. It must arrive with no manifest installed at all.

    The pane sleeps one second so the first `subscribe()` poll certainly SEES it
    before it goes; `subscribe` tracks pane ids, not states, so an unclassified pane
    is still announced when it exits.
    """
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="short", kind="pty"
    )
    assert ref.id in {i.ref.id for i in tmux_backend.list()}, "the pane exited too fast"
    deadline = time.time() + 8.0
    seen = None
    for event in tmux_backend.subscribe():
        if event.pane == ref.id and event.state == "done":
            seen = event
            break
        if time.time() > deadline:
            break
    assert seen is not None, "a pane that exited never reported done"
    assert seen.source == "process"
    assert seen.detail and "manifest" not in seen.detail


def test_done_arrives_even_when_no_manifests_are_installed(
    tmux_backend, tmp_path, tmp_path_factory
):
    """The regression this blocker was: state-keyed tracking dropped unclassified panes."""
    empty = tmp_path_factory.mktemp("no-manifests")
    backend = TmuxBackend(socket=SOCKET, manifest_dirs=[empty])
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="nomanifest", kind="pty"
    )
    info = next(i for i in backend.list() if i.ref.id == ref.id)
    assert info.state is None, "no manifests means no state, and that must not hide the exit"
    deadline = time.time() + 8.0
    for event in backend.subscribe():
        if event.pane == ref.id and event.state == "done":
            return
        if time.time() > deadline:
            break
    raise AssertionError("done never arrived for an unclassified pane")


def test_the_start_command_is_unquoted_before_it_is_split(tmux_backend, tmp_path):
    """tmux 3.7b returns pane_start_command WITH its surrounding quotes."""
    from opendaisugi.floor.tmux_backend import _unquote_start_command

    assert _unquote_start_command("\"sh -c 'echo hi'\"") == "sh -c 'echo hi'"
    assert _unquote_start_command("plain") == "plain"
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="quoted", kind="pty"
    )
    info = next(i for i in tmux_backend.list() if i.ref.id == ref.id)
    assert info.cmd[0] == "sh", f"the start command was not unquoted: {info.cmd!r}"


def test_read_rejects_an_unknown_source_instead_of_showing_visible(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="badsrc", kind="pty"
    )
    with pytest.raises(ValueError) as e:
        tmux_backend.read(ref, source="scrollback")
    assert "visible" in str(e.value) and "detection" in str(e.value)


def test_available_does_not_depend_on_a_parsable_version(monkeypatch):
    """A working tmux whose -V we cannot parse must still run the contract suite."""
    backend = TmuxBackend(socket=SOCKET)
    monkeypatch.setattr("shutil.which", lambda name: "/usr/bin/tmux")
    monkeypatch.setattr(backend, "_version_tuple", lambda: (0, 0))
    assert backend.available() is True


def test_spawn_accepts_the_protocols_harness_keyword_and_ignores_it(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path,
        cmd=["sh", "-c", "sleep 5"],
        env={},
        label="h",
        kind="pty",
        harness="claude-code",
    )
    assert ref.id.startswith("%")


def test_resize_is_a_documented_no_op(tmux_backend, tmp_path):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="sz", kind="pty"
    )
    assert tmux_backend.resize(ref, 200, 60) is None
    assert "tmux owns the layout" in TmuxBackend.resize.__doc__


# --- state: skip, no-match, gate precedence -- no live pane needed ----------


def test_a_skip_rule_leaves_the_previous_state_in_place(tmp_path):
    """`skip_state_update` means 'emit nothing'. A Claude pane sitting in its
    transcript viewer must not be reset to unknown every poll, wiping whatever
    the last real evidence said.
    """
    manifest_dir = tmp_path / "manifests"
    manifest_dir.mkdir()
    write_manifest(
        manifest_dir,
        "sleep",
        '[[rules]]\nid = "busy"\nstate = "working"\npriority = 10\ncontains = ["BUSY"]\n'
        '[[rules]]\nid = "viewer"\nstate = "unknown"\npriority = 20\n'
        'skip_state_update = true\ncontains = ["VIEWER"]\n',
    )
    backend = TmuxBackend(socket=SOCKET, manifest_dirs=[manifest_dir])
    screens = iter(["BUSY", "BUSY VIEWER"])
    backend.read = lambda ref, source="visible": next(screens)
    ref = PaneRef(backend="tmux", id="%9")

    first = backend._state_for(ref, "sleep", "", {}, time.time())
    assert first is not None and first.state == "working"

    second = backend._state_for(ref, "sleep", "", {}, time.time())
    assert second is first, "a winning skip rule must not touch the cached state at all"


def test_a_true_no_match_also_leaves_the_previous_state_in_place(tmp_path):
    """No rule matching means no event either -- master spec 3.1 forbids inventing
    idle, and the coppice Go twin in server/detect.go treats a bare no-match
    exactly the same as skip_state_update: the pane keeps whatever a real
    source last said.
    """
    manifest_dir = tmp_path / "manifests"
    manifest_dir.mkdir()
    write_manifest(
        manifest_dir, "sleep", '[[rules]]\nid = "busy"\nstate = "working"\ncontains = ["BUSY"]\n'
    )
    backend = TmuxBackend(socket=SOCKET, manifest_dirs=[manifest_dir])
    screens = iter(["BUSY", "nothing interesting here"])
    backend.read = lambda ref, source="visible": next(screens)
    ref = PaneRef(backend="tmux", id="%9")

    first = backend._state_for(ref, "sleep", "", {}, time.time())
    assert first is not None and first.state == "working"

    second = backend._state_for(ref, "sleep", "", {}, time.time())
    assert second is first, "an unmatched screen must not erase the last real state"


def test_a_stale_gate_snapshot_does_not_reclaim_a_pane_the_manifest_already_moved_past(tmp_path):
    """gate_states() may keep handing back the SAME already-resolved event forever;
    the caller supplying it is not obliged to prune its own snapshot. Re-merging
    that one fact on every poll must not flip a pane back to blocked once the
    manifest has moved it on.
    """
    manifest_dir = tmp_path / "manifests"
    manifest_dir.mkdir()
    write_manifest(
        manifest_dir, "sleep", '[[rules]]\nid = "busy"\nstate = "working"\ncontains = ["BUSY"]\n'
    )
    old_deadline = time.time() - 100.0
    stale = PaneStateEvent(
        session_id="s",
        harness="sleep",
        state="blocked",
        source="gate",
        ts=time.time() - 200.0,
        pane="%9",
        ask=Ask(id="a1", tool="Bash", summary="run it", deadline=old_deadline),
    )
    backend = TmuxBackend(
        socket=SOCKET, manifest_dirs=[manifest_dir], gate_states=lambda: {"%9": stale}
    )
    screens = iter(["BUSY", "quiet", "quiet"])
    backend.read = lambda ref, source="visible": next(screens)
    ref = PaneRef(backend="tmux", id="%9")
    gate = {"%9": stale}

    first = backend._state_for(ref, "sleep", "", gate, time.time())
    assert first.state == "working", "an already-expired gate hold must yield to fresh evidence"

    second = backend._state_for(ref, "sleep", "", gate, time.time())
    assert second.state == "working", (
        "re-merging the same stale gate snapshot must not reclaim a pane the manifest left"
    )


def test_a_cached_state_for_a_vanished_pane_id_is_evicted_but_a_live_one_is_kept(
    tmux_backend, tmp_path
):
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="alive", kind="pty"
    )
    live_ev = PaneStateEvent(
        session_id="s",
        harness="sleep",
        state="working",
        source="manifest",
        ts=time.time(),
        pane=ref.id,
        detail="rule=busy region=whole_recent",
    )
    tmux_backend._current[ref.id] = live_ev
    ghost = "%999999"
    tmux_backend._current[ghost] = PaneStateEvent(
        session_id="s",
        harness="sleep",
        state="working",
        source="manifest",
        ts=time.time(),
        pane=ghost,
        detail="rule=busy region=whole_recent",
    )
    tmux_backend._gate_seen[ghost] = time.time()

    tmux_backend.list()

    assert ref.id in tmux_backend._current
    assert ghost not in tmux_backend._current
    assert ghost not in tmux_backend._gate_seen


def test_the_pane_title_reaches_the_manifest_as_the_osc_title_region(tmux_backend, tmp_path):
    manifest_dir = tmp_path / "manifests"
    manifest_dir.mkdir()
    write_manifest(
        manifest_dir,
        "sleep",
        '[[rules]]\nid = "titled"\nstate = "working"\n'
        'region = "osc_title"\ncontains = ["MYTITLE"]\n',
    )
    backend = TmuxBackend(socket=SOCKET, manifest_dirs=[manifest_dir])
    ref = backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 5"], env={}, label="titled", kind="pty"
    )
    backend._run("select-pane", "-t", ref.id, "-T", "MYTITLE")

    deadline = time.time() + 3.0
    info = None
    while time.time() < deadline:
        info = next(i for i in backend.list() if i.ref.id == ref.id)
        if info.state is not None and info.state.state == "working":
            break
        time.sleep(0.1)
    assert info is not None and info.state is not None and info.state.state == "working"


def test_a_gate_event_still_outranks_a_matching_manifest_within_the_hold(tmp_path):
    """master §5.1: a gate fact outranks a screen guess. A live, unexpired gate
    'blocked' hold must not be shouldered aside by a manifest match the same tick.
    """
    manifest_dir = tmp_path / "manifests"
    manifest_dir.mkdir()
    write_manifest(
        manifest_dir, "sleep", '[[rules]]\nid = "busy"\nstate = "working"\ncontains = ["BUSY"]\n'
    )
    ev = PaneStateEvent(
        session_id="s",
        harness="sleep",
        state="blocked",
        source="gate",
        ts=time.time(),
        pane="%9",
        ask=Ask(id="a1", tool="Bash", summary="run it", deadline=time.time() + 3600),
    )
    backend = TmuxBackend(
        socket=SOCKET, manifest_dirs=[manifest_dir], gate_states=lambda: {"%9": ev}
    )
    backend.read = lambda ref, source="visible": "BUSY"
    ref = PaneRef(backend="tmux", id="%9")

    result = backend._state_for(ref, "sleep", "", {"%9": ev}, time.time())
    assert result.state == "blocked" and result.source == "gate"


# --- subscribe(): a pane already pane_dead before subscribe() ever polls it --


def test_a_dead_pane_never_previously_seen_reports_done_on_the_first_poll():
    """A pane the operator's own remain-on-exit config kept on screen, already
    pane_dead the very first time subscribe() observes it, must still report
    done. It was never in `seen`, so `dead_now & seen` alone would miss it.
    """
    backend = TmuxBackend(socket=SOCKET)
    dead_info = PaneInfo(
        ref=PaneRef("tmux", "%9"), label="l", cwd="/", cmd=["sh"], kind="dead", state=None
    )
    backend._list_raw = lambda: (True, [dead_info])
    seen: set[str] = set()
    done_sent: set[str] = set()
    last: dict[str, str] = {}

    proven, events = backend._poll(seen, done_sent, last)

    assert proven is True
    assert len(events) == 1
    assert events[0].pane == "%9" and events[0].state == "done" and events[0].source == "process"


def test_a_lingering_dead_pane_reports_done_exactly_once_across_many_polls():
    """remain-on-exit keeps a dead pane in list-panes forever. done must fire
    once, not again on every later poll that still observes pane_dead.
    """
    backend = TmuxBackend(socket=SOCKET)
    dead_info = PaneInfo(
        ref=PaneRef("tmux", "%9"), label="l", cwd="/", cmd=["sh"], kind="dead", state=None
    )
    backend._list_raw = lambda: (True, [dead_info])
    seen: set[str] = set()
    done_sent: set[str] = set()
    last: dict[str, str] = {}

    all_events = []
    for _ in range(5):
        proven, events = backend._poll(seen, done_sent, last)
        assert proven is True
        all_events += events

    done_events = [e for e in all_events if e.pane == "%9" and e.state == "done"]
    assert len(done_events) == 1, f"done fired {len(done_events)} times for a lingering dead pane"


def test_poll_never_fabricates_done_when_list_panes_fails_ambiguously():
    """A `list-panes` failure that is not one of the known no-server
    wordings proves nothing. A pane that was already seen alive must not
    be reported done just because one poll could not reach tmux.
    """
    backend = TmuxBackend(socket=SOCKET)
    backend._list_raw = lambda: (False, [])
    seen: set[str] = {"%9"}
    done_sent: set[str] = set()
    last: dict[str, str] = {}

    proven, events = backend._poll(seen, done_sent, last)

    assert proven is False
    assert not any(e.pane == "%9" and e.state == "done" for e in events), (
        "an unproven poll failure must never fabricate done for a pane already seen alive"
    )
    assert "%9" in seen, "an unproven failure must not drop the pane from the tracked set either"


def test_poll_returns_its_own_proven_flag_instead_of_writing_shared_state():
    """_polled_ok used to be one instance attribute two concurrent
    generators could interleave through. _poll now hands its own proof
    back alongside its events, so the fact never has to live on the
    instance at all."""
    backend = TmuxBackend(socket=SOCKET)
    backend._list_raw = lambda: (False, [])
    proven, events = backend._poll(set(), set(), {})
    assert proven is False
    assert events == []
    assert not hasattr(backend, "_polled_ok"), (
        "the proven flag must live on the stack, not the instance"
    )


def test_list_raw_treats_only_the_known_gone_wordings_as_proven():
    """The one expression that decides whether a failed list-panes call is
    proof of anything is exercised directly here, not just through
    _poll's own stubbed shortcut: a real degraded result, from a timeout
    or an unrelated failure, must never read as proven."""
    backend = TmuxBackend(socket=SOCKET)

    def _fake_run(*args, check=True, stderr=""):
        return subprocess.CompletedProcess(list(args), 1, stdout="", stderr=stderr)

    backend._run = lambda *a, check=True: _fake_run(
        *a, check=check, stderr="no server running on x"
    )
    assert backend._list_raw() == (True, [])

    backend._run = lambda *a, check=True: _fake_run(
        *a, check=check, stderr="tmux timed out after 5.0s"
    )
    assert backend._list_raw() == (False, [])

    backend._run = lambda *a, check=True: _fake_run(
        *a, check=check, stderr="protocol version mismatch"
    )
    assert backend._list_raw() == (False, [])


def test_subscribe_reports_done_for_a_pane_already_dead_the_first_time_it_polls(
    tmux_backend, tmp_path
):
    """End-to-end proof through the real generator and a real tmux server:
    set remain-on-exit on this private server, let a pane die and go
    pane_dead before subscribe() ever runs, and confirm done still arrives.
    """
    keepalive = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 30"], env={}, label="keepalive", kind="pty"
    )
    tmux_backend._run("set-option", "-g", "remain-on-exit", "on")
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "true"], env={}, label="predead", kind="pty"
    )

    deadline = time.time() + 5.0
    info = None
    while time.time() < deadline:
        info = next((i for i in tmux_backend.list() if i.ref.id == ref.id), None)
        if info is not None and info.kind == "dead":
            break
        time.sleep(0.1)
    assert info is not None and info.kind == "dead", "the pane never went pane_dead"

    seen_done = None
    deadline = time.time() + 5.0
    for event in tmux_backend.subscribe():
        if event.pane == ref.id and event.state == "done":
            seen_done = event
            break
        if time.time() > deadline:
            break
    assert seen_done is not None, "a pane already pane_dead on first sight never reported done"
    assert keepalive.id != ref.id


# --- a hung tmux: reads degrade, writes still raise -------------------------


def test_a_tmux_timeout_degrades_reads_but_still_raises_on_a_write(monkeypatch, tmp_path):
    """The check=False callers of _run are read, list and close. They degrade
    like any other failed tmux call on a timeout. The check=True callers are
    spawn, send_text and send_keys. They still raise, never silently
    swallowing a hung tmux.
    """
    backend = TmuxBackend(socket=SOCKET)

    def hang(*args, **kwargs):
        raise subprocess.TimeoutExpired(cmd="tmux", timeout=5.0)

    monkeypatch.setattr(subprocess, "run", hang)

    assert backend.read(PaneRef("tmux", "%0"), source="visible") == ""
    assert backend.list() == []
    with pytest.raises(RuntimeError):
        backend.close(PaneRef("tmux", "%0"))
    with pytest.raises(RuntimeError):
        backend.spawn(cwd=tmp_path, cmd=["sh", "-c", "sleep 1"], env={}, label="x", kind="pty")


# --- a host that has never existed -------------------------------------------


def test_close_never_raises_against_a_socket_that_was_never_started():
    """A killed server says "no server running"; a socket nobody ever
    created says "error connecting to ... No such file or directory". Both
    mean the pane is gone, which is what close() was asked for.
    """
    reason = hostfacts.skip_reason("tmux")
    if reason:
        pytest.skip(reason)
    backend = TmuxBackend(socket=f"daisugi-test-never-started-{os.getpid()}")
    backend.close(PaneRef("tmux", "%0"))  # must not raise
    assert backend.list() == []
    assert backend.read(PaneRef("tmux", "%0"), source="visible") == ""


def test_close_never_raises_on_a_wording_gone_errors_does_not_list(monkeypatch, tmp_path):
    """tmux's own wording for "gone mid-transition" is not a fixed set;
    measured four different messages across a handful of runs against a
    pane racing its own process exit. close() falls back to checking
    list() itself before raising, so a fifth wording never becomes a
    false alarm.
    """
    reason = hostfacts.skip_reason("tmux")
    if reason:
        pytest.skip(reason)
    backend = TmuxBackend(socket=SOCKET)
    real_run = backend._run

    def fake_kill_pane(*args, **kwargs):
        if args[:1] == ("kill-pane",):
            import subprocess as sp

            return sp.CompletedProcess(args, 1, stdout="", stderr="a wording nobody pinned yet")
        return real_run(*args, **kwargs)

    monkeypatch.setattr(backend, "_run", fake_kill_pane)
    backend.close(PaneRef("tmux", "%not-a-real-pane"))  # must not raise


def test_close_raises_when_kill_pane_and_the_confirming_list_panes_both_fail(
    tmux_backend, monkeypatch, tmp_path
):
    """An unrecognized kill-pane wording, while the pane is genuinely
    still alive, must still raise. A confirming list-panes that itself
    fails for some other reason, a client/server version mismatch for
    instance, proves nothing about whether the pane is gone, and must not
    read as success.

    Takes the tmux_backend fixture rather than building a backend by
    hand, so its teardown kills the server and unlinks the socket file
    the same as every other live test in this file: a hand-built backend
    here left one dead socket file behind per run, one for every pytest
    process, since no later test in the file used the fixture either.
    """
    ref = tmux_backend.spawn(
        cwd=tmp_path, cmd=["sh", "-c", "sleep 30"], env={}, label="alive", kind="pty"
    )
    real_run = tmux_backend._run

    def fake_run(*args, **kwargs):
        import subprocess as sp

        if args[:1] == ("kill-pane",):
            return sp.CompletedProcess(args, 1, stdout="", stderr="a wording nobody pinned yet")
        if args[:1] == ("list-panes",):
            return sp.CompletedProcess(args, 1, stdout="", stderr="protocol version mismatch")
        return real_run(*args, **kwargs)

    monkeypatch.setattr(tmux_backend, "_run", fake_run)
    with pytest.raises(RuntimeError):
        tmux_backend.close(ref)


def test_list_read_and_close_never_raise_when_tmux_is_not_on_path(monkeypatch):
    """Master §3.2: only spawn, send_text and send_keys may raise. A tmux
    binary that has vanished from PATH is the same shape as a host that is
    gone: list(), read() and close() must degrade, not crash the cockpit.
    """
    monkeypatch.setattr("shutil.which", lambda name: None)
    backend = TmuxBackend(socket=SOCKET)
    assert backend.list() == []
    assert backend.read(PaneRef("tmux", "p1"), source="visible") == ""
    backend.close(PaneRef("tmux", "p1"))  # must not raise


def test_list_proven_is_the_same_answer_as_list_raw():
    """list_proven is the exact proof wait_for_state needs. tmux already
    computes it in _list_raw; list_proven is that call with no translation."""
    backend = TmuxBackend(socket=SOCKET)
    backend._list_raw = lambda: (False, [])
    assert backend.list_proven() == (False, [])


def test_spawn_send_text_and_send_keys_still_raise_when_tmux_is_not_on_path(monkeypatch, tmp_path):
    monkeypatch.setattr("shutil.which", lambda name: None)
    backend = TmuxBackend(socket=SOCKET)
    with pytest.raises(OSError):
        backend.spawn(cwd=tmp_path, cmd=["true"], env={}, label="x", kind="pty")
    with pytest.raises(OSError):
        backend.send_text(PaneRef("tmux", "p1"), "hi")
    with pytest.raises(OSError):
        backend.send_keys(PaneRef("tmux", "p1"), ["enter"])


def test_subscribe_clears_a_leftover_ready_event_before_polling():
    """A caller that keeps one threading.Event across two subscribe()
    calls must not read a leftover set flag as a fresh baseline: a
    re-subscribe clears it at the top, the same way _stopped already is.
    """
    backend = TmuxBackend(socket=SOCKET)
    backend._list_raw = lambda: (False, [])  # never proven, so nothing sets ready again
    ready = threading.Event()
    ready.set()  # simulates a leftover flag from a subscription already stopped

    def pump():
        for _ in backend.subscribe(ready=ready):
            pass

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    time.sleep(0.05)
    assert not ready.is_set(), "a re-subscribe must clear a leftover ready event"
    backend.close_connection()
    thread.join(timeout=2.0)


def test_close_connection_stops_the_poll_thread_within_two_intervals():
    """subscribe() polls forever by design; close_connection() is the only
    way to make it stop. A caller that starts a pump thread and later
    tears it down must see that thread actually end, not keep polling a
    server nobody is listening to any more.

    The real bound is one poll interval plus whatever tmux call is
    already in flight, not a flat two intervals; on this box, against a
    private server with nothing to make any single call slow, the join
    below comfortably covers that.
    """
    from opendaisugi.floor.tmux_backend import _POLL_S

    reason = hostfacts.skip_reason("tmux")
    if reason:
        pytest.skip(reason)
    backend = TmuxBackend(socket=SOCKET)
    stopped = threading.Event()

    def pump():
        for _ in backend.subscribe():
            pass
        stopped.set()

    thread = threading.Thread(target=pump, daemon=True)
    thread.start()
    time.sleep(0.05)  # let the loop start polling
    backend.close_connection()
    thread.join(timeout=2 * _POLL_S + 1.0)
    assert stopped.is_set(), "the pump thread never stopped"
