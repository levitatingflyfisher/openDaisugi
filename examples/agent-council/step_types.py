"""Step types for the AI agent council kit (v0.18 worked example)."""
from __future__ import annotations

from typing import Literal

from opendaisugi.models import StepBase, step_type, Postcondition


@step_type
class SubmitContribution(StepBase):
    type: Literal["submit_contribution"] = "submit_contribution"
    submitter: str
    content: str
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="content_hash",
    )


@step_type
class AgentReview(StepBase):
    type: Literal["agent_review"] = "agent_review"
    contribution_id: str
    reviewer_id: str
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="signed_hash",
    )


@step_type
class AggregateVotes(StepBase):
    type: Literal["aggregate_votes"] = "aggregate_votes"
    contribution_id: str
    quorum_m: int
    council_size_n: int
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="quorum_met",
    )


@step_type
class CommitOrReject(StepBase):
    type: Literal["commit_or_reject"] = "commit_or_reject"
    contribution_id: str
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="decision",
    )
