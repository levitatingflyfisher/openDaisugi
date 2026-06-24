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
    ActionPlan, CartesianMoveStep, GripperStep, JointMoveStep,
    SimulationResetStep,
)

plan = ActionPlan(source="demo", task="pick and place", steps=[
    SimulationResetStep(id="reset"),
    JointMoveStep(id="home", joint_targets={"j1": 0.0, "j2": 0.0},
                  duration_s=1.0, depends_on=["reset"]),
    CartesianMoveStep(id="approach", target_position=(0.3, 0.2, 0.0),
                      depends_on=["home"]),
    GripperStep(id="grasp", action="close", depends_on=["approach"]),
    CartesianMoveStep(id="lift", target_position=(0.3, 0.3, 0.0),
                      depends_on=["grasp"]),
    GripperStep(id="release", action="open", depends_on=["lift"]),
])
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
