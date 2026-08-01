"""Phase E perf: opt-in concurrent execution of independent deterministic steps.

Default (max_parallel=1) is byte-identical sequential behaviour (guarded by the
whole existing suite). These tests prove the opt-in path (a) actually overlaps
independent deterministic steps and (b) produces the same outcomes.
"""

from __future__ import annotations

import time

from opendaisugi.executor import ExecutorResult
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor


class _SleepShellExecutor:
    """A fake 'shell' executor that sleeps, to make concurrency observable."""

    def __init__(self, delay: float) -> None:
        self.delay = delay

    def run(self, step, *, timeout_s, max_output_bytes):
        time.sleep(self.delay)
        return ExecutorResult(
            rc=0, stdout=f"ran {step.id}", duration_ms=self.delay * 1000, timed_out=False
        )


def _plan() -> ActionPlan:
    # Three independent shell steps → one dependency level.
    return ActionPlan(
        source="t",
        task="x",
        steps=[
            ShellStep(id="a", command="echo a"),
            ShellStep(id="b", command="echo b"),
            ShellStep(id="c", command="echo c"),
        ],
    )


def _env() -> Envelope:
    return Envelope(
        generated_by="t",
        task="x",
        permissions=Permission(shell=True, shell_allowlist=["echo"]),
    )


async def test_parallel_overlaps_independent_deterministic_steps():
    delay = 0.2
    seq = Supervisor(executors={"shell": _SleepShellExecutor(delay)}, max_parallel=1)
    t0 = time.monotonic()
    s_seq = await seq.run(_plan(), _env())
    seq_time = time.monotonic() - t0

    par = Supervisor(executors={"shell": _SleepShellExecutor(delay)}, max_parallel=4)
    t0 = time.monotonic()
    s_par = await par.run(_plan(), _env())
    par_time = time.monotonic() - t0

    # Same outcome, either way.
    assert s_seq.status == RunStatus.SUCCEEDED
    assert s_par.status == RunStatus.SUCCEEDED
    assert [o.step_id for o in s_par.steps] == ["a", "b", "c"]
    assert all(o.status == "succeeded" for o in s_par.steps)
    assert {o.stdout for o in s_par.steps} == {"ran a", "ran b", "ran c"}

    # Concurrency: 3 sleeps overlap to ~1, sequential is ~3.
    assert seq_time > delay * 2.5  # ~0.6s
    assert par_time < seq_time * 0.6  # comfortably under the sequential cost


async def test_parallel_default_is_sequential_and_correct():
    # max_parallel=1 must produce the same successful run (no prefetch path).
    sup = Supervisor(executors={"shell": _SleepShellExecutor(0.0)}, max_parallel=1)
    session = await sup.run(_plan(), _env())
    assert session.status == RunStatus.SUCCEEDED
    assert [o.step_id for o in session.steps] == ["a", "b", "c"]


async def test_prefetched_steps_are_not_re_verified(monkeypatch):
    # A prefetched step already ran; the loop must NOT re-verify it (a second
    # verify could time out under concurrent load and reject an executed step).
    # Prove it: each step is verified exactly once (in prefetch), never again.
    from collections import Counter

    from opendaisugi import supervisor as sup_mod

    real_verify_step = sup_mod.verify_step
    seen: list = []

    def counting(step, envelope, **kw):
        seen.append(step.id)
        return real_verify_step(step, envelope, **kw)

    monkeypatch.setattr(sup_mod, "verify_step", counting)
    sup = Supervisor(executors={"shell": _SleepShellExecutor(0.0)}, max_parallel=4)
    session = await sup.run(_plan(), _env())

    assert session.status == RunStatus.SUCCEEDED
    assert Counter(seen) == {"a": 1, "b": 1, "c": 1}  # once each, in prefetch only
