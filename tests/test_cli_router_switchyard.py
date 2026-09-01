"""The router choice on the command line: install, gateway, router status and
stop, and the router row of the module map.

No test runs the real switchyard-server. A fake one from
tests/fixtures/switchyard stands in on loopback where a real child is needed.
"""

from __future__ import annotations

import json
import os
import socket
import sys
import tomllib
from pathlib import Path

import httpx
from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import load_config, save_config
from opendaisugi.gateway_journal import GatewayJournal, GatewayTurnRecord
from opendaisugi.modules import ACTIVE, AVAILABLE, POSSIBLE, detect_stages
from opendaisugi.router_switchyard import INSTALL_CMD

runner = CliRunner()
FAKE_SERVER = Path(__file__).parent / "fixtures" / "switchyard" / "switchyard-server"


def _out(result) -> str:
    combined = result.output
    try:
        combined += result.stderr or ""
    except ValueError:
        pass
    return combined


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _fake_binary_on_path(tmp_path, monkeypatch) -> Path:
    bindir = tmp_path / "bin"
    bindir.mkdir(exist_ok=True)
    wrapper = bindir / "switchyard-server"
    wrapper.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{FAKE_SERVER}" "$@"\n')
    wrapper.chmod(0o755)
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', '')}")
    return wrapper


def _no_binary(monkeypatch):
    monkeypatch.setattr("opendaisugi.router_switchyard.shutil.which", lambda name: None)


def _configure(data_dir: Path, **fields) -> None:
    path = data_dir / "config.yaml"
    save_config(load_config(path).model_copy(update=fields), path)


# --- install --------------------------------------------------------------------


def _install(tmp_path, monkeypatch, *extra):
    (tmp_path / ".claude").mkdir(exist_ok=True)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    return runner.invoke(app, ["install", "--gateway", "--yes", "--runtime", "claude", *extra])


def test_install_router_switchyard_writes_config_and_toml(tmp_path, monkeypatch):
    result = _install(
        tmp_path, monkeypatch, "--router", "switchyard", "--efficient-model", "qwen3-coder:30b"
    )
    assert result.exit_code == 0, _out(result)
    cfg = load_config(tmp_path / ".opendaisugi" / "config.yaml")
    assert cfg.gateway_router == "switchyard"
    assert cfg.switchyard_efficient_model == "qwen3-coder:30b"
    toml_path = tmp_path / ".opendaisugi" / "switchyard.toml"
    assert oct(toml_path.stat().st_mode & 0o777) == "0o600"
    doc = tomllib.loads(toml_path.read_text())
    assert doc["targets"]["efficient"]["id"] == "qwen3-coder:30b"
    assert doc["llm_clients"]["capable"]["forward_auth"] is True
    assert "daisugi gateway" in result.output


def test_install_router_switchyard_without_efficient_model_writes_nothing(tmp_path, monkeypatch):
    result = _install(tmp_path, monkeypatch, "--router", "switchyard")
    assert result.exit_code == 1
    assert "--efficient-model" in _out(result)
    assert not (tmp_path / ".claude" / "settings.json").exists()
    assert not (tmp_path / ".opendaisugi" / "switchyard.toml").exists()


def test_install_router_switchyard_refuses_one_model_for_both_tiers(tmp_path, monkeypatch):
    result = _install(
        tmp_path, monkeypatch, "--router", "switchyard", "--efficient-model", "claude-sonnet-5"
    )
    assert result.exit_code == 1
    assert "same model" in _out(result)
    assert not (tmp_path / ".opendaisugi" / "switchyard.toml").exists()


def test_install_rejects_an_unknown_router(tmp_path, monkeypatch):
    result = _install(tmp_path, monkeypatch, "--router", "fancy")
    assert result.exit_code == 1
    assert "rules, switchyard, off" in _out(result)


def test_install_router_off_and_rules_persist_the_choice(tmp_path, monkeypatch):
    assert _install(tmp_path, monkeypatch, "--router", "off").exit_code == 0
    cfg_path = tmp_path / ".opendaisugi" / "config.yaml"
    assert load_config(cfg_path).gateway_router == "off"
    assert _install(tmp_path, monkeypatch, "--router", "rules").exit_code == 0
    assert load_config(cfg_path).gateway_router == "rules"


def test_a_plain_install_gateway_leaves_the_router_alone(tmp_path, monkeypatch):
    assert _install(tmp_path, monkeypatch, "--router", "off").exit_code == 0
    assert _install(tmp_path, monkeypatch).exit_code == 0
    assert load_config(tmp_path / ".opendaisugi" / "config.yaml").gateway_router == "off"


def test_install_router_without_gateway_writes_no_router_config(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    result = runner.invoke(app, ["install", "--yes", "--runtime", "claude", "--router", "off"])
    assert result.exit_code == 0, _out(result)
    assert "--router only applies with --gateway" in result.output
    assert load_config(tmp_path / ".opendaisugi" / "config.yaml").gateway_router == "rules"


# --- gateway --router -----------------------------------------------------------


def test_gateway_router_switchyard_reports_the_missing_binary(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    result = runner.invoke(app, ["gateway", "--router", "switchyard", "--data-dir", str(tmp_path)])
    assert result.exit_code == 3
    assert INSTALL_CMD in _out(result)


def test_gateway_router_switchyard_needs_an_efficient_model(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    result = runner.invoke(app, ["gateway", "--router", "switchyard", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "--efficient-model" in _out(result)


def test_gateway_rejects_an_unknown_router(tmp_path):
    result = runner.invoke(app, ["gateway", "--router", "fancy", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "rules, switchyard, off" in _out(result)


def test_gateway_router_defaults_to_config(tmp_path, monkeypatch):
    _configure(tmp_path, gateway_router="off")
    captured = {}
    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", lambda **kw: captured.update(kw))
    result = runner.invoke(app, ["gateway", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, _out(result)
    assert captured["router_mode"] == "off"
    assert captured["external"] is None


def test_gateway_router_switchyard_runs_the_child_meters_and_stops_it(tmp_path, monkeypatch):
    """The whole path on loopback: config, a real fake child, a real HTTP turn
    through the gateway app, the journal, and the stop on exit."""
    _fake_binary_on_path(tmp_path, monkeypatch)
    monkeypatch.delenv("ANTHROPIC_API_KEY", raising=False)
    _configure(tmp_path, gateway_router="switchyard", switchyard_efficient_model="qwen3-coder:30b")
    port = _free_port()
    seen = {}

    def fake_serve(**kw):
        import asyncio

        from opendaisugi.gateway_asgi import build_default_gateway, make_gateway_app

        seen.update(kw)
        gateway = build_default_gateway(
            data_dir=kw["data_dir"], router_mode=kw["router_mode"], external=kw["external"]
        )
        gw_app = make_gateway_app(
            gateway,
            upstream_base_url=kw["upstream_base_url"],
            upstream_kind=kw["upstream_kind"],
        )

        async def one_turn():
            transport = httpx.ASGITransport(app=gw_app)
            async with httpx.AsyncClient(transport=transport, base_url="http://gw") as client:
                return await client.post(
                    "/v1/messages",
                    json={
                        "model": "claude-sonnet-5",
                        "max_tokens": 64,
                        "messages": [{"role": "user", "content": "list the files"}],
                    },
                )

        seen["response"] = asyncio.run(one_turn())

    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", fake_serve)
    result = runner.invoke(
        app,
        [
            "gateway",
            "--data-dir",
            str(tmp_path),
            "--switchyard-port",
            str(port),
            "--openai-cheap-model",
            "",
        ],
    )
    assert result.exit_code == 0, _out(result)
    assert seen["upstream_base_url"] == f"http://127.0.0.1:{port}"
    assert seen["upstream_kind"] == "anthropic"
    assert seen["router_mode"] == "external"
    assert seen["external"].efficient_target == "qwen3-coder:30b"
    assert seen["response"].status_code == 200
    assert seen["response"].json()["content"][0]["text"] == "route daisugi"
    rec = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl").load()[0]
    assert rec.model == "qwen3-coder:30b"
    assert rec.downgraded is True
    assert "forwards your own login" in result.output
    assert "sent SIGTERM" in result.output
    assert not list((tmp_path / "gateway").glob("switchyard-*.json"))


def test_gateway_with_own_switchyard_config_leaves_it_untouched(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    own = tmp_path / "own.toml"
    text = (
        "schema_version = 1\n"
        "[llm_clients.c]\nformat = 'openai_chat'\nbase_url = 'http://127.0.0.1:9/v1'\n"
        "[targets.big]\nid = 'big-model'\nllm_client = 'c'\n"
        "[targets.small]\nid = 'small-model'\nllm_client = 'c'\n"
        "[routes.r]\nid = 'daisugi'\ntype = 'stage_router'\ncapable_target = 'big'\n"
        "efficient_target = 'small'\npicker = 'efficient_first'\nconfidence_threshold = 0.5\n"
    )
    own.write_text(text)
    captured = {}
    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", lambda **kw: captured.update(kw))
    result = runner.invoke(
        app,
        [
            "gateway",
            "--router",
            "switchyard",
            "--data-dir",
            str(tmp_path),
            "--switchyard-config",
            str(own),
            "--switchyard-port",
            str(_free_port()),
        ],
    )
    assert result.exit_code == 0, _out(result)
    assert own.read_text() == text
    assert not (tmp_path / "switchyard.toml").exists()
    assert "capable auth: no credential" in result.output
    assert captured["external"].capable_target == "big-model"
    assert captured["external"].efficient_target == "small-model"


def test_gateway_refuses_a_switchyard_config_it_cannot_meter(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    own = tmp_path / "own.toml"
    own.write_text("schema_version = 1\n[targets]\n[routes.r]\nid = 'daisugi'\ntype = 'noop'\n")
    result = runner.invoke(
        app,
        [
            "gateway",
            "--router",
            "switchyard",
            "--data-dir",
            str(tmp_path),
            "--switchyard-config",
            str(own),
        ],
    )
    assert result.exit_code == 1
    assert "two tiers" in _out(result)


# --- router status and stop -----------------------------------------------------


def test_router_status_reports_a_missing_binary(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "not found" in result.output
    assert INSTALL_CMD in result.output
    assert "not running" in result.output


def test_router_status_json_shape(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    payload = json.loads(result.output)
    assert set(payload) == {
        "router",
        "binary",
        "version",
        "children",
        "unreadable_state_files",
        "next_start_auth",
        "targets",
        "recent_turns",
    }
    assert payload["children"] == []


def _child_state(
    data_dir: Path, port: int, config_path: Path, pid: int = 4242, auth: dict | None = None
) -> None:
    gw = data_dir / "gateway"
    gw.mkdir(parents=True, exist_ok=True)
    (gw / f"switchyard-{port}.json").write_text(
        json.dumps(
            {
                "pid": pid,
                "host": "127.0.0.1",
                "port": port,
                "config_path": str(config_path),
                "route_id": "daisugi",
                "auth": auth,
            }
        )
    )


def _keyed_toml(path: Path) -> Path:
    from opendaisugi.router_switchyard import SwitchyardTargets, render_switchyard_toml

    targets = SwitchyardTargets(
        capable_id="claude-sonnet-5",
        efficient_id="qwen3-coder:30b",
        efficient_client_format="openai_chat",
        efficient_base_url="http://127.0.0.1:11434/v1",
        api_key_env="MY_ANTHROPIC_KEY",
    )
    path.write_text(render_switchyard_toml(targets, api_key_present=lambda n: True))
    return path


def test_router_status_probes_the_port_each_child_uses(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    toml_path = _keyed_toml(tmp_path / "running.toml")
    _child_state(tmp_path, 5123, toml_path)
    monkeypatch.setattr("opendaisugi.router_switchyard.child_is_running", lambda pid: True)
    probed = []
    monkeypatch.setattr(
        "opendaisugi.router_switchyard.probe_health",
        lambda host, port, **kw: probed.append((host, port)) or True,
    )
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    payload = json.loads(result.output)
    assert probed == [("127.0.0.1", 5123)]
    child = payload["children"][0]
    assert child["healthy"] is True
    assert child["port"] == 5123
    assert child["route_id"] == "daisugi"


def test_router_status_reads_who_pays_from_the_record_made_at_start(tmp_path, monkeypatch):
    # The shell that runs status has no key and config names none. The running
    # child recorded a key at start. Status must say the key is billed.
    _no_binary(monkeypatch)
    monkeypatch.delenv("MY_ANTHROPIC_KEY", raising=False)
    _configure(tmp_path, gateway_router="rules", switchyard_efficient_model="qwen3-coder:30b")
    keyed = "uses MY_ANTHROPIC_KEY: billed per token to that key"
    _child_state(
        tmp_path,
        5123,
        tmp_path / "gone.toml",
        auth={"capable": keyed, "efficient": "no credential: a local host"},
    )
    monkeypatch.setattr("opendaisugi.router_switchyard.child_is_running", lambda pid: True)
    monkeypatch.setattr("opendaisugi.router_switchyard.probe_health", lambda host, port, **kw: True)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    child = json.loads(result.output)["children"][0]
    assert child["auth"]["capable"] == keyed
    human = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert keyed in human.output
    assert "configured router: rules" in human.output
    assert "running router: switchyard" in human.output


def test_router_status_reports_a_bad_state_file_and_does_not_crash(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    gw = tmp_path / "gateway"
    gw.mkdir()
    (gw / "switchyard-4000.json").write_text(
        json.dumps({"pid": 1, "host": "127.0.0.1", "port": "not-a-port"})
    )
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, _out(result)
    assert "unreadable state file" in result.output
    as_json = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    assert json.loads(as_json.output)["unreadable_state_files"] == [
        str(gw / "switchyard-4000.json")
    ]


def test_router_status_output_has_no_brackets_in_the_turn_count(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "recent turns, last 0:" in result.output


def test_router_stop_stops_every_child_it_finds(tmp_path, monkeypatch):
    for port, pid in ((4000, 11), (4001, 12)):
        _child_state(tmp_path, port, tmp_path / "x.toml", pid=pid)
    sent = []
    monkeypatch.setattr("opendaisugi.router_switchyard.os.kill", lambda pid, sig: sent.append(pid))
    monkeypatch.setattr(
        "opendaisugi.router_switchyard._proc_cmdline",
        lambda pid: "/usr/bin/switchyard-server --config x",
    )
    result = runner.invoke(app, ["router", "stop", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0, _out(result)
    assert sorted(sent) == [11, 12]
    assert not list((tmp_path / "gateway").glob("switchyard-*.json"))


def test_router_status_lists_recent_external_turns_and_shares(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    journal = GatewayJournal(path=tmp_path / "gateway" / "turns.jsonl")
    for i in range(12):
        journal.append(
            GatewayTurnRecord(
                created_at="2026-09-23T00:00:00Z",
                signature="s",
                task=f"task {i}",
                tier="tier-switchyard",
                requested_model="claude-sonnet-5",
                model="qwen3-coder:30b" if i % 2 else "claude-sonnet-5",
                difficulty=0.0,
                downgraded=bool(i % 2),
                estimated=bool(i % 2),
                input_tokens=10,
                output_tokens=5,
                frontier_tokens_saved=15 if i % 2 else 0,
                actual_dollars=0.0,
                counterfactual_dollars=0.0,
            )
        )
    result = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"])
    payload = json.loads(result.output)
    assert len(payload["recent_turns"]) == 10
    assert payload["recent_turns"][-1]["task"] == "task 11"
    assert {row["model"] for row in payload["targets"]} == {"qwen3-coder:30b", "claude-sonnet-5"}
    human = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "50.0%" in human.output


def test_router_stop_with_nothing_running(tmp_path):
    result = runner.invoke(app, ["router", "stop", "--data-dir", str(tmp_path)])
    assert result.exit_code == 0
    assert "nothing to stop" in result.output


# --- the module map ---------------------------------------------------------------


def _switchyard_row(tmp_path, which):
    stages = detect_stages(tmp_path, home=tmp_path, which=which, env={})
    router = next(s for s in stages if s.key == "router")
    return next(m for m in router.modules if m.name == "NeMo Switchyard")


def _which_with_switchyard(name):
    return "/usr/local/bin/switchyard-server" if name == "switchyard-server" else None


def test_router_row_is_available_when_the_binary_is_present(tmp_path):
    row = _switchyard_row(tmp_path, _which_with_switchyard)
    assert row.state == AVAILABLE
    assert "--router switchyard" in row.note


def test_router_row_is_possible_without_the_binary(tmp_path):
    row = _switchyard_row(tmp_path, lambda name: None)
    assert row.state == POSSIBLE
    assert INSTALL_CMD in row.note


def test_router_row_is_active_when_selected_and_present(tmp_path):
    _configure(tmp_path, gateway_router="switchyard")
    row = _switchyard_row(tmp_path, _which_with_switchyard)
    assert row.state == ACTIVE
    assert "daisugi router status" in row.note
    assert "gateway row" in row.note


def test_router_row_says_a_selected_router_lacks_its_binary(tmp_path):
    _configure(tmp_path, gateway_router="switchyard")
    row = _switchyard_row(tmp_path, lambda name: None)
    assert row.state == POSSIBLE
    assert "selected" in row.note
    assert INSTALL_CMD in row.note


# --- who pays: login forwarding by default, a key only when config names it ------


def test_install_forwards_the_login_even_with_a_key_exported(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir(exist_ok=True)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    monkeypatch.setenv("ANTHROPIC_API_KEY", "sk-ant-fake")
    result = runner.invoke(
        app,
        [
            "install",
            "--gateway",
            "--yes",
            "--runtime",
            "claude",
            "--router",
            "switchyard",
            "--efficient-model",
            "claude-haiku-4-5",
        ],
    )
    assert result.exit_code == 0, _out(result)
    doc = tomllib.loads((tmp_path / ".opendaisugi" / "switchyard.toml").read_text())
    for tier in ("capable", "efficient"):
        assert doc["llm_clients"][tier]["forward_auth"] is True
    assert "forwards your own login" in result.output
    assert "billed per token" not in result.output


def test_install_api_key_env_names_the_key_for_every_cloud_tier(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir(exist_ok=True)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    monkeypatch.setenv("MY_ANTHROPIC_KEY", "sk-ant-fake")
    result = runner.invoke(
        app,
        [
            "install",
            "--gateway",
            "--yes",
            "--runtime",
            "claude",
            "--router",
            "switchyard",
            "--efficient-model",
            "claude-haiku-4-5",
            "--api-key-env",
            "MY_ANTHROPIC_KEY",
        ],
    )
    assert result.exit_code == 0, _out(result)
    cfg = load_config(tmp_path / ".opendaisugi" / "config.yaml")
    assert cfg.switchyard_api_key_env == "MY_ANTHROPIC_KEY"
    doc = tomllib.loads((tmp_path / ".opendaisugi" / "switchyard.toml").read_text())
    for tier in ("capable", "efficient"):
        assert doc["llm_clients"][tier]["api_key_env"] == "MY_ANTHROPIC_KEY"
    assert "uses MY_ANTHROPIC_KEY: billed per token to that key" in result.output
    cleared = runner.invoke(
        app,
        [
            "install",
            "--gateway",
            "--yes",
            "--runtime",
            "claude",
            "--router",
            "switchyard",
            "--api-key-env",
            "",
        ],
    )
    assert cleared.exit_code == 0, _out(cleared)
    assert load_config(tmp_path / ".opendaisugi" / "config.yaml").switchyard_api_key_env is None


def test_gateway_refuses_a_named_key_that_is_not_set(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    monkeypatch.delenv("MY_ANTHROPIC_KEY", raising=False)
    _configure(
        tmp_path,
        gateway_router="switchyard",
        switchyard_efficient_model="qwen3-coder:30b",
        switchyard_api_key_env="MY_ANTHROPIC_KEY",
    )
    result = runner.invoke(app, ["gateway", "--data-dir", str(tmp_path)])
    assert result.exit_code == 1
    assert "MY_ANTHROPIC_KEY" in _out(result)
    assert not (tmp_path / "gateway").exists() or not list((tmp_path / "gateway").glob("*.json"))


def test_gateway_startup_line_names_the_key_when_config_names_it(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    monkeypatch.setenv("MY_ANTHROPIC_KEY", "sk-ant-fake")
    _configure(
        tmp_path,
        gateway_router="switchyard",
        switchyard_efficient_model="qwen3-coder:30b",
        switchyard_api_key_env="MY_ANTHROPIC_KEY",
    )
    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", lambda **kw: None)
    result = runner.invoke(
        app, ["gateway", "--data-dir", str(tmp_path), "--switchyard-port", str(_free_port())]
    )
    assert result.exit_code == 0, _out(result)
    assert "capable auth: uses MY_ANTHROPIC_KEY: billed per token to that key" in result.output
    assert "efficient auth: no credential: a local host" in result.output


# --- install edge cases -------------------------------------------------------------


def test_install_router_switchyard_dry_run_writes_nothing(tmp_path, monkeypatch):
    result = _install(
        tmp_path,
        monkeypatch,
        "--router",
        "switchyard",
        "--efficient-model",
        "qwen3-coder:30b",
        "--dry-run",
    )
    assert result.exit_code == 0, _out(result)
    assert "gateway_router to switchyard" in result.output
    assert not (tmp_path / ".opendaisugi" / "config.yaml").exists()
    assert not (tmp_path / ".opendaisugi" / "switchyard.toml").exists()


def test_install_router_switchyard_twice_keeps_the_file_private(tmp_path, monkeypatch):
    args = ("--router", "switchyard", "--efficient-model", "qwen3-coder:30b")
    assert _install(tmp_path, monkeypatch, *args).exit_code == 0
    second = _install(tmp_path, monkeypatch, *args)
    assert second.exit_code == 0, _out(second)
    toml_path = tmp_path / ".opendaisugi" / "switchyard.toml"
    assert oct(toml_path.stat().st_mode & 0o777) == "0o600"


def test_uninstall_keeps_the_router_choice_because_config_is_user_data(tmp_path, monkeypatch):
    args = ("--router", "switchyard", "--efficient-model", "qwen3-coder:30b")
    assert _install(tmp_path, monkeypatch, *args).exit_code == 0
    result = runner.invoke(app, ["install", "--uninstall", "--runtime", "claude"])
    assert result.exit_code == 0, _out(result)
    assert load_config(tmp_path / ".opendaisugi" / "config.yaml").gateway_router == "switchyard"
    assert (tmp_path / ".opendaisugi" / "switchyard.toml").exists()


# --- status states the payer the running child really uses -----------------------


def _status_while_install_changes_the_key(tmp_path, monkeypatch, *, start_key, install_key):
    """Start a gateway, change the key with install while its child runs, read status."""
    _fake_binary_on_path(tmp_path, monkeypatch)
    (tmp_path / ".claude").mkdir(exist_ok=True)
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    monkeypatch.setenv("MY_KEY", "sk-ant-fake")
    data_dir = tmp_path / ".opendaisugi"
    _configure(
        data_dir,
        gateway_router="switchyard",
        switchyard_efficient_model="qwen3-coder:30b",
        switchyard_api_key_env=start_key,
    )
    seen = {}

    def fake_serve(**kw):
        inner = CliRunner()
        changed = inner.invoke(
            app,
            [
                "install",
                "--gateway",
                "--yes",
                "--runtime",
                "claude",
                "--router",
                "switchyard",
                "--api-key-env",
                install_key,
            ],
        )
        seen["install"] = changed
        status = inner.invoke(app, ["router", "status", "--data-dir", str(data_dir), "--json"])
        seen["status"] = json.loads(status.output)

    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", fake_serve)
    result = runner.invoke(
        app, ["gateway", "--data-dir", str(data_dir), "--switchyard-port", str(_free_port())]
    )
    assert result.exit_code == 0, _out(result)
    assert seen["install"].exit_code == 0, _out(seen["install"])
    return seen["status"]["children"][0]


def test_status_keeps_the_login_payer_after_install_names_a_key(tmp_path, monkeypatch):
    child = _status_while_install_changes_the_key(
        tmp_path, monkeypatch, start_key=None, install_key="MY_KEY"
    )
    assert child["auth"]["capable"] == "forwards your own login, as the gateway does"


def test_status_keeps_the_key_payer_after_install_clears_the_key(tmp_path, monkeypatch):
    child = _status_while_install_changes_the_key(
        tmp_path, monkeypatch, start_key="MY_KEY", install_key=""
    )
    assert child["auth"]["capable"] == "uses MY_KEY: billed per token to that key"


def test_each_child_gets_its_own_config_copy_and_logs(tmp_path, monkeypatch):
    _fake_binary_on_path(tmp_path, monkeypatch)
    _configure(tmp_path, gateway_router="switchyard", switchyard_efficient_model="qwen3-coder:30b")
    port = _free_port()
    seen = {}

    def fake_serve(**kw):
        seen["state"] = json.loads((tmp_path / "gateway" / f"switchyard-{port}.json").read_text())

    monkeypatch.setattr("opendaisugi.gateway_asgi.serve_gateway", fake_serve)
    result = runner.invoke(
        app, ["gateway", "--data-dir", str(tmp_path), "--switchyard-port", str(port)]
    )
    assert result.exit_code == 0, _out(result)
    copy = tmp_path / "gateway" / f"switchyard-{port}.toml"
    assert seen["state"]["config_path"] == str(copy)
    assert oct(copy.stat().st_mode & 0o777) == "0o600"
    assert seen["state"]["auth"]["capable"] == "forwards your own login, as the gateway does"
    assert seen["state"]["log_path"] == str(tmp_path / "gateway" / f"switchyard-{port}.log")
    assert (tmp_path / "gateway" / f"switchyard-{port}.log").exists()
    assert f"--routing-log-file {tmp_path / 'gateway' / f'switchyard-routing-{port}.jsonl'}" in (
        result.output
    )


def test_status_labels_a_dead_child_stale(tmp_path, monkeypatch):
    _no_binary(monkeypatch)
    _child_state(tmp_path, 5123, _keyed_toml(tmp_path / "running.toml"))
    monkeypatch.setattr("opendaisugi.router_switchyard.child_is_running", lambda pid: False)
    monkeypatch.setattr(
        "opendaisugi.router_switchyard.probe_health", lambda host, port, **kw: False
    )
    human = runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path)])
    assert "running router" not in human.output
    assert "stale state file" in human.output
    assert "daisugi router stop" in human.output
    payload = json.loads(
        runner.invoke(app, ["router", "status", "--data-dir", str(tmp_path), "--json"]).output
    )
    assert payload["children"][0]["running"] is False
