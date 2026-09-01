"""Fixtures for the manifest evaluator tests.

The manifest schema is fixed (it is Herdr's own format, recorded in
opendaisugi.floor.manifest_schema), so a test manifest is written with the
real key names directly, not through a schema-driven writer.
"""

from __future__ import annotations

from pathlib import Path

import pytest


def write_manifest(directory: Path, agent: str, rules_toml: str) -> Path:
    """Write one manifest file with the given agent id and rules.

    ``rules_toml`` is the already-formatted ``[[rules]]`` block text a test
    wants, so a rule that needs a gate or a non-default region can be
    written directly in the real schema instead of through a helper that
    cannot express one.
    """
    path = directory / f"{agent}.toml"
    path.write_text(f'id = "{agent}"\n\n{rules_toml}', encoding="utf-8")
    return path


@pytest.fixture
def manifest_dir(tmp_path: Path) -> Path:
    d = tmp_path / "manifests"
    d.mkdir()
    return d


@pytest.fixture
def fake_floor_backend(monkeypatch):
    """A backend `pick_backend` always returns, recording every call.

    `PaneBackend` is a Protocol with no runtime-checkable marker here, so
    this asserts conformance the plain way instead: every method
    `PaneBackend` declares must exist on the fake, found by introspecting
    its own class body. Without this, the fake can drift from the real
    contract, as it once did, lacking attach, detach, last_error and
    close_connection entirely, and every CLI test built on it would prove
    nothing about a real backend.
    """
    import time as _time

    from opendaisugi.floor import PaneInfo, PaneRef, PaneStateEvent, TaskInfo, registry
    from opendaisugi.floor.backend import PaneBackend

    class Fake:
        name = "fake"
        yields_frames = False

        def __init__(self):
            self.spawned: list[dict] = []
            self.sent: list[tuple[str, str]] = []
            self.keys: list[tuple[str, list[str]]] = []
            self.closed: list[str] = []
            self.reads: list[tuple[str, str]] = []
            self.tasks_created: list[dict] = []
            self.tasks_closed: list[tuple[str, bool]] = []

        def available(self):
            return True

        def spawn(self, *, cwd, cmd, env, label, kind, harness=None, task=None):
            self.spawned.append(
                {
                    "cwd": None if cwd is None else str(cwd),
                    "cmd": cmd,
                    "label": label,
                    "kind": kind,
                    "harness": harness,
                    "task": task,
                }
            )
            return PaneRef("fake", "p1")

        def list_tasks(self):
            return [
                TaskInfo(
                    ref="t1",
                    label="review-team",
                    parent="",
                    cwd="/repo",
                    worktree="",
                    model="",
                    state="blocked",
                    panes=("p1",),
                )
            ]

        def create_task(self, label, *, parent=None, cwd=None, worktree=False, model=None):
            self.tasks_created.append(
                {"label": label, "parent": parent, "cwd": cwd, "worktree": worktree, "model": model}
            )
            return TaskInfo(
                ref="t2",
                label=label,
                parent=parent or "",
                cwd=cwd or "",
                worktree="",
                model=model or "",
                state="",
                panes=(),
            )

        def close_task(self, task, *, keep_worktree=False):
            self.tasks_closed.append((task, keep_worktree))

        def list(self):
            ev = PaneStateEvent(
                session_id="s1",
                harness="claude-code",
                state="working",
                source="gate",
                ts=_time.time(),
                pane="p1",
            )
            return [
                PaneInfo(
                    ref=PaneRef("fake", "p1"),
                    label="fix",
                    cwd="/repo",
                    cmd=["claude"],
                    kind="pty",
                    state=ev,
                )
            ]

        def send_text(self, pane, text, *, enter=True):
            self.sent.append((pane.id, text))

        def send_keys(self, pane, keys):
            self.keys.append((pane.id, list(keys)))

        def read(self, pane, *, source="visible"):
            self.reads.append((pane.id, source))
            return f"[{source}]\n"

        def resize(self, pane, cols, rows):
            return None

        def close(self, pane):
            self.closed.append(pane.id)

        def report_state(self, pane, ev):
            return None

        def subscribe(self):
            return iter(())

        def attach(self, pane, cols, rows):
            return "req1"

        def detach(self, pane):
            return "req1"

        def last_error(self, request_id):
            return None

        def close_connection(self):
            return None

        def list_proven(self):
            return True, self.list()

    fake = Fake()
    protocol_methods = [
        name
        for name, value in vars(PaneBackend).items()
        if callable(value) and not name.startswith("_")
    ]
    for name in protocol_methods:
        assert hasattr(fake, name), f"fake_floor_backend is missing PaneBackend member {name!r}"
    monkeypatch.setattr(
        registry, "pick_backend", lambda config, *, name=None, autostart=False: fake
    )
    return fake
