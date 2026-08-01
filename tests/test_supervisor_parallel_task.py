"""Phase-follow-on: parallel SUBAGENT (task) steps under the Supervisor (v0.40).

Deterministic steps already parallelize (test_supervisor_parallel.py). These
prove task steps join the concurrent prefetch ONLY when their executor declares
``parallel_safe`` (true only under an unlimited budget), that the ``max_parallel``
semaphore actually caps in-flight LLM calls, and that a prefetched-but-preempted
step is receipted (never a silent skip).
"""

from __future__ import annotations

import threading
import time

from opendaisugi.approval import CallbackStrategy
from opendaisugi.executor import ExecutorResult
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Permission, TaskStep
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


class _FakeTaskExecutor:
    """A stand-in 'task' executor that sleeps and tracks peak concurrency."""

    def __init__(
        self, *, delay: float = 0.0, fail_ids=(), model: str = "haiku", parallel_safe: bool = True
    ) -> None:
        self.delay = delay
        self.fail_ids = set(fail_ids)
        self.model = model
        self.parallel_safe = parallel_safe
        self._inflight = 0
        self.max_inflight = 0
        self._lock = threading.Lock()

    def run(self, step, *, timeout_s, max_output_bytes):
        with self._lock:
            self._inflight += 1
            self.max_inflight = max(self.max_inflight, self._inflight)
        try:
            time.sleep(self.delay)
            rc = 1 if step.id in self.fail_ids else 0
            return ExecutorResult(
                rc=rc,
                stdout=f"ran {step.id}",
                duration_ms=self.delay * 1000,
                timed_out=False,
                model=self.model,
            )
        finally:
            with self._lock:
                self._inflight -= 1


def _task_plan(*ids: str) -> ActionPlan:
    return ActionPlan(
        source="t",
        task="x",
        steps=[TaskStep(id=i, prompt=f"do {i}") for i in ids],
    )


def _env() -> Envelope:
    # Task steps are LLM delegation; a low-stakes envelope permits them.
    return Envelope(generated_by="t", task="x", permissions=Permission())


def _sup(executor, *, max_parallel=1, journal=None):
    return Supervisor(
        executors={"task": executor},
        approval=CallbackStrategy(lambda s, e: True),
        journal=journal,
        max_parallel=max_parallel,
    )


async def test_task_steps_overlap_when_executor_parallel_safe():
    delay = 0.2
    seq = _sup(_FakeTaskExecutor(delay=delay), max_parallel=1)
    t0 = time.monotonic()
    s_seq = await seq.run(_task_plan("a", "b", "c"), _env())
    seq_time = time.monotonic() - t0

    par = _sup(_FakeTaskExecutor(delay=delay), max_parallel=4)
    t0 = time.monotonic()
    s_par = await par.run(_task_plan("a", "b", "c"), _env())
    par_time = time.monotonic() - t0

    assert s_seq.status == RunStatus.SUCCEEDED
    assert s_par.status == RunStatus.SUCCEEDED
    assert {o.step_id for o in s_par.steps} == {"a", "b", "c"}
    assert all(o.status == "succeeded" for o in s_par.steps)
    # each outcome carries the model that produced it (from the result, not self.last)
    assert all(o.model_id == "haiku" for o in s_par.steps)
    # 3 sleeps overlap to ~1, sequential ~3
    assert seq_time > delay * 2.5
    assert par_time < seq_time * 0.6


async def test_task_steps_stay_sequential_when_not_parallel_safe():
    """An executor that declines parallelism (e.g. a *budgeted* task executor)
    must run sequentially even at max_parallel>1 — the gate is the executor's
    own parallel_safe flag, not the step type."""
    delay = 0.15
    ex = _FakeTaskExecutor(delay=delay, parallel_safe=False)
    sup = _sup(ex, max_parallel=4)
    t0 = time.monotonic()
    s = await sup.run(_task_plan("a", "b", "c"), _env())
    elapsed = time.monotonic() - t0
    assert s.status == RunStatus.SUCCEEDED
    assert ex.max_inflight == 1  # never overlapped
    assert elapsed > delay * 2.5  # ~0.45s, i.e. serial


async def test_semaphore_caps_inflight_concurrency():
    """max_parallel is a real cap, not a boolean: 4 independent parallel-safe task
    steps at max_parallel=2 must never have more than 2 LLM calls in flight."""
    ex = _FakeTaskExecutor(delay=0.1)
    sup = _sup(ex, max_parallel=2)
    s = await sup.run(_task_plan("a", "b", "c", "d"), _env())
    assert s.status == RunStatus.SUCCEEDED
    assert ex.max_inflight <= 2
    assert ex.max_inflight >= 2  # it did use the allowance


async def test_preempted_prefetch_is_receipted_not_silently_skipped(tmp_path):
    """A prefetched task step that ran (spent tokens) but the loop broke before
    reaching it must still get a receipt — a silent skip is the one thing this
    layer must never do."""
    j = Journal(data_dir=tmp_path)
    # "a" fails; b, c, d were prefetched concurrently and succeeded.
    ex = _FakeTaskExecutor(delay=0.05, fail_ids={"a"})
    sup = _sup(ex, max_parallel=4, journal=j)
    s = await sup.run(_task_plan("a", "b", "c", "d"), _env())

    assert s.status == RunStatus.FAILED
    receipted = {r.step_id for r in j.receipts_for_run(s.id)}
    # every step that actually executed has a receipt — none silently dropped
    assert {"a", "b", "c", "d"} <= receipted
    # and the run's own outcome list accounts for them too
    assert {o.step_id for o in s.steps} >= {"a", "b", "c", "d"}


async def test_flush_journal_failure_does_not_sink_the_run():
    """The preempted-prefetch flush writes to the journal inside finally, right
    before the run itself is journaled. A receipt-write failure there must NOT
    replace the run status or stop the session from being recorded — the exact
    invariant _check_run_integrity guards for its read, applied to this write."""

    class _RaisingJournal:
        def __init__(self):
            self.logged = None

        def append_receipt(self, receipt):
            # Fail only on the FLUSH receipts (b/c/d) — the main-loop write for the
            # failed step "a" succeeds, isolating the flush-in-finally path.
            if receipt.step_id in {"b", "c", "d"}:
                raise RuntimeError("disk full")  # the flush's receipt write blows up

        def receipts_for_run(self, run_id):
            return []

        def write_refinement(self, record, *, session_id):
            pass

        def log_run(self, session, *, task, envelope, plan):
            self.logged = session.id
            return "trace_x"

    j = _RaisingJournal()
    ex = _FakeTaskExecutor(delay=0.05, fail_ids={"a"})
    sup = _sup(ex, max_parallel=4, journal=j)
    s = await sup.run(_task_plan("a", "b", "c", "d"), _env())

    # The failing step still defines the run status; the flush did not hijack it.
    assert s.status == RunStatus.FAILED
    # The session was still journaled despite the receipt-write failures.
    assert j.logged == s.id
    assert s.trace_id == "trace_x"
