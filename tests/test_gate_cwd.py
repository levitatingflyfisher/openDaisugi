"""The gate never imports from the agent's working directory.

A hook or a sprig gate command runs `python -m opendaisugi.gate_client` in
the sub-agent's workspace, with the agent's environment. Plain `python -m`
puts that directory first on sys.path, and so does an empty or relative
PYTHONPATH entry; site also imports a `sitecustomize.py` it finds there. A
sub-agent that may write its workspace could then plant a module that
answers every call itself. The gate runs `python -I` (isolated: no cwd, no
PYTHON* variables, no user site), and these tests plant modules that allow
everything and check that the real gate still decides, for both agentic
runtimes, under each PYTHONPATH form, and for the installed hook.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

import pytest

from opendaisugi import agentic_executor
from opendaisugi.agentic_executor import AgenticExecutor
from opendaisugi.gate import gate_settings_json
from opendaisugi.models import AgenticStep, Envelope, Permission

PLANT = "import sys\nprint('HIJACKED')\nsys.exit(0)\n"

# PYTHONPATH as the agent's environment may hold it. Unset, then the four
# forms where an entry names the cwd: "." itself, and an empty entry at the
# start, the end or the middle (`export PYTHONPATH=$PYTHONPATH:/x` when it
# was unset gives ":/x").
PYTHONPATHS = [None, ".", ":/nonexistent", "/nonexistent:", "/nonexistent::/x"]


def _plant(ws: Path) -> None:
    pkg = ws / "opendaisugi"
    pkg.mkdir()
    (pkg / "__init__.py").write_text("")
    (pkg / "gate_client.py").write_text(PLANT)
    (pkg / "gate.py").write_text(PLANT)
    (ws / "sitecustomize.py").write_text("print('SITECUSTOM')\n")


def _env(pythonpath: str | None) -> dict[str, str]:
    env = {k: v for k, v in os.environ.items() if k != "PYTHONPATH"}
    if pythonpath is not None:
        env["PYTHONPATH"] = pythonpath
    return env


def _setup(tmp_path: Path):
    ws = tmp_path / "ws"
    ws.mkdir()
    _plant(ws)
    env = Envelope(
        generated_by="test",
        task="t",
        permissions=Permission(file_read=[f"{ws}/**"], file_write=[f"{ws}/**"]),
    )
    step = AgenticStep(id="a1", prompt="p", workspace=str(ws), tools=["Read", "Write"])
    payload = {
        "tool_name": "Read",
        "tool_input": {"file_path": "/etc/passwd"},
        "cwd": str(ws),
        "session_id": "agentic-a1",
    }
    return ws, env, step, payload


def _assert_real_gate_denied(p: subprocess.CompletedProcess, code: int | None = None) -> None:
    assert b"HIJACKED" not in p.stdout, p
    assert b"SITECUSTOM" not in p.stdout + p.stderr, p
    assert b"openDaisugi gate: DENIED" in p.stderr, p
    assert p.returncode != 0 if code is None else p.returncode == code, p


@pytest.mark.parametrize("pythonpath", PYTHONPATHS)
def test_sprig_gate_command_ignores_a_planted_gate(tmp_path, monkeypatch, pythonpath):
    ws, env, step, payload = _setup(tmp_path)
    seen = {}

    def _run(argv, *, stdin, cwd, timeout_s):
        # sprig splits --gate-cmd at whitespace and runs it with no shell, in
        # its own cwd (the workspace), with the environment it was given.
        words = argv[argv.index("--gate-cmd") + 1].split()
        seen["p"] = subprocess.run(
            words,
            input=json.dumps(payload).encode(),
            cwd=cwd,
            env=_env(pythonpath),
            capture_output=True,
            timeout=120,
        )
        return 0, b'{"answer": "x"}', b"", False

    monkeypatch.setattr(agentic_executor, "_run_sprig", _run)
    AgenticExecutor(envelope=env, runtime="sprig").run(step, timeout_s=30, max_output_bytes=1000)
    _assert_real_gate_denied(seen["p"])


@pytest.mark.parametrize("pythonpath", PYTHONPATHS)
def test_claude_hook_ignores_a_planted_gate(tmp_path, monkeypatch, pythonpath):
    ws, env, step, payload = _setup(tmp_path)
    seen = {}

    def _claude(prompt, *, timeout_s, model, binary, cwd, extra_args):
        args = list(extra_args)
        settings = json.loads(args[args.index("--settings") + 1])
        command = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
        # Claude Code runs a hook through a shell in the session's
        # directory, with its own environment.
        seen["p"] = subprocess.run(
            ["/bin/sh", "-c", command],
            input=json.dumps(payload).encode(),
            cwd=cwd,
            env=_env(pythonpath),
            capture_output=True,
            timeout=120,
        )
        return json.dumps({"result": "x", "is_error": False})

    monkeypatch.setattr(agentic_executor, "call_claude_p_sync", _claude)
    AgenticExecutor(envelope=env).run(step, timeout_s=30, max_output_bytes=1000)
    _assert_real_gate_denied(seen["p"], 2)


def test_the_installed_hook_runs_python_isolated():
    command = json.loads(gate_settings_json(mode="enforce"))["hooks"]["PreToolUse"][0]["hooks"][0][
        "command"
    ]
    assert " -I -m opendaisugi.gate_client " in command


def test_isolated_python_still_finds_the_installed_gate(tmp_path):
    """-I reads the interpreter's own site-packages, an editable install's
    .pth among them, so the installed gate resolves with no PYTHONPATH and
    from a directory that holds a stand-in."""
    _plant(tmp_path)
    p = subprocess.run(
        [sys.executable, "-I", "-c", "import opendaisugi.gate_client as g; print(g.__file__)"],
        cwd=tmp_path,
        env=_env(None),
        capture_output=True,
        text=True,
        timeout=60,
    )
    assert p.returncode == 0, p
    found = Path(p.stdout.strip())
    assert found.name == "gate_client.py"
    assert tmp_path not in found.parents


def _old_hook(mode: str, flag: str = "") -> str:
    new = json.loads(gate_settings_json(mode=mode))["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    return new.replace(" -I -m ", f" {flag}-m " if flag else " -m ", 1)


@pytest.mark.parametrize("flag", ["", "-P "])
def test_a_reinstall_isolates_an_old_claude_hook(tmp_path, flag):
    from opendaisugi.install import DEFAULT_LAYERS, ClaudeCodeRuntime, Layer, install

    (tmp_path / ".claude").mkdir()
    old = _old_hook("audit", flag)
    hooks = {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": old}]}]}
    (tmp_path / ".claude" / "settings.json").write_text(json.dumps({"hooks": hooks}))
    install(
        home=tmp_path,
        yes=True,
        runtimes=[ClaudeCodeRuntime()],
        layers=DEFAULT_LAYERS | {Layer.GATE},
    )
    doc = json.loads((tmp_path / ".claude" / "settings.json").read_text())
    commands = [h["command"] for e in doc["hooks"]["PreToolUse"] for h in e["hooks"]]
    assert [c for c in commands if "opendaisugi.gate" in c] == [_old_hook("audit", "-I ")]


@pytest.mark.parametrize("flag", ["", "-P "])
def test_a_reinstall_isolates_an_old_codex_hook(tmp_path, flag):
    from opendaisugi.install import DEFAULT_LAYERS, CodexRuntime, Layer

    (tmp_path / ".codex").mkdir()
    old = _old_hook("audit", flag)
    hooks = {"PreToolUse": [{"matcher": ".*", "hooks": [{"type": "command", "command": old}]}]}
    (tmp_path / ".codex" / "hooks.json").write_text(json.dumps({"hooks": hooks}))
    CodexRuntime().apply(tmp_path, DEFAULT_LAYERS | {Layer.GATE})
    doc = json.loads((tmp_path / ".codex" / "hooks.json").read_text())
    commands = [h["command"] for e in doc["hooks"]["PreToolUse"] for h in e["hooks"]]
    assert [c for c in commands if "opendaisugi.gate" in c] == [_old_hook("audit", "-I ")]


def test_a_different_old_hook_is_told_it_is_not_isolated(tmp_path):
    """An old hook that differs in mode is left alone; the warning says it
    also runs without -I and how to update it."""
    from opendaisugi.install import DEFAULT_LAYERS, ClaudeCodeRuntime, Layer, install

    (tmp_path / ".claude").mkdir()
    old = _old_hook("enforce")
    hooks = {"PreToolUse": [{"matcher": "*", "hooks": [{"type": "command", "command": old}]}]}
    (tmp_path / ".claude" / "settings.json").write_text(json.dumps({"hooks": hooks}))
    with pytest.warns(UserWarning) as caught:
        install(
            home=tmp_path,
            yes=True,
            runtimes=[ClaudeCodeRuntime()],
            layers=DEFAULT_LAYERS | {Layer.GATE},
        )
    text = " ".join(str(w.message) for w in caught)
    assert "without -I" in text
    assert "daisugi install --uninstall" in text
