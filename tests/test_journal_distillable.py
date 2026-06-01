"""Tests for Journal.list_successful_traces (v0.3.0)."""

import json
import sqlite3
import time
from datetime import datetime, timezone

from opendaisugi.journal import DistillableTrace, Journal
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    Permission,
    ShellStep,
    VerificationResult,
)


def _write_trace(journal, task: str, *, ok: bool, run_status: str | None = "succeeded"):
    """Seed a trace row in the SQLite index.

    list_successful_traces() reads from the index only, so we insert
    directly rather than routing through log_run() / RunSession.
    """
    env = Envelope(generated_by="test", task=task, permissions=Permission(shell=True))
    plan = ActionPlan(source="t", task=task, steps=[ShellStep(id="s1", command="echo hi")])
    result = VerificationResult(
        ok=ok, violations=[], warnings=[],
        envelope_id=env.id, plan_id=plan.id, duration_ms=0.1,
    )
    trace_id = f"t-{task.replace(' ', '-')}-{time.time_ns()}"
    created_at = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%fZ")
    with sqlite3.connect(journal._db_path) as con:
        con.execute(
            "INSERT INTO traces "
            "(id, created_at, task, plan_id, envelope_id, ok, duration_ms, "
            " violations_json, run_id, run_status) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                trace_id, created_at, task, plan.id, env.id,
                1 if result.ok else 0, result.duration_ms,
                json.dumps([]), "run_x", run_status,
            ),
        )
    return trace_id


def test_list_successful_traces_returns_only_succeeded(tmp_path):
    j = Journal(data_dir=tmp_path)
    _write_trace(j, "task A", ok=True, run_status="succeeded")
    _write_trace(j, "task B", ok=True, run_status="failed")
    _write_trace(j, "task C", ok=False, run_status=None)

    results = j.list_successful_traces()
    assert len(results) == 1
    assert all(isinstance(t, DistillableTrace) for t in results)
    assert results[0].task == "task A"
    assert results[0].run_status == "succeeded"


def test_list_successful_traces_since_filter(tmp_path):
    j = Journal(data_dir=tmp_path)
    _write_trace(j, "old task", ok=True, run_status="succeeded")
    time.sleep(0.01)
    cutoff = time.time()
    time.sleep(0.01)
    _write_trace(j, "new task", ok=True, run_status="succeeded")

    results = j.list_successful_traces(since=cutoff)
    assert len(results) == 1
    assert results[0].task == "new task"


def test_list_successful_traces_newest_first(tmp_path):
    j = Journal(data_dir=tmp_path)
    _write_trace(j, "first", ok=True, run_status="succeeded")
    time.sleep(0.01)
    _write_trace(j, "second", ok=True, run_status="succeeded")

    results = j.list_successful_traces()
    assert [t.task for t in results] == ["second", "first"]
