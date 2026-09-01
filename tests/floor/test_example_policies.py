"""The three example policies, each driven by a fake coppice socket.

Each script connects, names itself, subscribes, and reacts to a fixture of
state events. The tests assert the exact requests it sent, and that none
of them is an allow or a deny.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from tests.floor.plugin_fakes import PLUGINS, FakeFloor, assert_ended, fake_gh, run_plugin, state


@pytest.fixture
def sock(tmp_path: Path) -> Path:
    return tmp_path / "f.sock"


def manifest(plugin: str) -> dict:
    return json.loads((PLUGINS / plugin / "manifest.json").read_text())


def acting(floor: FakeFloor) -> list[dict]:
    """Every request that is not the hello, a subscribe, or a read."""
    reads = {"hello", "events.subscribe", "task.list", "pane.list"}
    return [
        {k: v for k, v in r.items() if k != "id"}
        for r in floor.requests
        if r.get("cmd") not in reads
    ]


def assert_never_allows(floor: FakeFloor, plugin: str) -> None:
    cmds = {r.get("cmd") for r in floor.requests}
    assert "agent.allow" not in cmds and "agent.deny" not in cmds
    needs = set(manifest(plugin)["needs"])
    for r in floor.requests:
        cmd = r.get("cmd")
        if cmd in {"hello", "events.subscribe"}:
            continue
        assert cmd in needs, f"{plugin} sent {cmd}, which its manifest does not ask for"


def test_every_policy_ships_off_with_a_one_line_about():
    for plugin in ("merge-on-green", "close-quiet", "turn-budget"):
        m = manifest(plugin)
        assert m["kind"] == "policy"
        assert m["about"] and "\n" not in m["about"]
        assert "agent.allow" not in m["needs"] and "agent.deny" not in m["needs"]
        first = (PLUGINS / plugin / m["run"]).read_text().splitlines()[:5]
        assert first[0] == "#!/usr/bin/env python3"
        assert "# /// script" in first and "# dependencies = []" in first


def _merge(tmp_path: Path, sock: Path, pr_state: str, checks: list[int], events: list[dict]):
    log = tmp_path / "gh.log"
    fake_gh(tmp_path / "bin", log, pr_state, checks)
    wt = tmp_path / "auth"
    wt.mkdir()

    def tasks(req: dict) -> dict:
        return {
            "tasks": [
                {"id": "t1", "label": "auth", "worktree": str(wt), "panes": ["w1:p1"]},
                {"id": "t2", "label": "notes", "worktree": "", "panes": ["w2:p1"]},
            ]
        }

    floor = FakeFloor(sock, answers={"task.list": tasks}, events=events, quiet=1.5)
    proc = run_plugin(
        "merge-on-green",
        "merge-on-green.py",
        sock,
        config={"poll_seconds": 0.05, "max_polls": 5},
        extra_env={"PATH": f"{tmp_path / 'bin'}:/usr/bin:/bin"},
    )
    floor.close()
    calls = [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []
    return floor, proc, calls


def test_merge_on_green_prompts_a_done_pane_once_checks_pass(tmp_path, sock):
    floor, proc, calls = _merge(
        tmp_path, sock, "OPEN", [0], [state("w1:p1", "working", 1.0), state("w1:p1", "done", 2.0)]
    )
    assert_ended(proc)
    assert acting(floor) == [
        {"cmd": "agent.prompt", "pane": "w1:p1", "text": "checks are green, merge"},
        {"cmd": "floor.note", "text": "w1:p1: checks are green. Asked it to merge."},
    ]
    assert floor.requests[0] == {
        "id": "p1",
        "cmd": "hello",
        "role": "plugin",
        "plugin": "merge-on-green",
    }
    assert all("merge" not in c for c in calls), calls
    assert ["pr", "checks"] in calls
    assert_never_allows(floor, "merge-on-green")


def test_merge_on_green_waits_for_pending_checks(tmp_path, sock):
    floor, proc, calls = _merge(tmp_path, sock, "OPEN", [8, 8, 0], [state("w1:p1", "done", 2.0)])
    assert_ended(proc)
    assert [r["cmd"] for r in acting(floor)] == ["agent.prompt", "floor.note"]
    assert calls.count(["pr", "checks"]) == 3
    assert all("merge" not in c for c in calls), calls


def test_merge_on_green_does_not_prompt_on_failed_checks(tmp_path, sock):
    floor, proc, calls = _merge(tmp_path, sock, "OPEN", [1], [state("w1:p1", "done", 2.0)])
    assert_ended(proc)
    assert acting(floor) == [
        {"cmd": "floor.note", "text": "w1:p1: checks did not pass. Not asking it to merge."},
    ]
    assert_never_allows(floor, "merge-on-green")


def test_merge_on_green_ignores_a_pane_with_no_worktree_or_no_open_pr(tmp_path, sock):
    floor, proc, calls = _merge(
        tmp_path, sock, "", [0], [state("w2:p1", "done", 2.0), state("w1:p1", "done", 3.0)]
    )
    assert_ended(proc)
    assert acting(floor) == []
    # Only the pane with a worktree reaches gh, and only to look for a PR.
    assert calls == [["pr", "view", "--json", "state,number"]]


def test_merge_on_green_acts_on_the_edge_only(tmp_path, sock):
    floor, proc, calls = _merge(
        tmp_path, sock, "OPEN", [0], [state("w1:p1", "done", 2.0), state("w1:p1", "done", 3.0)]
    )
    assert [r["cmd"] for r in acting(floor)] == ["agent.prompt", "floor.note"]


def test_close_quiet_closes_a_pane_idle_for_sixty_minutes(sock):
    events = [
        state("w1:p1", "idle", 1000.0),
        state("w1:p2", "idle", 2000.0),
        state("w3:p1", "idle", 1000.0),
        state("w3:p1", "working", 1500.0),
        state("w2:p1", "working", 4601.0),
    ]
    floor = FakeFloor(sock, events=events)
    proc = run_plugin("close-quiet", "close-quiet.py", sock)
    floor.close()
    assert_ended(proc)
    assert acting(floor) == [
        {"cmd": "pane.close", "pane": "w1:p1"},
        {"cmd": "floor.note", "text": "closed w1:p1 after 60 minutes idle. Its worktree stays."},
    ]
    assert_never_allows(floor, "close-quiet")


def test_close_quiet_reads_its_minutes_from_config(sock):
    events = [state("w1:p1", "idle", 1000.0), state("w2:p1", "working", 1700.0)]
    floor = FakeFloor(sock, events=events)
    proc = run_plugin("close-quiet", "close-quiet.py", sock, config={"idle_minutes": 10})
    floor.close()
    assert_ended(proc)
    assert [r["cmd"] for r in acting(floor)] == ["pane.close", "floor.note"]
    assert acting(floor)[1]["text"] == "closed w1:p1 after 10 minutes idle. Its worktree stays."


def test_turn_budget_pauses_a_pane_past_its_budget(sock):
    events = []
    ts = 1.0
    for _ in range(4):
        events.append(state("w1:p1", "working", ts))
        events.append(state("w1:p1", "idle", ts + 0.5))
        ts += 1
    events.append(state("w2:p1", "working", ts))
    floor = FakeFloor(sock, events=events)
    proc = run_plugin("turn-budget", "turn-budget.py", sock, config={"budget": 3})
    floor.close()
    assert_ended(proc)
    assert acting(floor) == [
        {"cmd": "pane.send_keys", "pane": "w1:p1", "keys": ["ctrl+c"]},
        {"cmd": "floor.note", "text": "w1:p1 paused: past 3 turns. Prompt it to continue."},
    ]
    assert_never_allows(floor, "turn-budget")


def test_turn_budget_uses_the_manifest_budget_by_default(sock):
    budget = manifest("turn-budget")["config"]["budget"]
    events = [
        state("w1:p1", "working" if i % 2 == 0 else "idle", float(i)) for i in range(budget * 2)
    ]
    floor = FakeFloor(sock, events=events)
    proc = run_plugin(
        "turn-budget", "turn-budget.py", sock, config=manifest("turn-budget")["config"]
    )
    floor.close()
    assert_ended(proc)
    assert acting(floor) == []


def test_a_policy_that_is_refused_says_so_and_exits(sock):
    def refuse(req):
        return {"error": {"code": "unauthorized", "message": "plugin close-quiet did not ask"}}

    floor = FakeFloor(
        sock,
        answers={"pane.close": refuse},
        events=[state("w1:p1", "idle", 1000.0), state("w2:p1", "working", 4700.0)],
    )
    proc = run_plugin("close-quiet", "close-quiet.py", sock)
    floor.close()
    assert_ended(proc)
    assert "did not ask" in proc.stderr


def test_merge_on_green_goes_on_after_a_refused_prompt(tmp_path, sock):
    log = tmp_path / "gh.log"
    fake_gh(tmp_path / "bin", log, "OPEN", [0])
    wt = tmp_path / "wt"
    wt.mkdir()

    def tasks(req):
        return {"tasks": [{"id": "t1", "worktree": str(wt), "panes": ["w1:p1", "w3:p1"]}]}

    def prompt(req):
        if req["pane"] == "w1:p1":
            return {"error": {"code": "unauthorized", "message": "a pane can propose."}}
        return {}

    floor = FakeFloor(
        sock,
        answers={"task.list": tasks, "agent.prompt": prompt},
        events=[state("w1:p1", "done", 1.0), state("w3:p1", "done", 2.0)],
        quiet=1.5,
    )
    proc = run_plugin(
        "merge-on-green",
        "merge-on-green.py",
        sock,
        config={"poll_seconds": 0.05, "max_polls": 2},
        extra_env={"PATH": f"{tmp_path / 'bin'}:/usr/bin:/bin"},
    )
    floor.close()
    assert_ended(proc)
    assert [r["pane"] for r in floor.sent("agent.prompt")] == ["w1:p1", "w3:p1"]
    assert "a pane can propose" in proc.stderr


def test_a_refused_hello_is_a_failure(sock):
    def refuse(req):
        return {"error": {"code": "bad_request", "message": "no plugin turn-budget is enabled."}}

    floor = FakeFloor(sock, answers={"hello": refuse})
    proc = run_plugin("turn-budget", "turn-budget.py", sock)
    floor.close()
    assert proc.returncode == 1
    assert "is enabled" in proc.stderr


def test_close_quiet_does_not_trust_a_missing_or_old_ts(sock):
    no_ts = state("w1:p1", "idle", 0.0)
    del no_ts["ts"]
    events = [
        state("w2:p1", "working", 5000.0),
        no_ts,
        state("w3:p1", "idle", 10.0),
        state("w2:p1", "idle", 5100.0),
        state("w2:p1", "working", 5200.0),
    ]
    floor = FakeFloor(sock, events=events)
    proc = run_plugin("close-quiet", "close-quiet.py", sock)
    floor.close()
    assert_ended(proc)
    assert floor.sent("pane.close") == []
