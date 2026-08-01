"""Stage 9 — within-instance batch compilation (ADR-0011, the currency levers).

A task's own internal repetition is normally paid for turn by turn. This module
lets an agent **declare a batch** instead — program P (a step template), item set
I (bindings), effect footprint F (declared write globs), acceptance postcondition
Q — and then:

1. **Prove ``F ⊆ envelope`` before any iteration.** Every item is resolved to its
   concrete write-set up front and checked with the *same* concrete matcher the
   runtime gate uses (``verify._path_matches_any``) — not the envelope↔envelope Z3
   glob encoding, which diverges from it (single ``*`` crosses ``/``; no
   normalization) and would let the proof *admit* a write the gate would *reject*.
   One pre-flight pass covers all N; reject on any unprovable write. This is a
   *temporal* "one proof": no write happens until the whole footprint is
   authorized, so a mid-batch discovery never leaves the run half-done.

2. **Refuse irreversible programs.** Only ``file_write`` (reversible via the Stage-8
   deed ledger) and the read-only ``file_read`` / ``network`` (GET-only) kinds are
   batchable; ``shell`` / ``mcp`` / ``task`` / ``skill`` / ``agentic`` are not — they
   leave no reversal handle. But reversibility is **not** a property of the type: a
   ``file_write`` over a >1 MB or non-UTF-8 target comes back irreversible at
   runtime. So a static type check is necessary and *insufficient*; the guarantee is
   completed by a pre-flight probe (reject a target whose write would be
   irreversible) **and** a runtime rule (halt the instant a deed comes back
   irreversible, surfacing it honestly in the rollback report).

3. **Sample-validate Q on k items in a deed-ledger fork.** openDaisugi owns no
   copy-on-write workspace (ADR-0004 forecloses it); the fork *is* the deed ledger —
   run k items, check Q, then ``apply_reversal`` to discard the fork.

4. **Execute all N under the supervisor's per-step monitor with per-element
   rollback.** The monitor is the existing ``verify_step`` + halt path; per-element
   rollback is the Stage-8 ``ReversalHandle`` collected per element and applied
   newest-first.

**The two-ledger baseline — the load-bearing honesty.** The savings are reported in
two ledgers that are never merged. The *within-instance* ledger measures a *verified*
script against an *unverified* one, against the honest baseline (a competent agent
already scripts a bulk job) — so its marginal contribution is the proven blast
radius, not the script; the net-token number is small and often ≤ 0, and the meter
says so rather than manufacturing a win (the SKILL-DISCO +net-cost trap). The
*cross-instance* ledger (persistence + generalization of the distilled pathway) is a
compounding win, but it is the frequency-amortized family the hardest requirement
excludes as a complete answer, so it is reported apart and its at-scale value is
Stage 4's question, not this module's.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from pydantic import BaseModel, Field, computed_field

from opendaisugi import deeds
from opendaisugi.deeds import RollbackReport
from opendaisugi.executor import FileWriteExecutor, default_executors
from opendaisugi.models import ActionPlan, Envelope, Postcondition, ReversalHandle
from opendaisugi.pathway import PathwayParameter
from opendaisugi.pathway_params import apply_bindings
from opendaisugi.run_session import RunStatus
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import _path_matches_any

# Kinds a batch may contain. ``file_write`` is reversible through the deed ledger;
# ``file_read`` and ``network`` (GET-only) have no local effect to undo. Everything
# else (shell/mcp/task/skill/agentic/robotics) leaves no reversal handle and so can
# never enter a batch — deny-by-default. NOTE ``network``'s membership is contingent
# on ``NetworkExecutor`` staying GET-only; a non-GET path would make it a real,
# irreversible effect (see tests/test_batch.py::test_network_executor_stays_get_only).
_BATCHABLE_TYPES: frozenset[str] = frozenset({"file_write", "file_read", "network"})

# Rough token estimate for a per-call amortization figure. Matches accounting's
# tier-1 (single LLM discharge) order of magnitude; the meter is deliberately a
# *ruler* emitting labelled estimates, not a token oracle.
_DEFAULT_TOKENS_PER_CALL = 2000


# --------------------------------------------------------------------------- #
# the declaration — a JSON-round-trippable model an agent authors like an envelope
# --------------------------------------------------------------------------- #
class BatchDeclaration(BaseModel):
    """An agent-authored batch: program P, item set I, footprint F, acceptance Q.

    ``program`` is an ``ActionPlan`` template; ``parameters`` are its typed holes
    (ADR-0008 ``PathwayParameter``, capability-head-guarded so a binding can change
    data but never the program / directory / host); ``items`` are the bindings —
    **a concrete list, deliberately not a glob or generator**, so the footprint is
    enumerable before any iteration. ``footprint`` is the declared write blast
    radius F; ``acceptance`` is the postcondition Q sample-validated on k items.
    """

    program: ActionPlan
    parameters: list[PathwayParameter] = Field(default_factory=list)
    items: list[dict[str, str]]
    footprint: list[str] = Field(default_factory=list)
    acceptance: Postcondition | None = None
    sample_k: int = 2


# --------------------------------------------------------------------------- #
# result reports (runtime, not wire — free to hold dataclass RollbackReport)
# --------------------------------------------------------------------------- #
@dataclass
class BatchClassification:
    batchable: bool
    non_batchable: list[dict] = field(default_factory=list)


@dataclass
class FootprintProof:
    ok: bool
    resolved_writes: list[str] = field(default_factory=list)
    out_of_envelope: list[str] = field(default_factory=list)
    under_declared: list[str] = field(default_factory=list)
    bad_bindings: list[int] = field(default_factory=list)
    reason: str = ""
    # True when there were no writes to prove (a read-only / network program): the
    # write-set proof is then *vacuously* ok and proved nothing about blast radius —
    # flagged so "the proof passed" is never read as a meaningful claim (the yellow
    # paper's vacuity ethos). Per-read/URL admission is still gated by verify_step.
    vacuous: bool = False


@dataclass
class BatchResult:
    status: str  # "succeeded" | "rejected" | "halted"
    reason: str = ""
    classification: BatchClassification | None = None
    proof: FootprintProof | None = None
    sample_ok: bool = False
    executed: int = 0
    reversals: list[ReversalHandle] = field(default_factory=list)
    rollback: RollbackReport | None = None
    ledger: "TwoLedgerReport | None" = None


# --------------------------------------------------------------------------- #
# 1. batchability
# --------------------------------------------------------------------------- #
def is_batchable_type(step_type: str) -> bool:
    """True iff a step of this kind can be safely batched — reversible or read-only."""
    return step_type in _BATCHABLE_TYPES


def classify_declaration(decl: BatchDeclaration) -> BatchClassification:
    """Reject a program that contains any non-batchable (irreversible) step kind."""
    non_batchable = [
        {"id": s.id, "type": s.type}
        for s in decl.program.steps
        if not is_batchable_type(s.type)
    ]
    return BatchClassification(batchable=not non_batchable, non_batchable=non_batchable)


# --------------------------------------------------------------------------- #
# 2. static footprint proof
# --------------------------------------------------------------------------- #
def resolve_items(decl: BatchDeclaration) -> tuple[list[ActionPlan | None], list[int]]:
    """Resolve each item into a concrete plan; a binding that fails (unbound hole or
    a value that would change a capability head) resolves to None with its index
    recorded — fail-closed, never a silently-dropped write."""
    resolved: list[ActionPlan | None] = []
    bad: list[int] = []
    for i, values in enumerate(decl.items):
        plan = apply_bindings(decl.program, decl.parameters, values)
        if plan is None:
            bad.append(i)
        resolved.append(plan)
    return resolved, bad


def _writes_of(plan: ActionPlan) -> list[str]:
    return [s.path for s in plan.steps if s.type == "file_write"]


def prove_footprint(decl: BatchDeclaration, envelope: Envelope) -> FootprintProof:
    """Prove the resolved write-set lies inside both the envelope and the declared
    footprint F, before any iteration.

    Uses the concrete ``_path_matches_any`` the executor gate uses — so the proof is
    exactly as strict as the runtime, never admitting a write the gate would reject.
    Rejects on: any unbound/head-changing binding, any write outside the envelope
    (unprovable), any write outside the declared F (dishonest under-declaration).
    """
    resolved, bad = resolve_items(decl)
    perms = envelope.permissions.file_write
    resolved_writes: list[str] = []
    out_of_envelope: list[str] = []
    under_declared: list[str] = []
    for plan in resolved:
        if plan is None:
            continue
        for path in _writes_of(plan):
            resolved_writes.append(path)
            if not _path_matches_any(path, perms):
                out_of_envelope.append(path)
            if not _path_matches_any(path, decl.footprint):
                under_declared.append(path)
    ok = not bad and not out_of_envelope and not under_declared
    reasons = []
    if bad:
        reasons.append(f"{len(bad)} item(s) failed to bind (unbound hole or head change): {bad}")
    if out_of_envelope:
        reasons.append(f"{len(out_of_envelope)} write(s) outside the envelope: {out_of_envelope}")
    if under_declared:
        reasons.append(f"{len(under_declared)} write(s) outside the declared footprint F: {under_declared}")
    return FootprintProof(
        ok=ok,
        resolved_writes=resolved_writes,
        out_of_envelope=out_of_envelope,
        under_declared=under_declared,
        bad_bindings=bad,
        reason="; ".join(reasons),
        vacuous=not resolved_writes,
    )


# --------------------------------------------------------------------------- #
# 3. reversibility pre-probe
# --------------------------------------------------------------------------- #
def would_be_reversible(path: str) -> bool:
    """Would a write to ``path`` be undoable from the ledger? Reuses the executor's
    own pre-image oracle: a >MAX_REVERSAL_BYTES or non-UTF-8 existing target cannot
    be captured honestly and is irreversible. A symlink target is refused before it
    mutates (a permission concern, not an irreversibility one), so it is not the
    reason to reject a batch here."""
    if os.path.islink(path):
        return True
    parent = os.path.dirname(path) or "."
    _handle, reversible = FileWriteExecutor._capture_pre_image(path, parent)
    return reversible


# --------------------------------------------------------------------------- #
# 4. the net-token meter + the two-ledger discipline
# --------------------------------------------------------------------------- #
class NetTokenLedger(BaseModel):
    """One ledger of a batch's token economics — labelled evidence, not proof.

    ``net = (output_tokens_saved + calls_saved × tokens_per_call) − spec_input_injected``.
    A negative net is the SKILL-DISCO trap made visible: the injected spec cost more
    than it saved. The meter's whole job is to show that honestly.
    """

    label: str
    baseline: str
    output_tokens_saved: int = 0
    calls_saved: int = 0
    tokens_per_call: int = 0
    spec_input_injected: int = 0
    evidence_not_proof: bool = True

    @computed_field  # type: ignore[prop-decorator]
    @property
    def net(self) -> int:
        return (
            self.output_tokens_saved
            + self.calls_saved * self.tokens_per_call
            - self.spec_input_injected
        )

    @computed_field  # type: ignore[prop-decorator]
    @property
    def net_positive(self) -> bool:
        return self.net > 0

    @classmethod
    def within_instance(
        cls,
        decl: BatchDeclaration,
        *,
        output_tokens_saved: int = 0,
        calls_saved: int = 0,
        tokens_per_call: int | None = None,
        spec_input_injected: int | None = None,
    ) -> "NetTokenLedger":
        """The within-instance ledger, against the *honest* (not manual-turn)
        baseline. Defaults encode the honest finding: a competent agent already
        scripts the bulk job, so no LLM calls and no output are *saved* by the batch
        (both are one script) — only the declaration's spec is *injected*. The win is
        the proven blast radius, and the net-token number is ≤ 0 to say so."""
        if tokens_per_call is None:
            tokens_per_call = _DEFAULT_TOKENS_PER_CALL
        if spec_input_injected is None:
            spec_input_injected = _estimate_tokens(decl.model_dump_json())
        return cls(
            label="within-instance",
            baseline="honest-script (a competent agent already scripts the bulk job)",
            output_tokens_saved=output_tokens_saved,
            calls_saved=calls_saved,
            tokens_per_call=tokens_per_call,
            spec_input_injected=spec_input_injected,
        )


class TwoLedgerReport(BaseModel):
    """The within-instance and cross-instance ledgers, kept apart on purpose."""

    within_instance: NetTokenLedger
    cross_instance: NetTokenLedger | None = None
    note: str = (
        "Two ledgers reported separately and never merged (roadmap Stage 9). The "
        "within-instance win is the proven blast radius; the cross-instance "
        "(persistence + generalization) win is Stage 4's at-scale question."
    )


def _estimate_tokens(text: str) -> int:
    """A deliberately coarse ~4-chars-per-token estimate. Labelled evidence, not a
    token oracle — the corpus stores no per-call counts."""
    return max(1, len(text) // 4)


def two_ledger_report(
    decl: BatchDeclaration,
    *,
    cross_instance: NetTokenLedger | None = None,
    **within_kwargs,
) -> TwoLedgerReport:
    return TwoLedgerReport(
        within_instance=NetTokenLedger.within_instance(decl, **within_kwargs),
        cross_instance=cross_instance,
    )


# --------------------------------------------------------------------------- #
# 5. execution — sample-validate, monitor, per-element rollback, halt-on-irreversible
# --------------------------------------------------------------------------- #
def _handles_of(session) -> list[ReversalHandle]:
    return [o.reversal for o in session.steps if o.reversal is not None]


def _step_path(plan: ActionPlan, step_id: str) -> str:
    for s in plan.steps:
        if s.id == step_id:
            return getattr(s, "path", "")
    return ""


def _rollback(handles: list[ReversalHandle]) -> RollbackReport:
    """Undo handles newest-first (a later write may sit atop an earlier one), from
    the ledger alone — no model, no executor, no re-run. An ``apply_reversal`` that
    fails (permissions, disk full, a parent removed mid-batch) is recorded in
    ``skipped`` rather than raised, so the report never claims more undone than it
    achieved — the same honesty Stage 8's ledger demands, applied to the undo itself."""
    report = RollbackReport()
    for h in reversed(handles):
        try:
            deeds.apply_reversal(h)
            report.undone.append(h.path)
        except OSError as e:
            report.skipped.append({"path": h.path, "reason": f"rollback failed: {e}"})
    return report


def _acceptance_holds(q: Postcondition | None, session, plan: ActionPlan) -> bool:
    """Evaluate the batch acceptance postcondition Q on one element's result. An
    unrecognised Q type is fail-closed (we cannot confirm it ⇒ do not commit the
    full batch), and any non-succeeded step already fails acceptance."""
    if any(o.status != "succeeded" for o in session.steps):
        return False
    if q is None:
        return True
    if q.type in ("succeeded", "ran"):
        return True
    if q.type in ("rc_zero", "rc0"):
        return all(o.rc == 0 for o in session.steps if o.rc is not None)
    if q.type in ("file_exists", "file_nonempty"):
        paths = [q.path] if q.path else _writes_of(plan)
        for p in paths:
            if not p or not os.path.exists(p):
                return False
            if q.type == "file_nonempty" and os.path.getsize(p) == 0:
                return False
        return True
    return False  # unknown acceptance type → cannot validate → reject


async def run_batch(
    decl: BatchDeclaration,
    envelope: Envelope,
    *,
    journal,
    executors=None,
    sample_k: int | None = None,
) -> BatchResult:
    """Prove, sample-validate, then execute a declared batch under the monitor.

    Order is load-bearing: classify → prove footprint (before any write) →
    reversibility pre-probe → sample-validate Q in a deed-ledger fork →
    execute all N, halting the instant an element's deed comes back irreversible and
    rolling the reversible ones back from the ledger.
    """
    cls = classify_declaration(decl)
    if not cls.batchable:
        kinds = ", ".join(sorted({nb["type"] for nb in cls.non_batchable}))
        return BatchResult(
            status="rejected",
            reason=f"program contains non-batchable step kind(s): {kinds}",
            classification=cls,
        )

    proof = prove_footprint(decl, envelope)
    if not proof.ok:
        return BatchResult(
            status="rejected",
            reason=f"footprint not provable: {proof.reason}",
            classification=cls,
            proof=proof,
        )

    irreversible_targets = [w for w in proof.resolved_writes if not would_be_reversible(w)]
    if irreversible_targets:
        return BatchResult(
            status="rejected",
            reason=(
                "targets whose write would be irreversible cannot enter a batch: "
                f"{irreversible_targets}"
            ),
            classification=cls,
            proof=proof,
        )

    resolved, _bad = resolve_items(decl)
    plans = [p for p in resolved if p is not None]
    sup = Supervisor(executors=executors or default_executors(), journal=journal)
    k = sample_k if sample_k is not None else decl.sample_k

    # --- sample-validate Q on the first k items, in a deed-ledger fork ---
    sample_handles: list[ReversalHandle] = []
    sample_ok = True
    for plan in plans[:k]:
        session = await sup.run(plan, envelope)
        sample_handles.extend(_handles_of(session))
        if session.status != RunStatus.SUCCEEDED or not _acceptance_holds(
            decl.acceptance, session, plan
        ):
            sample_ok = False
            break
    sample_report = _rollback(sample_handles)  # discard the fork, validated or not
    if not sample_ok:
        return BatchResult(
            status="rejected",
            reason="acceptance postcondition Q failed on the sampled fork",
            classification=cls,
            proof=proof,
            sample_ok=False,
            rollback=sample_report,
        )

    # --- execute all N under the monitor, per-element rollback, halt-on-irreversible ---
    done_handles: list[ReversalHandle] = []
    executed = 0
    for plan in plans:
        session = await sup.run(plan, envelope)
        if session.status != RunStatus.SUCCEEDED:
            report = _rollback(done_handles)
            return BatchResult(
                status="halted",
                reason=f"element did not succeed ({session.status.value}); rolled back and halted",
                classification=cls,
                proof=proof,
                sample_ok=True,
                executed=executed,
                rollback=report,
            )
        # Defense in depth, TOCTOU-only: the pre-flight probe already rejected any
        # known-irreversible target, so this branch is unreachable unless a target
        # changed (grew past the cap / became non-UTF-8) between probe and write.
        # Do NOT delete the probe as "redundant" — without it the guarantee degrades
        # from "never enters a batch" to "enters once, then halts".
        irreversible = [o for o in session.steps if o.reversibility == "irreversible"]
        if irreversible:
            report = _rollback(done_handles + _handles_of(session))
            for o in irreversible:
                report.skipped.append(
                    {
                        "step_id": o.step_id,
                        "path": _step_path(plan, o.step_id),
                        "reason": "irreversible",
                    }
                )
            return BatchResult(
                status="halted",
                reason="an element produced an irreversible deed; halted before the rest",
                classification=cls,
                proof=proof,
                sample_ok=True,
                executed=executed,
                rollback=report,
            )
        done_handles.extend(_handles_of(session))
        executed += 1

    return BatchResult(
        status="succeeded",
        classification=cls,
        proof=proof,
        sample_ok=True,
        executed=executed,
        reversals=done_handles,
        ledger=two_ledger_report(decl),
    )


def rollback_result(result: BatchResult) -> RollbackReport:
    """Undo a batch from its ledger alone — the harness-consumable per-element undo,
    no model and no re-run.

    For a *succeeded* batch this performs the rollback now, from the collected
    reversal handles. For a *halted* or *rejected* batch the rollback already ran
    inside ``run_batch``; this returns that real report rather than a vacuous empty
    one (re-applying would double-undo, and an empty "nothing to undo" would be the
    very fail-open Stage 8 prevents, in a new costume)."""
    if result.rollback is not None:
        return result.rollback
    return _rollback(result.reversals)


__all__ = [
    "BatchClassification",
    "BatchDeclaration",
    "BatchResult",
    "FootprintProof",
    "NetTokenLedger",
    "TwoLedgerReport",
    "classify_declaration",
    "is_batchable_type",
    "prove_footprint",
    "resolve_items",
    "rollback_result",
    "run_batch",
    "two_ledger_report",
    "would_be_reversible",
]
