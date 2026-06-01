# Authoring a DSL for your problem

The goal of invention is to name the *shapes* that recur in your problem so the resulting plan reads as a faithful description of the problem domain, not as a pile of shell commands. This is what distinguishes opendaisugi from a generic task runner — you're authoring the vocabulary, not just following one.

## Design grain

- **One class per recurring shape.** `DraftEmail` and `SendEmail` are different shapes; don't collapse them into `EmailStep(action="draft" | "send")`. The grain should match the verbs in the task description.
- **Typed fields for what varies.** `DraftEmail` has `recipient` and `body`; `JointMove` has `joint_targets` and `duration_s`. Anything that's the same across instances doesn't need a field.
- **`postcondition` for evidence-worth-checking.** If there's a concrete check for "did this step's effect actually happen" — a file exists, a message-id came back, joint angles landed within ε — declare it.

## Minimum shape

```python
from typing import Literal
from opendaisugi.models import StepBase, step_type, Postcondition


@step_type
class DraftEmail(StepBase):
    type: Literal["draft_email"] = "draft_email"
    recipient: str
    body: str
    signature: str
    postcondition: Postcondition | None = Postcondition(
        type="evidence_present", path="draft_hash",
    )
```

- `@step_type` registers the class so the verifier, supervisor, parser, and dynamic-loader can all find it by its `type` discriminator.
- `type: Literal["draft_email"] = "draft_email"` is the Pydantic discriminator — keep the default and the literal matching.
- The `postcondition` here says "after this step executes, the receipt's evidence must contain the key `draft_hash`." The supervisor checks this as part of writing the Receipt.

## Cross-domain examples

- **Software.** `ParseTranscript`, `ExtractEpisodes`, `ClusterByEmbedding`, `GeneralizeTemplate`, `ValidateAgainstHeldOut` — the shapes of a transcript-ingest pipeline.
- **Email.** `DraftEmail`, `ReviewDraft`, `SendEmail`, `RecordInJournal` — the shapes of an approval-gated outbound flow.
- **Council.** `SubmitContribution`, `AgentReview`, `AggregateVotes`, `CommitOrReject` — the shapes of multi-agent approval.
- **Robotic (v0.19+).** `ApproachDish`, `LocateRim`, `BeginScrub`, `RinseWithHose`, `ReturnToDock` — the shapes of a dish-washing primitive that composes into a dishwashing pathway.

## What NOT to invent

- **Generic `DoThing` catch-alls.** If you find yourself writing one class with a stringly-typed action field, you don't have a DSL; you have a script.
- **DSL-that-is-just-renamed-shell.** `RunShellCommandForEmail` doesn't earn its type — it's `ShellStep`. Invention pays off when the fields and postconditions tell you something `ShellStep` couldn't.
- **Invariants in step fields.** Cross-step claims ("signature != 'Robin'" applied to every SendEmail) belong in the envelope, not in each step. See [contract-orchestration](contract-orchestration.md).

## Testing the invention

After declaring your step types, try to write the plan. If the plan reads fluently in the DSL you invented — if the DAG structure makes the problem look solved before execution — the DSL is earning its keep. If the plan reads as a sequence of `ShellStep`s with comments explaining what each one means, the DSL missed the grain; rework it.
