"""Tests for AgenticExecutor — running a tool-using sub-agent inside the
parent's envelope (roadmap Stage 2).

Defense in depth, both walls exercised here:
- static outer wall: --allowedTools is COMPUTED from the envelope ∩ the
  step's request — a tool the envelope doesn't back never reaches the argv;
- dynamic inner wall: the sub-agent runs under an enforce-mode gate whose
  settings and envelope live in a private root OUTSIDE the workspace —
  supplied from outside anything the sub-agent can write.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from opendaisugi.agentic_executor import AgenticExecutor
from opendaisugi.models import AgenticStep, Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/tmp/**", "/work/**"], **perm_kwargs}
    return Envelope(
        generated_by="test",
        task="agentic exec test",
        permissions=Permission(**perms),
    )


def _step(workspace, **kwargs) -> AgenticStep:
    defaults = dict(id="a1", prompt="do the thing", workspace=str(workspace), tools=["Read"])
    defaults.update(kwargs)
    return AgenticStep(**defaults)


def _ok_envelope_json(result="done", tokens=7, cost=0.001):
    return json.dumps(
        {
            "result": result,
            "is_error": False,
            "usage": {"input_tokens": tokens, "output_tokens": 0},
            "total_cost_usd": cost,
        }
    )


@pytest.fixture
def spy_call(monkeypatch):
    """Capture the claude call's kwargs; return a canned success envelope."""
    seen = {}

    def _fake(prompt, *, timeout_s, model, binary, cwd, extra_args):
        seen.update(
            prompt=prompt,
            timeout_s=timeout_s,
            model=model,
            binary=binary,
            cwd=cwd,
            extra_args=list(extra_args),
        )
        return _ok_envelope_json()

    monkeypatch.setattr(
        "opendaisugi.agentic_executor.call_claude_p_sync",
        _fake,
    )
    return seen


def _run(executor, step):
    return executor.run(step, timeout_s=30, max_output_bytes=100_000)


def test_success_returns_result_text_and_meters(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope())
    res = _run(exe, _step(tmp_path))
    assert res.rc == 0
    assert res.stdout == "done"
    assert exe.last.tokens == 7
    assert exe.last.cost_usd == 0.001


def test_runs_in_the_step_workspace(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope())
    _run(exe, _step(tmp_path))
    assert spy_call["cwd"] == str(tmp_path)


def test_allowed_tools_are_envelope_intersected(tmp_path, spy_call):
    """Step asks for Bash+Read+Write; envelope grants only file_read →
    only Read survives into --allowedTools. The unbacked tools never
    reach the argv at all (static wall, independent of the gate)."""
    exe = AgenticExecutor(envelope=_envelope())  # no shell, no file_write
    _run(exe, _step(tmp_path, tools=["Bash", "Read", "Write"]))
    args = spy_call["extra_args"]
    allowed = args[args.index("--allowedTools") + 1]
    assert "Read" in allowed
    assert "Bash" not in allowed
    assert "Write" not in allowed


def test_no_backed_tools_fails_without_spawning(tmp_path, monkeypatch):
    called = []
    monkeypatch.setattr(
        "opendaisugi.agentic_executor.call_claude_p_sync",
        lambda *a, **k: called.append(1),
    )
    exe = AgenticExecutor(envelope=_envelope())
    res = _run(exe, _step(tmp_path, tools=["Bash"]))  # envelope has no shell
    assert res.rc == 1
    assert "no requested tool" in res.stdout
    assert not called


def test_gate_settings_enforce_mode_with_registered_envelope(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope())
    _run(exe, _step(tmp_path))
    args = spy_call["extra_args"]
    settings = json.loads(args[args.index("--settings") + 1])
    hook = settings["hooks"]["PreToolUse"][0]
    assert hook["matcher"] == "*"
    cmd = hook["hooks"][0]["command"]
    assert "--mode enforce" in cmd
    # The gate root named in the command has the envelope registered for the
    # session the executor chose, never as default.
    root = Path(cmd.split("--root ")[1].split(" --")[0])
    assert (root / "envelopes" / "agentic-a1.json").exists()
    assert not (root / "envelopes" / "default.json").exists()
    assert exe.last_gate_root == root


def test_gate_root_is_outside_the_workspace(tmp_path, spy_call):
    """The hook settings and envelope must be supplied from OUTSIDE anything
    the sub-agent can write — never under its workspace."""
    exe = AgenticExecutor(envelope=_envelope())
    _run(exe, _step(tmp_path))
    assert not str(exe.last_gate_root).startswith(str(tmp_path))


def test_max_turns_forwarded(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope())
    _run(exe, _step(tmp_path, max_turns=4))
    args = spy_call["extra_args"]
    assert args[args.index("--max-turns") + 1] == "4"


def test_is_error_surfaces_as_failed_step(tmp_path, monkeypatch):
    monkeypatch.setattr(
        "opendaisugi.agentic_executor.call_claude_p_sync",
        lambda *a, **k: json.dumps({"result": "boom", "is_error": True}),
    )
    exe = AgenticExecutor(envelope=_envelope())
    res = _run(exe, _step(tmp_path))
    assert res.rc == 1
    assert "boom" in res.stdout


def test_missing_workspace_fails_without_spawning(monkeypatch):
    called = []
    monkeypatch.setattr(
        "opendaisugi.agentic_executor.call_claude_p_sync",
        lambda *a, **k: called.append(1),
    )
    exe = AgenticExecutor(envelope=_envelope())
    res = _run(exe, _step("/work/does-not-exist-xyz"))
    assert res.rc == 1
    assert "workspace" in res.stdout
    assert not called


def test_wrong_step_kind_raises_type_error(tmp_path):
    from opendaisugi.models import TaskStep

    exe = AgenticExecutor(envelope=_envelope())
    with pytest.raises(TypeError):
        _run(exe, TaskStep(id="t", prompt="hi"))


def test_capture_true_wires_captures_root_inside_gate_root(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope(), capture=True)
    _run(exe, _step(tmp_path))
    args = spy_call["extra_args"]
    settings = json.loads(args[args.index("--settings") + 1])
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--captures-root" in cmd
    caps = Path(cmd.split("--captures-root ")[1].split(" --")[0].strip())
    assert str(caps).startswith(str(exe.last_gate_root))


def test_capture_false_omits_captures_root(tmp_path, spy_call):
    exe = AgenticExecutor(envelope=_envelope(), capture=False)
    _run(exe, _step(tmp_path))
    args = spy_call["extra_args"]
    settings = json.loads(args[args.index("--settings") + 1])
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--captures-root" not in cmd


def test_sub_agent_gate_is_pinned_to_a_session_the_executor_chose(tmp_path, spy_call):
    """The sub-agent's gate must not honor a session id from the payload —
    the envelope is pinned from outside, like the gate root itself."""
    exe = AgenticExecutor(envelope=_envelope())
    _run(exe, _step(tmp_path))
    args = spy_call["extra_args"]
    settings = json.loads(args[args.index("--settings") + 1])
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    assert "--session agentic-a1" in cmd


def _registered(spy_call) -> dict:
    args = spy_call["extra_args"]
    settings = json.loads(args[args.index("--settings") + 1])
    cmd = settings["hooks"]["PreToolUse"][0]["hooks"][0]["command"]
    root = Path(cmd.split("--root ")[1].split(" --")[0])
    return json.loads((root / "envelopes" / "agentic-a1.json").read_text())


def test_a_narrower_child_envelope_is_proved_and_registered(tmp_path, spy_call):
    parent = _envelope(shell=True, shell_allowlist=["git", "ls"], file_write=["/work/**"])
    child = Envelope(
        generated_by="test",
        task="child",
        permissions=Permission(file_read=["/work/src/**"], shell=True, shell_allowlist=["git"]),
    )
    exe = AgenticExecutor(envelope=parent)
    res = _run(exe, _step(tmp_path, tools=["Read", "Bash", "Write"], child_envelope=child))
    assert res.rc == 0, res.stdout
    args = spy_call["extra_args"]
    # The tool wall comes from the child: it may not write.
    assert args[args.index("--allowedTools") + 1] == "Read Bash"
    reg = _registered(spy_call)
    assert reg["permissions"]["shell_allowlist"] == ["git"]
    assert reg["parent_envelope"] == parent.id


def test_a_wider_child_envelope_is_refused_before_anything_runs(tmp_path, monkeypatch):
    called = []
    monkeypatch.setattr(
        "opendaisugi.agentic_executor.call_claude_p_sync", lambda *a, **k: called.append(1)
    )
    parent = _envelope(shell=True, shell_allowlist=["git"])
    child = Envelope(
        generated_by="test",
        task="child",
        permissions=Permission(file_read=["/etc/**"], shell=True, shell_allowlist=["rm"]),
    )
    res = _run(AgenticExecutor(envelope=parent), _step(tmp_path, child_envelope=child))
    assert res.rc == 1
    assert res.stdout == (
        'the child envelope is refused: file_read: the child\'s "/etc/**" is not inside '
        'the parent\'s; shell_allowlist: the child adds ["rm"]'
    )
    assert not called


def test_a_step_with_no_child_envelope_restates_the_parents(tmp_path, spy_call):
    parent = _envelope()
    _run(AgenticExecutor(envelope=parent), _step(tmp_path))
    reg = _registered(spy_call)
    assert reg["permissions"] == json.loads(parent.model_dump_json())["permissions"]
