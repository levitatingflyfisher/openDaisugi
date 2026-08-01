"""Supervisor — composition root for runtime-supervised execution.

Owns the RunSession lifecycle: re-verifies the plan, runs each step under
an approval gate, hands results to an executor, and logs the final session
to the journal. Per-step verification feeds rejected steps to a
``FallbackHandler`` (default: halt). A failed step aborts the run.

This is the one place in the codebase that crosses the execution boundary.
Everything else (envelope generation, verify, journal) is side-effect-free
relative to the agent's environment.
"""

from __future__ import annotations

import logging
import time
from datetime import datetime, timezone
from uuid import uuid4

from opendaisugi.aliases import AliasRegistry
from opendaisugi.approval import ApprovalStrategy, default_strategy
from opendaisugi.dag import topological_order
from opendaisugi.executor import ExecutorResult, StepExecutor, default_executors
from opendaisugi.fallback import FallbackHandler, HaltHandler
from opendaisugi.journal import Journal
from opendaisugi.models import ActionPlan, Envelope, Receipt, compute_evidence_hash
from opendaisugi.refinement import RefinementRecord
from opendaisugi.run_session import RunSession, RunStatus, StepOutcome
from opendaisugi.verify import verify, verify_step

_log = logging.getLogger("opendaisugi.supervisor")

# Stage 8 (deed ledger): step kinds that mutate nothing, so a receipt with no
# reversal verdict from the executor is positively "none" (nothing to undo).
# Every other side-effecting kind without a handle is classified "irreversible".
_READ_ONLY_KINDS = frozenset({"file_read", "network"})


def _now_iso() -> str:
    return datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


class Supervisor:
    """Executes verified ActionPlans step-by-step under approval + journal."""

    def __init__(
        self,
        *,
        executors: dict[str, StepExecutor] | None = None,
        approval: ApprovalStrategy | None = None,
        journal: Journal | None = None,
        fallback: FallbackHandler | None = None,
        z3_timeout_ms: int = 500,
        step_timeout_s: int = 30,
        max_output_bytes: int = 10 * 1024 * 1024,
        aliases: AliasRegistry | None = None,
        strict: bool | None = None,
        max_parallel: int = 1,
    ) -> None:
        self._executors: dict[str, StepExecutor] = executors or default_executors()
        self._approval: ApprovalStrategy = approval or default_strategy()
        self._journal = journal
        self._fallback: FallbackHandler = fallback or HaltHandler()
        self._fallback_was_injected = fallback is not None
        self._z3_timeout_ms = z3_timeout_ms
        self._step_timeout_s = step_timeout_s
        self._max_output_bytes = max_output_bytes
        self._aliases = aliases
        # v0.28.3: explicit strict override for the facade path. None preserves
        # verify()'s stake-based default (True for high/physical, False
        # otherwise). True/False forces the policy and is forwarded to
        # verify() AND to stage2.verify_completed_step at step completion.
        self._strict = strict
        # >1 opts into concurrent execution of independent DETERMINISTIC steps
        # (shell/file/network) within a dependency level. Default 1 = the exact
        # sequential behaviour; nothing is prefetched, no path changes.
        self._max_parallel = max(1, int(max_parallel))

    def _resolve_fallback(self, envelope: Envelope) -> FallbackHandler:
        """Determine fallback handler from envelope if none was injected."""
        if self._fallback_was_injected:
            return self._fallback
        strategy = envelope.fallback
        if strategy.strategy == "tier2_recompute":
            from opendaisugi.fallback import RecomputeHandler

            return RecomputeHandler(
                model=strategy.model,
                z3_timeout_ms=self._z3_timeout_ms,
            )
        return self._fallback  # HaltHandler default

    async def run(self, plan: ActionPlan, envelope: Envelope) -> RunSession:
        run_id = f"run_{uuid4().hex[:8]}"
        started_at = _now_iso()
        _log.info(
            "run.start",
            extra={
                "run_id": run_id,
                "envelope_id": envelope.id,
                "plan_id": plan.id,
                "step_count": len(plan.steps),
                "stakes": getattr(envelope, "stakes", None),
            },
        )

        verification = verify(
            plan,
            envelope,
            z3_timeout_ms=self._z3_timeout_ms,
            aliases=self._aliases,
            strict=self._strict,
        )
        session = RunSession(
            id=run_id,
            envelope_id=envelope.id,
            plan_id=plan.id,
            status=RunStatus.PENDING,
            verification=verification,
            steps=[],
            started_at=started_at,
            ended_at=None,
            trace_id=None,
        )

        if not verification.ok:
            session.status = RunStatus.REJECTED
            session.ended_at = _now_iso()
            _log.warning(
                "run.rejected_by_verify",
                extra={
                    "run_id": run_id,
                    "envelope_id": envelope.id,
                    "violation_count": len(verification.violations),
                    "violation_stages": sorted({v.stage for v in verification.violations}),
                },
            )
            self._journal_session(session, plan, envelope, task=plan.task)
            return session

        session.status = RunStatus.RUNNING
        # Default (max_parallel=1): the exact sequential topological order, no
        # prefetch. Parallel mode groups steps by dependency level (still a valid
        # topological order when flattened) so each level's independent
        # deterministic steps can be prefetched concurrently.
        if self._max_parallel > 1:
            from opendaisugi.dag import dependency_levels

            _levels = dependency_levels(plan)
            ordered = [s for level in _levels for s in level]
            _step_level = {s.id: i for i, level in enumerate(_levels) for s in level}
        else:
            _levels = None
            ordered = topological_order(plan)
            _step_level = {}
        _prefetched: dict = {}
        _prefetched_approved: dict = {}
        _prefetched_level = -1

        # Surface envelope-level kwargs into executors that opt in. Only
        # executors that explicitly implement configure_from_envelope
        # participate — others (shell, file_read, etc.) stay untouched.
        seen: set[int] = set()
        for ex in self._executors.values():
            if id(ex) in seen:
                continue
            seen.add(id(ex))
            configure = getattr(ex, "configure_from_envelope", None)
            if callable(configure):
                configure(envelope)

        try:
            try:
                for step in ordered:
                    # Parallel mode: on entering a new dependency level, prefetch
                    # its independent deterministic steps concurrently. The main
                    # loop below still verifies, approves, receipts, and halts on
                    # each step in order — only the executor I/O was hoisted.
                    if _levels is not None:
                        lvl = _step_level[step.id]
                        if lvl != _prefetched_level:
                            _prefetched, _prefetched_approved = await self._prefetch_independent(
                                _levels[lvl], envelope
                            )
                            _prefetched_level = lvl
                    # Per-step verification before execution. Use the
                    # lightweight ``verify_step`` path — the whole-plan
                    # ``verify()`` above already proved envelope self-
                    # consistency and plan-vs-envelope structural checks.
                    # Strip depends_on so singleton-plan DAG check passes.
                    if step.id in _prefetched:
                        # Already verified (and approved) during prefetch, and its
                        # side effect has already happened. Do NOT re-verify: a z3
                        # timeout in this second call — likelier under the concurrent
                        # load prefetch creates — could otherwise reject a step that
                        # already ran, inverting fail-closed. Reuse the passing
                        # whole-plan verification instead.
                        step_result = verification
                    else:
                        isolated = step.model_copy(update={"depends_on": []})
                        step_result = verify_step(
                            isolated,
                            envelope,
                            z3_timeout_ms=self._z3_timeout_ms,
                        )

                    if not step_result.ok:
                        record = await self._on_rejection(step, step_result, envelope, session.id)
                        if record.fallback_action == "halted":
                            _log.warning(
                                "run.step_halted",
                                extra={
                                    "run_id": run_id,
                                    "step_id": step.id,
                                    "violation_count": len(step_result.violations),
                                },
                            )
                            session.steps.append(
                                StepOutcome(
                                    step_id=step.id,
                                    status="rejected_halted",
                                    approved_by=None,
                                    rc=None,
                                    stdout="",
                                    duration_ms=0.0,
                                    started_at=_now_iso(),
                                    error=f"rejected: {step_result.violations[0].message}"
                                    if step_result.violations
                                    else "rejected",
                                )
                            )
                            session.status = RunStatus.HALTED_BY_SIMPLEX
                            break
                        else:
                            _log.info(
                                "run.step_recomputed",
                                extra={"run_id": run_id, "step_id": step.id},
                            )
                            session.steps.append(
                                StepOutcome(
                                    step_id=step.id,
                                    status="rejected_recomputed",
                                    approved_by=None,
                                    rc=None,
                                    stdout="",
                                    duration_ms=0.0,
                                    started_at=_now_iso(),
                                    error=None,
                                )
                            )
                            step = record.recomputed_step
                            # The recomputed replacement is LLM-authored and must
                            # pass the same per-step gate as any other step before
                            # it can reach the executor — RecomputeHandler verified a
                            # bare singleton without the supervisor's strict setting.
                            # If the replacement is itself out of policy, halt (don't
                            # execute it, and don't loop into another recompute).
                            recheck = verify_step(
                                step.model_copy(update={"depends_on": []}),
                                envelope,
                                z3_timeout_ms=self._z3_timeout_ms,
                            )
                            if not recheck.ok:
                                _log.warning(
                                    "run.recomputed_step_rejected",
                                    extra={
                                        "run_id": run_id,
                                        "step_id": step.id,
                                        "violations": len(recheck.violations),
                                    },
                                )
                                session.steps.append(
                                    StepOutcome(
                                        step_id=step.id,
                                        status="rejected_halted",
                                        approved_by=None,
                                        rc=None,
                                        stdout="",
                                        duration_ms=0.0,
                                        started_at=_now_iso(),
                                        error=(
                                            f"recomputed step rejected: "
                                            f"{recheck.violations[0].message}"
                                            if recheck.violations
                                            else "recomputed step rejected"
                                        ),
                                    )
                                )
                                session.status = RunStatus.HALTED_BY_SIMPLEX
                                break

                    try:
                        decision = self._approval.decide(step, envelope)
                    except Exception as exc:
                        # An approval strategy that raises must not crash the
                        # run. Treat as a denial with the exception message.
                        _log.warning(
                            "run.approval_error",
                            extra={
                                "run_id": run_id,
                                "step_id": step.id,
                                "error": f"{type(exc).__name__}: {exc}",
                            },
                        )
                        session.steps.append(
                            StepOutcome(
                                step_id=step.id,
                                status="aborted",
                                approved_by=None,
                                rc=None,
                                stdout="",
                                duration_ms=0.0,
                                started_at=_now_iso(),
                                error=f"approval error: {type(exc).__name__}: {exc}",
                            )
                        )
                        session.status = RunStatus.ABORTED
                        break
                    step_started = _now_iso()
                    if not decision.approved:
                        _log.warning(
                            "run.approval_denied",
                            extra={
                                "run_id": run_id,
                                "step_id": step.id,
                                "approved_by": decision.approved_by,
                                "reason": decision.reason,
                            },
                        )
                        session.steps.append(
                            StepOutcome(
                                step_id=step.id,
                                status="aborted",
                                approved_by=decision.approved_by,
                                rc=None,
                                stdout="",
                                duration_ms=0.0,
                                started_at=step_started,
                                error=f"approval denied: {decision.reason}",
                            )
                        )
                        session.status = RunStatus.ABORTED
                        break
                    exec_outcome = self._execute_one(
                        step, step_started, decision, _prefetched.get(step.id)
                    )
                    if exec_outcome.status == "succeeded":
                        from opendaisugi.stage2 import verify_completed_step

                        current_metadata = dict(getattr(step, "metadata", {}) or {})
                        # Executor-produced fields MUST overwrite any upstream
                        # metadata (parser, pathway, attacker-controlled
                        # envelope) — otherwise an exit_code postcondition
                        # could be discharged against a forged rc planted in
                        # step.metadata before execution. v0.28.3-followup:
                        # the v0.28.3 first cut used setdefault for rc, which
                        # would let an upstream rc=0 hide a real exec failure.
                        current_metadata["output"] = exec_outcome.stdout
                        current_metadata["rc"] = exec_outcome.rc
                        completed = step.model_copy(update={"metadata": current_metadata})
                        stage2_violations = verify_completed_step(
                            completed,
                            envelope,
                            aliases=self._aliases,
                            strict=self._strict,
                        )
                        if stage2_violations:
                            from dataclasses import replace as _replace

                            exec_outcome = _replace(
                                exec_outcome,
                                status="failed",
                                error=f"stage2 rejection: {stage2_violations[0].message}",
                            )
                    session.steps.append(exec_outcome)
                    self._write_step_receipt(step, exec_outcome, session.id)
                    if session.steps[-1].status == "failed":
                        session.status = RunStatus.FAILED
                        session.failed_step_id = step.id
                        break
                else:
                    session.status = RunStatus.SUCCEEDED
            except KeyboardInterrupt:
                session.status = RunStatus.ABORTED
                raise
        finally:
            # A prefetched parallel-safe step may have executed (side effects, tokens
            # spent) in a level the main loop broke out of before reaching it. Record
            # those before the integrity check so the run never silently drops a step
            # that actually ran.
            if _levels is not None and 0 <= _prefetched_level < len(_levels):
                self._flush_unconsumed_prefetch(
                    session, _levels[_prefetched_level], _prefetched, _prefetched_approved
                )
            session.ended_at = _now_iso()
            self._check_run_integrity(session, plan)
            self._journal_session(session, plan, envelope, task=plan.task)
            log_level = logging.INFO if session.status == RunStatus.SUCCEEDED else logging.WARNING
            _log.log(
                log_level,
                "run.end",
                extra={
                    "run_id": run_id,
                    "envelope_id": envelope.id,
                    "status": session.status.value,
                    "step_count": len(session.steps),
                    "trace_id": session.trace_id,
                },
            )
        return session

    async def _on_rejection(self, step, result, envelope, session_id) -> RefinementRecord:
        handler = self._resolve_fallback(envelope)
        outcome = await handler.handle(step, result, envelope)
        record = RefinementRecord(
            step=step,
            violations=result.violations,
            z3_counterexample=None,
            envelope_id=envelope.id,
            fallback_action=outcome.action,
            recomputed_step=outcome.replacement_step,
            recomputed_verification=outcome.replacement_result,
            timestamp=time.time(),
            cache_key=envelope.cache_key,
        )
        if self._journal is not None:
            self._journal.write_refinement(record, session_id=session_id)
        return record

    # Deterministic executors are stateless and always safe to run concurrently.
    # A non-deterministic step (task/skill/mcp) joins only if its executor opts in
    # via a truthy ``parallel_safe`` — the budget-aware task executor sets this to
    # True only under an unlimited budget (order-independent sizing).
    _PARALLEL_SAFE_TYPES = frozenset({"shell", "file_read", "file_write", "network"})

    def _parallel_safe(self, step) -> bool:
        if step.type in self._PARALLEL_SAFE_TYPES:
            return True
        executor = self._executors.get(step.type)
        return bool(getattr(executor, "parallel_safe", False))

    async def _prefetch_independent(self, level, envelope) -> tuple[dict, dict]:
        """Concurrently run a level's independent PARALLEL-SAFE steps (parallel mode).

        Returns ``({step_id: ExecutorResult}, {step_id: Decision})`` for steps that
        pass BOTH verify and approval and executed cleanly — the results the main
        loop reuses, and the approvals a preempted-prefetch flush needs. Eligible
        steps are the deterministic kinds plus any whose executor declares
        ``parallel_safe`` (the task executor, under an unlimited budget). Concurrency
        is capped at ``max_parallel`` by a semaphore — a real ceiling on in-flight
        (possibly paid) LLM calls, not a boolean. Receipts, session updates, stage-2
        checks, and halt logic all stay sequential in the main loop. Anything omitted
        here (an ineligible step, a verify/approval failure, an executor error) is
        handled normally by the loop.
        """
        import asyncio

        candidates = []
        approvals: dict = {}
        for step in level:
            if not self._parallel_safe(step):
                continue
            isolated = step.model_copy(update={"depends_on": []})
            if not verify_step(isolated, envelope, z3_timeout_ms=self._z3_timeout_ms).ok:
                continue
            try:
                decision = self._approval.decide(step, envelope)
            except Exception:  # noqa: BLE001 — let the sequential loop handle it
                continue
            if not decision.approved:
                continue
            candidates.append(step)
            approvals[step.id] = decision
        if len(candidates) < 2:
            return {}, {}  # nothing to gain from concurrency for 0 or 1 step

        sem = asyncio.Semaphore(self._max_parallel)

        async def _run(step):
            executor = self._executors.get(step.type)
            if executor is None:
                return None
            async with sem:  # cap concurrent (paid) executor calls at max_parallel
                return await asyncio.to_thread(
                    executor.run,
                    step,
                    timeout_s=self._step_timeout_s,
                    max_output_bytes=self._max_output_bytes,
                )

        results = await asyncio.gather(*(_run(s) for s in candidates), return_exceptions=True)
        prefetched = {
            step.id: res
            for step, res in zip(candidates, results, strict=True)
            if isinstance(res, ExecutorResult)
        }
        approved = {sid: approvals[sid] for sid in prefetched}
        return prefetched, approved

    def _execute_one(self, step, step_started, decision, prefetched=None):
        # ``prefetched`` is an ExecutorResult already produced concurrently for this
        # exact step (parallel mode). Using it skips a redundant re-run; the step's
        # verify + approval gates were applied before it was prefetched.
        if prefetched is not None:
            result = prefetched
        else:
            try:
                executor = self._executors[step.type]
            except KeyError:
                # Defensive: verify should have rejected unknown kinds already.
                result = ExecutorResult(
                    rc=1,
                    stdout=f"no executor for kind '{step.type}'",
                    duration_ms=0.0,
                    timed_out=False,
                )
            else:
                try:
                    result = executor.run(
                        step,
                        timeout_s=self._step_timeout_s,
                        max_output_bytes=self._max_output_bytes,
                    )
                except Exception as e:  # executor infrastructure failure
                    return StepOutcome(
                        step_id=step.id,
                        status="failed",
                        approved_by=decision.approved_by,
                        rc=None,
                        stdout="",
                        duration_ms=0.0,
                        started_at=step_started,
                        error=f"executor error: {e}",
                    )
        status = "succeeded" if result.rc == 0 and not result.timed_out else "failed"
        # A failed step must carry WHY. The reason lives in result.stdout (an
        # executor's stderr is merged there, and DelegatingExecutor puts its
        # exhausted-retries message there) — surfacing it means a "failed" status
        # is never reason-less. (Previously error stayed None on any non-timeout
        # failure, so callers/CLI/JSON saw "failed" with no explanation.)
        if result.timed_out:
            error = "timed out"
        elif result.rc != 0:
            detail = (result.stdout or "").strip()
            error = f"exit {result.rc}: {detail[:500]}" if detail else f"exit {result.rc}"
        else:
            error = None
        return StepOutcome(
            step_id=step.id,
            status=status,
            approved_by=decision.approved_by,
            rc=result.rc,
            stdout=result.stdout,
            duration_ms=result.duration_ms,
            started_at=step_started,
            error=error,
            model_id=result.model,
            reversibility=result.reversibility,
            reversal=result.reversal,
        )

    def _flush_unconsumed_prefetch(self, session, level, prefetched, approved) -> None:
        """Record parallel-safe steps that ran during prefetch but were never
        reached by the main loop (it broke earlier in the level).

        They genuinely executed — side effects happened, tokens were spent — so a
        missing receipt would be a silent skip, the one thing this layer must never
        do. Emit each as an outcome + receipt reflecting its real execution result.
        Integrity's ``receipted >= expected`` tolerates the extra receipts. Stage-2
        is intentionally not re-run here: the run is already ending, and the receipt
        still records the raw evidence (and any postcondition) truthfully.
        """
        if not prefetched:
            return
        done = {o.step_id for o in session.steps}
        for step in level:
            if step.id not in prefetched or step.id in done:
                continue
            decision = approved.get(step.id)
            if decision is None:  # defensive: only approved steps are prefetched
                continue
            # This runs in finally, one step before the run itself is journaled. A
            # receipt-write failure (sqlite locked, disk error) must NOT replace an
            # in-flight exception or the run status, nor stop _journal_session from
            # recording the run — the same invariant _check_run_integrity holds for
            # its read. So swallow per-step and keep going; a missing flush receipt
            # degrades gracefully, it never sinks the run.
            try:
                outcome = self._execute_one(step, _now_iso(), decision, prefetched[step.id])
                session.steps.append(outcome)
                self._write_step_receipt(step, outcome, session.id)
            except Exception as exc:  # noqa: BLE001 — must not escape finally
                _log.warning(
                    "run.flush_prefetch_error",
                    extra={
                        "run_id": session.id,
                        "step_id": step.id,
                        "error": f"{type(exc).__name__}: {exc}",
                    },
                )

    def _check_run_integrity(self, session: RunSession, plan: ActionPlan) -> None:
        """Set ``session.integrity_passed`` based on receipt coverage (v0.18).

        Expected steps = all plan steps on success; contiguous prefix up to
        (and including) the failing step on halt-on-failure. A run that
        halted at step k is expected to have receipts for 1..k; missing
        receipts for steps in that prefix => silent skip => integrity fails.
        Steps after k legitimately unreached — not a violation.

        Rejected-at-verify and never-ran sessions: integrity_passed stays
        None (not checked).
        """
        if self._journal is None:
            return
        if session.status in (RunStatus.REJECTED, RunStatus.PENDING):
            return
        try:
            receipts = self._journal.receipts_for_run(session.id)
        except Exception as exc:
            # Journal read failure (sqlite locked, disk error) cannot be
            # allowed to suppress an earlier exception or replace the run
            # status. Mark integrity unknown and continue.
            _log.warning(
                "run.integrity_check_error",
                extra={"run_id": session.id, "error": f"{type(exc).__name__}: {exc}"},
            )
            session.integrity_passed = None
            return
        receipted = {r.step_id for r in receipts}
        if session.status == RunStatus.SUCCEEDED:
            expected = {s.id for s in plan.steps}
        elif session.status == RunStatus.FAILED and session.failed_step_id is not None:
            # Expected receipts = the EXECUTION-order prefix up to the failing step.
            # Steps execute in topological order, not declaration order — iterating
            # plan.steps here raised spurious integrity failures (a step that ran
            # after the failure in topo order but appears earlier in the list) and
            # could mask a genuine skip. Use the same order the supervisor ran.
            expected = set()
            try:
                ordered = topological_order(plan)
            except ValueError:
                ordered = plan.steps  # cyclic plan shouldn't reach here; be safe
            for s in ordered:
                expected.add(s.id)
                if s.id == session.failed_step_id:
                    break
        elif session.status in (RunStatus.ABORTED, RunStatus.HALTED_BY_SIMPLEX):
            # Steps whose executor was actually invoked must have receipts;
            # steps that aborted before execution (approval denied, simplex
            # rejection) legitimately produce no receipt. "succeeded" and
            # "failed" are the only outcomes that imply the executor ran.
            expected = {o.step_id for o in session.steps if o.status in ("succeeded", "failed")}
        else:
            expected = set()
        session.integrity_passed = receipted >= expected

    def _write_step_receipt(self, step, outcome, run_id: str) -> None:
        """Append a Receipt for an executed step (v0.18).

        Evidence carries execution output; postcondition (if any) gates
        verify_result. When no journal is attached (pure in-memory supervision)
        this is a no-op. When no postcondition is declared, the receipt's
        verify_result defaults to True (execution-happened-is-enough), but the
        receipt itself still gets written — that is what the run-end integrity
        check reads to prove no silent skip occurred.
        """
        if self._journal is None:
            return
        evidence: dict = {
            "rc": outcome.rc,
            "stdout": outcome.stdout,
            "duration_ms": outcome.duration_ms,
            "status": outcome.status,
        }
        if outcome.error:
            evidence["error"] = outcome.error
        verify_ok = outcome.status == "succeeded"
        verify_detail = ""
        postcondition = getattr(step, "postcondition", None)
        if postcondition is not None and verify_ok:
            try:
                verify_ok, verify_detail = self._check_step_postcondition(
                    postcondition,
                    evidence,
                )
            except Exception as exc:
                # Postcondition logic must not crash the run. Treat as failure
                # with the exception message; integrity check downstream still
                # sees the receipt with verify_result=False.
                verify_ok = False
                verify_detail = f"postcondition error: {type(exc).__name__}: {exc}"
                _log.warning(
                    "run.postcondition_error",
                    extra={"step_id": step.id, "run_id": run_id, "error": verify_detail},
                )
        # v0.19: which model produced the evidence, so Gardener can attribute
        # success/failure per model. v0.40: read it off the OUTCOME (carried from
        # the ExecutorResult) rather than the executor's shared self.last — the
        # latter is clobbered when task steps run concurrently. Non-LLM executors
        # leave model_id None.
        model_id = outcome.model_id
        # v0.41 (Stage 8, deed ledger): record the step's effect class and its
        # reversibility verdict. The executor that performed the effect is the
        # authority (it alone knows what was mutated); when it emits no verdict
        # we classify from the step kind. Crucially "none" is a *positive* claim
        # (read-only), never a fallback — any other side-effecting step without a
        # handle is irreversible, so a missing handle can never read as "nothing
        # to undo".
        effect_class = step.type
        reversibility = outcome.reversibility
        if reversibility is None:
            reversibility = "none" if step.type in _READ_ONLY_KINDS else "irreversible"
        receipt = Receipt(
            step_id=step.id,
            run_id=run_id,
            timestamp=time.time(),
            evidence=evidence,
            evidence_hash=compute_evidence_hash(evidence),
            verify_result=verify_ok,
            verify_details=verify_detail,
            model_id=model_id,
            effect_class=effect_class,
            reversibility=reversibility,
            reversal=outcome.reversal,
        )
        self._journal.append_receipt(receipt)

    @staticmethod
    def _check_step_postcondition(postcondition, evidence: dict) -> tuple[bool, str]:
        """Evaluate a per-step postcondition against execution evidence.

        v0.18 minimum: postcondition.path is treated as a required key in the
        evidence dict. Kits with richer checks subclass Supervisor and override.
        Returns (ok, details_string).
        """
        if postcondition.path:
            if postcondition.path in evidence:
                return True, f"evidence.{postcondition.path} present"
            return False, f"postcondition expected evidence.{postcondition.path}"
        return True, "no structural check configured"

    def _journal_session(self, session, plan, envelope, *, task):
        if self._journal is None:
            return
        session.trace_id = self._journal.log_run(
            session,
            task=task,
            envelope=envelope,
            plan=plan,
        )
