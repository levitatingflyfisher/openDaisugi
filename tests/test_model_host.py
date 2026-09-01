"""Tests for opendaisugi.model_host: the probe of a self-hosted model server.

The fixtures under tests/fixtures/model_host/ are real upstream response
shapes. SOURCES.txt lists the URL and the fetch date for each one. probe()
must parse exactly these shapes, not an invented one.
"""

from __future__ import annotations

import json
import os
from pathlib import Path

import httpx
import pytest

from opendaisugi.config import load_config
from opendaisugi.exceptions import ModelHostUnknownError
from opendaisugi.model_host import (
    CONTEXT_FLOOR,
    HostInfo,
    describe_host,
    harness_env,
    probe,
    record,
)

FIXTURES = Path(__file__).parent / "fixtures" / "model_host"


def _load(name: str) -> dict:
    return json.loads((FIXTURES / name).read_text())


def test_ollama_tags_fixture_has_a_named_model():
    tags = _load("ollama_tags.json")
    assert tags["models"][0]["name"] == "deepseek-r1:latest"
    assert tags["models"][1]["name"] == "llama3.2:latest"


def test_ollama_show_fixture_has_a_context_length_and_a_num_ctx_override():
    show = _load("ollama_show.json")
    assert show["model_info"]["llama.context_length"] == 8192
    assert "PARAMETER num_ctx 4096" in show["modelfile"]


def test_openai_models_fixture_has_an_id():
    models = _load("openai_models.json")
    assert models["object"] == "list"
    assert models["data"][0]["id"] == "qwen3-coder:latest"


def test_anthropic_message_fixture_has_usage():
    msg = _load("anthropic_message.json")
    assert msg["type"] == "message"
    assert msg["usage"] == {"input_tokens": 10, "output_tokens": 20}


def _mock_client(handler) -> httpx.Client:
    return httpx.Client(transport=httpx.MockTransport(handler))


def test_probe_detects_ollama_and_the_num_ctx_override_beats_context_length():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        if request.url.path == "/api/show":
            assert json.loads(request.content) == {"model": "deepseek-r1:latest"}
            return httpx.Response(200, json=show)
        return httpx.Response(404)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.kind == "ollama"
    assert info.chosen == "deepseek-r1:latest"
    assert info.models == ["deepseek-r1:latest", "llama3.2:latest"]
    # The num_ctx override wins over context_length 8192.
    assert info.context_window == 4096
    assert any("32K floor" in w for w in info.warnings)
    assert info.latency_ms >= 0


def test_probe_ollama_falls_back_to_context_length_without_a_num_ctx_override():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")
    show = dict(show, modelfile=show["modelfile"].replace("PARAMETER num_ctx 4096\n", ""))

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        return httpx.Response(200, json=show)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.context_window == 8192


def test_probe_no_floor_warning_when_context_is_wide_enough():
    tags = _load("ollama_tags.json")
    show = _load("ollama_show.json")
    show = dict(show, modelfile=show["modelfile"].replace("PARAMETER num_ctx 4096\n", ""))
    show = {**show, "model_info": {**show["model_info"], "llama.context_length": 65536}}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json=tags)
        return httpx.Response(200, json=show)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.context_window == 65536
    assert info.warnings == []


def test_probe_ollama_with_no_models_pulled_yet_is_still_identified():
    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(200, json={"models": []})
        return httpx.Response(404)

    info = probe("box", 11434, client=_mock_client(handler))
    assert info.kind == "ollama"
    assert info.models == []
    assert info.chosen is None


def test_probe_falls_through_to_openai_compatible():
    models = _load("openai_models.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/api/tags":
            return httpx.Response(404)
        if request.url.path == "/v1/models":
            return httpx.Response(200, json=models)
        return httpx.Response(404)

    info = probe("box", 8080, client=_mock_client(handler))
    assert info.kind == "openai"
    assert info.chosen == "qwen3-coder:latest"
    assert info.context_window is None
    assert any("context window unknown" in w for w in info.warnings)


def test_probe_falls_through_to_anthropic_compatible():
    msg = _load("anthropic_message.json")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path in ("/api/tags", "/v1/models"):
            return httpx.Response(404)
        if request.url.path == "/v1/messages":
            return httpx.Response(200, json=msg)
        return httpx.Response(404)

    info = probe("box", 4000, client=_mock_client(handler))
    assert info.kind == "anthropic"
    assert info.chosen == "qwen3-coder"


def test_probe_reports_unknown_with_a_teaching_warning_when_nothing_answers():
    def handler(request: httpx.Request) -> httpx.Response:
        return httpx.Response(404)

    info = probe("box", 9999, client=_mock_client(handler))
    assert info.kind == "unknown"
    assert info.models == []
    assert "box:9999" in info.warnings[0]
    # The host answered, on no known wire.
    assert info.reachable is True


def test_probe_reports_unreachable_when_no_wire_gets_any_response():
    def handler(request: httpx.Request) -> httpx.Response:
        raise httpx.ConnectError("connection refused")

    info = probe("box", 9999, client=_mock_client(handler))
    assert info.kind == "unknown"
    assert info.reachable is False
    assert "could not reach" in info.warnings[0]
    assert "box:9999" in info.warnings[0]


def test_probe_anthropic_records_no_model_when_the_response_omits_it():
    msg = {k: v for k, v in _load("anthropic_message.json").items() if k != "model"}

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/messages":
            return httpx.Response(200, json=msg)
        return httpx.Response(404)

    info = probe("box", 4000, client=_mock_client(handler))
    assert info.kind == "anthropic"
    assert info.models == []
    assert info.chosen is None


def test_probe_anthropic_records_no_model_when_the_server_echoes_the_probe_name():
    msg = dict(_load("anthropic_message.json"), model="daisugi-probe")

    def handler(request: httpx.Request) -> httpx.Response:
        if request.url.path == "/v1/messages":
            return httpx.Response(200, json=msg)
        return httpx.Response(404)

    info = probe("box", 4000, client=_mock_client(handler))
    assert info.kind == "anthropic"
    assert info.chosen is None


def test_probe_explicit_kind_skips_the_cascade():
    calls = []

    def handler(request: httpx.Request) -> httpx.Response:
        calls.append(request.url.path)
        return httpx.Response(200, json=_load("openai_models.json"))

    probe("box", 8080, kind="openai", client=_mock_client(handler))
    # The probe never touched /api/tags.
    assert calls == ["/v1/models"]


def test_probe_rejects_an_unsupported_kind():
    with pytest.raises(ValueError, match="unknown host kind"):
        probe("box", 11434, kind="bogus")


def test_context_floor_warning_thresholds():
    from opendaisugi.model_host import context_floor_warning

    assert context_floor_warning(None) is not None
    assert context_floor_warning(CONTEXT_FLOOR - 1) is not None
    assert context_floor_warning(CONTEXT_FLOOR) is None


def test_parse_remote_splits_host_and_port():
    from opendaisugi.model_host import parse_remote

    assert parse_remote("box:11434") == ("box", 11434)
    assert parse_remote("box") == ("box", None)
    assert parse_remote("gpu-box.tailnet.ts.net:8080") == ("gpu-box.tailnet.ts.net", 8080)


def test_parse_remote_rejects_a_scheme_with_a_teaching_error():
    from opendaisugi.model_host import parse_remote

    with pytest.raises(ValueError, match="without a scheme"):
        parse_remote("http://box:11434")


_LIVE_HOST = os.environ.get("OPENDAISUGI_TEST_MODEL_HOST")


@pytest.mark.skipif(
    not _LIVE_HOST, reason="requires OPENDAISUGI_TEST_MODEL_HOST=host[:port] (a real model host)"
)
def test_probe_live_host_has_at_least_one_model():
    from opendaisugi.model_host import parse_remote

    host, port = parse_remote(_LIVE_HOST)
    info = probe(host, port)
    assert len(info.models) >= 1


def test_record_writes_config_and_maps_kind_to_backend(tmp_path):
    info = HostInfo(
        base_url="http://box:11434",
        kind="ollama",
        models=["qwen3-coder:latest"],
        chosen="qwen3-coder:latest",
        context_window=4096,
        latency_ms=12.0,
        warnings=[],
    )
    path = tmp_path / "config.yaml"
    cfg = record(info, config_path=path)
    assert cfg.llm_base_url == "http://box:11434"
    assert cfg.llm_host_kind == "ollama"
    assert cfg.llm_host_model == "qwen3-coder:latest"
    assert cfg.llm_context_window == 4096
    assert cfg.llm_backend == "ollama"
    reloaded = load_config(path)
    assert reloaded.llm_base_url == "http://box:11434"


def test_record_model_and_context_overrides_beat_the_probe(tmp_path):
    info = HostInfo(
        base_url="http://box:8080",
        kind="openai",
        models=["a", "b"],
        chosen="a",
        context_window=None,
        latency_ms=5.0,
        warnings=["context window unknown; set it with `--context 32768` if you know it"],
    )
    cfg = record(info, model="b", context_window=32768, config_path=tmp_path / "config.yaml")
    assert cfg.llm_host_model == "b"
    assert cfg.llm_context_window == 32768
    assert cfg.llm_backend == "openai-compatible"


def test_record_refuses_an_unknown_host(tmp_path):
    path = tmp_path / "config.yaml"
    info = HostInfo(
        base_url="http://box:9999",
        kind="unknown",
        models=[],
        chosen=None,
        context_window=None,
        latency_ms=1.0,
        warnings=["could not identify a model server at http://box:9999"],
    )
    with pytest.raises(ModelHostUnknownError):
        record(info, config_path=path)
    assert not path.exists()


def test_record_refuses_a_kind_outside_the_known_wires(tmp_path):
    path = tmp_path / "config.yaml"
    info = HostInfo("http://box:9999", "bogus", [], None, None, 1.0, [])
    with pytest.raises(ModelHostUnknownError, match="bogus"):
        record(info, config_path=path)
    assert not path.exists()


def test_record_writes_no_model_name_when_the_probe_observed_none(tmp_path):
    info = HostInfo("http://box:4000", "anthropic", [], None, None, 1.0, [])
    cfg = record(info, config_path=tmp_path / "config.yaml")
    assert cfg.llm_host_model is None
    assert "model: unset" in "\n".join(describe_host(info, cfg))


def test_harness_env_for_ollama_sets_the_documented_auth_token():
    info = HostInfo("http://box:11434", "ollama", ["m"], "m", 4096, 1.0, [])
    assert harness_env(info) == {
        "ANTHROPIC_BASE_URL": "http://box:11434",
        "ANTHROPIC_AUTH_TOKEN": "ollama",
    }


def test_harness_env_for_anthropic_compatible_has_no_extra_token():
    info = HostInfo("http://box:4000", "anthropic", ["m"], "m", None, 1.0, [])
    assert harness_env(info) == {"ANTHROPIC_BASE_URL": "http://box:4000"}


def test_harness_env_is_empty_for_openai_only_hosts():
    info = HostInfo("http://box:8080", "openai", ["m"], "m", None, 1.0, [])
    assert harness_env(info) == {}


def test_describe_host_lines_reflect_the_recorded_config(tmp_path):
    info = HostInfo(
        "http://box:11434", "ollama", ["qwen3-coder:latest"], "qwen3-coder:latest", 4096, 9.0, []
    )
    cfg = record(info, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "host: http://box:11434 (ollama)" in joined
    assert "model: qwen3-coder:latest" in joined
    assert "context window: 4096 tokens" in joined
    # 4096 is below the 32K floor.
    assert "warning:" in joined
    assert "ANTHROPIC_BASE_URL=http://box:11434" in joined
    assert "ANTHROPIC_AUTH_TOKEN=ollama" in joined
    assert "daisugi gateway --upstream http://box:11434" in joined


def test_describe_host_warning_is_fresh_after_a_context_override(tmp_path):
    info = HostInfo(
        "http://box:8080",
        "openai",
        ["m"],
        "m",
        None,
        3.0,
        warnings=["context window unknown; set it with `--context 32768` if you know it"],
    )
    cfg = record(info, context_window=65536, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "context window: 65536 tokens" in joined
    # The override cleared the stale probe time warning.
    assert "warning:" not in joined


def test_describe_host_teaches_the_gateway_route_for_openai_only_hosts(tmp_path):
    info = HostInfo("http://box:8080", "openai", ["m"], "m", 65536, 3.0, [])
    cfg = record(info, config_path=tmp_path / "config.yaml")
    joined = "\n".join(describe_host(info, cfg))
    assert "OpenAI wire only" in joined
