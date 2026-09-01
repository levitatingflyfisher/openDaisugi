"""The config fields that hold a Switchyard router choice, and the adapters that
turn config or a TOML file into the two target ids the gateway meters by."""

from __future__ import annotations

import pytest

from opendaisugi.config import default_config, load_config, save_config
from opendaisugi.router_switchyard import (
    SwitchyardConfigError,
    render_switchyard_toml,
    route_targets_from_toml,
    targets_from_config,
)


def test_a_key_is_used_only_when_config_names_its_variable():
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "claude-haiku-4-5",
            "switchyard_api_key_env": "MY_ANTHROPIC_KEY",
        }
    )
    assert targets_from_config(cfg).api_key_env == "MY_ANTHROPIC_KEY"


def test_config_defaults_to_the_rules_router():
    cfg = default_config()
    assert cfg.switchyard_api_key_env is None
    assert cfg.gateway_router == "rules"
    assert cfg.switchyard_route_id == "daisugi"
    assert cfg.switchyard_capable_model == "claude-sonnet-5"
    assert cfg.switchyard_efficient_model is None


def test_config_round_trips_switchyard_fields(tmp_path):
    cfg = default_config().model_copy(
        update={
            "gateway_router": "switchyard",
            "switchyard_efficient_model": "qwen3-coder:30b",
        }
    )
    path = tmp_path / "config.yaml"
    save_config(cfg, path)
    loaded = load_config(path)
    assert loaded.gateway_router == "switchyard"
    assert loaded.switchyard_efficient_model == "qwen3-coder:30b"


def test_targets_from_config_is_none_without_an_efficient_model():
    assert targets_from_config(default_config()) is None


def test_a_local_efficient_model_defaults_to_ollama_on_loopback():
    cfg = default_config().model_copy(update={"switchyard_efficient_model": "qwen3-coder:30b"})
    targets = targets_from_config(cfg)
    assert targets is not None
    assert targets.efficient_client_format == "openai_chat"
    assert targets.efficient_base_url == "http://127.0.0.1:11434/v1"
    assert targets.efficient_local is True
    assert targets.capable_id == "claude-sonnet-5"
    assert targets.api_key_env is None


def test_a_claude_efficient_model_is_a_cloud_client():
    cfg = default_config().model_copy(update={"switchyard_efficient_model": "claude-haiku-4-5"})
    targets = targets_from_config(cfg)
    assert targets is not None
    assert targets.efficient_client_format == "anthropic_messages"
    assert targets.efficient_base_url == "https://api.anthropic.com"
    assert targets.efficient_local is False
    assert targets.api_key_env is None


def test_a_recorded_ollama_host_gets_the_v1_root():
    # model_host records a bare http://host:port. Switchyard's openai_chat
    # client only appends /chat/completions, so the /v1 root must be added.
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "qwen3-coder:30b",
            "llm_base_url": "http://192.168.1.50:11434",
            "llm_host_kind": "ollama",
        }
    )
    targets = targets_from_config(cfg)
    assert targets.efficient_client_format == "openai_chat"
    assert targets.efficient_base_url == "http://192.168.1.50:11434/v1"


def test_a_recorded_openai_host_keeps_an_existing_v1_root():
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "qwen3-coder",
            "llm_base_url": "http://10.0.0.2:8080/v1/",
            "llm_host_kind": "openai",
        }
    )
    targets = targets_from_config(cfg)
    assert targets.efficient_base_url == "http://10.0.0.2:8080/v1"


def test_a_recorded_anthropic_host_speaks_anthropic_messages():
    cfg = default_config().model_copy(
        update={
            "switchyard_efficient_model": "qwen3-coder",
            "llm_base_url": "http://10.0.0.2:11434",
            "llm_host_kind": "anthropic",
        }
    )
    targets = targets_from_config(cfg)
    assert targets.efficient_client_format == "anthropic_messages"
    assert targets.efficient_base_url == "http://10.0.0.2:11434"
    assert targets.efficient_local is True


# --- the target pair read back from a TOML file -------------------------------


def _rendered(tmp_path, **kwargs):
    cfg = default_config().model_copy(update={"switchyard_efficient_model": "qwen3-coder:30b"})
    path = tmp_path / "switchyard.toml"
    path.write_text(
        render_switchyard_toml(targets_from_config(cfg), api_key_present=lambda n: False, **kwargs)
    )
    return path


def test_route_targets_from_a_rendered_file(tmp_path):
    path = _rendered(tmp_path)
    assert route_targets_from_toml(path, "daisugi") == ("claude-sonnet-5", "qwen3-coder:30b")


def test_route_targets_from_an_llm_classifier_route(tmp_path):
    path = tmp_path / "own.toml"
    path.write_text(
        "schema_version = 1\n"
        "[targets.strong]\nid = 'big'\nllm_client = 'c'\n"
        "[targets.weak]\nid = 'small'\nllm_client = 'c'\n"
        "[routes.r]\nid = 'mine'\ntype = 'llm_classifier'\nmode = 'escalation'\n"
        "strong_target = 'strong'\nweak_target = 'weak'\n"
    )
    assert route_targets_from_toml(path, "mine") == ("big", "small")


@pytest.mark.parametrize(
    ("text", "match"),
    [
        ("schema_version = 1\n[targets]\n[routes]\n", "no route"),
        (
            "schema_version = 1\n[targets]\n"
            "[routes.r]\nid = 'daisugi'\ntype = 'passthrough'\ntarget = 'x'\n",
            "two tiers",
        ),
        (
            "schema_version = 1\n[targets.a]\nid = 'm'\nllm_client = 'c'\n"
            "[routes.r]\nid = 'daisugi'\ntype = 'stage_router'\n"
            "capable_target = 'a'\nefficient_target = 'missing'\n",
            "missing",
        ),
        ("not = [valid toml", "cannot read"),
    ],
)
def test_route_targets_refuses_what_it_cannot_meter(tmp_path, text, match):
    path = tmp_path / "bad.toml"
    path.write_text(text)
    with pytest.raises(SwitchyardConfigError, match=match):
        route_targets_from_toml(path, "daisugi")


def test_route_targets_refuses_a_missing_file(tmp_path):
    with pytest.raises(SwitchyardConfigError, match="cannot read"):
        route_targets_from_toml(tmp_path / "nope.toml", "daisugi")
