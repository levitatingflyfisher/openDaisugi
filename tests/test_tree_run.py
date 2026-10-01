"""The tree run: branch totals, overruns, parent use, minted sessions, the
one starter (`tree run`), and the executor's tree mode.

No real agent runs: a fake sprig script answers from a JSON table the
test writes, and records what it saw.
"""

from __future__ import annotations

import json
import os
import stat
import sys
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import tree
from opendaisugi.agentic_executor import AgenticExecutor
from opendaisugi.cli import app
from opendaisugi.models import AgenticStep, Envelope
from opendaisugi.tree_rule import names_tree_write


def _env(id_: str = "env_00000001", **perm) -> Envelope:
    p = {"file_read": ["/w/**", "/tmp/**"], "shell": True, "shell_allowlist": ["git", "ls"]}
    p.update(perm)
    return Envelope(id=id_, generated_by="t", task="t", permissions=p)


def _root(tmp_path, tokens=None, turns=None) -> tuple[Path, Path]:
    d, g = tmp_path / "d", tmp_path / "d" / "gate"
    tree.make_root(d, g, _env("env_root"), "top", tokens=tokens, turns=turns)
    return d, g


def _spawn(d, g, parent, session, **kw):
    kw.setdefault("tokens", None)
    kw.setdefault("turns", None)
    return tree.spawn(d, g, _env(), parent=parent, session=session, **kw)


# --- the ledger ------------------------------------------------------------


def test_branch_totals_roll_up_three_deep(tmp_path):
    d, g = _root(tmp_path, tokens=1000)
    assert _spawn(d, g, "top", "a", tokens=600).status == "started"
    assert _spawn(d, g, "a", "b", tokens=300).status == "started"
    assert _spawn(d, g, "b", "c", tokens=100).status == "started"
    tree.end(d, g, "c", tokens_used=40, turns_used=1)
    tree.end(d, g, "b", tokens_used=10, turns_used=1)
    node, _ = tree.end(d, g, "a", tokens_used=5, turns_used=1)
    assert node.tokens_used == 55 and node.turns_used == 3
    t = tree.read_tree(d)
    assert t.left(t.nodes["top"], "tokens") == 945


def test_an_overrun_is_charged_and_shown(tmp_path):
    d, g = _root(tmp_path, tokens=100)
    _spawn(d, g, "top", "a", tokens=60)
    tree.end(d, g, "a", tokens_used=150, turns_used=None)
    t = tree.read_tree(d)
    assert t.left(t.nodes["top"], "tokens") == -50
    text = tree.status_text(tree.status_doc(d))
    assert "top (running) tokens 100 (over by 50)" in text
    assert "a (ended) tokens 60 (used 150, over by 90)" in text


def test_a_child_with_no_report_keeps_its_reservation(tmp_path):
    d, g = _root(tmp_path, tokens=100)
    _spawn(d, g, "top", "a", tokens=60)
    _spawn(d, g, "a", "b", tokens=30)
    assert tree.end_subtree(d, g, "a") == ["b", "a"]
    t = tree.read_tree(d)
    assert t.nodes["a"].tokens_used is None
    assert t.left(t.nodes["top"], "tokens") == 40


def test_a_node_with_no_report_rolls_up_its_childrens_overrun(tmp_path):
    d, g = _root(tmp_path, tokens=1000)
    _spawn(d, g, "top", "c", tokens=100)
    _spawn(d, g, "c", "g", tokens=80)
    tree.end(d, g, "g", tokens_used=150, turns_used=None)
    t = tree.read_tree(d)
    # Running, with no report: the children's 150 counts, not the 100.
    assert t.left(t.nodes["top"], "tokens") == 850
    tree.end(d, g, "c", tokens_used=None, turns_used=None)
    t = tree.read_tree(d)
    assert t.left(t.nodes["top"], "tokens") == 850
    _spawn(d, g, "top", "x", tokens=10)
    _spawn(d, g, "x", "y", tokens=5)
    _spawn(d, g, "y", "z", tokens=1)
    tree.end(d, g, "z", tokens_used=400, turns_used=None)
    assert tree.end_subtree(d, g, "x") == ["y", "x"]
    t = tree.read_tree(d)
    assert t.left(t.nodes["top"], "tokens") == 450


def test_own_use_takes_no_negative_count_and_caps_the_sum():
    from opendaisugi.tree_run import _own_use

    neg = {"usage": {"input_tokens": 300, "output_tokens": -250}, "model_turns": 2}
    assert _own_use(neg) == (None, 2)
    huge = {"usage": dict.fromkeys(["input_tokens", "output_tokens"], 2**62)}
    assert _own_use(huge) == (tree.MAX_COUNT, None)


def test_a_model_id_with_a_control_character_is_bad_input(tmp_path):
    d, g = _root(tmp_path)
    for bad in ["llama\n3", "a\x1b[2Jb", "x\x9bm", "y\x7f"]:
        with pytest.raises(tree.TreeError) as e:
            _spawn(d, g, "top", None, model=bad)
        assert e.value.code == 2
        assert e.value.what == "Tried to read the model id."
    assert _spawn(d, g, "top", None, model="llama3.3:70b").status == "started"


def test_parent_use_lowers_what_it_has_left(tmp_path):
    d, g = _root(tmp_path, tokens=100)
    res = _spawn(d, g, "top", "a", tokens=60, parent_tokens_used=50)
    assert res.status == "refused"
    assert res.reasons == ["tokens: the child asks for 60; the parent has 50 left"]
    assert _spawn(d, g, "top", "a", tokens=50, parent_tokens_used=50).status == "started"


def test_a_minted_session_is_stable_and_named_in_the_result(tmp_path):
    d, g = _root(tmp_path)
    res = tree.spawn(d, g, _env(), parent="top", session=None, tokens=None, turns=None)
    assert res.session == tree.mint_session("top", 0)
    assert res.session.startswith("n-") and len(res.session) == 14
    res2 = tree.spawn(d, g, _env(), parent="top", session=None, tokens=None, turns=None)
    assert res2.session == tree.mint_session("top", 1)


def test_profile_and_model_reach_the_spawn_row_and_status(tmp_path):
    d, g = _root(tmp_path)
    _spawn(d, g, "top", "a", profile="desk", model="llama3.3:70b")
    rows = [json.loads(x) for x in (d / "tree" / "ledger.jsonl").read_text().splitlines()]
    assert rows[1]["profile"] == "desk" and rows[1]["model"] == "llama3.3:70b"
    doc = tree.status_doc(d)
    assert doc["nodes"][1]["profile"] == "desk"
    assert ", profile desk, model llama3.3:70b" in tree.status_text(doc)
    assert "profile" not in doc["nodes"][0]


def test_tree_run_is_a_tree_write_verb():
    assert names_tree_write("daisugi tree run --parent p -")


# --- tree run and the executor, with a fake sprig --------------------------

FAKE_SPRIG = r"""#!{PY}
import json, os, sys
argv = sys.argv[1:]
log = os.environ["FAKE_LOG"]
fds = {}
for n in range(3, 12):
    try:
        os.fstat(n)
    except OSError:
        continue
    fds[str(n)] = len(os.read(n, 65536))
task = sys.stdin.read()
with open(log, "a") as f:
    f.write(json.dumps({"argv": argv, "fds": fds, "task": task}) + "\n")
reply = json.load(open(os.environ["FAKE_REPLY"]))
if reply.get("exit"):
    sys.exit(reply["exit"])
print(json.dumps(reply["json"]))
"""


@pytest.fixture
def fake(tmp_path, monkeypatch):
    bin_ = tmp_path / "bin" / "sprig"
    bin_.parent.mkdir()
    bin_.write_text(FAKE_SPRIG.replace("{PY}", sys.executable))
    bin_.chmod(bin_.stat().st_mode | stat.S_IEXEC)
    log, reply = tmp_path / "log.jsonl", tmp_path / "reply.json"
    monkeypatch.setenv("FAKE_LOG", str(log))
    monkeypatch.setenv("FAKE_REPLY", str(reply))
    monkeypatch.setenv("DAISUGI_SPRIG", str(bin_))

    def set_reply(**kw):
        reply.write_text(json.dumps(kw))

    def calls():
        return [json.loads(x) for x in log.read_text().splitlines()] if log.exists() else []

    set_reply(json={"answer": "done", "turns": 4, "model_turns": 2, "usage": {"input_tokens": 7}})
    return set_reply, calls


def _flag(argv, name):
    return argv[argv.index(name) + 1] if name in argv else None


def _req(**kw) -> str:
    doc = {"task": "do it", "envelope": _env().model_dump(mode="json"), "tools": ["Read"]}
    doc.update(kw)
    return json.dumps(doc)


def test_tree_run_starts_waits_and_ends(tmp_path, fake, monkeypatch):
    _, calls = fake
    d, g = _root(tmp_path, tokens=500, turns=9)
    monkeypatch.chdir(tmp_path)
    res = CliRunner().invoke(
        app,
        ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "200"]
        + ["--turns", "5", "--data-dir", str(d), "--root", str(g), "-"],
        input=_req(max_turns=3),
    )
    assert res.exit_code == 0, res.output
    out = json.loads(res.stdout)
    assert out["status"] == "ran" and out["answer"] == "done"
    assert out["tokens_used"] == 7 and out["turns_used"] == 2
    assert out["session"] == tree.mint_session("top", 0)
    argv = calls()[0]["argv"]
    assert _flag(argv, "--max-turns") == "3" and _flag(argv, "--max-tokens") == "200"
    assert _flag(argv, "--session") == out["session"]
    t = tree.read_tree(d)
    assert t.nodes[out["session"]].state == "ended"
    assert t.left(t.nodes["top"], "tokens") == 493


def test_tree_run_refused_and_asked_start_nothing(tmp_path, fake, monkeypatch):
    _, calls = fake
    d, g = _root(tmp_path, tokens=10)
    args = ["tree", "run", "--parent", "top", "--profile", "default", "--tokens", "50"]
    args += ["--data-dir", str(d), "--root", str(g), "-"]
    res = CliRunner().invoke(app, args, input=_req())
    assert res.exit_code == 1
    out = json.loads(res.stdout)
    assert out["status"] == "refused" and out["reasons"][0].startswith("tokens:")
    for _ in range(2):
        CliRunner().invoke(app, args, input=_req())
    res = CliRunner().invoke(app, args, input=_req())
    assert res.exit_code == 3 and json.loads(res.stdout)["ask_id"].startswith("ask_")
    assert calls() == []


def test_tree_run_bad_input_exits_2(tmp_path, fake):
    d, g = _root(tmp_path)
    args = ["tree", "run", "--parent", "top", "--profile", "default"]
    res = CliRunner().invoke(app, args + ["--data-dir", str(d), "--root", str(g), "-"], input="[]")
    assert res.exit_code == 2


def test_tree_run_reads_its_profiles_from_the_keys_fd_only(tmp_path, fake, monkeypatch):
    _, calls = fake
    d, g = _root(tmp_path)
    monkeypatch.chdir(tmp_path)
    args = ["tree", "run", "--parent", "top", "--profile", "desk"]
    args += ["--data-dir", str(d), "--root", str(g), "-"]
    res = CliRunner().invoke(app, args, input=_req())
    assert res.exit_code == 1
    assert "no agent profile named 'desk'; the profiles are default" in res.stdout


def _step(ws, **kw):
    d = dict(id="a1", prompt="do it", workspace=str(ws), tools=["Read"])
    d.update(kw)
    return AgenticStep(**d)


def test_executor_runs_in_tree_mode(tmp_path, fake):
    _, calls = fake
    ws = tmp_path / "w"
    ws.mkdir()
    ex = AgenticExecutor(envelope=_env(), runtime="sprig", sprig_binary=os.environ["DAISUGI_SPRIG"])
    res = ex.run(_step(ws), timeout_s=30, max_output_bytes=10_000)
    assert res.rc == 0, res.stdout
    gate_root = ex.last_gate_root
    assert gate_root.name == "gate" and gate_root.parent.name.startswith("daisugi-agentic-")
    argv = calls()[0]["argv"]
    assert _flag(argv, "--session") == "agentic-a1"
    assert f"--root {gate_root} " in _flag(argv, "--gate-cmd")
    t = tree.read_tree(gate_root.parent)
    assert t.roots == ["root"] and t.nodes["agentic-a1"].state == "ended"
    assert t.nodes["agentic-a1"].tokens_used == 7


def test_executor_budget_reaches_the_child(tmp_path, fake):
    _, calls = fake
    ws = tmp_path / "w"
    ws.mkdir()
    ex = AgenticExecutor(
        envelope=_env(), runtime="sprig", sprig_binary=os.environ["DAISUGI_SPRIG"], tokens=900
    )
    assert ex.run(_step(ws), timeout_s=30, max_output_bytes=10_000).rc == 0
    assert _flag(calls()[0]["argv"], "--max-tokens") == "900"


def test_a_failed_child_is_ended_with_no_report(tmp_path, fake):
    set_reply, _ = fake
    set_reply(exit=1)
    ws = tmp_path / "w"
    ws.mkdir()
    ex = AgenticExecutor(envelope=_env(), runtime="sprig", sprig_binary=os.environ["DAISUGI_SPRIG"])
    res = ex.run(_step(ws), timeout_s=30, max_output_bytes=10_000)
    assert res.rc == 1 and "sprig exited with code 1" in res.stdout
    t = tree.read_tree(ex.last_gate_root.parent)
    node = t.nodes["agentic-a1"]
    assert node.state == "ended" and node.tokens_used is None


def test_the_gate_denies_an_agent_write_to_the_tree_ledger(tmp_path):
    from opendaisugi.gate_state_rule import gate_state_hit

    d = tmp_path / "daisugi-agentic-x"
    g = d / "gate"
    tree.make_root(d, g, _env("env_root"), "root", tokens=None, turns=None)
    payload = {"cwd": str(tmp_path)}
    ledger = str(d / "tree" / "ledger.jsonl")
    assert gate_state_hit(payload, {"step_type": "file_write", "path": ledger}, g)
    shell = {"step_type": "shell", "command": f"echo x >> {ledger}"}
    assert gate_state_hit(payload, shell, g)
    assert gate_state_hit(payload, {"step_type": "file_write", "path": str(g / "x")}, g)
