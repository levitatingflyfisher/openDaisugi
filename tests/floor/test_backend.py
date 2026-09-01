"""Structural tests for the PaneBackend value types (master spec §3.2).

No implementation exists yet (spec-02/03) — this only pins the shapes
later backends and the contract suite (tests/floor/test_backend_contract.py,
spec-03) will type against.
"""

from __future__ import annotations

import pytest

from opendaisugi.floor.backend import Frame, PaneBackend, PaneInfo, PaneRef
from opendaisugi.floor.events import PaneStateEvent


def test_pane_ref_is_hashable_and_comparable_by_value():
    a = PaneRef(backend="coppice", id="w1:p1")
    b = PaneRef(backend="coppice", id="w1:p1")
    c = PaneRef(backend="herdr", id="w1:p1")
    assert a == b and hash(a) == hash(b)
    assert a != c


def test_pane_info_carries_an_optional_state():
    ref = PaneRef(backend="coppice", id="w1:p1")
    info = PaneInfo(ref=ref, label="auth fix", cwd="/repo", cmd=["claude"], kind="pty")
    assert info.state is None
    ev = PaneStateEvent(
        session_id="s1", harness="claude-code", state="idle", source="manifest", ts=1.0
    )
    info2 = PaneInfo(ref=ref, label="auth fix", cwd="/repo", cmd=["claude"], kind="pty", state=ev)
    assert info2.state is ev


def test_frame_shape():
    f = Frame(
        pane="w1:p1",
        seq=1,
        cols=120,
        rows=40,
        cursor=(0, 0),
        rows_changed={0: [["hi", "default", "default", []]]},
    )
    assert f.cols == 120 and f.rows_changed[0][0][0] == "hi"


def test_a_fake_backend_satisfies_the_protocol_structurally():
    """PaneBackend is not @runtime_checkable (it carries a non-method `name`
    attribute) — this just proves a plausible implementation can supply
    every named member without a metaclass fight, so spec-02/03/04/05 have
    something real to implement against."""

    class _Fake:
        name = "fake"
        yields_frames = False

        def available(self) -> bool:
            return True

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
            if kind == "headless" and harness is None:
                raise ValueError("headless spawn needs a harness name (master §3.2)")
            return PaneRef(backend="fake", id="1")

        def list(self):
            return []

        def send_text(self, pane, text, *, enter=True):
            return None

        def send_keys(self, pane, keys):
            return None

        def read(self, pane, *, source="visible"):
            return ""

        def resize(self, pane, cols, rows):
            return None

        def close(self, pane):
            return None

        def subscribe(self):
            return iter(())

        def report_state(self, pane, ev):
            return None

    fake: PaneBackend = _Fake()
    assert fake.available() is True
    assert fake.spawn(cwd="/", cmd=["x"], env={}, label="l", kind="pty") == PaneRef("fake", "1")


def test_headless_spawn_needs_a_harness_name():
    """master §3.2 (amended): harness names the adapter (§3.4's table) so
    coppice-server can pick the right headless adapter — required for a
    headless spawn, optional for a PTY one (which infers it later)."""

    class _Fake:
        name = "fake"

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None):
            if kind == "headless" and harness is None:
                raise ValueError("headless spawn needs a harness name")
            return PaneRef(backend="fake", id="1")

    fake = _Fake()
    with pytest.raises(ValueError, match="harness"):
        fake.spawn(cwd="/", cmd=["claude", "-p"], env={}, label="l", kind="headless")
    ref = fake.spawn(
        cwd="/", cmd=["claude", "-p"], env={}, label="l", kind="headless", harness="claude-code"
    )
    assert ref == PaneRef("fake", "1")
