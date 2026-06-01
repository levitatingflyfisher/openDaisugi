# Using a VLA inside a verified plan (v0.26+)

When a task involves a learned visuomotor policy (Physical Intelligence's
π0/π0.5, an LeRobot policy, an RT-2-style stack), the right pattern is
**one `VLAStep` per skill**, not "translate every action into a
`JointMoveStep`."

## When to use VLAStep

- The skill is best handled by a learned policy: complex contact, vision-
  conditioned behavior, multi-modal sensor input, anything where a
  handful of joint targets aren't enough.
- The ENVELOPE has structural claims about the rollout's shape (workspace
  bounds, max-action cap, final-pose requirements) — those go on the
  envelope, not inside the rollout.

## When NOT to use VLAStep

- The motion is deterministic enough to express as `JointMoveStep` /
  `CartesianMoveStep`. Don't fire a 600M-parameter model to drive an
  arm to a known pose; that's `JointMoveStep` territory and gets full
  per-step verification for ~free.
- The task is structural reasoning (planning, scheduling, decision-
  making). Use the LLM-based envelope generator + symbolic step types,
  not a VLA.

## Authoring shape

```python
@step_type
class VLAStep(StepBase):
    type: Literal["vla"] = "vla"
    task: str                                    # natural-language skill
    target_pose: tuple[float, float, float] | None = None
    max_actions: int = 50
    timeout_s: float = 5.0
```

`target_pose` is what the envelope's workspace-bounds invariant gates
against — it's the verifier's view of "where this skill is trying to
end up." The actions inside the rollout aren't visible to the verifier.

## Substrate guarantees

- **Pre-execution gate**: `target_pose` outside `workspace_bounds` →
  rejected by v0.8's `_check_workspace_containment`. No actions fire.
- **Action cap**: `min(step.max_actions, executor.max_actions_global)`
  bounds the rollout length. A misbehaving policy can't run forever.
- **Per-step receipt**: rollout summary (action count, final pose,
  contact summary, observation snapshot) lands in the journal.
  v0.18 integrity check applies — a missing VLAStep receipt fails the
  run regardless of what the executor reports.
- **Physical-stakes refusal**: `preferred_model='haiku'` on a VLAStep
  under `stakes='physical'` is rejected by v0.19's
  `_check_delegation_safety`. The VLA itself is a motor primitive, not
  LLM delegation.

## Worked example

`examples/pi-vla/` — three-skill plan, MockVLAExecutor against the
2-DOF test fixture, three scenarios (clean / out-of-bounds / delegation
attempt). Real MuJoCo state in receipts; substrate end-to-end.

## Real PI / LeRobot deployment

`docs/pi-vla-integration.md` — subclass `VLAExecutorBase`, implement
`_predict_actions`, swap MJCF for your robot's URDF. The kit's step
types and envelope stay unchanged.
