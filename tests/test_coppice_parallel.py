"""The parallel runner of clients/coppice_compare.py: the job count formula,
the memory wait, the port allocation and the output order. No coppice binary
runs here."""

from __future__ import annotations

import sys
import threading
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

from clients import coppice_compare as cc  # noqa: E402

MIB = 1024  # KiB


def test_auto_jobs_is_min_of_cpus_and_memory():
    # 8 GiB free, 1.5 GiB reserve, 300 MiB per file: 6.5 GiB / 300 MiB = 22.
    assert cc.auto_jobs(cpus=4, avail_kib=8192 * MIB) == 4
    assert cc.auto_jobs(cpus=64, avail_kib=8192 * MIB) == 22


def test_auto_jobs_is_at_least_one():
    assert cc.auto_jobs(cpus=8, avail_kib=1000 * MIB) == 1
    assert cc.auto_jobs(cpus=0, avail_kib=10**9) == 1


def test_parse_jobs():
    assert cc.parse_jobs("1", cpus=4, avail_kib=10**9) == 1
    assert cc.parse_jobs("3", cpus=4, avail_kib=10**9) == 3
    assert cc.parse_jobs("auto", cpus=2, avail_kib=10**9) == 2
    assert cc.parse_jobs("0", cpus=4, avail_kib=10**9) == 1


def test_headroom_takes_the_smaller_of_host_and_cgroup():
    assert cc.headroom_kib(host_kib=5000, cg_max=None, cg_cur=None) == 5000
    assert cc.headroom_kib(host_kib=5000, cg_max=2000 * 1024, cg_cur=500 * 1024) == 1500
    assert cc.headroom_kib(host_kib=1000, cg_max=2000 * 1024, cg_cur=500 * 1024) == 1000


def test_cgroup_without_limit_is_ignored(tmp_path):
    cg = tmp_path / "cg"
    cg.mkdir()
    (cg / "memory.max").write_text("max\n")
    (cg / "memory.current").write_text("123\n")
    assert cc.read_cgroup_memory(cg) == (None, None)
    (cg / "memory.max").write_text("4294967296\n")
    assert cc.read_cgroup_memory(cg) == (4294967296, 123)
    assert cc.read_cgroup_memory(tmp_path / "nope") == (None, None)


def test_gate_waits_while_memory_is_low_and_a_file_runs():
    avail = [100]  # KiB, below the reserve
    gate = cc.MemoryGate(lambda: avail[0], reserve_kib=1000, poll=0.01)
    gate.acquire()  # first file always starts: nothing to wait for
    started = threading.Event()

    def second():
        gate.acquire()
        started.set()

    t = threading.Thread(target=second)
    t.start()
    time.sleep(0.15)
    assert not started.is_set()
    avail[0] = 5000  # memory returns
    t.join(2)
    assert started.is_set()


def test_gate_releases_a_waiter_when_a_file_finishes():
    gate = cc.MemoryGate(lambda: 0, reserve_kib=1000, poll=0.01)
    gate.acquire()
    started = threading.Event()

    def second():
        gate.acquire()
        started.set()

    t = threading.Thread(target=second)
    t.start()
    time.sleep(0.1)
    assert not started.is_set()
    gate.release()  # nothing runs now, so the waiter may start despite low memory
    t.join(2)
    assert started.is_set()


def test_ports_are_never_handed_out_twice():
    got: list[int] = []
    lock = threading.Lock()

    def grab():
        for _ in range(20):
            p = cc.free_port()
            with lock:
                got.append(p)

    ts = [threading.Thread(target=grab) for _ in range(8)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    try:
        assert len(got) == 160 and len(set(got)) == 160
    finally:
        cc.release_ports(got)


def test_released_ports_come_back():
    p = cc.free_port()
    assert p in cc._reserved_ports
    cc.release_ports([p])
    assert p not in cc._reserved_ports


def test_results_print_in_file_order_whatever_the_finish_order():
    files = [cc.CaseFile(f"f{i}", []) for i in range(5)]
    delays = {"f0": 0.25, "f1": 0.05, "f2": 0.15, "f3": 0.0, "f4": 0.1}

    def run(cf):
        time.sleep(delays[cf.name])
        return cc.FileResult(rows=[{"file": cf.name, "case": 1, "class": "agree"}], leaks=[])

    order = [cf.name for cf, _ in cc.run_files(files, jobs=5, run=run, gate=None)]
    assert order == [f"f{i}" for i in range(5)]


def _row(name, cls, diff=None):
    r = {"file": name, "case": 3, "class": cls}
    if diff:
        r["diff"] = diff
    return r


def test_disagreeing_file_is_rerun_alone_and_kept_as_masked():
    files = [cc.CaseFile("a", []), cc.CaseFile("b", [])]
    calls = []

    def run(cf):
        in_parent = threading.current_thread() is threading.main_thread()
        calls.append((cf.name, in_parent))
        if cf.name == "b" and not in_parent:
            return cc.FileResult([_row("b", "disagree", [".x: 1 vs 2"])], [])
        return cc.FileResult([_row(cf.name, "agree")], [])

    out = list(cc.run_files(files, jobs=2, run=run, gate=None))
    assert [r.rows[0]["class"] for _, r in out] == ["agree", "agree"]
    res = out[1][1]
    assert res.masked and not out[0][1].masked
    # The first run's diff is kept in the note, with the rerun verdict.
    assert "rerun b alone" in res.note and "masked" in res.note
    assert "b:3" in res.note and ".x: 1 vs 2" in res.note
    assert calls[-1] == ("b", True)  # the rerun ran alone, in the parent


def test_still_disagreeing_after_rerun_is_reported_not_masked():
    files = [cc.CaseFile("a", []), cc.CaseFile("b", [])]

    def run(cf):
        return cc.FileResult([_row(cf.name, "disagree" if cf.name == "b" else "agree")], [])

    out = list(cc.run_files(files, jobs=2, run=run, gate=None))
    assert out[1][1].rows[0]["class"] == "disagree"
    assert not out[1][1].masked and "disagrees alone too" in out[1][1].note


def test_masked_line_counts_and_names():
    assert cc.masked_line(["a", "b"]) == (
        "2 masked files (disagreed under --jobs, agreed alone): a, b"
    )


def test_crash_stops_servers_before_the_pool_is_waited_for():
    files = [cc.CaseFile("a", []), cc.CaseFile("b", [])]
    servers_stopped = threading.Event()
    seen = {}

    def run(cf):
        if cf.name == "a":
            time.sleep(0.1)
            raise RuntimeError("worker crash")
        # b is "running" until its servers are stopped, as a real file is.
        seen["b_ended_by_abort"] = servers_stopped.wait(5)
        return cc.FileResult([], [])

    try:
        list(cc.run_files(files, jobs=2, run=run, gate=None, abort=servers_stopped.set))
        raise AssertionError("the crash must propagate")
    except RuntimeError:
        pass
    assert seen["b_ended_by_abort"] is True


def test_keyboard_interrupt_in_a_worker_aborts():
    files = [cc.CaseFile("a", [])]
    aborted = []

    def run(cf):
        raise KeyboardInterrupt

    try:
        list(cc.run_files(files, jobs=2, run=run, gate=None, abort=lambda: aborted.append(1)))
    except KeyboardInterrupt:
        pass
    assert aborted == [1]


def test_bind_failure_at_start_retries_with_new_ports(tmp_path, monkeypatch):
    side = cc.Side("go", "/nonexistent", tmp_path / "w", tmp_path)
    side.work.mkdir()
    tried = []

    def once(self, config, env=None):
        self.ports = [cc.free_port(), cc.free_port()]
        self.paths = {"{PORT}": str(self.ports[0])}
        tried.append(list(self.ports))
        if len(tried) == 1:
            (self.work / "server.log").write_text("listen: address already in use\n")
            raise RuntimeError("the go server did not start")

    monkeypatch.setattr(cc.Side, "_start_once", once)
    try:
        side.start([])
        assert len(tried) == 2
        assert set(tried[0]).isdisjoint(tried[1])
        assert not set(tried[0]) & cc._reserved_ports  # the failed ports came back
    finally:
        cc.release_ports(tried[-1])


def test_other_start_failures_are_not_retried(tmp_path, monkeypatch):
    side = cc.Side("go", "/nonexistent", tmp_path / "w", tmp_path)
    side.work.mkdir()
    n = []

    def once(self, config, env=None):
        n.append(1)
        self.paths = {"{PORT}": "1"}
        (self.work / "server.log").write_text("panic: something else\n")
        raise RuntimeError("did not start")

    monkeypatch.setattr(cc.Side, "_start_once", once)
    try:
        side.start([])
    except RuntimeError:
        pass
    assert len(n) == 1


def test_jobs_text_that_is_not_a_number_is_a_clear_error():
    try:
        cc.parse_jobs("abc", cpus=2, avail_kib=10**9)
        raise AssertionError("no error")
    except ValueError as exc:
        assert "--jobs" in str(exc)


def test_cgroup_limit_on_a_parent_binds_the_leaf(tmp_path):
    parent = tmp_path / "slice"
    leaf = parent / "scope"
    leaf.mkdir(parents=True)
    (leaf / "memory.max").write_text("max\n")
    (parent / "memory.max").write_text("1000000\n")
    (parent / "memory.current").write_text("400000\n")
    assert cc.read_cgroup_memory(leaf) == (1000000, 400000)


def test_serial_files_run_after_the_batch_and_keep_their_place():
    files = [cc.CaseFile("cases/tui/windows.jsonl", []), cc.CaseFile("x", []), cc.CaseFile("y", [])]
    running: list[str] = []
    peak = [0]
    lock = threading.Lock()

    def run(cf):
        with lock:
            running.append(cf.name)
            peak[0] = max(peak[0], len(running))
        time.sleep(0.05)
        with lock:
            running.remove(cf.name)
        return cc.FileResult(rows=[], leaks=[])

    out = [cf.name for cf, _ in cc.run_files(files, jobs=3, run=run, gate=None)]
    assert out == ["cases/tui/windows.jsonl", "x", "y"]
    # x and y overlapped; the serial file was alone (peak 2, not 3).
    assert peak[0] == 2


def test_serial_directive_is_parsed():
    cf = cc.parse_cases("t", "#!serial\n{}\n{}\n")
    assert cf.serial and cc.is_serial(cf)
    assert not cc.is_serial(cc.parse_cases("t", "{}\n{}\n"))
