"""Build an ActionPlan for the agent-council kit."""
from __future__ import annotations

from opendaisugi.models import ActionPlan

from step_types import AgentReview, AggregateVotes, CommitOrReject, SubmitContribution


def build_plan(
    contribution: str,
    reviewer_ids: list[str],
    reviews: list[dict],  # [{reviewer_id, approve, pii_flag}, ...]
    quorum_m: int,
) -> ActionPlan:
    submit = SubmitContribution(
        id="s0",
        submitter="orchestrator",
        content=contribution,
    )
    review_steps = []
    for i, r in enumerate(reviews):
        rs = AgentReview(
            id=f"s{i+1}",
            contribution_id="c0",
            reviewer_id=r["reviewer_id"],
            depends_on=[submit.id],
            metadata={
                "approve": r["approve"],
                "pii_flag": r["pii_flag"],
            },
        )
        review_steps.append(rs)
    agg = AggregateVotes(
        id=f"s{len(reviews)+1}",
        contribution_id="c0",
        quorum_m=quorum_m,
        council_size_n=len(reviewer_ids),
        depends_on=[r.id for r in review_steps],
    )
    commit = CommitOrReject(
        id=f"s{len(reviews)+2}",
        contribution_id="c0",
        depends_on=[agg.id],
    )
    return ActionPlan(
        source="council-kit",
        task="Gate contribution on council review",
        steps=[submit, *review_steps, agg, commit],
    )
