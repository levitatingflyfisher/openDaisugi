# Worked example — AI agent orchestration council

A shared knowledge base accepts contributions only if a council of N reviewer agents approves by quorum M, and no reviewer flags PII or secrets. Either you want that guarantee or you don't; Z3 makes it a runtime property, not a hopeful guideline.

**Full kit:** `examples/agent-council/` in the opendaisugi repo.

**Step types invented:** `SubmitContribution`, `AgentReview`, `AggregateVotes`, `CommitOrReject`.

**Envelope invariants (Z3-verified):**
1. *Quorum*: `count(reviews where approve == true) >= quorum_M`
2. *No PII*: `forall reviews: pii_flag == false`
3. *All present*: `count(reviews) == council_size_N`

**Contract orchestration:** Each reviewer agent runs under its own delegated envelope that authorizes reviewing this contribution. The aggregator step cannot run until all N signed reviews exist; the commit step cannot run unless all three invariants hold.

**Separation of structural vs perceptual:** The Z3 invariants check *structural* claims — quorum reached, no PII flagged, all votes present. They do not check whether each review was *good*. That's the `AgentReview` step's job: the reviewing agent makes its perceptual judgement (quality, usefulness, coherence) and emits its verdict as evidence; Z3 counts verdicts. This is the right division of labor between solver and model.

**Per-step receipts:**
- `SubmitContribution`: `{submitter, content_hash}`
- `AgentReview`: `{reviewer_id, approve, pii_flag, reasoning, signed_hash}`
- `AggregateVotes`: `{approve_count, pii_flag_count, quorum_met, clean}`
- `CommitOrReject`: `{decision, commit_hash OR rejection_reason}`

**Integrity guarantee:** If a reviewer agent claims to have reviewed but produced no receipt, the integrity check catches the missing review; the aggregator's invariant #3 (`count(reviews) == council_size_N`) fails; the run is marked failed. An agent that "forgot" to vote cannot pass as voting yes.

**What Z3 proves that a per-call allowlist can't:** cross-step arithmetic (`count(...) >= M`), quantifiers over subsets (`forall reviews: pii_flag == false`), existence claims binding one step to many (`aggregator depends on all N reviews`). These are the structural claims that make multi-agent governance meaningful rather than performative.
