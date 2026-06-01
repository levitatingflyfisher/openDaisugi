"""Agent council envelope: permissions + Z3 invariants over the review DAG.

Structural claims (Z3):
- No AgentReview has metadata.pii_flag = true (forall_steps + implies)

Perceptual claims (LLM-as-judge): each AgentReview step's reviewing agent
produces its verdict; opendaisugi doesn't try to second-guess the perceptual
judgement. Z3 counts verdicts and checks structural properties.
"""
from __future__ import annotations

from opendaisugi.models import Envelope, Invariant, Permission


def build_envelope() -> Envelope:
    return Envelope(
        generated_by="agent-council-kit",
        task="Gate a contribution on N-agent council quorum + no-PII review",
        permissions=Permission(
            shell=False,
            file_read=["./contributions/**"],
            file_write=["./committed/**", "/tmp/**"],
            network=False,
            max_execution_time_s=30,
            max_output_size_mb=1,
        ),
        invariants=[
            Invariant(
                type="no_pii_in_reviews",
                description=(
                    "No AgentReview step may carry metadata.pii_flag = true. "
                    "If any reviewer flags PII/secrets, the plan refuses to "
                    "even reach the aggregator step."
                ),
                enforce=True,
                expr={
                    "op": "forall_steps",
                    "pred": {
                        "op": "implies",
                        "a": {"op": "equals", "path": "type", "value": "agent_review"},
                        "b": {"op": "not_equals", "path": "metadata.pii_flag", "value": True},
                    },
                },
            ),
        ],
    )
