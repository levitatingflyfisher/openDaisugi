# PI-VLA kit

A Vision-Language-Action policy (Physical Intelligence's π0/π0.5, an
LeRobot policy, an RT-2-style stack) participates as one `VLAStep` per
skill inside an openDaisugi-supervised plan. The verifier treats the
VLA as opaque — what's verified is the *envelope around the rollout*
(workspace bounds, max action count, final-pose postconditions), not
the per-action stream inside the skill.

## What's in here

- `envelope.py` — `stakes='physical'` envelope with workspace bounds +
  the v0.8 `end_effector_in_workspace` invariant.
- `plan.py` — `build_plan(skills)` composes a sequence of `VLAStep`s,
  each chained via `depends_on`.
- `run_dogfood.py` — three scenarios; uses `MockVLAExecutor` against
  the existing 2-DOF `two_joint_arm.xml` test fixture. No GPU, no
  model weights.
- `run_output.json` — captured run output.

## What the kit proves

- **VLAStep is a first-class step type.** Registers via `@step_type`,
  participates in the discriminated union, round-trips through
  `ActionPlan.model_validate`.
- **Workspace bounds gate VLA targets.** v0.8's
  `_check_workspace_containment` (`z3_checks.py:117`) now treats
  `VLAStep.target_pose` the same as `CartesianMoveStep.target_position`.
  An out-of-bounds target rejects pre-execution — *before* any action
  fires.
- **Physical-stakes guard fires on VLA steps too.** `preferred_model`
  on a `VLAStep` under `stakes='physical'` rejects via v0.19's
  `_check_delegation_safety`. The VLA itself isn't LLM-routing; the
  guard fires when an agent-author tries to set a model hint inside
  a physical envelope.
- **Per-step receipts capture rollout summaries.** Action count,
  final pose, contact summary, observation snapshot. Real MuJoCo
  state, not fabricated dicts. v0.18 integrity check holds across
  the multi-skill DAG.

## Run it

```bash
cd examples/pi-vla
python run_dogfood.py
```

## What the kit does NOT prove

- It does not demonstrate that a *real* π0 is conditioned correctly on
  a task description — `MockVLAExecutor` is a deterministic linear
  interpolator, not a learned policy. The substrate works; the
  policy-conditioning quality is the real-π0 deployment's concern.
- It does not test against Aloha hardware. The 2-DOF planar arm fixture
  is a substrate-validation rig.

## Real π0 / LeRobot / RT-2 deployment

See `docs/pi-vla-integration.md` for the recipe: subclass
`VLAExecutorBase`, implement `_predict_actions(step, observation)` to
call your model, swap MJCF for your robot's URDF, run.
