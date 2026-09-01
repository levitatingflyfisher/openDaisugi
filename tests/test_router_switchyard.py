"""NeMo Switchyard as a managed child: pinned facts, config, and process control.

No test here runs a real switchyard-server. PATH lookup, spawn, and the health
probe are injected, or a loopback http.server stands in for /health.
"""

from __future__ import annotations

import json
import signal
import sys
import threading
import tomllib
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

from opendaisugi.router_switchyard import (
    BINARY_NAME,
    DEFAULT_HOST,
    DEFAULT_PORT,
    HEALTH_PATH,
    INSTALL_CMD,
    SELECTED_MODEL_HEADER,
    SwitchyardHandle,
    SwitchyardTargets,
    binary_version,
    check_prerequisite,
    locate_binary,
    probe_health,
    read_state,
    render_switchyard_toml,
    start_switchyard,
    stop_switchyard,
    write_switchyard_config,
)

FIXTURE = Path(__file__).parent / "fixtures" / "switchyard" / "routes.example.toml"


def test_binary_and_install_facts_are_pinned():
    assert BINARY_NAME == "switchyard-server"
    assert INSTALL_CMD == (
        "cargo install --locked --git https://github.com/NVIDIA-NeMo/Switchyard "
        "--tag v0.3.0 switchyard-server"
    )
    assert HEALTH_PATH == "/health"
    assert DEFAULT_HOST == "127.0.0.1"
    assert DEFAULT_PORT == 4000
    assert SELECTED_MODEL_HEADER == "x-model-router-selected-model"


def test_fixture_is_valid_toml_with_the_real_schema_v1_keys():
    doc = tomllib.loads(FIXTURE.read_text())
    assert doc["schema_version"] == 1
    assert set(doc["llm_clients"]["capable"]) == {"format", "base_url", "forward_auth"}
    assert set(doc["llm_clients"]["efficient"]) == {"format", "base_url"}
    assert set(doc["targets"]["capable"]) == {"id", "llm_client"}
    assert set(doc["routes"]["daisugi"]) == {
        "id",
        "type",
        "capable_target",
        "efficient_target",
        "picker",
        "confidence_threshold",
    }
    assert doc["routes"]["daisugi"]["type"] == "stage_router"


def test_locate_binary_uses_the_injected_which():
    assert locate_binary(which=lambda name: None) is None
    assert (
        locate_binary(which=lambda name: "/usr/local/bin/switchyard-server")
        == "/usr/local/bin/switchyard-server"
    )


def test_check_prerequisite_teaches_the_pinned_install_when_absent():
    msg = check_prerequisite(which=lambda name: None)
    assert msg is not None
    assert INSTALL_CMD in msg
    assert "rust" in msg.lower()


def test_check_prerequisite_is_none_when_the_binary_is_present():
    assert check_prerequisite(which=lambda name: "/usr/bin/switchyard-server") is None


def test_binary_version_is_none_without_the_binary():
    assert binary_version(which=lambda name: None) is None


def test_binary_version_reads_the_dash_dash_version_output():
    seen = {}

    class _Result:
        stdout = "switchyard-server 0.3.0\n"

    def fake_run(argv, **kwargs):
        seen["argv"] = argv
        return _Result()

    version = binary_version(which=lambda name: "/opt/bin/switchyard-server", run=fake_run)
    assert version == "switchyard-server 0.3.0"
    assert seen["argv"] == ["/opt/bin/switchyard-server", "--version"]


def test_binary_version_is_none_when_the_binary_will_not_run():
    def broken_run(argv, **kwargs):
        raise OSError("exec format error")

    assert binary_version(which=lambda name: "/opt/bin/switchyard-server", run=broken_run) is None


# --- rendering the TOML ------------------------------------------------------


def _targets(**overrides) -> SwitchyardTargets:
    base = dict(
        capable_id="claude-sonnet-5",
        efficient_id="qwen3-coder:30b",
        efficient_client_format="openai_chat",
        efficient_base_url="http://127.0.0.1:11434/v1",
    )
    base.update(overrides)
    return SwitchyardTargets(**base)


def _route(parsed: dict, route_id: str = "daisugi") -> dict:
    return next(r for r in parsed["routes"].values() if r["id"] == route_id)


def test_render_matches_the_pinned_fixtures_key_shape():
    fixture = tomllib.loads(FIXTURE.read_text())
    rendered = tomllib.loads(render_switchyard_toml(_targets(), api_key_present=lambda name: False))
    assert rendered["schema_version"] == 1
    assert set(_route(rendered)) == set(fixture["routes"]["daisugi"])
    assert set(rendered["targets"]["capable"]) == set(fixture["targets"]["capable"])
    assert set(rendered["llm_clients"]["capable"]) == set(fixture["llm_clients"]["capable"])
    assert set(rendered["llm_clients"]["efficient"]) == set(fixture["llm_clients"]["efficient"])


def test_a_cloud_tier_forwards_the_login_even_when_a_key_is_in_the_environment():
    # An exported key never changes who pays. Only an explicit config field does.
    parsed = tomllib.loads(render_switchyard_toml(_targets(), api_key_present=lambda name: True))
    assert parsed["llm_clients"]["capable"]["forward_auth"] is True
    assert "api_key_env" not in parsed["llm_clients"]["capable"]


def test_a_named_key_variable_is_used_for_every_cloud_tier():
    targets = _targets(
        efficient_id="claude-haiku-4-5",
        efficient_client_format="anthropic_messages",
        efficient_base_url="https://api.anthropic.com",
        efficient_local=False,
        api_key_env="MY_ANTHROPIC_KEY",
    )
    parsed = tomllib.loads(render_switchyard_toml(targets, api_key_present=lambda name: True))
    for tier in ("capable", "efficient"):
        assert parsed["llm_clients"][tier]["api_key_env"] == "MY_ANTHROPIC_KEY"
        assert "forward_auth" not in parsed["llm_clients"][tier]


def test_a_named_key_variable_that_is_not_set_is_refused():
    # The server would refuse to load. Refusing here names the variable.
    with pytest.raises(ValueError, match="MY_ANTHROPIC_KEY"):
        render_switchyard_toml(
            _targets(api_key_env="MY_ANTHROPIC_KEY"), api_key_present=lambda name: False
        )


def test_a_local_efficient_client_gets_no_credential_at_all():
    # Forwarding the caller's Anthropic login to a local host would leak it.
    parsed = tomllib.loads(
        render_switchyard_toml(
            _targets(api_key_env="MY_ANTHROPIC_KEY"), api_key_present=lambda name: True
        )
    )
    assert "forward_auth" not in parsed["llm_clients"]["efficient"]
    assert "api_key_env" not in parsed["llm_clients"]["efficient"]


def test_a_cloud_efficient_tier_with_no_named_key_forwards_the_login():
    targets = _targets(
        efficient_id="claude-haiku-4-5",
        efficient_client_format="anthropic_messages",
        efficient_base_url="https://api.anthropic.com",
        efficient_local=False,
    )
    parsed = tomllib.loads(render_switchyard_toml(targets, api_key_present=lambda name: True))
    assert parsed["llm_clients"]["efficient"]["forward_auth"] is True
    assert "api_key_env" not in parsed["llm_clients"]["efficient"]


def test_render_stage_router_fields_come_from_the_arguments():
    toml_text = render_switchyard_toml(
        _targets(capable_id="claude-opus-4-8", efficient_id="claude-haiku-4-5"),
        route_id="switchyard/daisugi",
        picker="capable_first",
        confidence_threshold=0.7,
        api_key_present=lambda name: False,
    )
    parsed = tomllib.loads(toml_text)
    route = _route(parsed, "switchyard/daisugi")
    assert route["type"] == "stage_router"
    assert route["picker"] == "capable_first"
    assert route["confidence_threshold"] == 0.7
    assert route["capable_target"] == "capable"
    assert route["efficient_target"] == "efficient"
    assert parsed["targets"]["capable"]["id"] == "claude-opus-4-8"
    assert parsed["targets"]["efficient"]["id"] == "claude-haiku-4-5"


def test_render_quotes_ids_so_toml_stays_valid():
    toml_text = render_switchyard_toml(
        _targets(efficient_id='odd"model\\id'), api_key_present=lambda name: False
    )
    assert tomllib.loads(toml_text)["targets"]["efficient"]["id"] == 'odd"model\\id'


@pytest.mark.parametrize(
    "kwargs",
    [
        {"picker": "random_first"},
        {"confidence_threshold": 1.5},
        {"confidence_threshold": -0.1},
        {"route_id": ""},
    ],
)
def test_render_refuses_values_the_schema_rejects(kwargs):
    with pytest.raises(ValueError):
        render_switchyard_toml(_targets(), api_key_present=lambda name: False, **kwargs)


def test_render_refuses_one_id_for_both_tiers():
    with pytest.raises(ValueError, match="same model"):
        render_switchyard_toml(
            _targets(efficient_id="claude-sonnet-5"), api_key_present=lambda name: False
        )


def test_write_switchyard_config_writes_mode_0600(tmp_path):
    path = write_switchyard_config(tmp_path, "schema_version = 1\n")
    assert path == tmp_path / "switchyard.toml"
    assert path.read_text() == "schema_version = 1\n"
    assert oct(path.stat().st_mode & 0o777) == "0o600"


def test_write_switchyard_config_tightens_an_existing_open_file(tmp_path):
    path = tmp_path / "switchyard.toml"
    path.write_text("old\n")
    path.chmod(0o644)
    write_switchyard_config(tmp_path, "schema_version = 1\n")
    assert oct(path.stat().st_mode & 0o777) == "0o600"


def test_client_auth_modes_say_who_pays():
    from opendaisugi.router_switchyard import client_auth_modes

    assert client_auth_modes(_targets()) == {
        "capable": "forwards your own login, as the gateway does",
        "efficient": "no credential: a local host",
    }
    keyed = client_auth_modes(_targets(api_key_env="MY_ANTHROPIC_KEY"))
    assert keyed["capable"] == "uses MY_ANTHROPIC_KEY: billed per token to that key"


def test_client_auth_from_toml_reads_what_the_child_loaded(tmp_path):
    from opendaisugi.router_switchyard import client_auth_from_toml

    path = tmp_path / "routes.toml"
    path.write_text(
        render_switchyard_toml(
            _targets(api_key_env="MY_ANTHROPIC_KEY"), api_key_present=lambda name: True
        )
    )
    assert client_auth_from_toml(path, "daisugi") == {
        "capable": "uses MY_ANTHROPIC_KEY: billed per token to that key",
        "efficient": "no credential",
    }
    path.write_text(render_switchyard_toml(_targets(), api_key_present=lambda name: False))
    assert client_auth_from_toml(path, "daisugi")["capable"] == (
        "forwards your own login, as the gateway does"
    )
    assert client_auth_from_toml(tmp_path / "missing.toml", "daisugi") is None


# --- the managed child ---------------------------------------------------------


def _healthy_after(n: int):
    calls = {"n": 0}

    def probe(host: str, port: int) -> bool:
        calls["n"] += 1
        return calls["n"] > n

    return probe


def test_start_switchyard_reports_healthy_and_writes_its_state(tmp_path):
    state_path = tmp_path / "gateway" / "switchyard.json"
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        port=5001,
        spawn=lambda argv, log_path: 4242,
        health_probe=_healthy_after(2),  # the pre-spawn probe, then one more miss
        alive=lambda pid: True,
        state_path=state_path,
        wait_s=1.0,
        poll_interval_s=0.01,
    )
    assert isinstance(handle, SwitchyardHandle)
    assert handle.pid == 4242
    assert handle.base_url == "http://127.0.0.1:5001"
    assert "healthy" in msg
    state = read_state(state_path)
    assert state["pid"] == 4242
    assert state["host"] == "127.0.0.1"
    assert state["port"] == 5001
    assert state["config_path"] == str(tmp_path / "routes.toml")


def test_start_switchyard_passes_config_host_port_and_routing_log_in_argv(tmp_path):
    seen = {}

    def fake_spawn(argv, log_path):
        seen["argv"] = argv
        return 1

    start_switchyard(
        tmp_path / "routes.toml",
        port=5001,
        binary="/opt/bin/switchyard-server",
        routing_log_path=tmp_path / "routing.jsonl",
        spawn=fake_spawn,
        health_probe=_healthy_after(1),
        alive=lambda pid: True,
        wait_s=1.0,
        poll_interval_s=0.01,
    )
    assert seen["argv"] == [
        "/opt/bin/switchyard-server",
        "--config",
        str(tmp_path / "routes.toml"),
        "--host",
        "127.0.0.1",
        "--port",
        "5001",
        "--routing-log-file",
        str(tmp_path / "routing.jsonl"),
    ]


def test_start_switchyard_refuses_any_host_but_loopback(tmp_path):
    spawned = []
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        host="0.0.0.0",
        spawn=lambda argv, log_path: spawned.append(argv) or 1,
        health_probe=lambda h, p: True,
        wait_s=0.05,
    )
    assert handle is None
    assert spawned == []
    assert "127.0.0.1" in msg


def test_start_switchyard_refuses_a_port_that_already_answers(tmp_path):
    spawned = []
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=lambda argv, log_path: spawned.append(argv) or 1,
        health_probe=lambda h, p: True,
        wait_s=0.05,
    )
    assert handle is None
    assert spawned == []
    assert "already answers" in msg
    assert "daisugi router stop" in msg


def test_start_switchyard_reports_a_child_that_exits(tmp_path):
    log_path = tmp_path / "switchyard.log"
    log_path.write_text("invalid server config routes.toml: failed to parse TOML\n")
    state_path = tmp_path / "switchyard.json"
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=lambda argv, lp: 77,
        health_probe=lambda h, p: False,
        alive=lambda pid: False,
        log_path=log_path,
        state_path=state_path,
        wait_s=1.0,
        poll_interval_s=0.01,
    )
    assert handle is None
    assert "exited" in msg
    assert "failed to parse TOML" in msg
    assert not state_path.exists()


def test_start_switchyard_times_out_stops_the_child_and_names_the_dry_run(tmp_path):
    killed = []
    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=lambda argv, log_path: 88,
        health_probe=lambda h, p: False,
        alive=lambda pid: True,
        kill=lambda pid, sig: killed.append((pid, sig)),
        wait_s=0.05,
        poll_interval_s=0.01,
        kill_grace_s=0.05,
    )
    assert handle is None
    # A child that ignores SIGTERM gets SIGKILL: nothing else would stop it.
    assert killed == [(88, signal.SIGTERM), (88, signal.SIGKILL)]
    assert "--dry-run" in msg
    assert "switchyard-server" in msg


def test_start_switchyard_reports_a_binary_that_will_not_spawn(tmp_path):
    def broken(argv, log_path):
        raise FileNotFoundError(argv[0])

    handle, msg = start_switchyard(
        tmp_path / "routes.toml",
        spawn=broken,
        health_probe=lambda h, p: False,
        wait_s=0.05,
    )
    assert handle is None
    assert "could not start" in msg


def test_probe_health_reads_a_real_http_server():
    class _Health(BaseHTTPRequestHandler):
        def do_GET(self):  # noqa: N802
            self.send_response(200 if self.path == "/health" else 404)
            self.end_headers()

        def log_message(self, *a):
            pass

    server = HTTPServer(("127.0.0.1", 0), _Health)
    port = server.server_address[1]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        assert probe_health("127.0.0.1", port) is True
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def test_probe_health_is_false_when_nothing_is_listening():
    assert probe_health("127.0.0.1", 1) is False


def _write_state(path, pid=4242):
    path.write_text(json.dumps({"pid": pid, "host": "127.0.0.1", "port": 4000}))


def test_stop_switchyard_sends_sigterm_and_removes_the_state(tmp_path):
    state_path = tmp_path / "switchyard.json"
    _write_state(state_path)
    sent = {}

    def fake_kill(pid, sig):
        sent["pid"], sent["sig"] = pid, sig

    msg = stop_switchyard(
        state_path, kill=fake_kill, cmdline=lambda pid: "/usr/bin/switchyard-server --config x"
    )
    assert sent == {"pid": 4242, "sig": signal.SIGTERM}
    assert not state_path.exists()
    assert "sent SIGTERM" in msg


def test_stop_switchyard_never_signals_a_process_that_is_not_switchyard(tmp_path):
    # A stale state file whose pid now belongs to another program.
    state_path = tmp_path / "switchyard.json"
    _write_state(state_path)
    sent = []
    msg = stop_switchyard(
        state_path, kill=lambda pid, sig: sent.append(pid), cmdline=lambda pid: "/usr/bin/vim"
    )
    assert sent == []
    assert not state_path.exists()
    assert "not switchyard-server" in msg


def test_stop_switchyard_is_idempotent_with_no_state(tmp_path):
    assert "nothing to stop" in stop_switchyard(tmp_path / "nope.json")


def test_stop_switchyard_handles_an_already_dead_process(tmp_path):
    state_path = tmp_path / "switchyard.json"
    _write_state(state_path, pid=999999)

    def fake_kill(pid, sig):
        raise ProcessLookupError()

    msg = stop_switchyard(state_path, kill=fake_kill, cmdline=lambda pid: None)
    assert "already stopped" in msg
    assert not state_path.exists()


def test_stop_switchyard_removes_an_unreadable_state_file(tmp_path):
    state_path = tmp_path / "switchyard.json"
    state_path.write_text("not json")
    assert "unreadable" in stop_switchyard(state_path, kill=lambda pid, sig: None)
    assert not state_path.exists()


FAKE_SERVER = Path(__file__).parent / "fixtures" / "switchyard" / "switchyard-server"


def _free_port() -> int:
    import socket

    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def test_a_real_child_starts_answers_and_stops(tmp_path):
    import sys

    from opendaisugi.config import default_config
    from opendaisugi.router_switchyard import targets_from_config

    cfg = default_config().model_copy(update={"switchyard_efficient_model": "qwen3-coder:30b"})
    config_path = write_switchyard_config(
        tmp_path, render_switchyard_toml(targets_from_config(cfg), api_key_present=lambda n: False)
    )
    # A wrapper named switchyard-server, so the stop check sees the real name.
    wrapper = tmp_path / "bin" / "switchyard-server"
    wrapper.parent.mkdir()
    wrapper.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{FAKE_SERVER}" "$@"\n')
    wrapper.chmod(0o755)
    port = _free_port()
    state_path = tmp_path / "gateway" / "switchyard.json"
    handle, msg = start_switchyard(
        config_path,
        port=port,
        binary=str(wrapper),
        state_path=state_path,
        log_path=tmp_path / "gateway" / "switchyard.log",
        wait_s=10.0,
    )
    assert handle is not None, msg
    try:
        assert probe_health("127.0.0.1", port) is True
    finally:
        stop_msg = stop_switchyard(state_path, wait_s=5.0)
    assert stop_msg == f"sent SIGTERM to switchyard-server, pid {handle.pid}"
    assert probe_health("127.0.0.1", port) is False


@pytest.mark.skipif(not sys.platform.startswith("linux"), reason="PR_SET_PDEATHSIG is Linux only")
def test_the_child_dies_when_its_gateway_is_killed(tmp_path):
    """A gateway killed with SIGKILL runs no cleanup. The kernel must stop the child."""
    import os
    import subprocess
    import time

    from opendaisugi.config import default_config
    from opendaisugi.router_switchyard import targets_from_config

    cfg = default_config().model_copy(update={"switchyard_efficient_model": "qwen3-coder:30b"})
    config_path = write_switchyard_config(
        tmp_path, render_switchyard_toml(targets_from_config(cfg), api_key_present=lambda n: False)
    )
    wrapper = tmp_path / "bin" / "switchyard-server"
    wrapper.parent.mkdir()
    wrapper.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{FAKE_SERVER}" "$@"\n')
    wrapper.chmod(0o755)
    port = _free_port()
    parent_code = (
        "import sys, time\n"
        "from pathlib import Path\n"
        "from opendaisugi.router_switchyard import start_switchyard\n"
        f"handle, msg = start_switchyard(Path({str(config_path)!r}), port={port}, "
        f"binary={str(wrapper)!r})\n"
        "print(handle.pid if handle else 'FAIL ' + msg, flush=True)\n"
        "time.sleep(60)\n"
    )
    parent = subprocess.Popen(
        [sys.executable, "-c", parent_code], stdout=subprocess.PIPE, text=True
    )
    child_pid = None
    try:
        line = parent.stdout.readline().strip()
        assert line.isdigit(), line
        child_pid = int(line)
        assert probe_health("127.0.0.1", port) is True
        parent.kill()
        parent.wait(timeout=5)
        deadline = time.monotonic() + 5.0
        while probe_health("127.0.0.1", port) and time.monotonic() < deadline:
            time.sleep(0.05)
        assert probe_health("127.0.0.1", port) is False, "the child outlived its gateway"
    finally:
        if parent.poll() is None:
            parent.kill()
        if child_pid is not None:
            try:
                os.kill(child_pid, signal.SIGKILL)
            except ProcessLookupError:
                pass


def test_the_default_spawn_sets_a_parent_death_signal_on_linux(monkeypatch):
    from opendaisugi import router_switchyard as sy

    seen = {}

    class _Proc:
        pid = 1

    def fake_popen(argv, **kwargs):
        seen.update(kwargs)
        return _Proc()

    monkeypatch.setattr(sy.subprocess, "Popen", fake_popen)
    sy._default_spawn(["x"], None)
    sy._CHILDREN.pop(1, None)
    if sys.platform.startswith("linux"):
        assert callable(seen.get("preexec_fn"))


def test_a_timed_out_child_that_exits_on_sigterm_gets_no_sigkill(tmp_path):
    killed = []
    handle, _ = start_switchyard(
        tmp_path / "routes.toml",
        spawn=lambda argv, log_path: 88,
        health_probe=lambda h, p: False,
        alive=lambda pid: not killed,
        kill=lambda pid, sig: killed.append(sig),
        wait_s=0.05,
        poll_interval_s=0.01,
        kill_grace_s=0.5,
    )
    assert handle is None
    assert killed == [signal.SIGTERM]


# --- one state file per port ---------------------------------------------------

from opendaisugi.router_switchyard import (  # noqa: E402
    list_states,
    state_path_for,
    stop_own_child,
)


def test_the_state_file_is_keyed_by_port(tmp_path):
    assert state_path_for(tmp_path, 4001) == tmp_path / "gateway" / "switchyard-4001.json"


def test_list_states_finds_every_port_and_flags_a_bad_file(tmp_path):
    gw = tmp_path / "gateway"
    gw.mkdir()
    (gw / "switchyard-4000.json").write_text(
        json.dumps({"pid": 1, "host": "127.0.0.1", "port": 4000})
    )
    (gw / "switchyard-4001.json").write_text(
        json.dumps({"pid": 2, "host": "127.0.0.1", "port": "x"})
    )
    found = dict((p.name, st) for p, st in list_states(tmp_path))
    assert found["switchyard-4000.json"]["port"] == 4000
    assert found["switchyard-4001.json"] is None


def test_read_state_refuses_a_port_that_is_not_a_number(tmp_path):
    path = tmp_path / "s.json"
    path.write_text(json.dumps({"pid": 1, "host": "127.0.0.1", "port": "4000x"}))
    assert read_state(path) is None


def _handle(tmp_path, pid, port=4000):
    return SwitchyardHandle(
        pid=pid, host="127.0.0.1", port=port, config_path=tmp_path / "r.toml", argv=[]
    )


def test_stop_own_child_signals_its_own_pid_and_removes_its_own_state(tmp_path):
    state = state_path_for(tmp_path, 4000)
    state.parent.mkdir(parents=True)
    state.write_text(json.dumps({"pid": 4242, "host": "127.0.0.1", "port": 4000}))
    sent = []
    msg = stop_own_child(_handle(tmp_path, 4242), state, kill=lambda pid, sig: sent.append(pid))
    assert sent == [4242]
    assert not state.exists()
    assert "sent SIGTERM" in msg


def test_stop_own_child_leaves_a_state_file_that_names_another_pid(tmp_path):
    # Another gateway wrote this file later. It is not ours to remove.
    state = state_path_for(tmp_path, 4000)
    state.parent.mkdir(parents=True)
    state.write_text(json.dumps({"pid": 5555, "host": "127.0.0.1", "port": 4000}))
    sent = []
    stop_own_child(_handle(tmp_path, 4242), state, kill=lambda pid, sig: sent.append(pid))
    assert sent == [4242]
    assert state.exists()


def test_stop_switchyard_reports_an_exited_child_that_is_not_yet_reaped(tmp_path):
    # A zombie has an empty command line. It has exited; it is not another program.
    state = tmp_path / "s.json"
    state.write_text(json.dumps({"pid": 4242, "host": "127.0.0.1", "port": 4000}))
    sent = []
    msg = stop_switchyard(state, kill=lambda pid, sig: sent.append(pid), cmdline=lambda pid: "")
    assert sent == []
    assert "already exited" in msg
    assert not state.exists()


# --- an upstream example, copied byte for byte ---------------------------------

UPSTREAM = (
    Path(__file__).parent
    / "fixtures"
    / "switchyard"
    / ("upstream-v0.3.0-deepswe-stage-router.toml")
)
# Copied unchanged from NVIDIA-NeMo/Switchyard tag v0.3.0, commit 336196f,
# benchmark/routing-profiles/deepswe-v11-stage-router-luna-sol.toml. It is
# independent of this project's own reading of the schema.
STAGE_ROUTER_REQUIRED = {
    "id",
    "type",
    "capable_target",
    "efficient_target",
    "picker",
    "confidence_threshold",
}


def test_the_renderer_matches_an_upstream_stage_router_example():
    upstream = tomllib.loads(UPSTREAM.read_text())
    rendered = tomllib.loads(render_switchyard_toml(_targets(), api_key_present=lambda n: False))
    up_route = upstream["routes"]["switchyard"]
    assert up_route["type"] == "stage_router"
    assert STAGE_ROUTER_REQUIRED <= set(up_route)
    assert set(_route(rendered)) == STAGE_ROUTER_REQUIRED
    assert set(rendered["targets"]["capable"]) == set(upstream["targets"]["capable"])
    assert set(rendered["targets"]["efficient"]) == set(upstream["targets"]["efficient"])
    up_client = set(upstream["llm_clients"]["openrouter"])
    for tier in ("capable", "efficient"):
        # Both use format and base_url. Auth is api_key_env upstream and
        # forward_auth or nothing here.
        assert {"format", "base_url"} <= set(rendered["llm_clients"][tier]) & up_client


def test_the_meter_reads_the_tier_pair_and_auth_of_the_upstream_example():
    from opendaisugi.router_switchyard import client_auth_from_toml, route_targets_from_toml

    assert route_targets_from_toml(UPSTREAM, "gpt-5.6-luna") == (
        "openai/gpt-5.6-sol",
        "openai/gpt-5.6-luna",
    )
    assert client_auth_from_toml(UPSTREAM, "gpt-5.6-luna") == {
        "capable": "uses OPENROUTER_API_KEY: billed per token to that key",
        "efficient": "uses OPENROUTER_API_KEY: billed per token to that key",
    }
