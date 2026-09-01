"""Gate verdict-mode resolution: config is a fallback, never an override.

The installed hook passes --mode explicitly (the one thing the agent cannot
rewrite). config.yaml is user-writable, so it must never be able to flip an
installed `enforce` down to `audit` — the explicit flag always wins, and config
fills in only when the flag is absent.
"""

from opendaisugi.config import Config, save_config
from opendaisugi.gate import resolve_gate_mode


def test_explicit_flag_always_wins_over_config(tmp_path):
    root = tmp_path / "gate"  # data_dir = root.parent = tmp_path
    save_config(Config(gate_mode="enforce"), tmp_path / "config.yaml")
    # config says enforce, but an explicit audit flag must still win (no upgrade)
    assert resolve_gate_mode("audit", root=root) == "audit"
    save_config(Config(gate_mode="audit"), tmp_path / "config.yaml")
    # and an explicit enforce flag wins over a audit config
    assert resolve_gate_mode("enforce", root=root) == "enforce"


def test_config_is_the_fallback_when_flag_absent(tmp_path):
    root = tmp_path / "gate"
    save_config(Config(gate_mode="enforce"), tmp_path / "config.yaml")
    assert resolve_gate_mode(None, root=root) == "enforce"


def test_default_is_audit_when_no_flag_and_no_config(tmp_path):
    assert resolve_gate_mode(None, root=tmp_path / "gate") == "audit"


def test_garbage_config_mode_falls_back_to_audit(tmp_path):
    (tmp_path / "config.yaml").write_text("gate_mode: banana\n")
    assert resolve_gate_mode(None, root=tmp_path / "gate") == "audit"


def test_the_old_mode_word_is_refused_with_the_new_one(capsys):
    """Owner ruling: shadow mode is audit mode, a clean break. The old
    word gets one line that names the new one, in every argv form."""
    import pytest

    from opendaisugi.gate import _build_parser

    for argv in (["--mode", "shadow"], ["--mode=shadow"], ["--mo", "shadow"]):
        with pytest.raises(SystemExit) as exc:
            _build_parser().parse_args(argv)
        assert exc.value.code == 2
        err = capsys.readouterr().err
        assert "argument --mode: 'shadow' is now 'audit': use --mode audit" in err


def test_the_report_reads_the_log_written_before_the_rename(tmp_path):
    """<root>/shadow holds the operator's history from before the rename:
    the report reads it first, then <root>/audit."""
    import json

    from opendaisugi.gate import audit_report

    old = {"session_id": "s1", "tool_name": "Bash", "allow": True, "would_deny": True}
    new = {"session_id": "s1", "tool_name": "Read", "allow": True, "would_deny": False}
    for d, rec in (("shadow", old), ("audit", new)):
        (tmp_path / d).mkdir()
        (tmp_path / d / "s1.jsonl").write_text(json.dumps(rec) + "\n")
    for sid in ("s1", None):
        rep = audit_report(root=tmp_path, session_id=sid)
        assert rep["calls"] == 2
        assert rep["would_deny"] == 1


def test_a_hook_that_passes_the_old_mode_word_is_named(tmp_path):
    """A --mode shadow hook denies every call now: status counts it as a
    hook that may enforce, and says how to fix it."""
    import json

    from opendaisugi.config import installed_hook_mode, stale_mode_hooks, stale_mode_warning

    s = tmp_path / "settings.json"
    cmd = "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode shadow --root /x"
    s.write_text(
        json.dumps({"hooks": {"PreToolUse": [{"hooks": [{"type": "command", "command": cmd}]}]}})
    )
    assert stale_mode_hooks(s)
    assert installed_hook_mode(s) == "unknown"
    assert "--mode shadow" in stale_mode_warning(s)
