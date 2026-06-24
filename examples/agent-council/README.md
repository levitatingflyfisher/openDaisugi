# AI agent orchestration council kit

Multi-agent contract-orchestration demo: a council of N reviewer agents votes
on a contribution; it commits only if quorum is met AND no reviewer flagged PII.
Z3 verifies the *structural* claims; the reviewer agents themselves make
*perceptual* judgements. Separation of concerns that a per-call allowlist
cannot express.

## Step types

`SubmitContribution` → N × `AgentReview` → `AggregateVotes` → `CommitOrReject`

## Envelope invariants (Z3-verified)

1. **All reviews present**: `exists_step AgentReview` for each reviewer slot
2. **No PII flagged**: `forall_steps AgentReview: metadata.pii_flag == false`
3. **Quorum via metadata count** — expressed as a required property on
   `AggregateVotes.metadata.quorum_met == true`

## Run it

```bash
cd examples/agent-council
python run_dogfood.py
```

## Scenarios exercised

1. **Clean**: 3 of 3 approve, no PII → commits
2. **PII flagged**: one reviewer flags PII → invariant 2 rejects pre-execution
3. **Quorum missed**: 1 of 3 approves (supervisor aggregator marks quorum_met=false) → run ends with CommitOrReject emitting rejection evidence
