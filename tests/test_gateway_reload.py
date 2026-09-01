"""The gateway re-reads its config without a restart, and only loopback may ask
it to. A reload that anyone on the network can trigger is a control surface."""

import asyncio
import json
import signal
from pathlib import Path

import yaml

from opendaisugi.gateway_asgi import ConfigReloader, make_gateway_app


def _write(path: Path, **fields) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    existing = yaml.safe_load(path.read_text()) if path.exists() else {}
    existing = existing or {}
    existing.update(fields)
    path.write_text(yaml.safe_dump(existing, sort_keys=True))


def test_reloader_rebuilds_when_the_file_changes(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    seen: list[str] = []

    def _rebuild(config):
        seen.append(config.gateway_local_model)
        return config.gateway_local_model

    r = ConfigReloader(path, _rebuild, min_interval_s=0.0)
    assert r.current() == "one"
    _write(path, gateway_local_model="two")
    assert r.current() == "two"
    assert seen == ["one", "two"]


def test_reloader_hands_the_rebuild_a_whole_fresh_config(tmp_path):
    """Structural, not behavioural: a Config field that the rebuild reads
    arrives here with no new wiring, because the reloader passes the whole
    object. gateway_router is not such a field: `daisugi gateway` reads it
    once at launch, and serve_gateway keeps that launch mode on every
    rebuild."""
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one", verifier_client="rust", matcher_model="lexical")
    captured = {}

    def _rebuild(config):
        captured["config"] = config
        return config

    ConfigReloader(path, _rebuild, min_interval_s=0.0).current()
    config = captured["config"]
    assert config.gateway_local_model == "one"
    assert config.verifier_client == "rust"
    assert config.matcher_model == "lexical"


def test_reloader_throttles_stat_calls(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    calls = []
    r = ConfigReloader(path, lambda c: calls.append(1) or c, min_interval_s=3600.0)
    r.current()
    _write(path, gateway_local_model="two")
    r.current()
    assert len(calls) == 1, "a within-interval call must not re-read the file"
    assert r.reload() is True, "an explicit reload always re-reads"
    assert len(calls) == 2


def test_a_missing_or_broken_config_keeps_the_running_gateway(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c.gateway_local_model, min_interval_s=0.0)
    assert r.current() == "one"
    path.write_text("{ this: is: not: yaml\n")
    assert r.current() == "one", "a broken config must not take the gateway down"


def test_a_rebuild_that_raises_keeps_the_running_gateway(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    state = {"boom": False}

    def _rebuild(config):
        if state["boom"]:
            raise RuntimeError("rebuild exploded")
        return config.gateway_local_model

    r = ConfigReloader(path, _rebuild, min_interval_s=0.0)
    assert r.current() == "one"
    state["boom"] = True
    _write(path, gateway_local_model="two")
    assert r.reload() is False
    assert r.current() == "one"


def _call(app, scope, body=b""):
    sent = []

    async def receive():
        return {"type": "http.request", "body": body, "more_body": False}

    async def send(message):
        sent.append(message)

    asyncio.run(app(scope, receive, send))
    return sent


def _scope(path: str, client):
    return {"type": "http", "path": path, "method": "POST", "headers": [], "client": client}


def test_reload_endpoint_accepts_loopback(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    reloads = []
    r = ConfigReloader(
        path, lambda c: reloads.append(c.gateway_local_model) or c, min_interval_s=3600.0
    )
    app = make_gateway_app(object(), reloader=r)
    sent = _call(app, _scope("/_reload", ("127.0.0.1", 51234)))
    assert sent[0]["status"] == 200
    assert json.loads(sent[1]["body"])["reloaded"] is True
    sent = _call(app, _scope("/_reload", ("::1", 51234)))
    assert sent[0]["status"] == 200


def test_reload_endpoint_refuses_a_remote_client(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c, min_interval_s=3600.0)
    app = make_gateway_app(object(), reloader=r)
    for client in (("10.0.0.4", 51234), ("192.168.1.9", 80), ("::ffff:10.0.0.4", 1), None, ()):
        sent = _call(app, _scope("/_reload", client))
        assert sent[0]["status"] == 403, client


def test_reload_endpoint_is_absent_without_a_reloader(tmp_path):
    app = make_gateway_app(object())
    sent = _call(app, _scope("/_reload", ("127.0.0.1", 1)))
    assert sent[0]["status"] == 404


def test_reload_endpoint_refuses_a_non_post_method(tmp_path):
    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c, min_interval_s=3600.0)
    app = make_gateway_app(object(), reloader=r)
    scope = dict(_scope("/_reload", ("127.0.0.1", 1)), method="GET")
    sent = _call(app, scope)
    assert sent[0]["status"] == 405


def _turn_body() -> bytes:
    return json.dumps(
        {
            "model": "claude-opus-4-8",
            "max_tokens": 64,
            "messages": [{"role": "user", "content": "list the files in src"}],
        }
    ).encode()


async def test_a_config_change_routes_the_next_request_differently(tmp_path):
    """End to end: no restart, no new app object. Change the config on disk,
    replay a turn through the SAME app, and the model that reaches the
    upstream is the new one."""
    import httpx

    from opendaisugi.config import load_config, save_config
    from opendaisugi.gateway_asgi import build_default_gateway

    path = tmp_path / "config.yaml"
    save_config(load_config(path).model_copy(update={"gateway_local_model": None}), path)

    def _rebuild(config):
        return build_default_gateway(
            data_dir=tmp_path, journalling=False, local_model=config.gateway_local_model
        )

    reloader = ConfigReloader(path, _rebuild, min_interval_s=0.0)
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=tmp_path, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )

    async def _post():
        transport = httpx.ASGITransport(app=app)
        async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
            return await c.post(
                "/v1/messages",
                content=_turn_body(),
                headers={"content-type": "application/json"},
            )

    await _post()
    assert seen[-1] == "claude-haiku-4-5", "the easy turn should have gone to the cheap model"

    save_config(
        load_config(path).model_copy(update={"gateway_local_model": "qwen2.5-coder-7b"}), path
    )
    await _post()
    assert seen[-1] == "qwen2.5-coder-7b", "the handler is still serving the old gateway"


async def test_a_failed_rebuild_keeps_serving_the_original_gateway(tmp_path):
    """Fail-open on routing only: a broken config costs a saving, not a service."""
    import httpx

    from opendaisugi.gateway_asgi import build_default_gateway

    path = tmp_path / "config.yaml"
    path.write_text("{ not: valid: yaml\n")
    reloader = ConfigReloader(path, lambda c: None, min_interval_s=0.0)
    seen: list[str] = []

    def handler(request: httpx.Request) -> httpx.Response:
        seen.append(json.loads(request.content)["model"])
        return httpx.Response(200, json={"content": [], "usage": {}})

    app = make_gateway_app(
        build_default_gateway(data_dir=tmp_path, journalling=False),
        upstream_base_url="http://up",
        client=httpx.AsyncClient(transport=httpx.MockTransport(handler)),
        reloader=reloader,
    )
    transport = httpx.ASGITransport(app=app)
    async with httpx.AsyncClient(transport=transport, base_url="http://gw") as c:
        resp = await c.post(
            "/v1/messages",
            content=_turn_body(),
            headers={"content-type": "application/json"},
        )
    assert resp.status_code == 200
    assert seen == ["claude-haiku-4-5"], "a failed reload must not stop metering or routing"


def test_serve_installs_a_sighup_handler_that_reloads(tmp_path, monkeypatch):
    """uvicorn installs its own handlers, so 'we registered one that never
    fires' is the silent failure here. Record the registration, then fire the
    handler and watch the reloader rebuild."""
    import opendaisugi.gateway_asgi as asgi

    registered = {}
    apps = []

    monkeypatch.setattr(signal, "signal", lambda sig, handler: registered.__setitem__(sig, handler))
    monkeypatch.setattr("uvicorn.run", lambda app, **k: apps.append(app))
    built = []
    real_build = asgi.build_default_gateway

    def _spy(**kwargs):
        built.append(kwargs.get("local_model"))
        return real_build(**kwargs)

    monkeypatch.setattr(asgi, "build_default_gateway", _spy)

    _write(tmp_path / "config.yaml", gateway_local_model=None)
    asgi.serve_gateway(data_dir=tmp_path, port=0, openai_cheap_model=None)
    assert signal.SIGHUP in registered
    assert callable(registered[signal.SIGHUP])
    assert apps, "uvicorn was not handed the app"

    _write(tmp_path / "config.yaml", gateway_local_model="qwen2.5-coder-7b")
    registered[signal.SIGHUP](signal.SIGHUP, None)
    assert built[-1] == "qwen2.5-coder-7b", "SIGHUP did not rebuild from the new config"


def test_serve_lets_the_launch_flag_outrank_the_file(tmp_path, monkeypatch):
    import opendaisugi.gateway_asgi as asgi

    registered = {}
    monkeypatch.setattr(signal, "signal", lambda sig, handler: registered.__setitem__(sig, handler))
    monkeypatch.setattr("uvicorn.run", lambda app, **k: None)
    built = []
    real_build = asgi.build_default_gateway

    def _spy(**kwargs):
        built.append(kwargs.get("local_model"))
        return real_build(**kwargs)

    monkeypatch.setattr(asgi, "build_default_gateway", _spy)
    _write(tmp_path / "config.yaml", gateway_local_model="from-file")
    asgi.serve_gateway(data_dir=tmp_path, port=0, local_model="from-flag", openai_cheap_model=None)
    assert built[-1] == "from-flag"
    registered[signal.SIGHUP](signal.SIGHUP, None)
    assert built[-1] == "from-flag", "a reload let the file outrank the launch flag"


def test_a_broken_config_is_parsed_once_until_it_changes(tmp_path, caplog):
    """A YAML typo left in place must not write one warning per interval."""
    import logging

    path = tmp_path / "config.yaml"
    _write(path, gateway_local_model="one")
    r = ConfigReloader(path, lambda c: c.gateway_local_model, min_interval_s=0.0)
    assert r.current() == "one"
    path.write_text("{ this: is: not: yaml\n")
    with caplog.at_level(logging.WARNING, logger="opendaisugi.gateway_asgi"):
        for _ in range(5):
            assert r.current() == "one"
    assert len([rec for rec in caplog.records if "reload failed" in rec.message]) == 1
    assert r.reload() is False, "an explicit reload retries the same broken file"
    path.write_text(yaml.safe_dump({"gateway_local_model": "two"}))
    assert r.current() == "two", "a fixed file must be picked up again"


def test_serve_keeps_a_supplied_gateway_and_installs_no_reloader(tmp_path, monkeypatch):
    """A caller that hands in a built Gateway owns it. Config cannot rebuild
    what it did not build, so no reloader and no SIGHUP handler."""
    import opendaisugi.gateway_asgi as asgi

    registered = {}
    apps = []
    monkeypatch.setattr(signal, "signal", lambda sig, handler: registered.__setitem__(sig, handler))
    monkeypatch.setattr("uvicorn.run", lambda app, **k: apps.append(app))
    built = []
    real_build = asgi.build_default_gateway

    def _spy(**kwargs):
        built.append(kwargs)
        return real_build(**kwargs)

    monkeypatch.setattr(asgi, "build_default_gateway", _spy)
    _write(tmp_path / "config.yaml", gateway_local_model="from-file")
    own = real_build(data_dir=tmp_path, journalling=False)
    asgi.serve_gateway(data_dir=tmp_path, port=0, gateway=own, openai_cheap_model=None)
    assert signal.SIGHUP not in registered
    assert built == [], "a supplied gateway was rebuilt from config"
    sent = _call(apps[0], _scope("/_reload", ("127.0.0.1", 1)))
    assert sent[0]["status"] == 404
