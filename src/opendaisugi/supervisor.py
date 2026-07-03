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
            plan, envelope,
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
        ordered = topological_order(plan)

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
                    # Per-step verification before execution. Use the
                    # lightweight ``verify_step`` path — the whole-plan
                    # ``verify()`` above already proved envelope self-
                    # consistency and plan-vs-envelope structural checks.
                    # Strip depends_on so singleton-plan DAG check passes.
                    isolated = step.model_copy(update={"depends_on": []})
                    step_result = verify_step(
                        isolated, envelope, z3_timeout_ms=self._z3_timeout_ms,
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
                            session.steps.append(StepOutcome(
                                step_id=step.id,
                                status="rejected_halted",
                                approved_by=None,
                                rc=None,
                                stdout="",
                                duration_ms=0.0,
                                started_at=_now_iso(),
                                error=f"rejected: {step_result.violations[0].message}" if step_result.violations else "rejected",
                            ))
                            session.status = RunStatus.HALTED_BY_SIMPLEX
                            break
                        else:
                            _log.info(
                                "run.step_recomputed",
                                extra={"run_id": run_id, "step_id": step.id},
                            )
                            session.steps.append(StepOutcome(
                                step_id=step.id,
                                status="rejected_recomputed",
                                approved_by=None,
                                rc=None,
                                stdout="",
                                duration_ms=0.0,
                                started_at=_now_iso(),
                                error=None,
                            ))
                            step = record.recomputed_step
                            # The recomputed replacement is LLM-authored and must
                            # pass the same per-step gate as any other step before
                            # it can reach the executor — RecomputeHandler verified a
                            # bare singleton without the supervisor's strict setting.
                            # If the replacement is itself out of policy, halt (don't
                            # execute it, and don't loop into another recompute).
                            recheck = verify_step(
                                step.model_copy(update={"depends_on": []}),
                                envelope, z3_timeout_ms=self._z3_timeout_ms,
                            )
                            if not recheck.ok:
                                _log.warning(
                                    "run.recomputed_step_rejected",
                                    extra={"run_id": run_id, "step_id": step.id,
                                           "violations": len(recheck.violations)},
                                )
                                session.steps.append(StepOutcome(
                                    step_id=step.id, status="rejected_halted",
                                    approved_by=None, rc=None, stdout="",
                                    duration_ms=0.0, started_at=_now_iso(),
                                    error=(f"recomputed step rejected: "
                                           f"{recheck.violations[0].message}"
                                           if recheck.violations else "recomputed step rejected"),
                                ))
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
                                "run_id": run_id, "step_id": step.id,
                                "error": f"{type(exc).__name__}: {exc}",
                            },
                        )
                        session.steps.append(StepOutcome(
                            step_id=step.id,
                            status="aborted",
                            approved_by=None,
                            rc=None,
                            stdout="",
                            duration_ms=0.0,
                            started_at=_now_iso(),
                            error=f"approval error: {type(exc).__name__}: {exc}",
                        ))
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
                        session.steps.append(StepOutcome(
                            step_id=step.id,
                            status="aborted",
                            approved_by=decision.approved_by,
                            rc=None,
                            stdout="",
                            duration_ms=0.0,
                            started_at=step_started,
                            error=f"approval denied: {decision.reason}",
                        ))
                        session.status = RunStatus.ABORTED
                        break
                    exec_outcome = self._execute_one(step, step_started, decision)
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
                            completed, envelope, aliases=self._aliases,
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

    def _execute_one(self, step, step_started, decision):
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
            expected = {
                o.step_id for o in session.steps
                if o.status in ("succeeded", "failed")
            }
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
                    postcondition, evidence,
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
        # v0.19: if the executor that ran this step is a DelegatingExecutor,
        # capture which model produced the evidence so Gardener can attribute
        # success/failure per model. Non-LLM executors leave model_id as None.
        executor = self._executors.get(step.type)
        model_id: str | None = None
        last = getattr(executor, "last", None)
        if last is not None:
            model_id = getattr(last, "model", None)
        receipt = Receipt(
            step_id=step.id,
            run_id=run_id,
            timestamp=time.time(),
            evidence=evidence,
            evidence_hash=compute_evidence_hash(evidence),
            verify_result=verify_ok,
            verify_details=verify_detail,
            model_id=model_id,
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
            session, task=task, envelope=envelope, plan=plan,
        )
