# Per-step postconditions and evidence

A `Postcondition` on a step declares "here's what evidence must show up for this step to count as done." The supervisor writes the step's `Receipt` with the evidence and the postcondition verify result; the run-end integrity check reads that receipt to know every step actually executed.

## When to declare a postcondition

- **Declare it when there's concrete evidence worth checking.** `SendEmail` should produce a message-id. `JointMove` should produce final joint angles. `FileWrite` should produce the written file's hash. If such evidence exists, encode it.
- **Leave it None when execution-happened-is-enough.** A diagnostic `Ls` step that just enumerates a directory doesn't need a postcondition beyond "the step ran." The Receipt gets written with `verify_result=True` (by default on success) and the integrity check sees it.

## Evidence shape

`Receipt.evidence: dict[str, Any]` is free-form. Fill it with whatever the execution actually produced:

- Shell: `{"rc": 0, "stdout": "...", "duration_ms": 42.5}`
- Email: `{"message_id": "<abc@server>", "recipient": "...", "timestamp": ...}`
- Robotic: `{"final_joint_angles": [0.1, 0.2, 0.3], "pose_error_mm": 0.4}`
- File write: `{"path": "/tmp/out.yaml", "sha256": "...", "bytes_written": 1024}`

`evidence_hash` is the sha256 of canonical-JSON of `evidence`; content-addressing makes receipts comparable across runs without exposing raw evidence.

## Postcondition patterns

### Structural presence
"The evidence dict must contain key X."
```python
postcondition = Postcondition(type="evidence_present", path="message_id")
```
The v0.18 supervisor treats `path` as a required key in the evidence dict. Simple but catches most silent-skip cases.

### Structural range
"The evidence must contain a numeric X within [min, max]."
```python
postcondition = Postcondition(type="numeric_range", path="pose_error_mm", min=0.0, max=1.0)
```
For robotics, this is how you encode "arrived within tolerance." For v0.18 the supervisor's built-in postcondition check handles `path`-presence only; richer checks are delegable to kit-specific subclasses of `Supervisor` that override `_check_step_postcondition`.

### LLM-as-judge
"An agent reviewed this and said it's acceptable."
```python
postcondition = Postcondition(type="review_passed", path="reviewer_decision")
```
Evidence must contain `reviewer_decision == "approved"`. The supervisor doesn't know what "approved" means — it only checks structural presence; your executor or a separate review step handles the judgement.

## Postcondition failure

When a postcondition fails, the supervisor marks the step's receipt `verify_result=False`, halts the run, and marks `session.status = FAILED`. Subsequent steps don't run. This is the right semantics: a step that can't prove it happened shouldn't quietly be succeeded-past.

## Receipt-based integrity

Even without per-step postconditions, every executed step produces a Receipt. The run-end integrity check verifies `{receipted_step_ids} >= {expected_step_ids}`. If your executor is correct but your sub-agent silently skipped a step, the integrity check catches it. This is a separate guarantee from postcondition success — receipts prove steps ran; postconditions prove they produced the right thing.
