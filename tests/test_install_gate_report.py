"""`daisugi install --gate --report herdr|coppice` (spec-01 §Interfaces).

herdr wires Claude Code's Stop + Notification hooks so the floor gets exact
idle/blocked signal between tool calls, not just around them. coppice is a
config-only placeholder — harness/coppice doesn't exist yet (spec-02).
"""

from __future__ import annotations

import json

from opendaisugi.config import load_config
from opendaisugi.install import (
    DEFAULT_LAYERS,
    ClaudeCodeRuntime,
    CodexRuntime,
    Layer,
    install,
    uninstall,
)

GATE_LAYERS = DEFAULT_LAYERS | {Layer.GATE}


def _settings(home):
    return json.loads((home / ".claude" / "settings.json").read_text())


def _commands(settings: dict, event: str) -> list[str]:
    entries = settings.get("hooks", {}).get(event, [])
    return [h["command"] for e in entries for h in e.get("hooks", [])]


def test_report_none_by_default_writes_no_stop_or_notification_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS)
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_report_herdr_adds_stop_and_notification_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS, report="herdr"
    )
    settings = _settings(tmp_path)
    stop_cmds = _commands(settings, "Stop")
    notif_cmds = _commands(settings, "Notification")
    assert len(stop_cmds) == 1 and "--event stop" in stop_cmds[0]
    assert len(notif_cmds) == 1 and "--event notification" in notif_cmds[0]


def test_report_herdr_is_idempotent(tmp_path):
    (tmp_path / ".claude").mkdir()
    for _ in range(2):
        install(
            home=tmp_path,
            yes=True,
            runtimes=[ClaudeCodeRuntime()],
            layers=GATE_LAYERS,
            report="herdr",
        )
    settings = _settings(tmp_path)
    assert len(_commands(settings, "Stop")) == 1
    assert len(_commands(settings, "Notification")) == 1


def test_report_herdr_without_gate_installs_nothing(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=DEFAULT_LAYERS,
        report="herdr",
    )
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []


def test_report_coppice_writes_no_stop_or_notification_hooks(tmp_path):
    """coppice is config-only (cli.py's job, task 5), not a Runtime file
    change — harness/coppice doesn't exist yet (spec-02)."""
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=GATE_LAYERS,
        report="coppice",
    )
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []


def test_uninstall_removes_the_report_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path, yes=True, runtimes=[ClaudeCodeRuntime()], layers=GATE_LAYERS, report="herdr"
    )
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []
    assert _commands(settings, "SubagentStart") == []
    assert _commands(settings, "SubagentStop") == []


def test_report_hooks_include_the_subagent_hooks(tmp_path):
    (tmp_path / ".claude").mkdir()
    for _ in range(2):
        install(
            home=tmp_path,
            yes=True,
            runtimes=[ClaudeCodeRuntime()],
            layers=GATE_LAYERS,
            report="herdr",
        )
    settings = _settings(tmp_path)
    start = _commands(settings, "SubagentStart")
    stop = _commands(settings, "SubagentStop")
    assert len(start) == 1 and "--event subagent_start" in start[0]
    assert len(stop) == 1 and "--event subagent_stop" in stop[0]


def test_codex_silently_ignores_report(tmp_path):
    """Codex hooks.json has no Stop/Notification equivalent wired here —
    report is Claude Code only, same precedent as `ask`."""
    (tmp_path / ".codex").mkdir()
    steps = CodexRuntime().plan(tmp_path, GATE_LAYERS, report="herdr")
    assert all("report" not in s.description.lower() for s in steps)


def test_floor_report_config_field_defaults_to_none(tmp_path):
    assert load_config(tmp_path / "config.yaml").floor_report is None


def test_report_herdr_tolerates_a_malformed_stop_hook_entry(tmp_path):
    """Fix round 1, Minor finding: a Stop hook entry lacking 'command' (some
    other tool's malformed or partial write) must not raise — it must be
    skipped, and our own hook still gets added. Before the fix,
    `_patch_claude_report_hooks` indexed ``h["command"]`` directly, which
    raised KeyError; the runtime's own `except Exception` in `install()`
    then swallowed it and skipped every remaining layer for that runtime."""
    (tmp_path / ".claude").mkdir()
    settings_path = tmp_path / ".claude" / "settings.json"
    settings_path.write_text(json.dumps({"hooks": {"Stop": [{"hooks": [{"type": "command"}]}]}}))
    result = install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=GATE_LAYERS,
        report="herdr",
    )
    assert "Failures" not in result.summary
    settings = _settings(tmp_path)
    stop_entries = settings.get("hooks", {}).get("Stop", [])
    stop_cmds = [h.get("command", "") for e in stop_entries for h in e.get("hooks", [])]
    assert any("--event stop" in c for c in stop_cmds)
    assert len(_commands(settings, "Notification")) == 1


def test_report_coppice_adds_only_the_subagent_hooks(tmp_path):
    """coppice shows subagents as child rows, so --report coppice wires the
    two subagent hooks and still no Stop or Notification hook."""
    (tmp_path / ".claude").mkdir()
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=GATE_LAYERS,
        report="coppice",
    )
    settings = _settings(tmp_path)
    assert _commands(settings, "Stop") == []
    assert _commands(settings, "Notification") == []
    assert len(_commands(settings, "SubagentStart")) == 1
    assert len(_commands(settings, "SubagentStop")) == 1
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    settings = _settings(tmp_path)
    assert _commands(settings, "SubagentStart") == []
    assert _commands(settings, "SubagentStop") == []
