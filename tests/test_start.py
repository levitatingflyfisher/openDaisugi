"""`daisugi start`: five reported steps, idempotent, nothing hidden, nothing spent.

B-1: the gate hook is scoped to `cwd`, never `~/.claude/settings.json` — a
machine-global hook whose only envelope belonged to one project would deny
every other project's sessions. B-4: the gate-server step only says "done"
once `<root>/<SOCK_NAME>` actually appears.
"""

from __future__ import annotations

import json
from pathlib import Path

from opendaisugi.gate_server import SOCK_NAME
from opendaisugi.start import StartOptions, plan_start, run_start


def _opts(tmp_path: Path, **kw) -> StartOptions:
    proj = tmp_path / "proj"
    proj.mkdir(exist_ok=True)
    kw.setdefault("which", lambda n: "/usr/bin/claude")
    kw.setdefault("spawn", lambda argv: None)
    kw.setdefault("server_wait_s", 0.05)
    # A fake home, never the real one — start.py only reads it (to check for
    # a pre-existing machine-global hook), but a real box's ~/.claude may
    # hold one, and these tests must not depend on that.
    kw.setdefault("home", tmp_path / "home")
    return StartOptions(cwd=proj, data_dir=tmp_path / "data", **kw)


def _touch_sock(root: Path):
    def spawn(argv: list[str]) -> None:
        root.mkdir(parents=True, exist_ok=True)
        (root / SOCK_NAME).touch()

    return spawn


def test_plan_on_a_fresh_project_would_do_everything(tmp_path):
    steps = {s.key: s for s in plan_start(_opts(tmp_path))}
    assert set(steps) == {"harness", "hook", "envelope", "gate-server", "view"}
    assert steps["harness"].state == "done" and "claude" in steps["harness"].text
    assert steps["hook"].state == "would" and "shadow" in steps["hook"].text
    assert steps["envelope"].state == "would"
    assert steps["gate-server"].state == "would"
    assert steps["view"].state == "would"


def test_plan_never_writes_anything(tmp_path):
    opts = _opts(tmp_path)
    plan_start(opts)
    assert not (opts.cwd / ".claude" / "settings.json").exists()
    assert not (opts.data_dir / "gate").exists()


def test_run_installs_hook_scoped_to_cwd_not_home(tmp_path):
    root = tmp_path / "data" / "gate"
    opts = _opts(tmp_path, spawn=_touch_sock(root))
    steps = {s.key: s for s in run_start(opts)}

    settings_path = opts.cwd / ".claude" / "settings.json"
    settings = json.loads(settings_path.read_text())
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "opendaisugi.gate_client" in cmd
    assert "--mode shadow" in cmd
    assert f"--root {root}" in cmd
    assert steps["hook"].state == "done"
    assert str(opts.cwd) in steps["hook"].text
    # A project-settings hook needs Claude Code's folder-trust before it fires;
    # "done" here only means the file was written, not that the hook is live.
    assert "daisugi gate report" in steps["hook"].text
    assert "trust" in steps["hook"].text.lower()

    # B-1: nothing was ever written under a home directory — this is the whole point.
    home_settings = tmp_path / "home" / ".claude" / "settings.json"
    assert not home_settings.exists()


def test_run_registers_a_cwd_keyed_envelope_not_default(tmp_path):
    root = tmp_path / "data" / "gate"
    opts = _opts(tmp_path, spawn=_touch_sock(root))
    steps = {s.key: s for s in run_start(opts)}

    assert not (root / "envelopes" / "default.json").exists()
    envelopes = list((root / "envelopes").glob("*.json"))
    assert len(envelopes) == 1
    assert steps["envelope"].state == "done" and str(opts.cwd) in steps["envelope"].text

    # The hook's baked-in --session matches the envelope's actual filename —
    # a live session loads exactly the envelope `start` registered for it.
    settings = json.loads((opts.cwd / ".claude" / "settings.json").read_text())
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert envelopes[0].stem in cmd


def test_run_starts_the_gate_server_and_reports_done_once_the_socket_appears(tmp_path):
    root = tmp_path / "data" / "gate"
    spawned = []

    def spawn(argv):
        spawned.append(argv)
        _touch_sock(root)(argv)

    opts = _opts(tmp_path, spawn=spawn)
    steps = {s.key: s for s in run_start(opts)}
    assert steps["gate-server"].state == "done" and spawned and "serve" in " ".join(spawned[0])
    assert (root / SOCK_NAME).exists()
    assert steps["view"].state == "would"


def test_gate_server_step_fails_when_the_socket_never_appears(tmp_path):
    """B-4: a spawn that dies (or never binds) must not be reported "done"."""
    opts = _opts(tmp_path, spawn=lambda argv: None)  # never touches the socket
    steps = {s.key: s for s in run_start(opts)}
    assert steps["gate-server"].state == "failed"
    assert "gate serve" in steps["gate-server"].text


def test_second_run_skips_what_exists(tmp_path):
    root = tmp_path / "data" / "gate"
    opts = _opts(tmp_path, spawn=_touch_sock(root))
    run_start(opts)
    steps = {s.key: s for s in run_start(opts)}
    assert steps["hook"].state == "skipped" and "already installed" in steps["hook"].text
    assert steps["envelope"].state == "skipped"
    assert steps["gate-server"].state == "skipped" and "already running" in steps["gate-server"].text


def test_no_claude_is_a_failed_step_with_a_fix_and_short_circuits(tmp_path):
    opts = _opts(tmp_path, which=lambda n: None)
    steps = {s.key: s for s in run_start(opts)}
    assert set(steps) == {"harness", "hook", "envelope", "gate-server", "view"}
    assert steps["harness"].state == "failed"
    assert "PATH" in steps["harness"].text and "install" in steps["harness"].text.lower()
    assert steps["hook"].state == "skipped"
    assert steps["envelope"].state == "skipped"
    assert steps["gate-server"].state == "skipped"
    # short-circuited: nothing was written anywhere.
    assert not (opts.cwd / ".claude" / "settings.json").exists()
    assert not (opts.data_dir / "gate").exists()


def test_second_run_asking_for_a_different_mode_says_how_to_switch(tmp_path):
    root = tmp_path / "data" / "gate"
    opts = _opts(tmp_path, spawn=_touch_sock(root))
    run_start(opts)  # shadow, the default
    enforce_opts = _opts(tmp_path, spawn=_touch_sock(root), enforce=True)
    steps = {s.key: s for s in run_start(enforce_opts)}
    assert steps["hook"].state == "skipped"
    assert "shadow" in steps["hook"].text and "enforce" in steps["hook"].text
    assert "remove the gate hook line" in steps["hook"].text


def test_enforce_flag_installs_enforce(tmp_path):
    root = tmp_path / "data" / "gate"
    opts = _opts(tmp_path, spawn=_touch_sock(root), enforce=True)
    run_start(opts)
    settings = json.loads((opts.cwd / ".claude" / "settings.json").read_text())
    assert "--mode enforce" in settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]


def test_hook_step_names_a_coexisting_machine_global_hook(tmp_path):
    """`daisugi install --gate` writes a hook to ~/.claude/settings.json with no
    --session, resolving to the shared `default` envelope. `start`'s hook is a
    different file, so it can't see or change that one — but it must not stay
    silent about it: silence here is the same honesty gap B-1 named, from the
    other direction (start's OWN text implying full coverage of this session)."""
    opts = _opts(tmp_path)
    (opts.home / ".claude").mkdir(parents=True)
    (opts.home / ".claude" / "settings.json").write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "*",
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "python -m opendaisugi.gate_client --mode enforce --root x",
                                }
                            ],
                        }
                    ]
                }
            }
        )
    )
    for steps_fn in (plan_start, run_start):
        steps = {s.key: s for s in steps_fn(_opts(tmp_path, home=opts.home))}
        assert "machine-global" in steps["hook"].text
        assert "enforce" in steps["hook"].text
        # The referent must be explicit — "it fires on this session too" (the
        # original wording) leaves "it"/"this" ambiguous between the two hooks.
        assert "fires on every directory's sessions, including this one" in steps["hook"].text
        assert str(opts.home / ".claude" / "settings.json") in steps["hook"].text


def test_hook_step_says_nothing_when_there_is_no_global_hook(tmp_path):
    opts = _opts(tmp_path)
    steps = {s.key: s for s in plan_start(opts)}
    assert "machine-global" not in steps["hook"].text


def test_session_key_is_stable_and_filename_safe(tmp_path):
    from opendaisugi.start import _session_key

    proj = tmp_path / "My Project (v2)"
    proj.mkdir()
    key = _session_key(proj)
    assert key == _session_key(proj)  # deterministic
    assert all(c.isalnum() or c in "._-" for c in key)
