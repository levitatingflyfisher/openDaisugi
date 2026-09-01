"""Picking a backend, and the two drivers every client shares.

`auto` takes the first available backend in the master's order. An explicit name
that is not available is a hard, teaching refusal (exit 3 at the CLI), never a
silent downgrade to a different substrate.
"""

from __future__ import annotations

import time
from dataclasses import dataclass

import pytest

from opendaisugi.exceptions import FloorNotAvailable
from opendaisugi.floor import Ask, PaneInfo, PaneRef, PaneStateEvent
from opendaisugi.floor.registry import (
    BACKEND_ORDER,
    backend_statuses,
    pick_backend,
    prompt_pane,
    wait_for_state,
)


@dataclass
class _Floor:
    backend: str = "auto"
    notify_cmd: str | None = None
    tmux_socket: str | None = None


@dataclass
class _Cfg:
    floor: _Floor


class FakeBackend:
    name = "fake"

    def __init__(self, *, available=True, state="working"):
        self._available = available
        self._state = state
        self.sent: list[str] = []
        self.prompted: list[str] = []

    def available(self) -> bool:
        return self._available

    def send_text(self, pane, text, *, enter=True):
        self.sent.append(text)

    def list(self):
        ev = PaneStateEvent(
            session_id="s",
            harness="shell",
            state=self._state,
            source="process",
            ts=time.time(),
            pane="p1",
        )
        return [
            PaneInfo(
                ref=PaneRef("fake", "p1"), label="l", cwd="/", cmd=["sh"], kind="pty", state=ev
            )
        ]


class PromptingBackend(FakeBackend):
    def __init__(self, *, available=True, state="working"):
        super().__init__(available=available, state=state)
        self.prompt_calls: list[tuple] = []

    def prompt(self, pane, text, *, wait=False, timeout_s=60.0):
        self.prompted.append(text)
        self.prompt_calls.append((pane, text, wait, timeout_s))
        return "sent"


def test_order_is_the_masters_order():
    assert BACKEND_ORDER == ("coppice", "herdr", "tmux")


def test_auto_takes_the_first_available(monkeypatch):
    from opendaisugi.floor import registry

    built = {
        "coppice": FakeBackend(available=False),
        "herdr": FakeBackend(available=True),
        "tmux": FakeBackend(available=True),
    }
    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: built[name]
    )
    picked = pick_backend(_Cfg(_Floor(backend="auto")))
    assert picked is built["herdr"]


def test_auto_with_nothing_available_teaches_the_install(monkeypatch):
    from opendaisugi.floor import registry

    monkeypatch.setattr(
        registry,
        "build_backend",
        lambda name, config, *, autostart=False: FakeBackend(available=False),
    )
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="auto")))
    assert "coppice server start" in str(e.value)


def test_an_explicit_unavailable_backend_never_downgrades(monkeypatch):
    from opendaisugi.floor import registry

    built = {
        "coppice": FakeBackend(available=False),
        "herdr": FakeBackend(available=True),
        "tmux": FakeBackend(available=True),
    }
    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: built[name]
    )
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="coppice")))
    assert "coppice" in str(e.value) and "herdr" not in str(e.value)


def test_an_unknown_backend_name_lists_the_real_ones():
    with pytest.raises(FloorNotAvailable) as e:
        pick_backend(_Cfg(_Floor(backend="screen")))
    assert "coppice, herdr, tmux" in str(e.value)


def test_statuses_report_every_backend_with_a_reason(monkeypatch):
    from opendaisugi.floor import registry

    monkeypatch.setattr(
        registry,
        "build_backend",
        lambda name, config, *, autostart=False: FakeBackend(available=False),
    )
    rows = backend_statuses(_Cfg(_Floor()))
    assert [r.name for r in rows] == list(BACKEND_ORDER)
    for r in rows:
        assert r.available is False and r.why_not and r.fix


def test_a_backend_that_raises_in_available_is_reported_unavailable(monkeypatch):
    from opendaisugi.floor import registry

    class Exploding(FakeBackend):
        def available(self):
            raise RuntimeError("boom")

    monkeypatch.setattr(
        registry, "build_backend", lambda name, config, *, autostart=False: Exploding()
    )
    rows = backend_statuses(_Cfg(_Floor()))
    assert all(r.available is False for r in rows)
    assert "boom" in rows[0].why_not


def test_auto_never_asks_a_backend_to_autostart(monkeypatch):
    """Opening the floor must not spawn a daemon the operator did not name."""
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=(name == "tmux"))

    monkeypatch.setattr(registry, "build_backend", build)
    pick_backend(_Cfg(_Floor(backend="auto")))
    assert seen and not any(seen)


def test_an_explicit_backend_can_ask_for_autostart(monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=True)

    monkeypatch.setattr(registry, "build_backend", build)
    pick_backend(_Cfg(_Floor()), name="coppice", autostart=True)
    assert seen == [True]


def test_backend_statuses_never_autostart(monkeypatch):
    from opendaisugi.floor import registry

    seen: list[bool] = []

    def build(name, config, *, autostart=False):
        seen.append(autostart)
        return FakeBackend(available=False)

    monkeypatch.setattr(registry, "build_backend", build)
    backend_statuses(_Cfg(_Floor()))
    assert seen == [False, False, False]


def test_prompt_pane_prompts_a_headless_pane_on_a_backend_that_can():
    b = PromptingBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello", kind="headless") == "prompted"
    assert b.prompted == ["hello"] and b.sent == []


def test_prompt_pane_types_into_a_pty_pane_even_when_the_backend_can_prompt():
    """coppice's own server has no adapter for a pty pane: it falls back to
    typing the text in, so the word this returns must say typed, never
    prompted, whatever the backend happens to support."""
    b = PromptingBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello", kind="pty") == "typed"
    assert b.sent == ["hello"] and b.prompted == []


def test_prompt_pane_types_when_the_backend_has_no_prompt_at_all():
    b = FakeBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello", kind="headless") == "typed"
    assert b.sent == ["hello"]


def test_prompt_pane_looks_up_the_kind_itself_when_the_caller_omits_it():
    """FakeBackend.list() reports p1 as a pty pane, so an omitted kind must
    read that and type, never call the backend's own prompt."""
    b = PromptingBackend()
    assert prompt_pane(b, PaneRef("fake", "p1"), "hello") == "typed"
    assert b.sent == ["hello"] and b.prompted == []


def test_prompt_pane_defaults_to_typed_for_a_pane_not_in_the_list():
    b = PromptingBackend()
    assert prompt_pane(b, PaneRef("fake", "ghost"), "hello") == "typed"
    assert b.sent == ["hello"] and b.prompted == []


def test_prompt_pane_waits_through_prompt_for_a_pty_pane_on_a_backend_that_can():
    """Before kind-based dispatch existed, a pty pane on coppice went
    through backend.prompt(..., wait=wait, ...), and the server honoured
    wait for a pty pane too. Typing through send_text dropped both wait
    and timeout_s entirely: --wait became a no-op the moment the word
    changed to typed. The mechanism that waits is prompt itself, the one
    server-side call that already knows how to watch a pane settle; only
    the reported word changes, to typed, for a pty pane."""
    b = PromptingBackend()
    ref = PaneRef("fake", "p1")

    result = prompt_pane(b, ref, "hello", kind="pty", wait=True, timeout_s=12.0)

    assert result == "typed"
    assert b.prompt_calls == [(ref, "hello", True, 12.0)]
    assert b.sent == []


def test_prompt_pane_does_not_call_prompt_when_the_caller_did_not_ask_to_wait():
    b = PromptingBackend()

    result = prompt_pane(b, PaneRef("fake", "p1"), "hello", kind="pty")

    assert result == "typed"
    assert b.prompt_calls == []
    assert b.sent == ["hello"]


def test_prompt_pane_waiting_on_a_backend_with_no_prompt_still_types_at_once():
    """tmux never has a prompt method at all. Watching a floor-side state
    a plain pty pane may never report, through wait_for_state, would turn
    a dropped wait into a full-timeout stall instead of the no-op this
    closes: a worse failure, not a fix. A backend with nothing to wait
    through must still return at once."""
    b = FakeBackend()

    started = time.monotonic()
    result = prompt_pane(b, PaneRef("fake", "p1"), "hello", kind="pty", wait=True, timeout_s=30.0)
    elapsed = time.monotonic() - started

    assert result == "typed"
    assert b.sent == ["hello"]
    assert elapsed < 1.0, "prompt_pane must not poll toward a 30s timeout with no prompt method"


def test_prompt_pane_self_lookup_degrades_to_typed_when_list_raises():
    """Master §3.2: only spawn, send_text and send_keys may raise. The
    self-lookup's own backend.list() call used to go unguarded, so a
    live-but-erroring backend answered CoppiceError straight out of
    prompt_pane, where the old code, before kind-based dispatch, went
    straight to backend.prompt and never called list() at all."""
    from opendaisugi.exceptions import OpenDaisugiError

    class RaisingListBackend:
        def __init__(self):
            self.sent: list[str] = []

        def list(self):
            raise OpenDaisugiError("internal: server said no")

        def send_text(self, pane, text, *, enter=True):
            self.sent.append(text)

    b = RaisingListBackend()
    result = prompt_pane(b, PaneRef("fake", "p1"), "hello")
    assert result == "typed"
    assert b.sent == ["hello"]


def test_wait_for_state_returns_the_event_when_it_arrives():
    b = FakeBackend(state="blocked")
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="blocked", timeout_s=1.0, poll_s=0.01)
    assert ev is not None and ev.state == "blocked"


def test_wait_for_state_returns_none_on_timeout_never_a_fake_event():
    b = FakeBackend(state="working")
    started = time.time()
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="blocked", timeout_s=0.2, poll_s=0.01)
    assert ev is None and time.time() - started < 1.0


def test_wait_for_a_pane_seen_then_gone_resolves_done():
    """A pane must actually be seen at least once before its absence reads
    as done: a poll that saw the pane, followed by one that does not, is
    what proves the process exited."""

    class SeenThenGone(FakeBackend):
        def __init__(self):
            super().__init__()
            self.calls = 0

        def list(self):
            self.calls += 1
            return super().list() if self.calls == 1 else []

    ev = wait_for_state(
        SeenThenGone(), PaneRef("fake", "p1"), until="done", timeout_s=1.0, poll_s=0.01
    )
    assert ev is not None and ev.state == "done" and ev.source == "process"


def test_wait_for_a_pane_never_seen_times_out_instead_of_fabricating_done():
    """A pane absent from list() on every poll, having never been seen at
    all, must not read as done: it is exactly as consistent with a slow
    or unreachable host as with a pane that never existed."""

    class NeverSeen(FakeBackend):
        def list(self):
            return []

    ev = wait_for_state(
        NeverSeen(), PaneRef("fake", "p1"), until="done", timeout_s=0.2, poll_s=0.01
    )
    assert ev is None


def test_wait_for_state_never_fabricates_done_when_list_proven_says_unproven():
    """A backend that can tell a failed call apart from a proven empty
    roster must have that fact honoured: an unreachable host after the
    pane was seen alive must time out, not report done.

    list() itself is left always answering empty here, on purpose: a
    caller that fell back to it instead of list_proven would fabricate
    done on the very first poll. Only reading list_proven correctly
    times out with None instead.
    """

    class DiesAfterOneProvenPoll(FakeBackend):
        def __init__(self):
            super().__init__()
            self.calls = 0

        def list(self):
            return []

        def list_proven(self):
            self.calls += 1
            if self.calls == 1:
                return True, super().list()
            return False, []

    ev = wait_for_state(
        DiesAfterOneProvenPoll(),
        PaneRef("fake", "p1"),
        until="done",
        timeout_s=0.3,
        poll_s=0.01,
    )
    assert ev is None


def test_wait_for_state_resolves_done_through_list_proven_when_the_pane_leaves():
    class SeenThenProvenGone(FakeBackend):
        def __init__(self):
            super().__init__()
            self.calls = 0

        def list_proven(self):
            self.calls += 1
            return (True, super().list()) if self.calls == 1 else (True, [])

    ev = wait_for_state(
        SeenThenProvenGone(), PaneRef("fake", "p1"), until="done", timeout_s=1.0, poll_s=0.01
    )
    assert ev is not None and ev.state == "done" and ev.source == "process"


def test_wait_for_a_pane_already_ended_before_the_first_poll_resolves_done():
    """A pane gone from list() from the very first poll - never seen
    alive during this wait at all - must still resolve done at once when
    the backend can positively confirm it ended, through the optional
    find_ended method. Without this, only a pane seen alive first and
    then gone can ever resolve this way (see
    test_wait_for_a_pane_never_seen_times_out_instead_of_fabricating_done),
    which times out a wait started after, or racing, the pane's own end."""

    class AlreadyEnded(FakeBackend):
        def list(self):
            return []

        def find_ended(self, ref):
            return PaneStateEvent(
                session_id="s",
                harness="shell",
                state="done",
                source="process",
                ts=time.time(),
                pane=ref.id,
                detail="exit=3",
            )

    started = time.time()
    ev = wait_for_state(
        AlreadyEnded(), PaneRef("fake", "p1"), until="done", timeout_s=1.0, poll_s=0.01
    )
    elapsed = time.time() - started

    assert ev is not None and ev.state == "done" and ev.detail == "exit=3"
    assert elapsed < 0.5, "find_ended must resolve at once, not poll toward the timeout"


def test_wait_for_a_pane_already_ended_is_still_honest_about_an_unmatched_state():
    """find_ended reporting done must not satisfy a wait for a state the
    pane will never reach again - working, say. It is gone; nothing
    later changes that."""

    class AlreadyEndedButWrongTarget(FakeBackend):
        def list(self):
            return []

        def find_ended(self, ref):
            return PaneStateEvent(
                session_id="s",
                harness="shell",
                state="done",
                source="process",
                ts=time.time(),
                pane=ref.id,
            )

    ev = wait_for_state(
        AlreadyEndedButWrongTarget(),
        PaneRef("fake", "p1"),
        until="working",
        timeout_s=0.2,
        poll_s=0.01,
    )
    assert ev is None


def test_wait_for_state_reads_an_expired_blocked_ask_as_working():
    """A stale gate hold must not satisfy `until="blocked"` forever.

    merge() only clears a gate blocked hold when a fresh event arrives. A
    poller has no fresh event, only the last stored one, so it must apply
    the same expiry effective_state applies at read time.
    """

    class ExpiredBlock(FakeBackend):
        def list(self):
            ev = PaneStateEvent(
                session_id="s",
                harness="shell",
                state="blocked",
                source="gate",
                ts=time.time(),
                pane="p1",
                ask=Ask(id="a1", tool="Bash", summary="run it", deadline=0.0),
            )
            return [
                PaneInfo(
                    ref=PaneRef("fake", "p1"), label="l", cwd="/", cmd=["sh"], kind="pty", state=ev
                )
            ]

    b = ExpiredBlock()
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="blocked", timeout_s=0.2, poll_s=0.01)
    assert ev is None
    ev = wait_for_state(b, PaneRef("fake", "p1"), until="working", timeout_s=1.0, poll_s=0.01)
    assert ev is not None and ev.state == "working" and ev.ask is None


def test_build_backend_coppice_uses_the_configured_socket_and_nested_data_dir(tmp_path):
    """Fix round 1: the socket and data dir plumbing through build_backend."""
    from opendaisugi.config import Config, FloorConfig
    from opendaisugi.floor.registry import build_backend

    sock = tmp_path / "custom.sock"
    cfg = Config(data_dir=tmp_path, floor=FloorConfig(coppice_socket=str(sock)))
    backend = build_backend("coppice", cfg)
    assert backend.sock_path == sock
    assert backend.data_dir == tmp_path / "coppice"


def test_build_backend_coppice_falls_back_to_the_default_socket(tmp_path):
    from opendaisugi.config import Config
    from opendaisugi.floor.coppice_backend import default_socket_path
    from opendaisugi.floor.registry import build_backend

    cfg = Config(data_dir=tmp_path)
    backend = build_backend("coppice", cfg)
    assert backend.sock_path == default_socket_path()
    assert backend.data_dir == tmp_path / "coppice"


def _write_gate_state(data_dir, pane_id: str, ev: PaneStateEvent) -> None:
    import json

    from opendaisugi.session_tree import SessionTree

    tree = SessionTree.open_or_create(
        data_dir / "sessions",
        session_id=ev.session_id,
        harness=ev.harness,
        cwd="/repo",
    )
    tree.append("state", json.loads(ev.to_json()), clock=lambda: ev.ts)


def test_build_backend_tmux_reads_gate_states_from_the_configured_data_dir(tmp_path):
    """The tmux backend the registry builds must be able to see a gate fact
    the session tree already holds, not the always-empty default a bare
    TmuxBackend() falls back to."""
    from opendaisugi.config import Config
    from opendaisugi.floor.registry import build_backend

    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    _write_gate_state(tmp_path, "p1", ev)

    backend = build_backend("tmux", Config(data_dir=tmp_path))
    assert backend._gate_states()["p1"].state == "working"


def test_build_backend_herdr_reads_gate_states_from_the_configured_data_dir(tmp_path):
    from opendaisugi.config import Config
    from opendaisugi.floor.registry import build_backend

    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="working", source="gate", ts=1.0, pane="p1"
    )
    _write_gate_state(tmp_path, "p1", ev)

    backend = build_backend("herdr", Config(data_dir=tmp_path))
    assert backend._gate_states()["p1"].state == "working"


# The coppice config file. The Go side writes it; the Python side reads the
# same tables, so one name means one command on both sides.


def test_coppice_config_path_honours_xdg_config_home(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import coppice_config_path

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    assert coppice_config_path() == tmp_path / "coppice" / "coppice.toml"
    monkeypatch.delenv("XDG_CONFIG_HOME")
    assert coppice_config_path().parts[-3:] == (".config", "coppice", "coppice.toml")
    assert (
        coppice_config_path(env={"XDG_CONFIG_HOME": "/x"}).as_posix() == "/x/coppice/coppice.toml"
    )


def _write_coppice_toml(tmp_path, text: str) -> None:
    d = tmp_path / "coppice"
    d.mkdir(parents=True, exist_ok=True)
    (d / "coppice.toml").write_text(text, encoding="utf-8")


def test_harness_command_reads_coppice_toml(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(
        tmp_path, '[harness.pi-llama]\ncommand = "pi"\nargs = ["--model", "llama-3.3-70b"]\n'
    )
    assert harness_command("pi-llama") == ["pi", "--model", "llama-3.3-70b"]


def test_harness_command_without_args_is_the_command_alone(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.claude]\ncommand = "claude"\nstate = "hooks"\n')
    assert harness_command("claude") == ["claude"]


def test_harness_command_with_no_file_is_none(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    assert harness_command("claude") is None


def test_harness_command_with_no_such_table_is_none(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.claude]\ncommand = "claude"\n')
    assert harness_command("pi") is None


def test_harness_command_with_a_table_but_no_command_is_none(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.claude]\nargs = ["-x"]\n')
    assert harness_command("claude") is None


def test_harness_command_with_a_bad_toml_file_is_none(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, "default = [unclosed\n")
    assert harness_command("claude") is None


def test_harness_command_with_wrong_shapes_is_none(monkeypatch, tmp_path):
    from opendaisugi.floor.registry import harness_command

    monkeypatch.setenv("XDG_CONFIG_HOME", str(tmp_path))
    _write_coppice_toml(tmp_path, '[harness.claude]\ncommand = 3\nargs = "x"\n')
    assert harness_command("claude") is None
    _write_coppice_toml(tmp_path, 'harness = "not a table"\n')
    assert harness_command("claude") is None
