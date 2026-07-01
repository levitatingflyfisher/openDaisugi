"""Dogfood runner for the agent-council kit.

Three scenarios:
1. Clean — 3 of 3 approve, no PII flagged → verify passes, runs clean.
2. PII flagged — one reviewer raises metadata.pii_flag=True → invariant
   'no_pii_in_reviews' rejects pre-execution.
3. Quorum missed — 1 of 3 approve, no PII → invariant 'no_pii_in_reviews'
   passes, run completes, aggregator's evidence shows quorum_met=false.
"""
from __future__ import annotations

import asyncio
import json
import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

from pathlib import Path

import step_types  # noqa: F401  (registers step types via @step_type)
from envelope import build_envelope
from plan import build_plan

from opendaisugi.approval import CallbackStrategy
from opendaisugi.executor import ExecutorResult
from opendaisugi.journal import Journal
from opendaisugi.supervisor import Supervisor
from opendaisugi.verify import verify


class CouncilExecutor:
    """Deterministic executor for the four council step types.

    Produces evidence each step's postcondition is happy with, and for
    AggregateVotes reflects the quorum computation so the run's receipt
    trail carries the decision evidence.
    """

    def __init__(self, reviews: list[dict], quorum_m: int) -> None:
        self._reviews = reviews
        self._quorum_m = quorum_m

    def run(self, step, *, timeout_s: int, max_output_bytes: int) -> ExecutorResult:
        if step.type == "submit_contribution":
            ev = {"content_hash": f"ch_{step.id}"}
        elif step.type == "agent_review":
            ev = {
                "signed_hash": f"sig_{step.reviewer_id}",
                "approve": step.metadata.get("approve"),
                "pii_flag": step.metadata.get("pii_flag"),
            }
        elif step.type == "aggregate_votes":
            approve_count = sum(1 for r in self._reviews if r["approve"])
            pii_count = sum(1 for r in self._reviews if r["pii_flag"])
            ev = {
                "approve_count": approve_count,
                "pii_flag_count": pii_count,
                "quorum_met": approve_count >= self._quorum_m and pii_count == 0,
                "clean": pii_count == 0,
            }
        elif step.type == "commit_or_reject":
            approve_count = sum(1 for r in self._reviews if r["approve"])
            pii_count = sum(1 for r in self._reviews if r["pii_flag"])
            accepted = approve_count >= self._quorum_m and pii_count == 0
            ev = {
                "decision": "accept" if accepted else "reject",
                "commit_hash": "ok" if accepted else None,
                "rejection_reason": None if accepted else "quorum or PII",
            }
        else:
            ev = {}
        return ExecutorResult(rc=0, stdout=json.dumps(ev), duration_ms=0.5, timed_out=False)


async def run_scenario(label, reviews, quorum_m, journal_dir):
    env = build_envelope()
    reviewer_ids = [r["reviewer_id"] for r in reviews]
    plan = build_plan("important_contribution_text", reviewer_ids, reviews, quorum_m)
    vr = verify(plan, env, z3_timeout_ms=500)
    if not vr.ok:
        return {
            "label": label,
            "verify_ok": False,
            "violations": [
                {"stage": v.stage, "message": v.message[:200]} for v in vr.violations
            ],
        }
    j = Journal(data_dir=journal_dir)
    executors = {
        "submit_contribution": CouncilExecutor(reviews, quorum_m),
        "agent_review": CouncilExecutor(reviews, quorum_m),
        "aggregate_votes": CouncilExecutor(reviews, quorum_m),
        "commit_or_reject": CouncilExecutor(reviews, quorum_m),
    }
    sup = Supervisor(
        executors=executors, journal=j,
        approval=CallbackStrategy(lambda s, e: True),
    )
    session = await sup.run(plan, env)
    receipts = j.receipts_for_run(session.id)
    commit_receipt = next(
        (r for r in receipts if r.step_id == f"s{len(reviews)+2}"),
        None,
    )
    commit_evidence = {}
    if commit_receipt is not None:
        try:
            commit_evidence = json.loads(commit_receipt.evidence.get("stdout", "{}"))
        except (json.JSONDecodeError, TypeError):
            commit_evidence = {}
    return {
        "label": label,
        "verify_ok": True,
        "run_status": session.status.value,
        "integrity_passed": session.integrity_passed,
        "receipts": len(receipts),
        "expected_steps": len(plan.steps),
        "decision": commit_evidence.get("decision"),
    }


async def main():
    jdir = Path("/tmp/agent_council_journal")
    clean = await run_scenario(
        "scenario_1_clean",
        reviews=[
            {"reviewer_id": "a1", "approve": True, "pii_flag": False},
            {"reviewer_id": "a2", "approve": True, "pii_flag": False},
            {"reviewer_id": "a3", "approve": True, "pii_flag": False},
        ],
        quorum_m=2,
        journal_dir=jdir,
    )
    pii = await run_scenario(
        "scenario_2_pii_flagged",
        reviews=[
            {"reviewer_id": "a1", "approve": True, "pii_flag": False},
            {"reviewer_id": "a2", "approve": True, "pii_flag": True},
            {"reviewer_id": "a3", "approve": True, "pii_flag": False},
        ],
        quorum_m=2,
        journal_dir=jdir,
    )
    quorum_missed = await run_scenario(
        "scenario_3_quorum_missed",
        reviews=[
            {"reviewer_id": "a1", "approve": True, "pii_flag": False},
            {"reviewer_id": "a2", "approve": False, "pii_flag": False},
            {"reviewer_id": "a3", "approve": False, "pii_flag": False},
        ],
        quorum_m=2,
        journal_dir=jdir,
    )
    print(json.dumps({
        "scenario_1_clean": clean,
        "scenario_2_pii_flagged": pii,
        "scenario_3_quorum_missed": quorum_missed,
    }, indent=2))


if __name__ == "__main__":
    asyncio.run(main())
