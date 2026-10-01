"""Tests for AgenticExecutor's sprig runtime: the same edge proof, private
gate root, pinned session and tool wall as the claude runtime, with the
`sprig` binary as the sub-agent.

No real agent runs here: a spy stands in for the process call, and a small
fake `sprig` script stands in for the binary where the process itself is
the point (the timeout kill, a missing binary, the session file).
"""

from __future__ import annotations

import json
import os
import stat
import sys
import time
from pathlib import Path

import pytest

from opendaisugi import agentic_executor
from opendaisugi.agentic_executor import AgenticExecutor
from opendaisugi.models import AgenticStep, Envelope, Permission


def _envelope(**perm_kwargs) -> Envelope:
    perms = {"file_read": ["/tmp/**", "/work/**"], **perm_kwargs}
    return Envelope(generated_by="test", task="sprig exec test", permissions=Permission(**perms))


def _step(workspace, **kwargs) -> AgenticStep:
    defaults = dict(id="a1", prompt="do the thing", workspace=str(workspace), tools=["Read"])
    defaults.update(kwargs)
    return AgenticStep(**defaults)


def _run(executor, step, timeout_s=30):
    return executor.run(step, timeout_s=timeout_s, max_output_bytes=100_000)


def _reply(**kw) -> bytes:
    body = {"answer": "done", "turns": 2, "model": "haiku", "usage": {"input_tokens": 5}}
    body.update(kw)
    return json.dumps(body).encode()


@pytest.fixture
def spy_run(monkeypatch):
    """Capture the sprig process call; answer with a canned reply."""
    seen: dict = {"reply": (0, _reply(), b"")}

    def _fake(argv, *, stdin, cwd, timeout_s):
        seen.update(argv=list(argv), stdin=stdin, cwd=cwd, timeout_s=timeout_s)
        root = Path(_flag(argv, "--gate-cmd").split(" --root ")[1].split(" ")[0])
        seen["root"] = root
        seen["envelopes"] = sorted(p.name for p in (root / "envelopes").iterdir())
        rc, out, err = seen["reply"]
        return rc, out, err, False

    monkeypatch.setattr(agentic_executor, "_run_sprig", _fake)
    return seen


def _flag(argv, name):
    return argv[argv.index(name) + 1]


def _sprig(**kw) -> AgenticExecutor:
    return AgenticExecutor(envelope=kw.pop("envelope", _envelope()), runtime="sprig", **kw)


def test_the_default_runtime_is_claude():
    assert AgenticExecutor(envelope=_envelope()).runtime == "claude"


def test_an_unknown_runtime_is_refused():
    with pytest.raises(ValueError):
        AgenticExecutor(envelope=_envelope(), runtime="pi")


def test_the_binary_comes_from_daisugi_sprig(monkeypatch):
    monkeypatch.setenv("DAISUGI_SPRIG", "/opt/x/sprig")
    assert _sprig().sprig_binary == "/opt/x/sprig"
    monkeypatch.delenv("DAISUGI_SPRIG")
    assert _sprig().sprig_binary == "sprig"


def test_success_returns_the_answer_and_meters(tmp_path, spy_run):
    spy_run["reply"] = (
        0,
        _reply(
            answer="fixed it",
            usage={
                "input_tokens": 5,
                "output_tokens": 6,
                "cache_read_input_tokens": 7,
                "cache_creation_input_tokens": 8,
            },
        ),
        b"",
    )
    exe = _sprig()
    res = _run(exe, _step(tmp_path))
    assert (res.rc, res.stdout) == (0, "fixed it")
    assert exe.last.tokens == 26
    assert exe.last.cost_usd is None
    assert exe.last.model == "haiku"


def test_no_usage_meters_no_tokens(tmp_path, spy_run):
    spy_run["reply"] = (0, json.dumps({"answer": "ok"}).encode(), b"")
    exe = _sprig()
    assert _run(exe, _step(tmp_path)).rc == 0
    assert exe.last.tokens is None


def test_the_argv_pins_the_gate_and_the_session(tmp_path, spy_run):
    exe = _sprig(model="sonnet", envelope=_envelope(shell=True, shell_allowlist=["ls"]))
    _run(exe, _step(tmp_path, tools=["Read", "Bash"], max_turns=4), timeout_s=9)
    argv = spy_run["argv"]
    root = exe.last_gate_root
    gate = " ".join([sys.executable, "-I", "-m", "opendaisugi.gate_client"])
    assert argv == [
        "sprig",
        "--json",
        "--gate",
        "--gate-cmd",
        f"{gate} --mode enforce --root {root} --session agentic-a1"
        f" --captures-root {root / 'captures'}",
        "--tools",
        "read,bash",
        "--model",
        "sonnet",
        "--session-dir",
        str(root / "sessions"),
        "--session",
        "agentic-a1",
        "--max-turns",
        "4",
        "-",
    ]
    assert spy_run["stdin"] == "do the thing"
    assert spy_run["cwd"] == str(tmp_path)
    assert spy_run["timeout_s"] == 9
    # The envelope is registered under the pinned session only.
    assert spy_run["envelopes"] == ["agentic-a1.json"]


def test_capture_false_leaves_out_the_captures_root(tmp_path, spy_run):
    _run(_sprig(capture=False), _step(tmp_path))
    assert "--captures-root" not in _flag(spy_run["argv"], "--gate-cmd")


def test_the_gate_root_is_outside_the_workspace(tmp_path, spy_run):
    exe = _sprig()
    _run(exe, _step(tmp_path))
    assert not str(exe.last_gate_root).startswith(str(tmp_path))
    assert (exe.last_gate_root / "envelopes" / "agentic-a1.json").exists()


def test_the_wall_maps_host_tools_and_drops_the_rest(tmp_path, spy_run):
    env = _envelope(shell=True, shell_allowlist=["ls"], file_write=["/work/**"], network=True)
    _run(
        _sprig(envelope=env),
        _step(tmp_path, tools=["Glob", "Bash", "WebFetch", "Edit", "Read", "Bash", "Write"]),
    )
    assert _flag(spy_run["argv"], "--tools") == "bash,edit,read,write"


def test_only_tools_sprig_lacks_is_nothing_to_delegate(tmp_path, monkeypatch):
    called = []
    monkeypatch.setattr(agentic_executor, "_run_sprig", lambda *a, **k: called.append(1))
    res = _run(_sprig(), _step(tmp_path, tools=["Grep", "Glob"]))
    assert res.rc == 1
    assert res.stdout == (
        "no requested tool is backed by the envelope "
        "(requested ['Grep', 'Glob']); nothing to delegate"
    )
    assert not called


def test_a_wider_child_is_refused_before_sprig_starts(tmp_path, monkeypatch):
    called = []
    monkeypatch.setattr(agentic_executor, "_run_sprig", lambda *a, **k: called.append(1))
    child = Envelope(
        generated_by="test", task="child", permissions=Permission(file_read=["/etc/**"])
    )
    res = _run(_sprig(), _step(tmp_path, child_envelope=child))
    assert res.rc == 1
    assert res.stdout.startswith("the child envelope is refused: ")
    assert not called


def test_a_narrower_child_sets_the_wall(tmp_path, spy_run):
    parent = _envelope(shell=True, shell_allowlist=["git"], file_write=["/work/**"])
    child = Envelope(
        generated_by="test",
        task="child",
        permissions=Permission(file_read=["/work/src/**"], shell=True, shell_allowlist=["git"]),
    )
    exe = _sprig(envelope=parent)
    res = _run(exe, _step(tmp_path, tools=["Read", "Bash", "Write"], child_envelope=child))
    assert res.rc == 0, res.stdout
    assert _flag(spy_run["argv"], "--tools") == "read,bash"
    reg = json.loads((exe.last_gate_root / "envelopes" / "agentic-a1.json").read_text())
    assert reg["parent_envelope"] == parent.id


def test_a_space_in_the_gate_command_fails_before_anything(tmp_path, monkeypatch):
    called = []
    monkeypatch.setattr(agentic_executor, "_run_sprig", lambda *a, **k: called.append(1))
    spaced = tmp_path / "t mp"
    spaced.mkdir()
    monkeypatch.setenv("TMPDIR", str(spaced))
    monkeypatch.setattr("tempfile.tempdir", None)
    ws = tmp_path / "ws"
    ws.mkdir()
    exe = _sprig()
    res = _run(exe, _step(ws))
    assert res.rc == 1
    assert res.stdout == agentic_executor.SPRIG_SPACE_TEXT
    assert not called
    assert exe.last_gate_root is None
    assert list(spaced.iterdir()) == []


@pytest.mark.parametrize(
    "reply, text",
    [
        ((3, b"", b"sprig: first\nsprig: gave up\n\n"), "sprig exited with code 3: sprig: gave up"),
        ((1, b"", b""), "sprig exited with code 1"),
        ((0, b"I did it.", b""), "returned unparseable output: 'I did it.'"),
        ((0, b"[1, 2]", b""), "reply has no answer: '[1, 2]'"),
        ((0, b'{"turns": 1}', b""), "reply has no answer: '{\"turns\": 1}'"),
        ((0, b'{"answer": 7}', b""), "reply has no answer: '{\"answer\": 7}'"),
    ],
)
def test_each_failure_is_a_failed_step(tmp_path, spy_run, reply, text):
    spy_run["reply"] = reply
    res = _run(_sprig(), _step(tmp_path))
    assert res.rc == 1
    assert text in res.stdout
    assert res.stdout.startswith("agentic sub-agent ")


def test_odd_usage_values_are_not_counted(tmp_path, spy_run):
    spy_run["reply"] = (
        0,
        _reply(usage={"input_tokens": "5", "output_tokens": True, "cache_read_input_tokens": 3}),
        b"",
    )
    exe = _sprig()
    assert _run(exe, _step(tmp_path)).rc == 0
    assert exe.last.tokens == 3


# --- the process itself, with a fake sprig script ---------------------------

FAKE = """#!{python}
import json, os, sys, time
argv = sys.argv[1:]
task = sys.stdin.read()
d = argv[argv.index("--session-dir") + 1]
os.makedirs(d, exist_ok=True)
s = argv[argv.index("--session") + 1]
open(os.path.join(d, s + ".jsonl"), "w").write(json.dumps({{"task": task}}) + "\\n")
if task == "sleep":
    open(os.path.join(os.environ["MARK"], "child.pid"), "w").write(str(os.getpid()))
    time.sleep(60)
print(json.dumps({{"answer": "ran in " + os.getcwd(), "turns": 2}}), flush=True)
if task in ("leftover", "escape"):
    # A process left behind that still holds sprig's stdout. "escape" puts
    # it in a session of its own, out of reach of the group kill.
    import subprocess
    subprocess.Popen(["/bin/sleep", "60"], start_new_session=(task == "escape"))
"""


def _fake_sprig(tmp_path) -> str:
    p = tmp_path / "bin" / "sprig"
    p.parent.mkdir()
    p.write_text(FAKE.format(python=sys.executable))
    p.chmod(p.stat().st_mode | stat.S_IXUSR)
    return str(p)


def test_a_real_process_runs_in_the_workspace_and_keeps_its_session(tmp_path):
    ws = tmp_path / "ws"
    ws.mkdir()
    exe = _sprig(sprig_binary=_fake_sprig(tmp_path))
    res = _run(exe, _step(ws))
    assert (res.rc, res.stdout) == (0, f"ran in {ws}")
    sess = exe.last_gate_root / "sessions" / "agentic-a1.jsonl"
    assert json.loads(sess.read_text()) == {"task": "do the thing"}


def test_the_timeout_kills_the_process(tmp_path, monkeypatch):
    ws = tmp_path / "ws"
    ws.mkdir()
    monkeypatch.setenv("MARK", str(tmp_path))
    exe = _sprig(sprig_binary=_fake_sprig(tmp_path))
    t0 = time.monotonic()
    res = _run(exe, _step(ws, prompt="sleep"), timeout_s=2)
    assert time.monotonic() - t0 < 20
    assert res.rc == 1
    assert res.stdout == "agentic sub-agent failed: sprig ran past 2s and was killed"
    pid = int((tmp_path / "child.pid").read_text())
    with pytest.raises(ProcessLookupError):
        os.kill(pid, 0)


def test_a_missing_binary_is_a_failed_step(tmp_path):
    res = _run(_sprig(sprig_binary=str(tmp_path / "nope")), _step(tmp_path))
    assert res.rc == 1
    assert res.stdout == f"agentic sub-agent failed: cannot start sprig ({tmp_path / 'nope'})"


def test_physical_stakes_still_refuse_an_agentic_step(tmp_path):
    from opendaisugi.models import ActionPlan
    from opendaisugi.verify import verify

    env = Envelope(
        generated_by="test",
        task="arm",
        stakes="physical",
        permissions=Permission(file_read=[f"{tmp_path}/**"]),
    )
    plan = ActionPlan(id="plan_00000001", source="script", task="t", steps=[_step(tmp_path)])
    result = verify(plan, env)
    assert not result.ok
    assert any("agentic delegation" in v.message for v in result.violations)


@pytest.mark.parametrize("task", ["leftover", "escape"])
def test_a_leftover_pipe_ends_as_a_timeout_in_bounded_time(tmp_path, task):
    """sprig answers and exits, but a process it left still holds its
    stdout: the run is a timeout, as communicate(timeout) makes it, and it
    ends soon after the timeout even when the group kill cannot reach that
    process."""
    ws = tmp_path / "ws"
    ws.mkdir()
    exe = _sprig(sprig_binary=_fake_sprig(tmp_path))
    t0 = time.monotonic()
    res = _run(exe, _step(ws, prompt=task), timeout_s=2)
    assert time.monotonic() - t0 < 2 + agentic_executor.SPRIG_DRAIN_S + 3
    assert res.rc == 1
    assert res.stdout == "agentic sub-agent failed: sprig ran past 2s and was killed"
