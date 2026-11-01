"""The one-time move after the rename.

The old names below are read on purpose (names-ok): the owner's own state
still carries them, and `daisugi install` moves it once.
"""

from __future__ import annotations

import io
import json
import socket
import sys
from datetime import datetime, timezone

import pytest
from typer.testing import CliRunner

from opendaisugi import install
from opendaisugi.cli import app

NOW = datetime(2026, 10, 30, 12, 0, 0, tzinfo=timezone.utc)
OLD_HOOK = (
    "DAISUGI_GATE_HOOK=opendaisugi.gate python -I -m opendaisugi.gate_client check "  # names-ok
    "--mode enforce --root /x/gate"  # names-ok
)


def test_moves_the_old_dirs(tmp_path, capsys):
    (tmp_path / "gate" / "envelopes").mkdir(parents=True)  # names-ok
    (tmp_path / "gateway").mkdir()  # names-ok
    lines = install.move_old_state(tmp_path, home=tmp_path / "h", now=NOW, out=sys.stdout)
    assert (tmp_path / "warden" / "envelopes").is_dir()
    assert not (tmp_path / "gate").exists()  # names-ok
    assert (tmp_path / "router").is_dir()
    assert lines == [
        f"moved {tmp_path}/gate to {tmp_path}/warden",  # names-ok
        f"moved {tmp_path}/gateway to {tmp_path}/router",  # names-ok
    ]
    assert capsys.readouterr().out.splitlines() == lines


def test_router_files_join_an_existing_router_dir(tmp_path):
    (tmp_path / "gateway").mkdir()  # names-ok
    (tmp_path / "gateway" / "turns.jsonl").write_text("{}\n")  # names-ok
    (tmp_path / "router").mkdir()
    (tmp_path / "router" / "delegations.jsonl").write_text("{}\n")
    lines = install.move_old_state(tmp_path, home=tmp_path / "h", now=NOW, out=io.StringIO())
    assert lines == [f"moved {tmp_path}/gateway into {tmp_path}/router"]  # names-ok
    assert (tmp_path / "router" / "turns.jsonl").exists()
    assert (tmp_path / "router" / "delegations.jsonl").exists()
    assert not (tmp_path / "gateway").exists()  # names-ok


def test_router_file_that_clashes_stops_the_move(tmp_path):
    (tmp_path / "gateway").mkdir()  # names-ok
    (tmp_path / "gateway" / "turns.jsonl").write_text("a")  # names-ok
    (tmp_path / "router").mkdir()
    (tmp_path / "router" / "turns.jsonl").write_text("b")
    with pytest.raises(SystemExit) as e:
        install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert e.value.code == 2
    assert (tmp_path / "gateway" / "turns.jsonl").read_text() == "a"  # names-ok


def test_refuses_when_both_exist(tmp_path):
    (tmp_path / "gate").mkdir()  # names-ok
    (tmp_path / "warden").mkdir()
    with pytest.raises(SystemExit) as e:
        install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert e.value.code == 2
    assert (tmp_path / "gate").exists()  # names-ok


def test_install_refuses_move_while_socket_live(tmp_path):
    (tmp_path / "gate").mkdir()  # names-ok
    sock = tmp_path / "gate" / "gate.sock"  # names-ok
    srv = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    srv.bind(str(sock))
    srv.listen(1)
    try:
        out = io.StringIO()
        with pytest.raises(SystemExit) as e:
            install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=out)
        assert e.value.code == 2
        assert "still listens" in out.getvalue()
        assert (tmp_path / "gate").exists()  # names-ok
        assert not (tmp_path / "warden").exists()
    finally:
        srv.close()


def test_rewrites_old_config_keys(tmp_path):
    cfg = tmp_path / "config.yaml"
    cfg.write_text("gate_mode: enforce\ngateway_router: knn\ngateway_router_model: m\n")  # names-ok
    lines = install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert cfg.read_text() == "warden_mode: enforce\nrouting_mode: knn\nrouting_model: m\n"
    assert "config: gate_mode is now warden_mode" in lines  # names-ok
    assert "config: gateway_router is now routing_mode" in lines  # names-ok


def _claude_settings(tmp_path):
    f = tmp_path / ".claude" / "settings.json"
    f.parent.mkdir(parents=True)
    f.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {"matcher": "*", "hooks": [{"type": "command", "command": OLD_HOOK}]}
                    ]
                }
            }
        )
    )
    return f


def test_install_replaces_old_marker_hook(tmp_path):
    f = _claude_settings(tmp_path)
    lines = install.move_old_state(tmp_path / "d", home=tmp_path, now=NOW, out=io.StringIO())
    pre = json.loads(f.read_text())["hooks"]["PreToolUse"]
    assert len(pre) == 1
    cmd = pre[0]["hooks"][0]["command"]
    assert "DAISUGI_WARDEN_HOOK=opendaisugi.warden" in cmd
    assert "--mode enforce" in cmd  # an enforce hook stays an enforce hook
    assert "DAISUGI_GATE" not in cmd  # names-ok
    assert lines == [f"replaced the old warden hook in {f}"]


def test_a_foreign_hook_is_left_alone(tmp_path):
    f = tmp_path / ".claude" / "settings.json"
    f.parent.mkdir(parents=True)
    other = "other-gate check"  # names-ok: not ours
    body = {"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": other}]}]}}
    f.write_text(json.dumps(body))
    assert install.move_old_state(tmp_path / "d", home=tmp_path, now=NOW, out=io.StringIO()) == []
    assert json.loads(f.read_text()) == body


def test_install_moves_old_extensions_aside(tmp_path):
    ext = tmp_path / ".pi" / "agent" / "extensions" / "daisugi-gate"  # names-ok
    ext.mkdir(parents=True)
    (ext / "index.ts").write_text("// old")
    data = tmp_path / "d"
    lines = install.move_old_state(data, home=tmp_path, now=NOW, out=io.StringIO())
    moved = data / "moved-aside" / "20261030T120000Z" / "pi-daisugi-gate" / "index.ts"  # names-ok
    assert moved.read_text() == "// old"
    assert not ext.exists()
    assert lines == [f"moved aside {ext} to {moved.parent}"]


def test_dry_run_changes_nothing(tmp_path):
    (tmp_path / "gate").mkdir()  # names-ok
    lines = install.move_old_state(
        tmp_path, home=tmp_path, now=NOW, out=io.StringIO(), dry_run=True
    )
    assert lines == [f"would move {tmp_path}/gate to {tmp_path}/warden"]  # names-ok
    assert (tmp_path / "gate").exists()  # names-ok


def test_warden_status_names_stale_state(tmp_path, monkeypatch):
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("OPENDAISUGI_HOME", str(tmp_path / "d"))
    (tmp_path / "d" / "gate").mkdir(parents=True)  # names-ok
    (tmp_path / "d" / "config.yaml").write_text("gate_mode: enforce\n")  # names-ok
    r = CliRunner().invoke(app, ["warden", "status", "--root", str(tmp_path / "d" / "warden")])
    assert "from before the rename: run daisugi install to move it" in r.output
    assert "config: unknown key gate_mode (now warden_mode)" in r.output  # names-ok


def test_install_names_stale_delegate_allow(tmp_path):
    env = tmp_path / "warden" / "envelopes"
    env.mkdir(parents=True)
    (env / "e1.json").write_text(
        json.dumps({"permissions": {"mcp_allowlist": ["opendaisugi/delegate"]}})  # names-ok
    )
    (env / "e2.json").write_text(
        json.dumps(
            {"permissions": {"mcp_allowlist": ["opendaisugi/delegate", "opendaisugi/spawn"]}}
        )  # names-ok
    )
    lines = install.move_old_state(tmp_path, home=tmp_path, now=NOW, out=io.StringIO())
    assert lines == [
        "envelope e1 allows mcp opendaisugi/delegate; "  # names-ok
        "add opendaisugi/spawn (the tool was renamed)"
    ]
    assert json.loads((env / "e1.json").read_text())["permissions"]["mcp_allowlist"] == [
        "opendaisugi/delegate"  # names-ok
    ]


def _e2e_home(tmp_path, monkeypatch, root_text):
    """A fake HOME holding the hook master wrote and a populated old dir."""
    monkeypatch.setenv("HOME", str(tmp_path))
    monkeypatch.setenv("OPENDAISUGI_HOME", str(tmp_path / "d"))
    old = tmp_path / "d" / "gate"  # names-ok: the old dir
    (old / "envelopes").mkdir(parents=True)
    (old / "envelopes" / "e1.json").write_text("{}")
    cmd = (
        "DAISUGI_GATE_HOOK=opendaisugi.gate python -I -m opendaisugi.gate_client "  # names-ok
        f"--mode enforce --root {root_text.format(d=tmp_path / 'd')} "
        "--format claude --verify-timeout 10.0 || exit 2"
    )
    f = tmp_path / ".claude" / "settings.json"
    f.parent.mkdir(parents=True)
    body = {
        "hooks": {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": cmd}]}]}
    }
    f.write_text(json.dumps(body))
    return f


@pytest.mark.parametrize("root_text", ["{d}/gate", "'{d}/gate'"])  # names-ok
def test_install_end_to_end_points_the_old_hook_at_the_new_dir(tmp_path, monkeypatch, root_text):
    f = _e2e_home(tmp_path, monkeypatch, root_text)
    d = tmp_path / "d"
    r = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude"])
    assert r.exit_code == 0, r.output
    pre = json.loads(f.read_text())["hooks"]["PreToolUse"]
    cmds = [h["command"] for e in pre for h in e["hooks"] if "hook record" not in h["command"]]
    assert len(cmds) == 1, cmds
    assert f"--root {d}/warden " in cmds[0].replace("'", "")
    assert "--mode enforce" in cmds[0]
    assert "different mode/config" not in r.output
    assert (d / "warden" / "envelopes" / "e1.json").is_file()
    assert not (d / "gate").exists()  # names-ok
    r2 = CliRunner().invoke(app, ["install", "--yes", "--runtime", "claude"])
    assert r2.exit_code == 0, r2.output
    assert "both" not in r2.output


def test_the_old_gate_verb_says_why_it_denies():  # names-ok
    r = CliRunner().invoke(app, ["gate", "check", "--mode", "enforce", "--root", "/x"])  # names-ok
    line = "daisugi gate is now daisugi warden; run daisugi install to update the hook"  # names-ok
    assert r.exit_code == 2
    assert line in r.output
    assert "gate" not in [c.name for c in app.registered_commands if not c.hidden]  # names-ok
