"""A kill reaches every process under a node (MX-R-8): the starter kills
its child's group on every exit, and a timeout walks the branch's
recorded groups, deepest first. A child's output is capped at 16 MiB."""

from __future__ import annotations

import json
import os
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path

import pytest
from typer.testing import CliRunner

from opendaisugi import agentic_executor, tree
from opendaisugi.cli import app
from opendaisugi.models import Envelope

ENV = {
    "id": "env_00000001",
    "generated_by": "t",
    "task": "t",
    "permissions": {"file_read": ["/w/**"], "max_execution_time_s": 3},
}

FAKE = r"""#!{PY}
import json, os, subprocess, sys, time
from pathlib import Path
argv = sys.argv[1:]
spec = json.load(open(os.environ["FAKE_SPEC"]))
sys.stdin.read()
def note(pid):
    with open(os.environ["FAKE_PIDS"], "a") as f:
        f.write("%d\n" % pid)
quiet = dict(stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
if spec.get("background"):
    note(subprocess.Popen(["sleep", "300"], **quiet).pid)
if spec.get("grandchild"):
    from opendaisugi import tree
    from opendaisugi.models import Envelope
    me = argv[argv.index("--session") + 1]
    d, g = Path(spec["data_dir"]), Path(spec["gate"])
    tree.spawn(d, g, Envelope(**spec["envelope"]), parent=me, session="gc", tokens=None, turns=None)
    p = subprocess.Popen(["sleep", "300"], start_new_session=True, **quiet)
    tree.record_group(d, "gc", p.pid)
    note(p.pid)
if spec.get("big"):
    sys.stdout.write("x" * (17 << 20))
    sys.stdout.flush()
time.sleep(spec.get("hang", 0))
print(json.dumps({"answer": "done", "turns": 1, "model_turns": 1, "usage": {"input_tokens": 1}}))
"""


@pytest.fixture
def run(tmp_path, monkeypatch):
    bin_ = tmp_path / "bin" / "sprig"
    bin_.parent.mkdir()
    bin_.write_text(FAKE.replace("{PY}", sys.executable))
    bin_.chmod(bin_.stat().st_mode | stat.S_IEXEC)
    d, g = tmp_path / "d", tmp_path / "d" / "gate"
    tree.make_root(d, g, Envelope(**ENV), "top", tokens=None, turns=None)
    spec, pids = tmp_path / "spec.json", tmp_path / "pids"
    monkeypatch.setenv("FAKE_SPEC", str(spec))
    monkeypatch.setenv("FAKE_PIDS", str(pids))
    monkeypatch.setenv("DAISUGI_SPRIG", str(bin_))
    monkeypatch.chdir(tmp_path)

    def go(**kw):
        spec.write_text(json.dumps({"data_dir": str(d), "gate": str(g), "envelope": ENV, **kw}))
        args = ["tree", "run", "--parent", "top", "--profile", "default", "--session", "kid"]
        args += ["--data-dir", str(d), "--root", str(g), "-"]
        req = {"task": "t", "envelope": ENV, "tools": ["Read"]}
        res = CliRunner().invoke(app, args, input=json.dumps(req))
        listed = [int(x) for x in pids.read_text().split()] if pids.exists() else []
        return json.loads(res.stdout), listed, d

    return go


def _alive(pid: int) -> bool:
    try:
        stat_line = Path(f"/proc/{pid}/stat").read_text()
    except OSError:
        return False
    return stat_line.rsplit(")", 1)[1].split()[0] != "Z"


def _gone(pids: list[int]) -> bool:
    end = time.monotonic() + 5
    while time.monotonic() < end:
        if not any(_alive(p) for p in pids):
            return True
        time.sleep(0.05)
    for p in pids:
        try:
            os.kill(p, signal.SIGKILL)
        except ProcessLookupError:
            pass
    return False


def test_a_child_that_succeeds_leaves_nothing_running(run):
    out, pids, _ = run(background=True)
    assert out["status"] == "ran" and len(pids) == 1
    assert _gone(pids)


def test_a_timeout_kills_the_branch_deepest_first(run):
    out, pids, d = run(grandchild=True, hang=60)
    assert out["status"] == "failed" and "ran past 3s" in out["reasons"][0]
    assert len(pids) == 1
    assert _gone(pids)
    rows = [json.loads(x) for x in (d / "tree" / "ledger.jsonl").read_text().splitlines()]
    groups = [r for r in rows if r["event"] == "group"]
    assert [r["session"] for r in groups] == ["kid", "gc"]
    t = tree.read_tree(d)
    assert t.nodes["gc"].state == "ended" and t.nodes["kid"].state == "ended"


def test_output_past_16_mib_fails_the_child(run):
    out, _, _ = run(big=True)
    assert out["status"] == "failed"
    assert out["reasons"] == ["agentic sub-agent failed: sprig wrote more than 16 MiB"]


def test_kill_groups_signals_then_kills(tmp_path):
    stubborn = subprocess.Popen(
        [
            sys.executable,
            "-c",
            "import signal,time; signal.signal(15, signal.SIG_IGN); time.sleep(60)",
        ],
        start_new_session=True,
    )
    plain = subprocess.Popen(["sleep", "60"], start_new_session=True)
    time.sleep(0.3)
    agentic_executor.kill_groups([stubborn.pid, plain.pid, 1, os.getpgrp()])
    assert stubborn.wait(timeout=10) == -signal.SIGKILL
    assert plain.wait(timeout=10) == -signal.SIGTERM
