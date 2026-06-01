"""Tests for opendaisugi.config — Config model + load/save helpers."""

from pathlib import Path

import yaml

from opendaisugi.config import Config, default_config, load_config, save_config


def test_default_config_has_expected_defaults():
    cfg = default_config()
    assert cfg.model == "anthropic/claude-sonnet-4-20250514"
    assert cfg.max_task_chars == 4000
    assert cfg.z3_timeout_ms == 500
    assert cfg.data_dir == Path.home() / ".opendaisugi"


def test_config_accepts_overrides():
    cfg = Config(model="openai/gpt-4o", max_task_chars=2000)
    assert cfg.model == "openai/gpt-4o"
    assert cfg.max_task_chars == 2000
    # Unspecified fields keep defaults
    assert cfg.z3_timeout_ms == 500


def test_load_config_missing_file_returns_defaults(tmp_path):
    missing = tmp_path / "config.yaml"
    cfg = load_config(missing)
    assert cfg.model == "anthropic/claude-sonnet-4-20250514"
    assert cfg.max_task_chars == 4000


def test_load_config_reads_existing_yaml(tmp_path):
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump({
        "model": "openai/gpt-4o-mini",
        "max_task_chars": 1500,
        "z3_timeout_ms": 250,
    }))
    cfg = load_config(path)
    assert cfg.model == "openai/gpt-4o-mini"
    assert cfg.max_task_chars == 1500
    assert cfg.z3_timeout_ms == 250


def test_load_config_ignores_unknown_keys(tmp_path):
    # Unknown keys must not crash — forward compat with v0.1 fields.
    path = tmp_path / "config.yaml"
    path.write_text(yaml.safe_dump({
        "model": "anthropic/claude-sonnet-4-20250514",
        "future_v01_field": "ignore_me",
    }))
    cfg = load_config(path)
    assert cfg.model == "anthropic/claude-sonnet-4-20250514"


def test_save_config_creates_parent_directory(tmp_path):
    path = tmp_path / "nested" / "dir" / "config.yaml"
    cfg = Config(model="anthropic/claude-haiku-4-5-20251001")
    save_config(cfg, path)
    assert path.exists()
    loaded = yaml.safe_load(path.read_text())
    assert loaded["model"] == "anthropic/claude-haiku-4-5-20251001"


def test_save_then_load_is_stable(tmp_path):
    path = tmp_path / "config.yaml"
    original = Config(model="openai/gpt-4o", max_task_chars=3000, z3_timeout_ms=750)
    save_config(original, path)
    loaded = load_config(path)
    assert loaded.model == original.model
    assert loaded.max_task_chars == original.max_task_chars
    assert loaded.z3_timeout_ms == original.z3_timeout_ms


def test_config_ignores_retired_runtime_supervision_fields(tmp_path):
    """v0.1.0 shipped four aspirational supervisor-config fields (step_timeout_s,
    execution_timeout_s, approval_policy, max_output_bytes) that were never
    wired through the Daisugi facade to the Supervisor. They were removed in
    v0.9.0. This test pins the forward-compat guarantee: a config.yaml written
    by v0.1–v0.8 still loads, thanks to load_config's unknown-key filtering."""
    path = tmp_path / "legacy.yaml"
    path.write_text(yaml.safe_dump({
        "model": "anthropic/claude-sonnet-4-20250514",
        "max_task_chars": 4000,
        "z3_timeout_ms": 500,
        "data_dir": str(Path.home() / ".opendaisugi"),
        "step_timeout_s": 60,
        "execution_timeout_s": 1200,
        "approval_policy": "allowlist+env",
        "max_output_bytes": 5 * 1024 * 1024,
    }))
    loaded = load_config(path)
    assert loaded.model == "anthropic/claude-sonnet-4-20250514"
    assert loaded.max_task_chars == 4000
