# Dish-wash kit

Robotic worked-example proving openDaisugi's domain-agnosticism: the
substrate that supervised email-drafting and council-voting also
supervises a dish-washing arm. Same primitives (envelope, plan, verify,
receipts, integrity check), different domain (motion).

## What's in here

- `step_types.py` — five domain primitives (`ApproachDish`, `LocateRim`,
  `BeginScrub`, `RinseWithHose`, `ReturnToDock`) authored as
  `StepBase` subclasses with motion-specific fields (`dish_index`,
  `contact_force_n`, `flow_rate_lps`, etc.) and evidence-presence
  postconditions.
- `envelope.py` — `stakes="physical"` envelope with one structural
  invariant (`exists_step ReturnToDock`).
- `plan.py` — `_plate_wash_steps(dish_index)` builds a 5-step plate-wash
  sub-DAG; `build_plan(num_dishes)` composes N plate-wash sub-DAGs in
  series via `depends_on`. No `SubPlanStep` type needed — sequential
  composition is what the existing DAG model gives you for free.
- `run_dogfood.py` — three scenarios; mocked `MockRoboticExecutor`
  produces motion telemetry as evidence.

## What the kit proves

- **Domain-agnostic DSL invention.** The substrate doesn't care that
  `ApproachDish.dish_index` is an integer instead of `DraftEmail.recipient`
  being a string. `@step_type` plus `coerce_step` plus per-step
  postconditions handle motion and email identically.
- **Pathway-as-step composition.** Plate-wash is a reusable 5-step
  sub-DAG; dish-wash is N copies stitched with `depends_on`. The whole
  plan is one ActionPlan, one verify, one supervisor run.
- **`stakes='physical'` enforcement.** Two structural protections fire
  pre-execution: the `_check_delegation_safety` guard (refuses
  `preferred_model` on physical-stakes envelopes — no LLM-delegated
  joint targets) and the `LLMCheck` block at the predicate evaluator
  (no perceptual postconditions on motion plans).
- **Integrity check on a long DAG.** 15 steps, 15 receipts, no silent
  skips. A misbehaving executor that drops a `RinseWithHose` is caught.

## Run it

```bash
cd examples/dish-wash
# Mock executor (fast, deterministic, no physics)
python run_dogfood.py
# Or with real MuJoCo physics against the two-joint test arm
OPENDAISUGI_DISHWASH_MUJOCO=1 python run_dogfood.py
```

## Real-physics path (v0.25.1+)

`mujoco_executor.py` ships a `DishWashMuJoCoExecutor` that translates
each domain step type into concrete joint targets on a 2-DOF test arm
(`tests/fixtures/mjcf/two_joint_arm.xml`) and delegates to the in-tree
`MuJoCoExecutor`. Receipts then carry **real** `mujoco.MjData` joint
positions and end-effector poses, not fabricated telemetry. The 2-DOF
arm can't physically wash a dish, but the substrate (envelope verify
+ per-step receipts + run-end integrity) is exercised against actual
contact dynamics, torque, and the settle loop.

`tests/test_dishwash_mujoco.py` runs the kit end-to-end through real
MuJoCo on every test invocation and asserts the receipts carry
distinct, non-zero arm configurations across the five step types.

For a real dish-wash deployment, replace `mujoco_executor.py` with an
executor that drives your actual robot's URDF/MJCF and joint set; the
kit's step types and envelope stay unchanged.

## Expected output (captured in `run_output.json`)

- Scenario 1 (clean, 3 plates): `verify_ok=true`, `run_status=succeeded`,
  `integrity_passed=true`, 15/15 receipts.
- Scenario 2 (no `ReturnToDock`): `verify_ok=false`, predicate invariant
  rejects pre-execution.
- Scenario 3 (`preferred_model='haiku'` on a motion step): `verify_ok=false`,
  `_check_delegation_safety` rejects pre-execution.

## Real deployment

Replace `MockRoboticExecutor` with an executor that drives a real
robot stack — MuJoCo (`opendaisugi.executor_mujoco`), ROS-bridge, a
proprietary arm SDK, etc. The kit's step types and envelope stay
unchanged; only the executor knows how to actuate. The receipt schema
(`end_effector_xyz`, `rim_pose_error_mm`, etc.) stays the contract
between the supervisor and whatever executor is wired.
