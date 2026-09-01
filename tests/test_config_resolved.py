# tests/test_config_resolved.py
"""`daisugi config`: every setting, where it came from, nothing hidden."""

from __future__ import annotations

import json

from opendaisugi.config import (
    installed_hook_mode,
    resolved_config,
    unknown_config_keys,
)


def _by_key(fields):
    return {f.key: f for f in fields}


def test_file_values_are_marked_file_and_others_default(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: enforce\n")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert got["gate_mode"].value == "enforce" and got["gate_mode"].source == "file"
    assert got["z3_timeout_ms"].source == "default"


def test_missing_file_is_all_defaults(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert all(f.source == "default" for f in got.values() if "(resolved)" not in f.key)


def test_backend_source_env_vs_auto(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert got["llm_backend (resolved)"].source == "auto"
    got = _by_key(
        resolved_config(
            tmp_path / "config.yaml",
            home=tmp_path,
            cwd=tmp_path,
            env={"OPENDAISUGI_LLM_BACKEND": "litellm"},
        )
    )
    assert got["llm_backend (resolved)"].source == "env"
    assert got["llm_backend (resolved)"].value == "litellm"


def test_installed_hook_mode_reads_claude_settings(tmp_path):
    settings = tmp_path / ".claude" / "settings.json"
    settings.parent.mkdir()
    settings.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "matcher": "*",
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": "/py -m opendaisugi.gate --mode enforce --root /r",
                                }
                            ],
                        }
                    ]
                }
            }
        )
    )
    assert installed_hook_mode(settings) == "enforce"
    assert installed_hook_mode(tmp_path / "nope.json") is None
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert got["gate_mode (resolved)"].value == "enforce"
    assert got["gate_mode (resolved)"].source == "global"


def test_gate_mode_falls_back_to_file_then_default(tmp_path):
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert got["gate_mode (resolved)"].source == "default"
    (tmp_path / "config.yaml").write_text("gate_mode: enforce\n")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, cwd=tmp_path, env={}))
    assert got["gate_mode (resolved)"].source == "file"


def _write_hook(settings_path, mode):
    settings_path.parent.mkdir(parents=True, exist_ok=True)
    settings_path.write_text(
        json.dumps(
            {
                "hooks": {
                    "PreToolUse": [
                        {
                            "hooks": [
                                {
                                    "type": "command",
                                    "command": f"py -m opendaisugi.gate_client --mode {mode} --root /r",
                                }
                            ]
                        }
                    ]
                }
            }
        )
    )


def test_cwd_only_hook_reads_as_a_directory_hook_not_default(tmp_path):
    """A `daisugi start --enforce` hook with no global hook installed must not
    under-report as `default` — that would hide an active enforce gate."""
    home = tmp_path / "home"
    cwd = tmp_path / "proj"
    _write_hook(cwd / ".claude" / "settings.json", "enforce")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=home, cwd=cwd, env={}))
    assert got["gate_mode (resolved)"].value == "enforce"
    assert got["gate_mode (resolved)"].source == "project"


def test_global_only_hook_reads_as_global(tmp_path):
    home = tmp_path / "home"
    cwd = tmp_path / "proj"
    _write_hook(home / ".claude" / "settings.json", "shadow")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=home, cwd=cwd, env={}))
    assert got["gate_mode (resolved)"].value == "shadow"
    assert got["gate_mode (resolved)"].source == "global"


def test_both_hooks_present_reports_the_stricter_and_lists_both(tmp_path):
    home = tmp_path / "home"
    cwd = tmp_path / "proj"
    _write_hook(home / ".claude" / "settings.json", "shadow")
    _write_hook(cwd / ".claude" / "settings.json", "enforce")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=home, cwd=cwd, env={}))
    # effective = the stricter of the two, never just one of them
    assert got["gate_mode (resolved)"].value == "enforce"
    assert got["gate_mode (resolved)"].source == "project+global"
    # each hook is still named individually
    assert got["gate_mode (project)"].value == "enforce"
    assert got["gate_mode (global)"].value == "shadow"
    # and how they combine is spelled out, not left implicit
    assert "intersection" in got["gate_mode (coexistence)"].value


def test_a_global_shadow_hook_cannot_soften_a_cwd_enforce_hook(tmp_path):
    """The opposite ordering — enforce must still win when it's the global one
    and the directory one is only shadow."""
    home = tmp_path / "home"
    cwd = tmp_path / "proj"
    _write_hook(home / ".claude" / "settings.json", "enforce")
    _write_hook(cwd / ".claude" / "settings.json", "shadow")
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=home, cwd=cwd, env={}))
    assert got["gate_mode (resolved)"].value == "enforce"


def test_a_deleted_cwd_falls_back_instead_of_crashing(tmp_path, monkeypatch):
    """Path.cwd() raises OSError if the launch directory was deleted out from
    under a still-running process — resolved_config must not crash over it."""

    def _raise():
        raise FileNotFoundError("cwd deleted")

    monkeypatch.setattr("pathlib.Path.cwd", _raise)
    got = _by_key(resolved_config(tmp_path / "config.yaml", home=tmp_path, env={}))
    assert got["gate_mode (resolved)"].source == "default"


def test_unknown_keys_are_reported_not_hidden(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: shadow\nbanana: 1\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == ["banana"]


def test_unknown_keys_recurse_into_a_nested_group(tmp_path):
    (tmp_path / "config.yaml").write_text("floor:\n  backnd: tmux\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == ["floor.backnd"]


def test_a_known_nested_key_is_not_reported(tmp_path):
    (tmp_path / "config.yaml").write_text("floor:\n  backend: tmux\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == []


def test_unknown_keys_combine_top_level_and_nested(tmp_path):
    (tmp_path / "config.yaml").write_text("banana: 1\nfloor:\n  backnd: tmux\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == ["banana", "floor.backnd"]


def test_a_nested_group_that_is_not_a_mapping_never_crashes(tmp_path):
    (tmp_path / "config.yaml").write_text("floor: not-a-mapping\n")
    assert unknown_config_keys(tmp_path / "config.yaml") == []
