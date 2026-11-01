# Robotics in openDaisugi

openDaisugi v0.8 extends the runtime-assurance pipeline — envelope generation,
static verification, supervised execution, journaling — to robotics plans that
drive a simulator (or, later, a real robot) through MuJoCo. Everything you
already know about `Envelope`, `ActionPlan`, `verify()`, and the `Supervisor`
applies; this guide covers the pieces new to robot plans.

## Install the robotics extra

```
uv pip install 'opendaisugi[robotics]'
```

This installs `mujoco>=3.0,<4.0` and `numpy>=1.24`. The base install does
not depend on MuJoCo — robot-plan support degrades gracefully to validation
only when the extra is absent.

## Robot step types

All four are members of the `ActionStep` discriminated union and verify,
journal, and export like any other step:

| Step                  | Purpose                                       |
| --------------------- | --------------------------------------------- |
| `SimulationResetStep` | Reset physics state (optional deterministic seed). |
| `JointMoveStep`       | Drive named joints to target positions.       |
| `CartesianMoveStep`   | Drive end-effector to a 3-D position (IK-solved). |
| `GripperStep`         | Open/close the gripper with a hold duration.  |

```python
from opendaisugi.models import (
    ActionPlan,
    CartesianMoveStep,
    GripperStep,
    JointMoveStep,
    SimulationResetStep,
)

plan = ActionPlan(
    source="demo",
    task="pick and place",
    steps=[
        SimulationResetStep(id="reset"),
        JointMoveStep(
            id="home", joint_targets={"j1": 0.0, "j2": 0.0}, duration_s=1.0, depends_on=["reset"]
        ),
        CartesianMoveStep(id="approach", target_position=(0.3, 0.2, 0.0), depends_on=["home"]),
        GripperStep(id="grasp", action="close", depends_on=["approach"]),
        CartesianMoveStep(id="lift", target_position=(0.3, 0.3, 0.0), depends_on=["grasp"]),
        GripperStep(id="release", action="open", depends_on=["lift"]),
    ],
)
```

## Robot permissions

`Permission` gains five optional fields — envelopes without any of them
behave exactly as before:

| Field              | Meaning                                                    |
| ------------------ | ---------------------------------------------------------- |
| `workspace_bounds` | `(min, max)` AABB constraining end-effector position.      |
| `obstacles`        | List of `(min, max)` AABBs the trajectory must not enter.  |
| `velocity_limit`   | Peak joint velocity in rad/s.                              |
| `joint_limits`     | `joint_name -> (lo, hi)` radian bounds.                    |
| `torque_limit`     | Peak `|actuator_force|` permitted at rollout time.         |

## Invariants

Declare only the invariants you want checked — each name is a handler key:

- `end_effector_in_workspace` — every `CartesianMoveStep.target_position` is inside `workspace_bounds`.
- `no_obstacle_penetration` — sampled points along the Cartesian trajectory stay outside every declared obstacle AABB.
- `velocity_bounded` — `|Δjoint|/duration · velocity_scale ≤ velocity_limit` for each `JointMoveStep`.
- `joint_limits_respected` — every `JointMoveStep.joint_targets` sits inside its declared range.

Unknown invariant types are treated as documentation — `verify()` does not
flag them.

## Running a plan under the Supervisor

```python
from opendaisugi.executor_mujoco import robotics_executors
from opendaisugi.supervisor import Supervisor

executors = robotics_executors("path/to/arm.xml")
supervisor = Supervisor(executors=executors)
session = await supervisor.run(plan, envelope)
```

The `robotics_executors(mjcf_path, **kw)` factory returns a single
`MuJoCoExecutor` wired to all four robot step kinds. The executor shares
one `MjData` across the session — `joint_move` after `sim_reset` sees the
reset state, a `cartesian_move` after a close-gripper keeps the gripper
closed.

### Envelope kwargs that reach the executor

When `Supervisor.run()` starts, it calls `executor.configure_from_envelope(envelope)`
on any executor that implements that method. `MuJoCoExecutor` uses this to
surface two fields automatically:

- `permissions.torque_limit` → `executor.torque_limit`
- non-empty `permissions.obstacles` → `executor.forbid_contacts = True`

Other executor knobs (`settle_steps`, `ik_max_iter`, `ee_body`) are
constructor-time — pass them through `robotics_executors(...)`.

## Rollout-time guards

Two guards fire during `mj_step` rollout, after every timestep, and abort
the step with a non-zero `rc`:

| rc | Constant                | Meaning                                          |
| -- | ----------------------- | ------------------------------------------------ |
| 0  | `RC_OK`                 | Step completed.                                  |
| 3  | `RC_TORQUE_VIOLATION`   | `|actuator_force|` exceeded `torque_limit`.      |
| 4  | `RC_CONTACT_VIOLATION`  | A contact occurred while `forbid_contacts=True`. |
| 5  | `RC_IK_FAILED`          | Damped-LS IK did not converge within tolerance.  |

Failures land in the run journal with the step id and a message that
names which guard fired, so the refinement loop can tighten the envelope
on the next generation.

## MJCF conventions

The executor expects a few conventions in the MJCF it loads:

- **Gripper actuators** are named with the `a_grip` prefix (e.g. `a_grip`,
  `a_grip_l`, `a_grip_r`). `GripperStep.action="open"` drives them to the
  upper end of their joint range, `close` drives them to the lower.
- **End-effector body** is named `end_effector` by default. Pass
  `ee_body="tool_tip"` to `MuJoCoExecutor` to override.
- **Angles are in radians** — include `<compiler angle="radian"/>` in the
  MJCF. The compiler otherwise interprets joint ranges in degrees.

A minimal two-DOF fixture lives at
`tests/fixtures/mjcf/two_joint_arm.xml` — copy it as the starting point
for experimentation.

## Portability

Robot pathways export through the same `export(pathway, "json")` and
`export(pathway, "skill")` routes as any other pathway. The discriminated
union carries each step's type tag end-to-end — skills exported from one
openDaisugi instance can be imported elsewhere without losing
`joint_targets`, `target_position`, `action`, or `hold_s`.

## Where openDaisugi sits in a robot stack

openDaisugi works in the slow loop, above the controllers that run in real time.

```
LLM or VLA        slow loop. Proposes the task. A separate call writes the envelope.   openDaisugi
Envelope check    before execution. Z3, schema, bounds and trajectory checks.          openDaisugi
CBF-QP filter     real-time filter, about 100 Hz. Changes an action as little as it must.   controls stack
Simplex fallback  a verified baseline takes over when the envelope is broken.          controls stack
Perception        not solved. Every line above assumes it is right.
```

A foundation model such as pi0 turns pixels into motor commands with no safety layer of
its own. Wrapping it bounds the damage a bad output can do. It does not raise the share of
tasks the model completes. The envelope can prove a force stays under 5 N. It cannot prove
the camera saw the glass. That is the perception gap, and the work here bounds it.

### What exists

| Layer | State |
|---|---|
| Agent verification: envelope, `verify`, supervisor, journal | Built and tested in Python, Go and Rust |
| Robot step types, robot permissions, four invariants | Built, experimental. Checked by `verify`. |
| MuJoCo executor with rollout-time torque and contact guards | Built, experimental. Behind the `robotics` extra. |
| VLA executor scaffolding and a pi/SmolVLA recipe | Built, experimental ([pi-vla-integration.md](pi-vla-integration.md)) |
| Evidence that the checker helps a real VLA | None. No intervention measurement has been run. |
| CBF-QP filter, trajectory pre-simulation, world-model rollout | Not built. The first is the controls stack's job. |
| Perception-conditioned envelopes | Idea only. See [the north star](north-star.md). |
| Hardware-attested checker | Designed, not built ([security model](security-model.md#hardware-attestation-designed-not-built)) |

### Prior art

Runtime safety filters under a VLA already exist, and openDaisugi does not build one.

- SafeVLA (NeurIPS 2025) aligns a VLA for safety with constrained reinforcement learning at
  training time.
- The Attention-Guided Safety Filter ([arXiv 2606.09749](https://arxiv.org/abs/2606.09749), 2026) reads the target from the
  VLA's attention heads, treats the rest of the scene as obstacles and enforces avoidance
  with a runtime control barrier function filter. It needs no retraining.
- Safe-Night VLA (arXiv 2603.05754, 2026) adds thermal perception to a VLA and enforces workspace limits with a control barrier function filter
  ([source](https://arxiv.org/abs/2603.05754)).

What we did not find is the level above the filter. Here a model writes the task-specific
constraint set that the filter enforces, and the set can only tighten a fixed base. We also
did not find verified VLA trajectories compiled into movement primitives, so that a
repeated task skips the VLA. "Not found" is not "does not exist". Check newer work before
you quote this.

### Checks at four time scales (designed)

| Scale | Check | Catches |
|---|---|---|
| About 10 ms per cycle | CBF-QP filter | A control step that leaves the safe set |
| 10 to 50 ms | Trajectory pre-simulation | A bad VLA chunk, before it runs |
| About 10 ms | World-model rollout | A meaning-level constraint, such as "do not tip the cup" |
| Each decision | Perception confidence gating | A margin that is too thin for how well the robot sees |

Only the second row has a start in this repo (the MuJoCo executor and its guards).

### The test that would show the checker works

Replay stored VLA trajectories against the checker in MuJoCo. This tests the checker, not
the closed loop. Report three numbers:

- **Intervention rate.** The share of chunks the checker blocks.
- **Intervention precision.** The share of blocks that were right.
- **Task-success retention.** The task success rate with the checker on, as a share of the rate with it off.

A checker that blocks everything scores well on safety and fails on retention. The three
numbers are read together. None has been measured.
