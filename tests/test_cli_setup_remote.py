"""CLI: ``daisugi tiers setup --remote`` probes a host and records what it found."""

from __future__ import annotations

import json
from pathlib import Path

import httpx
from typer.testing import CliRunner

import opendaisugi.model_host as model_host
from opendaisugi.cli import app
from opendaisugi.config import load_config

runner = CliRunner()
FIXTURES = Path(__file__).parent / "fixtures" / "model_host"


def _load(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def _patched_probe(handler):
    """Return a probe() that runs the real code against a mocked transport."""
    real_probe = model_host.probe

    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        return real_probe(
            host,
            port,
            kind=kind,
            timeout_s=timeout_s,
            client=httpx.Client(transport=httpx.MockTransport(handler)),
        )

    return fake_probe


def test_setup_remote_ollama_end_to_end(tmp_path, monkeypatch):
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    monkeypatch.setattr(model_host, "probe", _patched_probe(handler))

    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:11434"]
    )
    assert res.exit_code == 0, res.output
    assert "host: http://box:11434 (ollama)" in res.output
    assert "ANTHROPIC_AUTH_TOKEN=ollama" in res.output

    cfg = load_config(tmp_path / "config.yaml")
    assert cfg.llm_host_kind == "ollama"
    assert cfg.llm_host_model == "deepseek-r1:latest"


def test_setup_remote_model_and_context_flags_override_the_probe(tmp_path, monkeypatch):
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    monkeypatch.setattr(model_host, "probe", _patched_probe(handler))

    res = runner.invoke(
        app,
        [
            "tiers",
            "setup",
            "--data-dir",
            str(tmp_path),
            "--remote",
            "box:11434",
            "--model",
            "llama3.2:latest",
            "--context",
            "65536",
        ],
    )
    assert res.exit_code == 0, res.output
    cfg = load_config(tmp_path / "config.yaml")
    assert cfg.llm_host_model == "llama3.2:latest"
    assert cfg.llm_context_window == 65536
    assert "warning:" not in res.output


def test_setup_remote_and_endpoint_are_mutually_exclusive(tmp_path):
    res = runner.invoke(
        app,
        [
            "tiers",
            "setup",
            "--data-dir",
            str(tmp_path),
            "--remote",
            "box:11434",
            "--endpoint",
            "http://localhost:8080/v1",
            "--model",
            "m",
        ],
    )
    assert res.exit_code == 1
    assert "mutually exclusive" in res.output


def test_setup_remote_refuses_an_unidentified_host(tmp_path, monkeypatch):
    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        return model_host.HostInfo(
            base_url=f"http://{host}:{port}",
            kind="unknown",
            models=[],
            chosen=None,
            context_window=None,
            latency_ms=1.0,
            warnings=[f"could not identify a model server at http://{host}:{port}"],
        )

    monkeypatch.setattr(model_host, "probe", fake_probe)
    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:9999"]
    )
    assert res.exit_code == 1
    assert "nothing was written" in res.output
    assert not (tmp_path / "config.yaml").exists()


def test_setup_remote_exits_3_when_the_host_is_unreachable(tmp_path, monkeypatch):
    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        return model_host.HostInfo(
            base_url=f"http://{host}:{port}",
            kind="unknown",
            models=[],
            chosen=None,
            context_window=None,
            latency_ms=1.0,
            warnings=[f"could not reach http://{host}:{port}"],
            reachable=False,
        )

    monkeypatch.setattr(model_host, "probe", fake_probe)
    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:9999"]
    )
    assert res.exit_code == 3
    assert "nothing was written" in res.output
    assert not (tmp_path / "config.yaml").exists()


def test_setup_remote_rejects_a_scheme_in_the_host(tmp_path):
    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "http://box:11434"]
    )
    assert res.exit_code == 1
    assert "without a scheme" in res.output
    assert not (tmp_path / "config.yaml").exists()


def test_setup_remote_exits_1_when_httpx_is_missing(tmp_path, monkeypatch):
    def fake_probe(host, port, *, kind="auto", timeout_s=3.0, client=None):
        raise ImportError("the model-host probe needs the [gateway] extra")

    monkeypatch.setattr(model_host, "probe", fake_probe)
    res = runner.invoke(
        app, ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:11434"]
    )
    assert res.exit_code == 1
    assert "opendaisugi[gateway]" in res.output


def test_setup_remote_rejects_a_bad_kind(tmp_path):
    res = runner.invoke(
        app,
        ["tiers", "setup", "--data-dir", str(tmp_path), "--remote", "box:11434", "--kind", "bogus"],
    )
    assert res.exit_code == 1
    assert "unknown host kind" in res.output
