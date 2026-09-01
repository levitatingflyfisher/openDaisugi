"""Proves the Z3 solve lock actually serializes concurrent solves.

Plan 2's resident gate server (ADR-0017) runs thread-per-connection so a
later plan's ~90s operator-ask can't head-of-line-block other gate requests.
But Z3's process-global default context (used by every bare ``z3.Solver()``
in this codebase) is not safe for concurrent native solving from multiple OS
threads -- Z3 releases the GIL while it works, so two threads solving at
once is a data race that can produce a wrong, non-crashing verdict, not just
a crash.

These tests don't just check that a lock object exists -- for each guarded
call site, they hold the (monkeypatched) lock from the test's own thread and
prove the guarded function genuinely blocks on it until released. That's a
deterministic proof of mutual exclusion without ever running two threads of
real Z3 native work concurrently: an actual concurrent-solve stress test was
tried first and found flaky for reasons unrelated to this fix (see the
"NOT tested" note below) -- this design proves the same claim without
tripping over that.

NOT tested here: literally running N threads through real Z3 solves at once
and asserting max-observed-concurrency == 1. That was the first draft, and
it intermittently hung at process exit inside Z3's native `Z3_del_context`
-- concurrent AST-build+solve leaves Python-level z3 wrapper objects (Bool/
String/Solver) whose `__del__` (native ref-count decrement) fires when a
guarded function's stack frame is torn down, which happens *after* the
`with` block's `__exit__` already released the lock. That off-lock native
finalization is a real, narrower residual race this lock does not close
(the sound fix is a per-call `z3.Context()`, out of scope here -- see
ADR-0017). It's a platform-level teardown hazard, not evidence against the
lock: a "held lock blocks a concurrent caller" test proves the actual
contract without exercising that hazard.
"""

from __future__ import annotations

import threading

from opendaisugi import subsumption, vacuity, z3_checks
from opendaisugi.models import ActionPlan, Envelope, Permission, ShellStep
from opendaisugi.predicate import Equals, parse_expression
from opendaisugi.predicate_z3 import verify_predicate_z3
from opendaisugi.subsumption import _patterns_subsume, envelope_subsumes
from opendaisugi.z3_checks import check_envelope_self_consistency, check_plan_against_envelope

_BLOCK_WINDOW_S = 0.2
_JOIN_TIMEOUT_S = 5.0


def _plan_and_envelope() -> tuple[ActionPlan, Envelope]:
    env = Envelope(
        generated_by="test",
        task="test",
        permissions=Permission(shell=True, shell_allowlist=["python3"]),
    )
    plan = ActionPlan(
        source="test",
        task="test",
        steps=[ShellStep(id="s1", command="python3 chart.py")],
    )
    return plan, env


def _predicate_expr():
    return parse_expression(
        {
            "op": "forall_steps",
            "pred": {"op": "not_matches", "path": "command", "regex": r"^rm "},
        }
    )


def _assert_blocks_until_released(monkeypatch, call) -> None:
    """Hold Z3_SOLVE_LOCK on this thread; assert `call()` (run on a worker
    thread) doesn't return until the lock is released, then completes
    promptly once it is."""
    lock = threading.Lock()
    monkeypatch.setattr(z3_checks, "Z3_SOLVE_LOCK", lock)

    lock.acquire()
    done = threading.Event()

    def worker() -> None:
        call()
        done.set()

    t = threading.Thread(target=worker)
    t.start()
    try:
        # Still held: the guarded call must not have completed yet.
        assert not done.wait(timeout=_BLOCK_WINDOW_S), (
            "guarded call returned while Z3_SOLVE_LOCK was still held elsewhere "
            "-- it isn't actually taking the shared lock"
        )
    finally:
        lock.release()
    assert done.wait(timeout=_JOIN_TIMEOUT_S), "guarded call never completed after lock release"
    t.join(timeout=_JOIN_TIMEOUT_S)
    assert not t.is_alive()


def test_lock_is_shared_by_every_guarded_module():
    """One lock object, not look-alikes -- z3_checks, predicate_z3, subsumption, vacuity."""
    import opendaisugi.predicate_z3 as predicate_z3_mod

    assert predicate_z3_mod.z3_checks.Z3_SOLVE_LOCK is z3_checks.Z3_SOLVE_LOCK
    assert subsumption.z3_checks.Z3_SOLVE_LOCK is z3_checks.Z3_SOLVE_LOCK
    assert vacuity.z3_checks.Z3_SOLVE_LOCK is z3_checks.Z3_SOLVE_LOCK


def test_check_plan_against_envelope_blocks_while_lock_is_held(monkeypatch):
    plan, env = _plan_and_envelope()
    _assert_blocks_until_released(monkeypatch, lambda: check_plan_against_envelope(plan, env))


def test_check_envelope_self_consistency_blocks_while_lock_is_held(monkeypatch):
    _, env = _plan_and_envelope()
    _assert_blocks_until_released(monkeypatch, lambda: check_envelope_self_consistency(env))


def test_verify_predicate_z3_blocks_while_lock_is_held(monkeypatch):
    plan, env = _plan_and_envelope()
    expr = _predicate_expr()
    _assert_blocks_until_released(monkeypatch, lambda: verify_predicate_z3(expr, plan, env))


def test_patterns_subsume_blocks_while_lock_is_held(monkeypatch):
    _assert_blocks_until_released(
        monkeypatch,
        lambda: _patterns_subsume(["a"], ["a"], label="file_read", timeout_ms=2000),
    )


def test_envelope_subsumes_blocks_while_lock_is_held(monkeypatch):
    env = Envelope(
        generated_by="t",
        task="t",
        permissions=Permission(shell=True, shell_allowlist=["echo", "ls"]),
    )
    _assert_blocks_until_released(monkeypatch, lambda: envelope_subsumes(env, env))


def test_check_vacuity_blocks_while_lock_is_held(monkeypatch):
    # Bypass the LRU memo so the call always reaches the real solve region —
    # a cache hit would return without ever touching Z3_SOLVE_LOCK.
    vacuity.clear_vacuity_cache()
    expr = Equals(path="command", value="echo hi")
    _assert_blocks_until_released(monkeypatch, lambda: vacuity.check_vacuity(expr, timeout_ms=2000))
