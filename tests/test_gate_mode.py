"""Gate verdict-mode resolution: config is a fallback, never an override.

The installed hook passes --mode explicitly (the one thing the agent cannot
rewrite). config.yaml is user-writable, so it must never be able to flip an
installed `enforce` down to `shadow` — the explicit flag always wins, and config
fills in only when the flag is absent.
"""

from opendaisugi.config import Config, save_config
from opendaisugi.gate import resolve_gate_mode


def test_explicit_flag_always_wins_over_config(tmp_path):
    root = tmp_path / "gate"  # data_dir = root.parent = tmp_path
    save_config(Config(gate_mode="enforce"), tmp_path / "config.yaml")
    # config says enforce, but an explicit shadow flag must still win (no upgrade)
    assert resolve_gate_mode("shadow", root=root) == "shadow"
    save_config(Config(gate_mode="shadow"), tmp_path / "config.yaml")
    # and an explicit enforce flag wins over a shadow config
    assert resolve_gate_mode("enforce", root=root) == "enforce"


def test_config_is_the_fallback_when_flag_absent(tmp_path):
    root = tmp_path / "gate"
    save_config(Config(gate_mode="enforce"), tmp_path / "config.yaml")
    assert resolve_gate_mode(None, root=root) == "enforce"


def test_default_is_shadow_when_no_flag_and_no_config(tmp_path):
    assert resolve_gate_mode(None, root=tmp_path / "gate") == "shadow"


def test_garbage_config_mode_falls_back_to_shadow(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: banana\n")
    assert resolve_gate_mode(None, root=tmp_path / "gate") == "shadow"
