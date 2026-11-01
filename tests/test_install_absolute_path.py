"""Install writes the absolute path of the daisugi entry point.

An agent starts its hooks with its own PATH, which often lacks
~/.local/bin. A bare `daisugi` then fails with "command not found" and the
hook fails open. These tests use a fake HOME and a fake executable.
"""

from __future__ import annotations

import json
import shlex
import sys

import yaml
from typer.testing import CliRunner

from opendaisugi import install as inst
from opendaisugi.cli import app
from opendaisugi.config import is_record_hook
from opendaisugi.install import (
    ClaudeCodeRuntime,
    CodexRuntime,
    HermesRuntime,
    OpenClawRuntime,
    uninstall,
)

BARE_PRE = "daisugi hook record --format claude"
BARE_STOP = "daisugi hook record --format claude --event stop"


def _claude_commands(home):
    cfg = json.loads((home / ".claude" / "settings.json").read_text())
    return [
        h["command"]
        for ev in cfg["hooks"].values()
        for e in ev
        for h in e.get("hooks", [])
        if is_record_hook(h["command"])
    ]


def test_exe_is_argv0_when_it_is_a_daisugi_entry_point(fake_daisugi_exe):
    assert inst.daisugi_exe() == fake_daisugi_exe


def test_exe_falls_back_to_path_then_bare_name(tmp_path, monkeypatch):
    monkeypatch.setattr(sys, "argv", ["pytest"])
    bindir = tmp_path / "bin"
    bindir.mkdir()
    exe = bindir / "daisugi"
    exe.write_text("#!/bin/sh\n")
    exe.chmod(0o755)
    monkeypatch.setenv("PATH", str(bindir))
    assert inst.daisugi_exe() == str(exe)
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    assert inst.daisugi_exe() == "daisugi"


def test_claude_hooks_and_mcp_use_the_absolute_path(tmp_path, fake_daisugi_exe):
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path, report="herdr")
    cmds = _claude_commands(tmp_path)
    assert len(cmds) == 5  # PreToolUse, Stop, Notification, SubagentStart, SubagentStop
    assert all(shlex.split(c)[0] == fake_daisugi_exe for c in cmds)
    mcp = json.loads((tmp_path / ".claude.json").read_text())["mcpServers"]["opendaisugi"]
    assert mcp["command"] == fake_daisugi_exe and mcp["args"] == ["mcp", "serve"]


def test_a_path_with_a_space_is_quoted(tmp_path, monkeypatch):
    exe = "/opt/my tools/daisugi"
    monkeypatch.setattr(inst, "daisugi_exe", lambda: exe)
    (tmp_path / ".claude").mkdir()
    ClaudeCodeRuntime().apply(tmp_path)
    (cmd,) = _claude_commands(tmp_path)
    assert shlex.split(cmd)[:4] == [exe, "hook", "record", "--format"]
    assert is_record_hook(cmd)


def test_rerun_replaces_an_old_bare_entry_in_place(tmp_path, fake_daisugi_exe):
    claude = tmp_path / ".claude"
    claude.mkdir()
    old = {
        "hooks": {
            "PreToolUse": [
                {"matcher": "Bash", "hooks": [{"type": "command", "command": BARE_PRE}]}
            ],
            "Stop": [{"hooks": [{"type": "command", "command": BARE_STOP}]}],
        }
    }
    (claude / "settings.json").write_text(json.dumps(old))
    bare_mcp = {"type": "stdio", "command": "daisugi", "args": ["mcp", "serve"]}
    (tmp_path / ".claude.json").write_text(json.dumps({"mcpServers": {"opendaisugi": bare_mcp}}))
    ClaudeCodeRuntime().apply(tmp_path, report="herdr")
    cmds = _claude_commands(tmp_path)
    assert sorted(shlex.split(c)[0] for c in cmds) == [fake_daisugi_exe] * 5
    pre = json.loads((claude / "settings.json").read_text())["hooks"]["PreToolUse"]
    assert sum(is_record_hook(h["command"]) for e in pre for h in e["hooks"]) == 1
    mcp = json.loads((tmp_path / ".claude.json").read_text())["mcpServers"]
    assert mcp["opendaisugi"]["command"] == fake_daisugi_exe
    assert mcp["opendaisugi"]["type"] == "stdio"
    # a second run changes nothing
    before = (claude / "settings.json").read_text()
    assert ClaudeCodeRuntime().apply(tmp_path, report="herdr")  # the warden hook, only
    assert (claude / "settings.json").read_text().count("hook record") == before.count(
        "hook record"
    )


def test_uninstall_removes_both_forms(tmp_path):
    claude = tmp_path / ".claude"
    claude.mkdir()
    hooks = [
        {"hooks": [{"type": "command", "command": BARE_PRE}]},
        {"hooks": [{"type": "command", "command": "/x/y/daisugi hook record --format claude"}]},
        {"hooks": [{"type": "command", "command": "/x/y/daisugi-rs hook record --format claude"}]},
    ]
    (claude / "settings.json").write_text(json.dumps({"hooks": {"PreToolUse": hooks}}))
    uninstall(home=tmp_path, runtimes=[ClaudeCodeRuntime()])
    assert "hook record" not in (claude / "settings.json").read_text()


def test_hermes_old_bare_entry_is_replaced_not_added(tmp_path, fake_daisugi_exe):
    h = tmp_path / ".hermes"
    h.mkdir()
    old = {
        "mcp_servers": {"opendaisugi": {"command": "daisugi", "args": ["mcp", "serve"]}},
        "hooks": {
            "pre_tool_call": [
                {"matcher": ".*", "command": "daisugi hook record --format hermes", "timeout": 10}
            ]
        },
    }
    (h / "config.yaml").write_text(yaml.dump(old))
    HermesRuntime().apply(tmp_path)
    cfg = yaml.safe_load((h / "config.yaml").read_text())
    pre = cfg["hooks"]["pre_tool_call"]
    assert len(pre) == 1 and shlex.split(pre[0]["command"])[0] == fake_daisugi_exe
    assert cfg["mcp_servers"]["opendaisugi"]["command"] == fake_daisugi_exe
    uninstall(home=tmp_path, runtimes=[HermesRuntime()])
    assert "opendaisugi" not in (h / "config.yaml").read_text()


def test_codex_old_bare_block_is_replaced_and_uninstalled(tmp_path, fake_daisugi_exe):
    codex = tmp_path / ".codex"
    codex.mkdir()
    (codex / "config.toml").write_text(
        'model = "x"\n\n[mcp_servers.opendaisugi]\ncommand = "daisugi"\nargs = ["mcp", "serve"]\n'
    )
    CodexRuntime().apply(tmp_path)
    text = (codex / "config.toml").read_text()
    assert text.count("[mcp_servers.opendaisugi]") == 1
    assert f'command = "{fake_daisugi_exe}"' in text
    uninstall(home=tmp_path, runtimes=[CodexRuntime()])
    assert "opendaisugi" not in (codex / "config.toml").read_text()


def test_openclaw_old_bare_entry_is_replaced(tmp_path, fake_daisugi_exe):
    oc = tmp_path / ".openclaw"
    (oc / "workspace").mkdir(parents=True)
    bare = {"command": "daisugi", "args": ["mcp", "serve"]}
    (oc / "openclaw.json").write_text(json.dumps({"mcp": {"servers": {"opendaisugi": bare}}}))
    OpenClawRuntime().apply(tmp_path)
    cfg = json.loads((oc / "openclaw.json").read_text())
    assert cfg["mcp"]["servers"]["opendaisugi"]["command"] == fake_daisugi_exe


def test_a_user_edited_mcp_command_is_left_alone(tmp_path):
    oc = tmp_path / ".openclaw"
    (oc / "workspace").mkdir(parents=True)
    own = {"command": "/my/wrapper", "args": ["mcp", "serve"]}
    (oc / "openclaw.json").write_text(json.dumps({"mcp": {"servers": {"opendaisugi": own}}}))
    OpenClawRuntime().apply(tmp_path)
    assert json.loads((oc / "openclaw.json").read_text())["mcp"]["servers"]["opendaisugi"] == own


# -- the check after writing ----------------------------------------------


def test_check_is_quiet_when_nothing_was_written(tmp_path):
    assert inst.unrunnable_commands([ClaudeCodeRuntime(), CodexRuntime()], home=tmp_path) == []


def test_cli_install_exits_nonzero_when_the_hook_cannot_run(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(inst, "daisugi_exe", lambda: str(tmp_path / "gone" / "daisugi"))
    r = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude"])
    assert r.exit_code == 1
    assert "[Claude Code] The command" in r.output


def test_cli_install_tells_the_user_to_restart_and_how_to_check(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    (tmp_path / ".claude").mkdir()
    r = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude"])
    assert r.exit_code == 0, r.output
    assert inst.RESTART_LINE in r.output
    assert "daisugi hook list" in inst.RESTART_LINE
