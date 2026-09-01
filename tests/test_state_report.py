"""Unit tests for opendaisugi._state_report's build/validate/hook-report
pieces that don't fit tests/floor/test_report.py's report_state focus.

hook_report_argv is tested here as a pure function; its two real entry
points (the `daisugi hook report` CLI command, and gate_server.py's
gate.sock dispatch for {"argv": ["hook", "report"], ...}) are wired and
tested in tests/test_hook_report_events.py (task 4) and
tests/test_gate_resident.py (task 4).
"""

from __future__ import annotations

import json
import time

from opendaisugi._state_report import build_event, hook_report_argv


def test_build_event_shape_matches_section_3_1():
    ev = json.loads(build_event(session_id="s1", harness="claude-code", state="working"))
    assert ev["v"] == 1 and ev["session_id"] == "s1" and ev["harness"] == "claude-code"
    assert ev["state"] == "working" and ev["source"] == "gate"
    assert "ts" in ev and "pane" in ev and "harness_session_id" in ev


def test_build_event_clips_an_overlong_detail():
    ev = json.loads(
        build_event(session_id="s1", harness="claude-code", state="idle", detail="x" * 500)
    )
    assert len(ev["detail"]) == 200


def _row(**over):
    base = {
        "v": 1,
        "ts": time.time(),
        "session_id": "s1",
        "harness": "pi",
        "state": "idle",
        "source": "headless",
    }
    base.update(over)
    return json.dumps(base).encode()


def test_hook_report_downgrades_a_claimed_gate_source(tmp_path):
    from opendaisugi.session_tree import SessionTree

    # --root is the GATE root (mirrors gate.py: root.parent / "sessions" is
    # where the tree lives), so tmp_path/"gate" here, not bare tmp_path —
    # bare tmp_path would write into tmp_path.parent (pytest's shared
    # per-run basetemp) instead of this test's own tmp_path.
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(source="gate"))
    assert out.exit_code == 0
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["source"] == "headless"


def test_hook_report_downgrades_a_claimed_operator_source(tmp_path):
    from opendaisugi.session_tree import SessionTree

    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(source="operator"))
    assert out.exit_code == 0
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["source"] == "headless"


def test_hook_report_stamps_the_pane_flag_onto_the_event(tmp_path):
    from opendaisugi.session_tree import SessionTree

    hook_report_argv(["--root", str(tmp_path / "gate"), "--pane", "w1:p2"], _row())
    t = SessionTree.open(tmp_path / "sessions", "s1")
    states = [e for e in t.entries() if e.type == "state"]
    assert states[-1].data["pane"] == "w1:p2"


def test_hook_report_exits_one_on_malformed_json(tmp_path):
    out = hook_report_argv(["--root", str(tmp_path / "gate")], b"not json")
    assert out.exit_code == 1 and "not valid JSON" in out.stderr


def test_hook_report_exits_one_on_a_rule_violation(tmp_path):
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row(state="napping"))
    assert out.exit_code == 1 and "unknown state" in out.stderr


def test_hook_report_never_raises_on_a_reporting_failure(tmp_path, monkeypatch):
    def _boom(*_a, **_k):
        raise OSError("boom")

    monkeypatch.setattr("opendaisugi._state_report.report_state", _boom)
    out = hook_report_argv(["--root", str(tmp_path / "gate")], _row())
    assert out.exit_code == 0  # the event still parsed and validated; delivery is best-effort


def test_hook_report_returns_exit_1_not_a_traceback_for_a_non_dict_ask(tmp_path):
    """Fix round 1, Finding 2 (Important): a blocked event whose ask is not
    an object used to escape as an uncaught TypeError (`5 not in ask`
    raises TypeError, not ValueError, and hook_report_argv only catches
    ValueError). It must exit 1 with a message, never raise."""
    out = hook_report_argv(
        ["--root", str(tmp_path / "gate")], _row(state="blocked", source="gate", ask=5)
    )
    assert out.exit_code == 1
    assert out.stderr != ""


def _live_pids(needle: str) -> list[int]:
    """Every live (not zombie) process whose argv contains ``needle``."""
    from pathlib import Path

    out = []
    for d in Path("/proc").iterdir():
        if not d.name.isdigit():
            continue
        try:
            cmd = (d / "cmdline").read_bytes()
            stat = (d / "stat").read_text()
        except OSError:
            continue
        if needle.encode() in cmd and not stat[stat.rindex(")") :].startswith(") Z"):
            out.append(int(d.name))
    return out


def test_a_herdr_call_past_its_budget_leaves_nothing_behind(tmp_path):
    """herdr may start processes of its own. At the budget the whole
    process group is killed, so no process from the call is left running."""
    import os
    import signal

    from opendaisugi._state_report import _send_herdr_report

    marker = f"96.{time.time_ns() % 1_000_000_000:09d}"
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    herdr = bin_dir / "herdr"
    herdr.write_text(f"#!/bin/sh\nsleep {marker} &\nsleep {marker}\n")
    herdr.chmod(0o755)
    env = {"PATH": f"{bin_dir}:/usr/bin:/bin"}
    end = time.monotonic() + 0.3
    try:
        got = _send_herdr_report(
            "w1:p1",
            {"state": "working", "harness": "x"},
            env,
            lambda: max(0.0, end - time.monotonic()),
        )
        assert got == "none"
        for _ in range(100):
            if not _live_pids(marker):
                break
            time.sleep(0.02)
        assert _live_pids(marker) == []
    finally:
        for pid in _live_pids(marker):
            os.kill(pid, signal.SIGKILL)
