# MuJoCo × openDaisugi

Smoke kit that proves the envelope/executor loop closes:

1. Declare a robotics envelope with workspace + joint limits + velocity cap.
2. Stage 1 verify the proposed plan against the envelope (no sim needed).
3. Run the plan on a real MuJoCo model.
4. Read back the actual joint state and assert it sits inside the
   envelope's declared bounds — i.e., MuJoCo did what the envelope said
   it would.

## Install

The MuJoCo executor is an optional extra:

```bash
pip install 'opendaisugi[robotics]'
```

## Run

```bash
python examples/integrations/mujoco/smoke.py
```

Expected output (numbers will vary slightly between MuJoCo versions):

```
Stage 1 verify OK (X.X ms)
Rollout OK: j1=+0.700 (bounds [-1.5, 1.5])
            j2=-0.600 (bounds [-1.5, 1.5])
Envelope bounds held across rollout. Smoke kit green.
```

## What this kit does NOT do

This kit is a smoke test, not a physics validation suite. It does not:

- Verify torque saturation (see `MuJoCoExecutor`'s built-in
  `RC_TORQUE_VIOLATION`).
- Verify obstacle avoidance along the trajectory (see
  `tests/test_z3_robotics.py` for Z3-checked segment/AABB intersection).
- Drive an RL training loop or batched rollout.

For full test coverage of the robotics pipeline run the suite:

```bash
pytest tests/test_mujoco_executor.py tests/test_z3_robotics.py
```

## Hardware

On a CPU-only machine: `two_joint_arm.xml` loads in <100 ms, rollouts
run in milliseconds, peak resident memory is a few hundred MB. No GPU
is required. The kit is explicitly tuned to run on a 12 GB laptop as
well as the 40 GB / RTX 4080 box.
