"""Gate hooks are found by their words, and three CLI faults found while
porting the gate commands to Go.

- A foreign hook whose command merely contains "opendaisugi.gate" (say
  `opendaisugi.gateway-watch`) is not ours: install must not skip on it,
  status must not read a mode from it, uninstall must not remove it. A
  `--mode` inside a quoted path is not the hook's mode.
- `install` names a runtime whose apply failed and exits 1; it never says
  "already configured".
- `gate init --session a/b` checks the file it writes (a_b.json).
- `gate report` skips a shadow-log line that is JSON but not an object.
"""

from __future__ import annotations

import json
from pathlib import Path

from typer.testing import CliRunner

from opendaisugi.cli import app
from opendaisugi.config import (
    gate_hook_args,
    gate_hook_kind,
    gate_hook_mode,
    installed_hook_mode,
    is_record_hook,
)

runner = CliRunner()

GO = (
    "DAISUGI_GATE_HOOK=opendaisugi.gate /opt/daisugi gate check --mode enforce "
    "--root /x --format claude --verify-timeout 10.0 || exit 2"
)
PY = "/usr/bin/python3 -m opendaisugi.gate_client --mode shadow --root /x --format claude"
FOREIGN = "/opt/opendaisugi.gateway-watch --mode enforce"


def _write(path: Path, commands: list[str]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    hooks = [{"type": "command", "command": c} for c in commands]
    path.write_text(json.dumps({"hooks": {"PreToolUse": [{"matcher": "*", "hooks": hooks}]}}))


def test_gate_hook_words():
    assert gate_hook_mode(GO) == "enforce"
    assert gate_hook_mode(PY) == "shadow"
    assert gate_hook_args(FOREIGN) is None
    assert gate_hook_args("python3 -m opendaisugi.gateway") is None
    # A --mode inside a quoted path is a word of the path, not a flag.
    assert (
        gate_hook_mode("'/opt/x --mode enforce/python' -m opendaisugi.gate --mode shadow")
        == "shadow"
    )
    assert gate_hook_mode("python3 -m opendaisugi.gate --mode enforce --mode shadow") == "shadow"
    assert gate_hook_mode("python3 -m opendaisugi.gate --root '/r --mode enforce'") is None
    assert gate_hook_mode(5) is None
    assert gate_hook_mode("python3 -m opendaisugi.gate 'unclosed") is None


def test_record_hook_words():
    assert is_record_hook("daisugi hook record --format claude")
    assert is_record_hook("/usr/local/bin/daisugi hook record --event stop", "stop")
    assert not is_record_hook("daisugi hook record --event stop", "notification")
    assert not is_record_hook("mydaisugi hook record")
    assert not is_record_hook("echo 'daisugi hook record'")


def test_status_ignores_a_foreign_hook(tmp_path):
    p = tmp_path / "settings.json"
    _write(p, [FOREIGN])
    assert installed_hook_mode(p) is None
    _write(p, [FOREIGN, GO])
    assert installed_hook_mode(p) == "enforce"


def test_install_and_uninstall_leave_a_foreign_hook_alone(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    settings = tmp_path / ".claude" / "settings.json"
    _write(settings, [FOREIGN])
    r = runner.invoke(app, ["install", "--gate", "--yes", "--runtime", "claude"])
    assert r.exit_code == 0, r.output
    commands = [
        h["command"]
        for e in json.loads(settings.read_text())["hooks"]["PreToolUse"]
        for h in e["hooks"]
    ]
    assert FOREIGN in commands
    assert any(gate_hook_args(c) is not None for c in commands)
    r = runner.invoke(app, ["install", "--uninstall", "--runtime", "claude"])
    assert r.exit_code == 0, r.output
    commands = [
        h["command"]
        for e in json.loads(settings.read_text())["hooks"]["PreToolUse"]
        for h in e["hooks"]
    ]
    assert commands == [FOREIGN]


def test_install_names_a_failed_runtime_and_exits_1(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    (tmp_path / ".claude" / "settings.json").write_text('{"hooks": []}')
    r = runner.invoke(app, ["install", "--gate", "--yes", "--runtime", "claude"])
    assert r.exit_code == 1
    assert "already configured" not in r.stdout
    assert "Failed: Claude Code:" in r.stderr


def test_install_forced_runtime_without_its_directory_fails(tmp_path, monkeypatch):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    r = runner.invoke(app, ["install", "--gate", "--yes", "--runtime", "claude"])
    assert r.exit_code == 1
    assert "Failed: Claude Code:" in r.stderr


def test_init_checks_the_file_it_writes(tmp_path):
    root = tmp_path / "gate"
    d = root / "envelopes"
    d.mkdir(parents=True)
    (d / "a_b.json").write_text("{}")
    r = runner.invoke(
        app, ["gate", "init", "--session", "a/b", "--workspace", str(tmp_path), "--root", str(root)]
    )
    assert r.exit_code == 1
    assert (d / "a_b.json").read_text() == "{}"


def test_report_skips_a_line_that_is_json_but_not_an_object(tmp_path):
    root = tmp_path / "gate"
    (root / "shadow").mkdir(parents=True)
    (root / "shadow" / "s.jsonl").write_text(
        json.dumps({"would_deny": True, "reason": "r"}) + "\n[1, 2]\n"
    )
    r = runner.invoke(app, ["gate", "report", "--root", str(root)])
    assert r.exit_code == 0, r.output
    assert r.stdout.startswith("calls=1 allowed=0 would_deny=1 ")


# Hand-written gate hooks the words must still find (fix round 2), and
# commands that only look like one. The Go table in
# clients/go/internal/config/config_test.go holds the same rows.
FORMS = {
    "/usr/bin/python3 -m opendaisugi.gate --mode enforce --root /x || exit 2": ("gate", "enforce"),
    "'/opt/my env/bin/python' -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "uv run --project /p python -m opendaisugi.gate --mode enforce || exit 2": ("gate", "enforce"),
    "uv run python -m opendaisugi.gate_client --mode shadow": ("gate", "shadow"),
    "FOO=1 python3 -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "env FOO=1 python3 -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "env -u X -i python3 -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "python3 -I -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "python3 -Im opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "python3 -W ignore -m opendaisugi.gate --mode shadow": ("gate", "shadow"),
    "python3 -mopendaisugi.gate --mode enforce": ("gate", "enforce"),
    "python3.12 -m opendaisugi.gate --mode enforce||exit 2": ("gate", "enforce"),
    "python3 -m opendaisugi.gate --mode enforce; true": ("gate", "enforce"),
    "cd /w && python3 -m opendaisugi.gate --mode enforce": ("gate", "enforce"),
    "sh -c 'python3 -m opendaisugi.gate --mode enforce'": ("gate", "enforce"),
    'bash -lc "uv run python -m opendaisugi.gate --mode enforce"': ("gate", "enforce"),
    "daisugi gate check --mode enforce --root /x || exit 2": ("gate", "enforce"),
    "DAISUGI_GATE_HOOK=opendaisugi.gate FOO=1 daisugi gate check --mode enforce": (
        "gate",
        "enforce",
    ),
    "python3 -m opendaisugi.gate --mode 'enforce||x'": ("gate", None),
    "python3 -m opendaisugi.gateway-watch --mode enforce": (None, None),
    "'/opt/x --mode enforce/python' -m opendaisugi.gatex --root /r": (None, None),
    "DAISUGI_GATE_HOOK=opendaisugi.gate /bin/true gate check --mode enforce": (None, None),
    "echo -m opendaisugi.gate --mode enforce": ("unknown", None),
    "python3 -c 'import x' -m opendaisugi.gate --mode enforce": ("unknown", None),
    "nice python3 -m opendaisugi.gate --mode enforce": ("unknown", None),
    "python3 -m opendaisugi.gate --mode enforce 'unclosed": ("unknown", None),
}


def test_hand_written_forms():
    for command, (kind, mode) in FORMS.items():
        assert (gate_hook_kind(command), gate_hook_mode(command)) == (kind, mode), command


def test_status_never_reads_an_unknown_hook_as_shadow(tmp_path):
    p = tmp_path / "settings.json"
    unknown = "nice python3 -m opendaisugi.gate --mode enforce"
    _write(p, [unknown])
    assert installed_hook_mode(p) == "unknown"
    _write(p, [PY, unknown])
    assert installed_hook_mode(p) == "unknown"
    _write(p, [unknown, GO])
    assert installed_hook_mode(p) == "enforce"


def test_uninstall_refuses_an_unknown_gate_hook(tmp_path, monkeypatch):
    (tmp_path / ".claude").mkdir()
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    settings = tmp_path / ".claude" / "settings.json"
    unknown = "nice python3 -m opendaisugi.gate --mode enforce"
    _write(settings, [unknown, GO])
    before = settings.read_text()
    r = runner.invoke(app, ["install", "--uninstall", "--runtime", "claude"])
    assert r.exit_code == 1
    assert "Failures (left untouched): Claude Code:" in r.stdout
    assert unknown in r.stdout
    assert settings.read_text() == before
