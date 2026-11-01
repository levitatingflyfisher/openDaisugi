# W3: the three envelope-kernel gaps robots need (index paths, a gripper effort field in ROS 2 vocabulary, the delta-sat verdict class) Implementation Plan

> **Owner rulings of 2026-10-30 (second round), folded in below:** (1) the Z3-timeout policy stays as today (a warning below strict stakes); only `delta_sat` denies at every stakes. (2) The delta query's variable box comes from the envelope's own bounds; a variable the envelope leaves unbounded falls back to the fixed default box. (3) No actuator vocabulary of our own: field names and meanings follow ros2_control's command and state interfaces (`position`, `velocity`, `effort`) and the control_msgs gripper action, where the gripper command carries a position and a ceiling on the effort used to reach it. daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware. **Where the ROS 2 docs differ from the ruling's wording:** the ruling named `max_effort` from `control_msgs/msg/GripperCommand`; that action's controller is deprecated in ros2_controllers Jazzy and Rolling in favour of `parallel_gripper_controllers`, whose `ParallelGripperCommand` goal is a `sensor_msgs/JointState command` with `effort: (optional) max effort of the joint allowed while moving (Newtons or Newton-meters)`. The meaning is the same (a ceiling); the current name is `effort`. This plan follows the current docs and uses `effort`.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the three kernel gaps named in `docs/plans/2026-09-24-omarchy/design-dialects.md` section 3.4 and decision 7, in the Python oracle, Go and Rust, with golden cases for each: (1) a predicate path can index an element of a tuple or list field, so `target_position.0` and `workspace_bounds.1.2` are paths; (2) the kernel has a gripper effort dimension in ROS 2 vocabulary: a `GripperStep` carries a ROS 2 gripper command (`position`, optional `velocity`, optional `effort` as the ceiling on the effort used while moving, newtons for a linear gripper), `Permission.gripper` bounds the command fields and the measured state interface (`command_effort_max`, `command_velocity_max`, `state_effort_max`), with a `gripper_effort_bounded` invariant at plan time, a subsumption rule, and a rollout guard that maps the same words onto MuJoCo (`ctrl`, `actuator_forcerange`, `actuator_force`); (3) the verifier has a fourth verdict class, `delta_sat`, beside proved, refuted and unknown. It is never `ok`, it denies in enforce, and it comes only from an optional external delta-complete solver. Z3 stays the only required solver. The owner ruled on 2026-10-30 to build these now instead of waiting for a robot user (overturns the DI-7 default).

**Architecture:** Gap 1 is one rule in the four copies of the path resolver (Python `predicate_z3._resolve_path`, Go `verify/evaluate.go` and `gate/predicate.go`, Rust `predicate.rs` and `gate/predicate.rs`) plus a shared golden table. Gap 2 is three optional command fields on the gripper step and three optional bounds on the permission, named as ros2_control names them, one recognized opaque invariant with a handler beside the four robotics handlers, one subsumption clause, and one rollout guard beside the torque guard, in each port. daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware. Gap 3 adds one nonlinear predicate op (`distance_range`, Euclidean distance of a 3-vector, polynomial in its squared form), a verdict vocabulary (`SolverVerdict`), and one solver module per port that routes a nonlinear goal: Z3 first; on Z3 `unknown`, an optional external `dreal` binary over SMT-LIB2 text on stdin; no binary means `unknown`. Only an exact `unsat` ever yields a proof. A `delta-sat` answer becomes the `delta_sat` verdict: a Violation at every stakes, a deny in enforce, a would-deny in audit. The yellow paper gets the path grammar, the gripper effort dimension, the verdict classes and the soundness argument; ADR-0029 records the verdict-class decision.

**Tech Stack:** Python 3.12 (pydantic, pytest, z3-solver), Go (cgo, the linked Z3 5.1.0, SMT-LIB2 text), Rust (cargo, z3-sys vendored), MuJoCo behind the `mujoco` build tag and Cargo feature, shell. No new dependency: the delta solver is an external binary the operator installs and names in `config.yaml`; tests use a fake.

**Spec:** this file's sections "What exists and what is open" and "Rulings RK-1 to RK-13". Also `docs/plans/2026-09-24-omarchy/design-dialects.md` section 3.4 (the table and the paragraph after it) and section 12 item 7; `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md:103` (DI-7); the yellow paper `docs/spec/yellow-paper.md` sections 1, 2.1, 2.4 property 4, 4.4, 5.3, 6 and 7; `clients/PORTING-NOTES.md`; `docs/spec/conformance.md` (the wire protocol and the Full profile); `docs/robotics.md`.

**Names.** This plan is written in the names W2 lands: warden (was gate), router (was gateway). Builds start after W2 merges. Where a path differs before and after W2 it is written as `today-path` (W2: `new-path`).

**Research used (online, 2026-10-30).** dReal4 is Apache-2.0 ([github.com/dreal/dreal4](https://github.com/dreal/dreal4)); its last release is 4.21.06.2 of June 2021 ([pypi.org/project/dreal](https://pypi.org/project/dreal), [pyup.io/packages/pypi/dreal](https://pyup.io/packages/pypi/dreal/)); it builds on Ubuntu 20.04 and 22.04 and macOS, and ships a Docker image; it reads SMT-LIB2 on stdin with `--in --format smt2 --precision ARG` (default 0.001); it prints `unsat`, or `delta-sat with delta = <d>` (plain mode), or `delta-sat` (SMT-LIB-compliant mode) (`dreal/smt2/driver.cc`). Its system dependencies IBEX (LGPL-3.0) and CLP (EPL) stay outside our tree because we never link it. The delta-complete framework is Gao, Avigad and Clarke, [arXiv 1204.3513](https://arxiv.org/abs/1204.3513): on a bounded formula the procedure answers `unsat` (exact) or `delta-sat` (the delta-weakening is satisfiable). Z3 decides polynomial real arithmetic with the NLSAT procedure of Jovanović and de Moura, IJCAR 2012 ([leodemoura.github.io/files/nlsat.pdf](https://leodemoura.github.io/files/nlsat.pdf), [Z3 internals](https://z3prover.github.io/papers/z3internals.html)), complete modulo resources, so a Euclidean-distance goal in squared form needs no second solver in the common case. No Fedora package of dReal exists; this box cannot run it. The real binary is a hand check on a box that has it; every test and fixture uses the fake.

**ROS 2 vocabulary (online, 2026-10-30).** ros2_control's standard interface names are `position`, `velocity`, `acceleration`, `effort`, `torque`, `force`, `current`, `temperature` ([hardware_interface_type_values.hpp](https://github.com/ros-controls/ros2_control/blob/master/hardware_interface/include/hardware_interface/types/hardware_interface_type_values.hpp)). The legacy gripper command is `control_msgs/msg/GripperCommand` with `float64 position` and `float64 max_effort`, no comments ([GripperCommand.msg](https://github.com/ros-controls/control_msgs/blob/master/control_msgs/msg/GripperCommand.msg)); its action result reports `position` "(in meters)", `effort` "(in Newtons)", `stalled` "True iff the gripper is exerting max effort and not moving", `reached_goal` ([GripperCommand.action](https://github.com/ros-controls/control_msgs/blob/master/control_msgs/action/GripperCommand.action)); the controller's `max_effort` parameter is "Max allowable effort" ([gripper_controllers, Jazzy](https://control.ros.org/jazzy/doc/ros2_controllers/gripper_controllers/doc/userdoc.html)), and that controller is deprecated in Jazzy and Rolling in favour of `parallel_gripper_controllers`. The current action is `ParallelGripperCommand`: goal `sensor_msgs/JointState command` with `position` "(radians or meters)", `velocity` "(optional) max velocity of the joint allowed while moving", `effort` "(optional, not used if empty) max effort of the joint allowed while moving (Newtons or Newton-meters)"; result and feedback carry the measured `position`, `velocity`, `effort` and the two booleans ([ParallelGripperCommand.action](https://github.com/ros-controls/control_msgs/blob/master/control_msgs/action/ParallelGripperCommand.action)). The parallel gripper controller claims `{joint}/position` by default and the velocity and effort command interfaces when `max_velocity_interface` or `max_effort_interface` is set; its `max_effort` parameter is "Default effort used for grasping (Newtons)" ([parallel_gripper_controller, Jazzy](https://control.ros.org/jazzy/doc/ros2_controllers/parallel_gripper_controller/doc/userdoc.html)). So the current ROS 2 name for the ceiling is `effort` inside the command, and the state interface that measures it is also `effort`. This plan uses those words. MuJoCo carries the same three: `data.ctrl` (the position command of a position actuator), `model.actuator_forcerange` with `actuator_forcelimited` (the ceiling on the force the actuator may apply), `data.actuator_force` (the measured effort).

## Global Constraints

- Box: 4 cores, 15 GB. **One heavy job at a time**, each under `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait`, with `-j2` (`CARGO_BUILD_JOBS=2`, `GOFLAGS=-p=2`, `go test -p 1`). A heavy job is: any Go or Rust build, a MuJoCo-tagged build, the full pytest suite, any compare suite, any fixture regeneration, any fuzz run. Never rebuild Z3, never `cargo clean` in `clients/rust`, never rebuild the native prefix; `clients/go/scripts/native.sh --mujoco` runs at most once on this branch, and only if `clients/go/.native` has no `lib/libmujoco.so`.
- `/tmp` is RAM. The worktree is `/mnt/tera/working/programming/od-wt/w3-robot-kernel` (branch `w3-robot-kernel` from master after W2 merges). Scratch is `/mnt/tera/.tt/w3-robot-kernel/` (fake HOMEs, the fake solver's logs, `DAISUGI_WARDEN_SCRATCH`). Delete scratch when a task ends; delete `clients/rust/target` at merge.
- No network and no real model in any test. No real `dreal` in any test or fixture: the fake `clients/fake_delta.py` answers. `MUJOCO_GL=disable` for every robotics run. The robotics compare needs `libmujoco.so` from the native prefix; the Go and Rust MuJoCo builds are behind the `mujoco` tag and feature and never ship.
- Never `pkill -f`. Stop a process by the pid in its lock file or its own stop verb.
- No AI attribution in commits. Commits state the why and the failure they fix. Never commit `CLAUDE.md` or `docs/superpowers/`. Commit by explicit path.
- Prose in STE: short sentences, plain words, active voice, no em dashes. Never the words "seam" or "the layer". No changelog voice in docs: write the current state.
- Every warden run in a hand check passes its own `--data-dir` and root under scratch. The owner's `~/.opendaisugi`, `~/.local/state/coppice` and `~/.local/bin` are never read, written or moved. Tests never run `daisugi install`.
- **Fail closed holds at every commit of this branch in enforce mode.** Every change here is in the deny direction or adds a field that is unset by default. No existing golden case flips from deny to allow. A case that flips from allow to deny is listed in the Task 12 commit message with its reason.
- Full parity: a behaviour the oracle gains, Go and Rust gain in this branch, and the golden cases cover it. A port that cannot do a case refuses it (exit 2, one line, nothing written) and that refusal gets a ruling in `clients/ADJUDICATIONS.md`. A fail-open disagreement (the binary allowed, the oracle denied) blocks the merge. Lean and TypeScript are Core-profile clients (`clients/lean/README.md`, `docs/client-diversity.md`); they do not see the predicate algebra and are out of this plan.
- All Z3 term building and solving stays inside `z3_checks.Z3_SOLVE_LOCK` (Python). The delta subprocess runs outside the lock: build the text under the lock, release, run.

## Review Focus

1. **A delta-sat never becomes `ok`.** Trace every path that reads a `SolverAnswer`: vacuity (contradiction and tautology), subsumption (predicate dimensions), `verify_predicate_z3`, the warden's deny. In each, the only `⊤` comes from an exact `unsat`. Task 11's `test_delta_sat_is_never_ok` runs the fake in `delta-sat` mode through all of them. A reviewer looks for any branch where `delta_sat` is handled like `non_trivial` on the proof side.
2. **Polarity of the two vacuity queries.** The contradiction query asks "is the goal satisfiable"; `delta-sat` there means "not shown contradictory", which is `non_trivial`, and that is the correct, lenient-looking answer because concrete evaluation still decides. The tautology query asks "is the negation satisfiable"; `delta-sat` there is `non_trivial` too. Neither yields a proof. Task 10's table test pins all four cells.
3. **The box.** A proof through the delta solver holds inside each Real variable's box (RK-9): the envelope's own bound where it has one, the default `[-B, B]` where it has none. The concrete range rule (`outside_delta_box`) is what makes it a proof about the plan. A reviewer checks that the rule fires on a value of exactly `bound + 1e-9` and on `inf` and `nan`, that `target_position.1` takes its box from `workspace_bounds` and not from `B`, and that the Z3 path (which needs no box) never emits the rule's violation on a linear goal.
4. **Index segments do not reach dict keys by mistake.** `joint_targets.0` on `{"joint_targets": {"0": 1.0}}` is the dict key `"0"` (dicts first); on `{"joint_targets": [1.0]}` it is the element. `target_position.-1`, `.01`, `.3` on a 3-vector are MISSING. Task 2's table has every row; Tasks 3 and 4 read the same table.
5. **The gripper words are ROS 2's, and MuJoCo gets the same mapping.** The step's `effort` is a commanded ceiling (`ParallelGripperCommand.command.effort`); the executor applies it as `actuator_forcerange = [-effort, effort]` with `actuator_forcelimited = 1` on the `a_grip*` actuators for the step, and measures the state interface `effort` as `|actuator_force|` against `gripper.state_effort_max`. A reviewer checks no field name of our own invention appears (`force_n`, `grip_force`), that the docs carry the one sentence "daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware", and that `RC_GRIPPER_VIOLATION` is a new code (6), not a reuse of 3.
6. **Nothing new is required.** `uv sync --locked` after this branch installs no new wheel; the Go and Rust binaries link nothing new; `delta_solver` unset means every nonlinear goal that Z3 cannot decide is `unknown`, which denies as today. Task 16 checks the lock, `go.sum` and `Cargo.lock` are unchanged except for nothing.

---

## What exists and what is open (measured on master 88488cbf, 2026-10-30)

| Item | What exists (Built) | What is open (confirmed) |
|---|---|---|
| 1. Index paths | `_resolve_path` (`predicate_z3.py:57`) splits on `.` and walks dict keys, then attributes. Go `verify/evaluate.go:69` (dict only) and `gate/predicate.go:263` (dict, then getattr); Rust `predicate.rs:283` and `gate/predicate.rs:914`. `length_range` already counts lists. Robot steps carry tuples: `CartesianMoveStep.target_position: tuple[float, float, float]` (`models.py:459`), `target_orientation` (4-tuple), `Permission.workspace_bounds` (pair of 3-tuples), `obstacles` (list of pairs). | A segment is only ever a key or attribute. `target_position.0` is MISSING in all three ports, so `numeric_range` on it is false and `exists` on it is false. No golden table covers path resolution on its own; it is only exercised through whole predicates. |
| 2. Gripper effort | `Permission.torque_limit` (Nm, `models.py:141`) is a rollout guard on `|actuator_force|` over every actuator (`executor_mujoco.py:265-275`, `RC_TORQUE_VIOLATION = 3`); `velocity_limit` has a plan-time handler (`z3_checks.py:225`) and a backing-bound vacuity rule (`verify.py:1118`); subsumption covers `workspace_bounds`, `velocity_limit`, `torque_limit`, `joint_limits`, `obstacles` (`subsumption.py:384` onward, yellow paper 5.3). `GripperStep` has `action` and `hold_s` (`models.py:466`). Go `verify/robotics.go`, `models.go:42-46`; Rust `z3_checks.rs:84-297`, `models.rs:66-75`. Robotics golden cases: 79 (`clients/fixtures/robotics/cases.jsonl`, `grip_ctrlrange.xml`, `grip_unbounded.xml`). | No effort or force field anywhere: not on the permission, not on a step, not in subsumption, not at rollout. The design-dialects table marks `grip_force_below(n)` "not expressible". The torque guard reads every actuator, so a gripper effort bound cannot be stated apart from arm torque. `GripperStep` has no ROS 2 command shape: `action: open|close` only, no `position`, `velocity` or `effort`. |
| 3. Verdict classes | Verdicts are `ok` plus violations and warnings (`VerificationResult`, `models.py:706`). Z3 `unknown` raises `VerificationTimeout`; `verify()` keeps it as a warning below strict stakes and a Violation under strict or physical (`verify.py:50-100`, `test_verify_z3_timeout_policy.py`); the warden denies on the warning in every mode (`gate.py:1065`); subsumption returns `timed_out=True, holds=False` (`subsumption.py:647`); vacuity returns `non_trivial` on unknown (`vacuity.py:72`). The predicate stage decides concrete plans by evaluation (`verify.py:1212`); Z3 runs on predicates only in vacuity and subsumption (symbolic). `verify_predicate_z3` (`predicate_z3.py:600`) has no caller in `src/`. Go and Rust emit SMT-LIB2 text and read `sat`, `unsat`, `unknown` (`z3client.go:27`, `z3_bridge.rs`). | No op of the algebra is nonlinear: `numeric_range` is a bound on one path. So no goal ever reaches a nonlinear theory, and the yellow paper says "Nonlinear robot dynamics would bring the same outcome" as a string `unknown` (2.4 property 4). There is no verdict word for "a perturbed witness exists". There is no second solver and no place to put one. The `timeout_ms` of `check_plan_invariants` is "reserved for future symbolic-trajectory work" and unused. |

Dropped from this plan, with evidence: a **cross-agent predicate** for `keep_separation(d)` (design-dialects 3.4, row 2) stays with `swarm.py` (analytic AABB, "space, not spacetime"); `distance_range` gives a single-step radius bound, not a pairwise one, and that is said in the docs (Task 15). **Dialect words** (`grip_force_below`, `within_geofence`) are Stage 3 work in design-dialects section 11; this plan makes them writable and leaves them unwritten. **Transcendental functions** (rotations, trigonometry) are not added: no op emits `sin` or `cos`, so Z3's polynomial procedure covers every nonlinear goal this plan creates, and the delta route is a fallback on `unknown` plus the operator's choice (`nonlinear_solver: delta`). The verdict class and the solver module are built now so that a transcendental op later is one compile case, not a new architecture.

## Rulings RK-1 to RK-13 (this plan; provisional until the owner reads them)

- **RK-1: index segments.** A path is `seg ("." seg)*`. A segment that is `0`, or a decimal integer with no leading zero and no sign, is an index when the current value is a list or tuple: element `i` when `0 ≤ i < len`, else MISSING. When the current value is a dict, the segment is a key first (a dict with key `"0"` wins). A negative, a leading zero (`01`), a `+`, a float, an index into a string, number, bool, null, or into an object with attributes, is MISSING. MISSING keeps its meaning: `equals`, `numeric_range`, `matches`, `in_set` and `exists` are false on it; `not_equals`, `not_in_set` and `not_matches` keep today's rule for a missing path, whatever it is in each port (the golden table records it).
- **RK-2: names.** `_var_name(prefix, path)` is unchanged (`s0__target_position__0__real`). The Go and Rust SMT-LIB2 emitters produce the same symbol; a digit never starts a symbol, so it stays a simple symbol.
- **RK-3: the gripper effort dimension, in ROS 2 words.** The step: `GripperStep` keeps `action: open|close` and `hold_s`, and gains the `ParallelGripperCommand` command fields, flat on the step with the ROS 2 names and comments: `position: float | None` (metres for a linear gripper, radians for a revolute one; when set it is the setpoint and `action` only names the direction for the journal), `velocity: float | None` (`ge=0`; max velocity allowed while moving, m/s or rad/s), `effort: float | None` (`ge=0`; max effort allowed while moving, newtons or newton-metres; `None` means undeclared). The envelope: `Permission.gripper: GripperLimits | None` with `command_effort_max`, `command_velocity_max`, `state_effort_max` (each `float | None`, `ge=0`, the same units); the first two bound the command fields, the third bounds the measured state interface `effort`. `gripper_effort_bounded` joins `RECOGNIZED_OPAQUE_TYPES`. Its handler: for every gripper step, `effort is None` is a violation (reason `effort_undeclared`); `effort > gripper.command_effort_max` is a violation (reason `effort_exceeds_limit`); when `gripper.command_velocity_max` is set, `velocity is None` or `velocity > command_velocity_max` is a violation (reasons `velocity_undeclared`, `velocity_exceeds_limit`). `gripper_effort_bounded` declared with `gripper is None` or `command_effort_max is None` is vacuous (backing bound `gripper.command_effort_max`), as `velocity_bounded` is with `velocity_limit`. `state_effort_max` is a rollout bound only (RK-5). Non-gripper steps are not read: `cartesian_move` and `joint_move` gain no effort field in this plan; the arm's `effort` state stays the existing `torque_limit` guard. No name of our own: the words are ros2_control's interface names and the control_msgs comments.
- **RK-4: subsumption.** Each of `command_effort_max`, `command_velocity_max`, `state_effort_max`: inner ≤ outer; inner `None` where outer is set denies ("you cannot delegate into an unbounded region", yellow paper 5.3). Outer `None`, or outer `gripper` `None`, constrains nothing.
- **RK-5: the rollout guard, the same words on MuJoCo.** `MuJoCoExecutor.gripper` is set from the envelope by `configure_from_envelope`. For a gripper step, over the actuators whose name starts with `a_grip`: `position` (when set) becomes `data.ctrl` for the step (today's open/close target range when unset); `effort` (when set) becomes `model.actuator_forcerange = [-effort, effort]` with `actuator_forcelimited = 1` for the step and is restored after it (a commanded ceiling, as `ParallelGripperCommand` means it); `velocity` is recorded in the receipt and not enforced in the simulator (position actuators have no velocity cap; the docs say so). After each physics step of the hold, the measured state `effort`, `|data.actuator_force|` over the `a_grip*` actuators, must not exceed `gripper.state_effort_max` when set. A breach, a `position` outside the actuator's `ctrlrange` or joint range, or a gripper bound with no `a_grip*` actuator to apply it to, returns `RC_GRIPPER_VIOLATION = 6` with the measured value, the bound and the step (fail closed). Go and Rust mirror the guard bit for bit; the robotics compare proves it. daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware.
- **RK-6: `distance_range`.** `{"op": "distance_range", "path": P, "point": [px, py, pz], "min": m, "max": M}` with `0 ≤ m ≤ M`. Concretely: the value at `P` must be a list or tuple of exactly three numbers (ints or floats, not bools), else the op is false; then `m ≤ sqrt(Σ (vi − pi)²) ≤ M` computed as `m² ≤ Σ (vi − pi)² ≤ M²` in exact rational arithmetic (`fractions.Fraction` in Python; the ports use the same squared comparison on `f64` and the golden table has no case within 1e-9 of a boundary). Symbolically: three Real variables `P.0`, `P.1`, `P.2` and the squared form; the compiled term is **nonlinear**. `point` entries must be finite. It is the only nonlinear op. `length_range`, `exists` and `is_empty` on `P` are unchanged.
- **RK-7: verdict classes.** `SolverVerdict = Literal["proved", "refuted", "unknown", "delta_sat"]` (`src/opendaisugi/verdict.py`). Only `proved` admits. `delta_sat` is a Violation at every stakes, at plan time and at call time (stricter than the Z3-timeout policy, which is a warning below strict). `VerificationResult.verdict: SolverVerdict` is derived in `_result`: `refuted` if any violation whose `detail.reason` is neither `z3_timeout` nor `delta_sat`; else `delta_sat` if any violation has reason `delta_sat`; else `unknown` if any violation or warning is a Z3 timeout or a delta-solver no-answer; else `proved`. `ok` is unchanged in meaning; `verdict == "unknown"` with `ok == True` is exactly today's lenient timeout case and nothing else.
- **RK-8: solver routing and the external binary.** Config keys (`config.yaml`, read by the oracle, the Go and the Rust warden and CLI the way `dialect_enforce` is read): `delta_solver` (a path to an executable; default unset), `delta_precision` (float, default `0.001`), `nonlinear_solver` (`z3` default, or `delta`), `delta_timeout_ms` (int, default: the call's Z3 timeout). A linear goal goes to Z3 only, as today. A nonlinear goal with `nonlinear_solver: z3` goes to Z3; on `unknown`, to the delta solver if set, else it is `unknown`. With `nonlinear_solver: delta` it goes to the delta solver at once; unset means `unknown`. The delta solver runs as `<delta_solver> --in --format smt2 --precision <delta_precision>` with the SMT-LIB2 text on stdin, in a fresh process, with a wall-clock budget of `delta_timeout_ms`, cwd the scratch root, an empty environment but `PATH`, `HOME`, `LANG`. Output: a first non-empty stdout line equal to `unsat` is an exact unsat; a first line starting with `delta-sat` is `delta_sat` with `delta` parsed after `delta = ` when present, else `delta_precision`; anything else, a non-zero exit, no output, or the budget spent is `unknown`, with the reason text `delta solver <which> gave no answer (<why>)` kept where a Z3 timeout text is kept today. `is_z3_timeout_warning` matches that text too, so the warden denies on it in every mode.
- **RK-9: eligibility and the box (owner ruling: the envelope's own bounds first).** A query goes to the delta solver only when every declared variable has sort Real or Bool (no String, no Int, no regex, no `Length`). Each Real variable gets a box from the envelope's own bounds, by its path: `target_position.i` from `workspace_bounds[0][i]..[1][i]`; `joint_targets.<j>` from `joint_limits[j]`; `velocity_scale` from `[0, 1]`; gripper `effort` from `[0, gripper.command_effort_max]`; gripper `velocity` from `[0, gripper.command_velocity_max]`; `duration_s` and `hold_s` from `[0, max_execution_time_s]`; a variable with no such bound (the envelope leaves it unbounded, or the path is none of these) falls back to `[-B, B]` with `B = delta_box` (config, default `10000.0`, SI units: metres, newtons, radians, seconds). The function `solver.variable_box(path, envelope, cfg) -> tuple[float, float]` is the one place this table lives, in each port. The text is `(set-logic QF_NRA)`, `(set-option :precision <delta>)`, the declarations, one `(assert (and (<= lo x) (<= x hi)))` per Real variable, the assertions, `(check-sat)`. An `unsat` from the delta solver therefore proves the goal inside those boxes. To make that a proof about the plan: when a nonlinear goal is evaluated on a concrete step, every number it reads must lie in its own box and be finite, else the step gets a violation (reason `outside_delta_box`, detail names the path, the value and the box and whether the box came from the envelope or the default), in every solver mode. A linear goal never emits this violation.
- **RK-10: polarity.** Vacuity contradiction query (goal satisfiable?): delta `unsat` is `contradiction`; `delta-sat` is `non_trivial`. Vacuity tautology query (negation satisfiable?): `unsat` is `tautology`; `delta-sat` is `non_trivial`. Subsumption and entailment (negation of "inner implies outer" satisfiable?): `unsat` holds; `delta-sat` is `holds=False`, `delta_sat=<d>`, reason `delta-satisfiable (delta = <d>): not proved`. No cell yields `⊤` from a `delta-sat`.
- **RK-11: the warden.** A verify result with `verdict == "delta_sat"` denies in enforce with the message `the verifier found a witness within delta = <d> for <which>; not proved; denied`; audit logs a would-deny with `reason: delta_sat` and the delta. `delta_sat` violations carry `detail = {"reason": "delta_sat", "delta": d, "solver": "delta", "which": which}`.
- **RK-12: the binary is never a dependency.** No pyproject entry, no vendoring, no download, no link. `docs/reference/dependencies.md` lists dReal4 as an optional external tool (Apache-2.0; its IBEX LGPL-3.0 and CLP EPL dependencies are the operator's). Tests and fixtures use `clients/fake_delta.py` only. The real binary is a hand check on a box that has it (Ubuntu 22.04 or the dReal Docker image), recorded in the ledger, never a test.
- **RK-13: numbering.** Port disagreements found while building get `RK-R-n` entries in `clients/ADJUDICATIONS.md` under a dated heading; the rulings above are copied there once in Task 15.

## Task ranking by risk

1. Tasks 9 to 11 and 13 to 14: the solver module and the verdict plumbing. A branch that reads `delta-sat` as a proof is a silent fail-open in the proof path, the worst class. Each port has a table test over the four polarity cells and the three output classes.
2. Task 2 to 4: the resolver. A wrong index rule that resolves to a neighbour element makes a bound check pass on the wrong coordinate. The table is shared.
3. Tasks 5 to 8: force. The plan-time handler and subsumption are set checks; the rollout guard is the one place a measurement decides, and the robotics compare checks every float.
4. Tasks 1, 12, 15, 16: the worktree, the golden cases, the docs, the merge check.

---

### Task 1: Worktree, baseline, the fake delta solver

**Files:**
- Create: `clients/fake_delta.py` (the fake delta solver, a Python script; laid out into case roots the way `garden_cases.FAKE_CLAUDE` is)
- Test: `tests/test_fake_delta.py`

**Interfaces:**
- Produces: `clients/fake_delta.py`, run as `fake_delta.py --in --format smt2 --precision <d>`. It reads stdin to EOF; it answers by the environment variable `DAISUGI_FAKE_DELTA` (`unsat`, `delta-sat`, `delta-sat-plain`, `garbage`, `crash`, `hang`, `empty`): `unsat` prints `unsat`; `delta-sat` prints `delta-sat with delta = <precision>`; `delta-sat-plain` prints `delta-sat`; `garbage` prints `sat`; `crash` exits 3 with a stderr line; `hang` sleeps 600 s; `empty` prints nothing and exits 0. Before answering it checks the input: parentheses balanced, one `(check-sat)`, a `(set-logic QF_NRA)` line, no `String`, no `Int`, no `str.`, no `re.`; a failed check prints `unsupported` and exits 2. It appends the input to `$DAISUGI_FAKE_DELTA_LOG` when set.

- [ ] **Step 1: Make the worktree and scratch**

```bash
cd /mnt/tera/working/programming/openDaisugi
git fetch --all --prune
git worktree add ../od-wt/w3-robot-kernel -b w3-robot-kernel master
mkdir -p /mnt/tera/.tt/w3-robot-kernel
df -h /mnt/tera | tail -1
cd ../od-wt/w3-robot-kernel && uv sync --locked --extra dev
```
Expected: the worktree exists on real disk; more than 20 GB free.

- [ ] **Step 2: Record the baseline**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait uv run --no-sync pytest -q -x -p no:cacheprovider tests/test_predicate.py tests/test_predicate_z3.py tests/test_verify_robotics.py tests/test_z3_robotics.py tests/test_subsumption_robot.py tests/test_vacuity.py tests/test_verify_z3_timeout_policy.py tests/test_gate_z3_timeout_deny.py
```
Expected: green. Keep the count for the commit message.

- [ ] **Step 3: Write the failing test for the fake**

```python
# tests/test_fake_delta.py
import os, subprocess, sys
from pathlib import Path

FAKE = Path(__file__).resolve().parents[1] / "clients" / "fake_delta.py"
TEXT = "(set-logic QF_NRA)\n(declare-fun x () Real)\n(assert (> (* x x) 2.0))\n(check-sat)\n"


def _run(mode, text=TEXT, precision="0.001"):
    env = {**os.environ, "DAISUGI_FAKE_DELTA": mode}
    return subprocess.run(
        [sys.executable, str(FAKE), "--in", "--format", "smt2", "--precision", precision],
        input=text,
        capture_output=True,
        text=True,
        env=env,
        timeout=10,
    )


def test_unsat():
    assert _run("unsat").stdout.strip() == "unsat"


def test_delta_sat_names_the_precision():
    assert _run("delta-sat", precision="0.5").stdout.strip() == "delta-sat with delta = 0.5"


def test_plain_delta_sat():
    assert _run("delta-sat-plain").stdout.strip() == "delta-sat"


def test_garbage_and_crash_and_empty():
    assert _run("garbage").stdout.strip() == "sat"
    r = _run("crash")
    assert r.returncode == 3 and r.stderr
    r = _run("empty")
    assert r.returncode == 0 and r.stdout == ""


def test_rejects_strings_and_missing_logic():
    assert _run("unsat", text=TEXT.replace("Real", "String")).returncode == 2
    assert _run("unsat", text=TEXT.replace("(set-logic QF_NRA)\n", "")).returncode == 2
```

- [ ] **Step 4: Run to verify it fails**: `uv run --no-sync pytest -q tests/test_fake_delta.py` → `FileNotFoundError` or import error.
- [ ] **Step 5: Write `clients/fake_delta.py`** (stdlib only; the shebang `#!/usr/bin/env python3`; `argparse` for the three flags; `hang` uses `time.sleep(600)`).
- [ ] **Step 6: Run to verify it passes**: `uv run --no-sync pytest -q tests/test_fake_delta.py` → 5 passed.
- [ ] **Step 7: Commit**: `git add clients/fake_delta.py tests/test_fake_delta.py && git commit -m "clients: a fake delta solver so no test or fixture needs dReal (RK-12)"`.

---

### Task 2: The oracle: index segments in predicate paths

**Files:**
- Modify: `src/opendaisugi/predicate_z3.py` (`_resolve_path`, one helper `_index_of(segment) -> int | None`)
- Modify: `src/opendaisugi/predicate.py` (docstring of the module: the path grammar, one paragraph)
- Create: `clients/kernel_cases.py` (writer; this task writes only `paths.json`, later tasks add tables)
- Create: `clients/fixtures/kernel/paths.json` (generated)
- Test: `tests/test_predicate_paths.py`

**Interfaces:**
- Produces: `predicate_z3._index_of(segment: str) -> int | None`: `0`, or a decimal with no leading zero, sign or dot, else `None`.
- Produces: `_resolve_path` honouring RK-1. Signature unchanged.
- Produces: `clients/fixtures/kernel/paths.json`: `{"version": 1, "cases": [{"scope": {...}, "path": "a.0", "value": <json> | null, "present": true | false}, ...]}`. `present` false means MISSING. Scopes are JSON (lists stand for tuples).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_predicate_paths.py
import pytest
from opendaisugi.models import ActionPlan, CartesianMoveStep, Envelope, Permission
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import _MISSING, _resolve_path, evaluate_predicate


def _env():
    return Envelope(generated_by="t", task="t", permissions=Permission())


def _plan():
    return ActionPlan(
        source="t", task="t", steps=[CartesianMoveStep(id="m", target_position=(0.35, 0.2, 0.0))]
    )


@pytest.mark.parametrize(
    "scope,path,expected",
    [
        ({"p": [1.5, 2.5, 3.5]}, "p.0", 1.5),
        ({"p": [1.5, 2.5, 3.5]}, "p.2", 3.5),
        ({"p": (1.5, 2.5, 3.5)}, "p.1", 2.5),
        ({"b": [[0, 0, 0], [1, 2, 3]]}, "b.1.2", 3),
        ({"j": {"0": 9.0}}, "j.0", 9.0),  # dict key first
        ({"p": [1.5, 2.5, 3.5]}, "p.3", _MISSING),
        ({"p": [1.5, 2.5, 3.5]}, "p.-1", _MISSING),
        ({"p": [1.5, 2.5, 3.5]}, "p.01", _MISSING),
        ({"p": [1.5, 2.5, 3.5]}, "p.+1", _MISSING),
        ({"p": [1.5, 2.5, 3.5]}, "p.1.0", _MISSING),
        ({"s": "abc"}, "s.0", _MISSING),
        ({"n": 7}, "n.0", _MISSING),
        ({"p": None}, "p.0", _MISSING),
    ],
)
def test_index_segments(scope, path, expected):
    assert (
        _resolve_path(scope, path) is expected
        if expected is _MISSING
        else _resolve_path(scope, path) == expected
    )


def test_numeric_range_on_a_coordinate():
    inside = parse_expression(
        {
            "op": "forall_steps",
            "pred": {"op": "numeric_range", "path": "target_position.0", "min": 0.0, "max": 0.5},
        }
    )
    outside = parse_expression(
        {
            "op": "forall_steps",
            "pred": {"op": "numeric_range", "path": "target_position.1", "min": 0.5, "max": 1.0},
        }
    )
    assert evaluate_predicate(inside, _plan(), _env()) is True
    assert evaluate_predicate(outside, _plan(), _env()) is False


def test_exists_out_of_range_is_false():
    e = parse_expression(
        {"op": "forall_steps", "pred": {"op": "exists", "path": "target_position.3"}}
    )
    assert evaluate_predicate(e, _plan(), _env()) is False


def test_vacuity_accepts_an_index_path():
    from opendaisugi.vacuity import check_vacuity

    e = parse_expression(
        {
            "op": "forall_steps",
            "pred": {"op": "numeric_range", "path": "target_position.0", "min": 0.0, "max": 0.5},
        }
    )
    assert check_vacuity(e) == "non_trivial"
```

- [ ] **Step 2: Run to verify they fail**: `uv run --no-sync pytest -q tests/test_predicate_paths.py` → the index rows fail (today every index is MISSING).
- [ ] **Step 3: Implement** `_index_of` and the list/tuple branch in `_resolve_path` (dict branch first, then list/tuple with `_index_of`, then the attribute branch; an attribute lookup never applies to a list or tuple).
- [ ] **Step 4: Write the table writer**: `clients/kernel_cases.py --out clients/fixtures/kernel` with `write_paths()` producing the rows of Step 1 plus: a 4-tuple, a list of dicts (`obs.1.min.2`), an empty list, a bool scope, a nested dict with key `"1"` next to a list. Tuples are written as lists. `clients/fixture_paths.py` runs over the output (no local path). Run it; commit the JSON.
- [ ] **Step 5: Run the suite slice**: `uv run --no-sync pytest -q tests/test_predicate_paths.py tests/test_predicate.py tests/test_predicate_z3.py tests/test_vacuity.py tests/test_verify_predicate_invariants.py` → green.
- [ ] **Step 6: Commit**: `git add src/opendaisugi/predicate_z3.py src/opendaisugi/predicate.py clients/kernel_cases.py clients/fixtures/kernel/paths.json tests/test_predicate_paths.py && git commit -m "kernel: a predicate path can index a tuple or list element; dict keys first; everything else MISSING (RK-1)"`.

---

### Task 3: The Go port: index segments

**Files:**
- Modify: `clients/go/internal/verify/evaluate.go` (`resolvePath`: list and `pyTuple` branch), `clients/go/internal/gate/predicate.go` (W2: `internal/warden/predicate.go`) (`resolvePath`: the same), a shared `indexOf(seg string) (int, bool)` in `internal/verify` exported for the gate package through `exports.go`
- Create: `clients/go/internal/verify/paths_test.go` (table over `clients/fixtures/kernel/paths.json`, read the way `dialect_test.go` reads `globs.json`)

**Interfaces:** `IndexOf(seg string) (int, bool)`; both `resolvePath` functions call it after the map branch and before any attribute branch.

- [ ] **Step 1: Write the failing test**

```go
// clients/go/internal/verify/paths_test.go
func TestPathsFixture(t *testing.T) {
	var fx struct {
		Cases []struct {
			Scope   map[string]any `json:"scope"`
			Path    string         `json:"path"`
			Value   any            `json:"value"`
			Present bool           `json:"present"`
		} `json:"cases"`
	}
	readJSON(t, "../../../fixtures/kernel/paths.json", &fx)
	for _, c := range fx.Cases {
		got, ok := resolvePath(c.Scope, c.Path)
		if ok != c.Present {
			t.Errorf("%q: present=%v want %v", c.Path, ok, c.Present)
			continue
		}
		if ok && !reflect.DeepEqual(normalizeJSON(got), normalizeJSON(c.Value)) {
			t.Errorf("%q: got %v want %v", c.Path, got, c.Value)
		}
	}
}
```
- [ ] **Step 2: Run to verify it fails**: `systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./internal/verify/ -run TestPathsFixture'` → index rows fail.
- [ ] **Step 3: Implement** in both resolvers. The gate copy also returns `pyMissing` for an index into a non-list.
- [ ] **Step 4: Build and test**: `systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./internal/verify/ ./internal/gate/'` (W2: `./internal/warden/`) → green.
- [ ] **Step 5: Commit**: `git commit -m "go: index segments in predicate paths, both resolvers, from the shared table (RK-1)"`.

---

### Task 4: The Rust port: index segments

**Files:**
- Modify: `clients/rust/src/predicate.rs` (`resolve_path`), `clients/rust/src/gate/predicate.rs` (`resolve_path`), a shared `pub fn index_of(seg: &str) -> Option<usize>` in `predicate.rs`
- Create: `clients/rust/tests/paths_fixture.rs` (reads `clients/fixtures/kernel/paths.json`)

- [ ] **Step 1: Write the failing test** (same table, same assertions as Task 3; `serde_json::Value` scopes; call the verify-side `resolve_path` through a `pub(crate)` test shim if it is private).
- [ ] **Step 2: Run to verify it fails**: `. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo test --manifest-path clients/rust/Cargo.toml --test paths_fixture` → index rows fail.
- [ ] **Step 3: Implement** in both resolvers (`Value::Array` and the `Py::List`/tuple variants of `At`; objects first).
- [ ] **Step 4: Test**: the same command → green; then `cargo test --manifest-path clients/rust/Cargo.toml --lib predicate` under the cap.
- [ ] **Step 5: Commit**: `git commit -m "rust: index segments in predicate paths, both resolvers, from the shared table (RK-1)"`.

---

### Task 5: The oracle: the gripper effort dimension at plan time, in ROS 2 words

**Files:**
- Modify: `src/opendaisugi/models.py` (`GripperLimits`, `Permission.gripper`, `GripperStep.position`, `.velocity`, `.effort`)
- Modify: `src/opendaisugi/_invariant_types.py` (`gripper_effort_bounded` in `RECOGNIZED_OPAQUE_TYPES`)
- Modify: `src/opendaisugi/z3_checks.py` (`_check_gripper_effort_bounds`, registered in `_INVARIANT_HANDLERS`)
- Modify: `src/opendaisugi/verify.py` (the backing-bound function at `:1112-1119`: `gripper_effort_bounded` → `gripper.command_effort_max`)
- Modify: `src/opendaisugi/subsumption.py` (the robotics block after `torque_limit`: the three gripper bounds)
- Modify: `src/opendaisugi/envelope.py` (the prompt's field list names `gripper.command_effort_max` with its unit and the ROS 2 origin, one line) and the Tier-1 shape check if it lists permission keys (`grep -n "torque_limit" src/opendaisugi/tier1.py src/opendaisugi/schema.py 2>/dev/null`)
- Test: `tests/test_gripper_effort_bounds.py`, `tests/test_subsumption_robot.py` (three new tests)

**Interfaces:**
- Produces: `class GripperLimits(BaseModel): command_effort_max: float | None = Field(default=None, ge=0, description="Ceiling on a gripper command's effort (ParallelGripperCommand.command.effort): newtons for a linear gripper, newton-metres for a revolute one.")`, `command_velocity_max: float | None` (m/s or rad/s), `state_effort_max: float | None` (the measured state interface `effort`, same units).
- Produces: `Permission.gripper: GripperLimits | None = None`.
- Produces: `GripperStep.position: float | None = None` (m or rad), `GripperStep.velocity: float | None = Field(default=None, ge=0)`, `GripperStep.effort: float | None = Field(default=None, ge=0, description="Max effort of the joint allowed while moving (newtons or newton-metres), as ParallelGripperCommand.command.effort. None = undeclared; gripper_effort_bounded then rejects the step.")`.
- Produces: `z3_checks._check_gripper_effort_bounds(plan, envelope) -> list[Violation]` with `detail["invariant"] = "gripper_effort_bounded"`, `detail["reason"]` in `{"effort_undeclared", "effort_exceeds_limit", "velocity_undeclared", "velocity_exceeds_limit"}`, `detail["step"]`, and the values when present.

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_gripper_effort_bounds.py
import pytest
from pydantic import ValidationError
from opendaisugi.models import (
    ActionPlan,
    Envelope,
    GripperLimits,
    GripperStep,
    Invariant,
    Permission,
)
from opendaisugi.verify import verify


def _env(effort_max, vel_max=None, inv=True, stakes="physical"):
    g = (
        None
        if effort_max is None and vel_max is None
        else GripperLimits(command_effort_max=effort_max, command_velocity_max=vel_max)
    )
    return Envelope(
        generated_by="t",
        task="grip",
        stakes=stakes,
        permissions=Permission(gripper=g),
        invariants=[
            Invariant(type="gripper_effort_bounded", description="grip effort under the ceiling")
        ]
        if inv
        else [],
    )


def _plan(effort, velocity=None):
    return ActionPlan(
        source="t",
        task="grip",
        steps=[GripperStep(id="g", action="close", effort=effort, velocity=velocity)],
    )


def test_under_limit_passes():
    assert verify(_plan(4.0), _env(5.0)).ok


def test_over_limit_is_a_violation():
    r = verify(_plan(6.0), _env(5.0))
    assert not r.ok
    assert r.violations[0].detail["reason"] == "effort_exceeds_limit"


def test_undeclared_effort_is_a_violation():
    r = verify(_plan(None), _env(5.0))
    assert not r.ok
    assert r.violations[0].detail["reason"] == "effort_undeclared"


def test_velocity_bound_applies_only_when_the_envelope_sets_it():
    assert verify(_plan(4.0), _env(5.0)).ok  # no velocity bound: velocity may be None
    r = verify(_plan(4.0), _env(5.0, vel_max=0.1))
    assert r.violations[0].detail["reason"] == "velocity_undeclared"
    r = verify(_plan(4.0, velocity=0.2), _env(5.0, vel_max=0.1))
    assert r.violations[0].detail["reason"] == "velocity_exceeds_limit"
    assert verify(_plan(4.0, velocity=0.05), _env(5.0, vel_max=0.1)).ok


def test_invariant_without_backing_bound_is_vacuous_under_strict():
    r = verify(_plan(4.0), _env(None))
    assert not r.ok
    assert any("command_effort_max" in v.message for v in r.violations)


def test_no_invariant_means_no_effort_check():
    assert verify(_plan(60.0), _env(5.0, inv=False, stakes="low")).ok


def test_negative_values_rejected_by_the_model():
    with pytest.raises(ValidationError):
        GripperStep(id="g", action="close", effort=-1.0)
    with pytest.raises(ValidationError):
        GripperLimits(command_effort_max=-1.0)


def test_ros2_names_only():
    assert not hasattr(GripperStep(id="g", action="close"), "force_n") and not hasattr(
        Permission(), "force_limit_n"
    )
```
And in `tests/test_subsumption_robot.py`: `test_gripper_inner_le_outer_holds` (outer `command_effort_max` 10, inner 5 holds; outer 5, inner 10 fails with "command_effort_max" in a reason), `test_gripper_inner_unbounded_where_outer_bounded_denies` (outer 5, inner None; and outer `gripper` set, inner `gripper` None), `test_gripper_state_effort_max_subsumes` (the same two rules on `state_effort_max`).

- [ ] **Step 2: Run to verify they fail**: `uv run --no-sync pytest -q tests/test_gripper_effort_bounds.py tests/test_subsumption_robot.py` → import error on `GripperLimits`.
- [ ] **Step 3: Implement** the models, the handler, the backing-bound rule, the subsumption clause, the prompt line.
- [ ] **Step 4: Run**: `uv run --no-sync pytest -q tests/test_gripper_effort_bounds.py tests/test_subsumption_robot.py tests/test_verify_robotics.py tests/test_z3_robotics.py tests/test_robot_envelope.py tests/test_robot_steps.py tests/test_vacuity_provenance.py` → green.
- [ ] **Step 5: Commit**: `git commit -m "kernel: the gripper command in ROS 2 words (position, velocity, effort) and the envelope's gripper bounds; gripper_effort_bounded; subsumption inner<=outer (RK-3, RK-4)"`.

---

### Task 6: The oracle: the gripper command and guard on MuJoCo, and the robotics cases

**Files:**
- Modify: `src/opendaisugi/executor_mujoco.py` (`gripper: GripperLimits | None` ctor arg and attribute, `configure_from_envelope`, `_do_gripper` applies `position` to `ctrl` and `effort` to `actuator_forcerange`/`actuator_forcelimited` for the step and restores them after, `_step_with_guards` gains the state-effort check, `RC_GRIPPER_VIOLATION = 6`)
- Modify: `clients/robotics_cases.py` (new cases on `grip_ctrlrange.xml`: `gripper effort ceiling applied` (the receipt shows the forcerange used and the peak measured effort), `gripper state effort over bound`, `gripper position setpoint`, `gripper position outside ctrlrange`, `gripper bound with no a_grip actuator` (on `two_joint_arm.xml`); and one `run` case with an envelope that sets `gripper.state_effort_max`)
- Modify: `clients/fixtures/robotics/cases.jsonl` (regenerated)
- Test: `tests/test_mujoco_executor.py` (four tests; skip when `mujoco` is absent, as the file does today)

**Interfaces:** `MuJoCoExecutor(..., gripper: GripperLimits | None = None)`; `_do_gripper(step)` sets, for each `a_grip*` actuator id (collected once in `__init__` as `self._grip_actuators`): `data.ctrl[aid] = step.position` when set (else today's open/close target), and when `step.effort` is set `model.actuator_forcerange[aid] = (-effort, effort)`, `model.actuator_forcelimited[aid] = 1`, restoring the model's values in a `finally`; `_step_with_guards(n_steps, *, where, state_effort_max: float | None = None)` reads `|self.data.actuator_force[aid]|` for the gripper ids after each step. The receipt's stdout names the command fields used and the peak measured effort, as the torque guard's text does.

- [ ] **Step 1: Write the failing tests** (`test_gripper_effort_ceiling_limits_actuator_force` (with `effort=0.5` the peak `|actuator_force|` on the gripper never exceeds 0.5), `test_gripper_state_effort_over_bound_is_rc_6`, `test_gripper_position_outside_ctrlrange_is_rc_6`, `test_gripper_bound_needs_an_a_grip_actuator`: on `two_joint_arm.xml`, a gripper step with `effort=1.0` returns rc 6 and the text names "no a_grip actuator").
- [ ] **Step 2: Run to verify they fail**: `MUJOCO_GL=disable uv run --no-sync --extra robotics pytest -q tests/test_mujoco_executor.py -k gripper` (the robotics extra is installed with `uv sync --locked --extra dev --extra robotics`; this is a heavy job the first time, under the cap).
- [ ] **Step 3: Implement** the command mapping and the guard; a `velocity` on the step is written to the receipt and not enforced (position actuators have no velocity cap; say so in the docstring).
- [ ] **Step 4: Regenerate the robotics cases**: `MUJOCO_GL=disable systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/robotics_cases.py --out clients/fixtures/robotics`. Expected: 79 cases unchanged byte for byte except the new `position: null`, `velocity: null`, `effort: null` keys on gripper steps (check with `git diff --stat`; a changed float in an old case is a bug in Step 3), plus 6 new cases.
- [ ] **Step 5: Commit**: `git commit -m "mujoco: the ROS 2 gripper command mapped onto ctrl and actuator_forcerange, the state effort measured against the envelope, rc 6 (RK-5)"`.

---

### Task 7: The Go port: the gripper effort dimension

**Files:**
- Modify: `clients/go/internal/verify/models.go` (`GripperLimits{CommandEffortMax, CommandVelocityMax, StateEffortMax *float64}`, `Permission.Gripper *GripperLimits`, `Step.GripperPosition()`, `Step.GripperVelocity()`, `Step.GripperEffort()` each `(float64, bool)`), `robotics.go` (`checkGripperEffortBounds`, registered beside the four), `subsumption.go` (the three gripper clauses), the backing-bound function (`grep -n "velocity_limit" clients/go/internal/verify/*.go`), `internal/pmodel` if permission keys are listed there
- Modify: `clients/go/internal/mujoco/executor.go` (or the file that holds the torque guard: `grep -rn "RC_TORQUE\|TorqueLimit" clients/go/internal/mujoco`): the gripper command mapping (`ctrl`, `actuator_forcerange`, `actuator_forcelimited`, restored after the step) and the state-effort guard, `RCGripperViolation = 6`
- Create: `clients/go/internal/verify/robotics_gripper_test.go`

- [ ] **Step 1: Write the failing tests** (the eight cases of Task 5 as Go table tests over `Envelope`/`ActionPlan` JSON; plus the three subsumption tests).
- [ ] **Step 2: Run to verify they fail** under the cap (`go test -p 1 ./internal/verify/ -run Gripper`).
- [ ] **Step 3: Implement**, the executor mapping and guard behind the `mujoco` tag line for line with Python (the same model fields written and restored in the same order, so the floats agree).
- [ ] **Step 4: Build, test, compare, one job at a time**

```bash
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./...'
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go build -tags mujoco -o /mnt/tera/.tt/w3-robot-kernel/robot-probe ./cmd/robot-probe'
LD_LIBRARY_PATH=$(readlink -f clients/go/.native)/lib systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/robotics_compare.py --probe /mnt/tera/.tt/w3-robot-kernel/robot-probe
```
Expected: green; the robotics compare agrees on all 85 cases, every float.
- [ ] **Step 5: Commit**: `git commit -m "go: the gripper effort dimension in ROS 2 words at plan time, in subsumption and on MuJoCo (RK-3 to RK-5)"`.

---

### Task 8: The Rust port: the gripper effort dimension

**Files:**
- Modify: `clients/rust/src/models.rs` (`GripperLimits`, `Permission.gripper: Option<GripperLimits>`, gripper step `position`, `velocity`, `effort: Option<f64>`), `z3_checks.rs` (`check_gripper_effort_bounds`), `subsumption.rs` (three clauses), the backing-bound function, `src/mujoco/` executor mapping and guard (`grep -rn "RC_TORQUE\|torque_limit" clients/rust/src/mujoco`; `RC_GRIPPER_VIOLATION = 6`), `src/bin/robot_probe.rs` if it maps rc codes
- Create: `clients/rust/tests/gripper_effort_bounds.rs`

- [ ] **Step 1: Write the failing tests** (same eight, plus the three subsumption tests).
- [ ] **Step 2: Run to verify they fail** under the cap.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Build, test, compare**

```bash
. clients/rust/tools/z3-build-env.sh
systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo test --manifest-path clients/rust/Cargo.toml
systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml --features mujoco --bin robot_probe
LD_LIBRARY_PATH=$(readlink -f clients/go/.native)/lib systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/robotics_compare.py --probe clients/rust/target/release/robot_probe --probe /mnt/tera/.tt/w3-robot-kernel/robot-probe
```
Expected: green; both probes agree with the oracle and each other on all 85 cases.
- [ ] **Step 5: Commit**: `git commit -m "rust: the gripper effort dimension in ROS 2 words at plan time, in subsumption and on MuJoCo (RK-3 to RK-5)"`.

---

### Task 9: The oracle: the `distance_range` op

**Files:**
- Modify: `src/opendaisugi/predicate.py` (`DistanceRange`, in the union and `__all__`)
- Modify: `src/opendaisugi/predicate_z3.py` (`_eval_scalar` concrete case with `Fraction`; `_compile_scalar` symbolic case; `CompiledPredicate.nonlinear: bool`; `_Scope.resolve_numeric` is reused for `P.0`, `P.1`, `P.2`)
- Modify: `src/opendaisugi/vacuity.py` (the compile returns the nonlinear flag; this task only threads it, Task 11 uses it)
- Modify: `src/opendaisugi/aliases.py` if it lists op names (`grep -n "numeric_range" src/opendaisugi/aliases.py`)
- Test: `tests/test_predicate_distance.py`

**Interfaces:**
- Produces: `class DistanceRange(_ExprBase): op: Literal["distance_range"]; path: str; point: tuple[float, float, float]; min: float = 0.0; max: float` with a model validator: `0 ≤ min ≤ max`, every number finite.
- Produces: `CompiledPredicate.nonlinear: bool` (default `False`; `True` when a symbolic `distance_range` was compiled). `compile_to_z3` and `vacuity`'s `_compile_scalar` call set it.
- Produces: `predicate_z3.nonlinear_reads(expr, step_dict) -> list[tuple[str, float]]`: the `(path, value)` pairs a nonlinear op reads on a concrete step (used by Task 11's box rule).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_predicate_distance.py
import math, pytest, z3
from pydantic import ValidationError
from opendaisugi.models import ActionPlan, CartesianMoveStep, Envelope, Permission, ShellStep
from opendaisugi.predicate import parse_expression
from opendaisugi.predicate_z3 import compile_to_z3, evaluate_predicate


def _env():
    return Envelope(generated_by="t", task="t", permissions=Permission())


def _plan(pos=(3.0, 4.0, 0.0)):
    return ActionPlan(source="t", task="t", steps=[CartesianMoveStep(id="m", target_position=pos)])


def _d(mx, mn=0.0, path="target_position"):
    return parse_expression(
        {
            "op": "forall_steps",
            "pred": {
                "op": "distance_range",
                "path": path,
                "point": [0, 0, 0],
                "min": mn,
                "max": mx,
            },
        }
    )


def test_inside_and_outside():
    assert evaluate_predicate(_d(5.0), _plan(), _env()) is True  # distance exactly 5
    assert evaluate_predicate(_d(4.999), _plan(), _env()) is False
    assert evaluate_predicate(_d(10.0, mn=5.001), _plan(), _env()) is False


def test_exact_arithmetic_at_the_boundary():
    assert (
        evaluate_predicate(_d(0.3, path="target_position"), _plan((0.1, 0.2, 0.2)), _env()) is True
    )  # 0.09 == 0.01+0.04+0.04 exactly in rationals


def test_not_a_3_vector_is_false():
    plan = ActionPlan(source="t", task="t", steps=[ShellStep(id="s", command="ls")])
    assert evaluate_predicate(_d(5.0, path="command"), plan, _env()) is False
    assert evaluate_predicate(_d(5.0, path="target_orientation"), _plan(), _env()) is False


def test_validator():
    with pytest.raises(ValidationError):
        parse_expression(
            {"op": "distance_range", "path": "p", "point": [0, 0, 0], "min": 2.0, "max": 1.0}
        )
    with pytest.raises(ValidationError):
        parse_expression(
            {"op": "distance_range", "path": "p", "point": [0, 0, math.inf], "max": 1.0}
        )


def test_symbolic_compile_is_nonlinear_and_polynomial():
    c = compile_to_z3(_d(5.0), _plan(), _env())
    assert c.nonlinear is True
    s = z3.Solver()
    s.add(*c.assumptions)
    s.add(z3.Not(c.term))
    assert s.check() == z3.unsat  # the concrete step at distance 5 satisfies max 5
```

- [ ] **Step 2: Run to verify they fail**: `uv run --no-sync pytest -q tests/test_predicate_distance.py` → discriminator error on `distance_range`.
- [ ] **Step 3: Implement**. Concretely: `Fraction(str(v))` is wrong for floats; use `Fraction(v)` (exact binary value) for every number, compare `m² ≤ Σ ≤ M²`. Symbolically: `d2 = Sum((var_i - RealVal(p_i))**2)`, term `And(d2 >= m*m, d2 <= M*M)`; on a concrete scope with a 3-vector, bind the three vars as `resolve_numeric` does and return the same term (Z3 then decides the ground query); when the path is not a 3-vector, `BoolVal(False)`.
- [ ] **Step 4: Run**: `uv run --no-sync pytest -q tests/test_predicate_distance.py tests/test_predicate.py tests/test_predicate_z3.py tests/test_predicate_z3_real.py tests/test_vacuity.py` → green.
- [ ] **Step 5: Commit**: `git commit -m "kernel: distance_range, the one nonlinear op: Euclidean radius of a 3-vector, exact rationals concretely, squared form for the solver (RK-6)"`.

---

### Task 10: The oracle: `verdict.py` and `solver.py`

**Files:**
- Create: `src/opendaisugi/verdict.py`
- Create: `src/opendaisugi/solver.py`
- Modify: `src/opendaisugi/config.py` (the four keys with defaults and types; `delta_box`; read the way `dialect_enforce` is)
- Test: `tests/test_solver_delta.py`

**Interfaces:**
- Produces (`verdict.py`): `SolverVerdict = Literal["proved", "refuted", "unknown", "delta_sat"]`; `@dataclass(frozen=True) class SolverAnswer: verdict: SolverVerdict; delta: float | None = None; solver: Literal["z3", "delta"] = "z3"; why: str = ""`; `def admits(a: SolverAnswer) -> bool: return a.verdict == "proved"`; `DELTA_SAT_REASON = "delta_sat"`; `OUTSIDE_BOX_REASON = "outside_delta_box"`.
- Produces (`solver.py`):
  - `@dataclass(frozen=True) class DeltaConfig: solver: str | None; precision: float = 0.001; nonlinear: Literal["z3", "delta"] = "z3"; timeout_ms: int | None = None; box: float = 10000.0` and `DeltaConfig.from_config(cfg) -> DeltaConfig`.
  - `def check_negation(term, assumptions, *, nonlinear: bool, timeout_ms: int, delta: DeltaConfig, which: str) -> SolverAnswer`: asks whether `Not(term)` is satisfiable under `assumptions`; `unsat` → `proved`; `sat` → `refuted` (the caller reads the model as today); `unknown` → the delta route (RK-8) → `proved` on exact `unsat`, `delta_sat` on `delta-sat`, else `unknown` with `why`. Linear goals never take the delta route.
  - `def check_sat(term, assumptions, *, nonlinear, timeout_ms, delta, which) -> SolverAnswer`: the same for the positive query (vacuity's contradiction check): `unsat` → `refuted` (the goal is unsatisfiable), `sat` → `proved` (satisfiable), `unknown` → delta route: `unsat` → `refuted`, `delta-sat` → `unknown` (RK-10: a delta-sat on the positive side proves nothing), else `unknown`.
  - `def to_delta_text(solver: z3.Solver, box: float, precision: float) -> str | None`: `solver.to_smt2()` with the trailing `(check-sat)` removed, `(set-logic QF_NRA)` and `(set-option :precision …)` prepended, one box assertion per Real declaration, `(check-sat)` appended; `None` when any declaration is not Real or Bool, or the text contains `str.`, `re.`, `Int`, `String`, `seq.`.
  - `def run_delta(text: str, cfg: DeltaConfig, *, timeout_ms: int) -> SolverAnswer` (the subprocess and the parser of RK-8).
  - `def classify_delta_output(stdout: str, returncode: int, timed_out: bool, precision: float) -> SolverAnswer` (pure; the golden table of Task 12 pins it).

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_solver_delta.py
import os, sys
from pathlib import Path
import pytest, z3
from opendaisugi.solver import (
    DeltaConfig,
    check_negation,
    check_sat,
    classify_delta_output,
    to_delta_text,
)

FAKE = str(Path(__file__).resolve().parents[1] / "clients" / "fake_delta.py")


@pytest.fixture
def fake(monkeypatch):
    def _set(mode):
        monkeypatch.setenv("DAISUGI_FAKE_DELTA", mode)
        return DeltaConfig(
            solver=f"{sys.executable} {FAKE}", precision=0.25, nonlinear="delta", timeout_ms=2000
        )

    return _set


def _nonlinear():
    x, y = z3.Reals("x y")
    return z3.And(x * x + y * y <= 25.0), [x == 3.0, y == 4.0]


@pytest.mark.parametrize(
    "mode,verdict",
    [
        ("unsat", "proved"),
        ("delta-sat", "delta_sat"),
        ("delta-sat-plain", "delta_sat"),
        ("garbage", "unknown"),
        ("crash", "unknown"),
        ("empty", "unknown"),
    ],
)
def test_negation_route(fake, mode, verdict):
    term, assumptions = _nonlinear()
    a = check_negation(
        term, assumptions, nonlinear=True, timeout_ms=500, delta=fake(mode), which="t"
    )
    assert a.verdict == verdict
    if verdict == "delta_sat":
        assert a.delta == 0.25 and a.solver == "delta"


@pytest.mark.parametrize(
    "mode,verdict", [("unsat", "refuted"), ("delta-sat", "unknown"), ("garbage", "unknown")]
)
def test_positive_route_never_proves_on_delta_sat(fake, mode, verdict):
    term, assumptions = _nonlinear()
    assert (
        check_sat(
            term, assumptions, nonlinear=True, timeout_ms=500, delta=fake(mode), which="t"
        ).verdict
        == verdict
    )


def test_linear_goal_never_runs_the_delta_solver(fake, monkeypatch):
    log = "/mnt/tera/.tt/w3-robot-kernel/linear.log"
    monkeypatch.setenv("DAISUGI_FAKE_DELTA_LOG", log)
    x = z3.Real("x")
    a = check_negation(
        x <= 5.0, [x == 3.0], nonlinear=False, timeout_ms=500, delta=fake("delta-sat"), which="t"
    )
    assert a.verdict == "proved" and a.solver == "z3" and not os.path.exists(log)


def test_no_solver_means_unknown():
    term, assumptions = _nonlinear()
    a = check_negation(
        term,
        assumptions,
        nonlinear=True,
        timeout_ms=500,
        delta=DeltaConfig(solver=None, nonlinear="delta"),
        which="t",
    )
    assert a.verdict == "unknown" and "gave no answer" in a.why


def test_hang_is_unknown(fake):
    term, assumptions = _nonlinear()
    cfg = fake("hang")
    cfg = DeltaConfig(**{**cfg.__dict__, "timeout_ms": 200})
    assert (
        check_negation(
            term, assumptions, nonlinear=True, timeout_ms=500, delta=cfg, which="t"
        ).verdict
        == "unknown"
    )


def test_text_has_logic_precision_and_box():
    x = z3.Real("x")
    s = z3.Solver()
    s.add(x * x > 2.0)
    t = to_delta_text(s, box=10000.0, precision=0.001)
    assert (
        t.startswith("(set-logic QF_NRA)\n(set-option :precision 0.001)")
        and "(<= x 10000.0)" in t
        and t.rstrip().endswith("(check-sat)")
    )


def test_text_refuses_strings():
    s = z3.String("s")
    so = z3.Solver()
    so.add(z3.Length(s) > 2)
    assert to_delta_text(so, box=1.0, precision=0.1) is None


def test_classify():
    assert classify_delta_output("unsat\n", 0, False, 0.1).verdict == "proved"
    a = classify_delta_output("delta-sat with delta = 0.5\n", 0, False, 0.1)
    assert (a.verdict, a.delta) == ("delta_sat", 0.5)
    assert classify_delta_output("delta-sat\n", 0, False, 0.1).delta == 0.1
    assert classify_delta_output("sat\n", 0, False, 0.1).verdict == "unknown"
    assert classify_delta_output("unsat\n", 1, False, 0.1).verdict == "unknown"
    assert classify_delta_output("", 0, True, 0.1).verdict == "unknown"
```
(`classify_delta_output` names `proved`/`refuted` for the negation side; the positive side maps them in `check_sat`. Keep one parser.)

- [ ] **Step 2: Run to verify they fail**: `uv run --no-sync pytest -q tests/test_solver_delta.py` → import error.
- [ ] **Step 3: Implement** `verdict.py`, `solver.py` (the subprocess with `subprocess.run(shlex.split(cfg.solver) + [...], input=text, capture_output=True, text=True, timeout=…, env=minimal, cwd=scratch)`; `TimeoutExpired` → `timed_out=True`; the Z3 calls under `Z3_SOLVE_LOCK`, the subprocess outside it), and the config keys.
- [ ] **Step 4: Run**: green; `uv run --no-sync ruff check src/opendaisugi/solver.py src/opendaisugi/verdict.py`.
- [ ] **Step 5: Commit**: `git commit -m "solver: four verdict classes and the optional delta route over SMT-LIB2 text; only an exact unsat proves; Z3 stays the only required solver (RK-7 to RK-10)"`.

---

### Task 11: The oracle: the class flows through vacuity, subsumption, verify, the warden and the wire

**Files:**
- Modify: `src/opendaisugi/vacuity.py` (both queries through `solver.check_sat`/`check_negation` with the compiled `nonlinear` flag; `Verdict` keeps its three words)
- Modify: `src/opendaisugi/subsumption.py` (`SubsumptionResult.delta_sat: float | None = None`; the final predicate query through `check_negation`; the `delta_sat` reason)
- Modify: `src/opendaisugi/predicate_z3.py` (`verify_predicate_z3` through `check_negation`; it returns `(False, None)` with a raised-free path on `delta_sat`: add a third return element or a `SolverAnswer` attribute on `Counterexample`; pick the smaller change and say which in the commit)
- Modify: `src/opendaisugi/verify.py` (`_result` derives `verdict`; the predicate stage adds the `outside_delta_box` check for steps a nonlinear invariant reads (`nonlinear_reads`); Stage 1b turns a `delta_sat` subsumption into a Violation with reason `delta_sat` at every stakes; `_Z3_TIMEOUT_WARNING_RE` also matches `delta solver .* gave no answer`)
- Modify: `src/opendaisugi/models.py` (`VerificationResult.verdict: SolverVerdict = "proved"`)
- Modify: `src/opendaisugi/gate.py` (W2: `warden.py`) (deny on `result.verdict == "delta_sat"` before the timeout-warning branch; the audit would-deny reason)
- Modify: `src/opendaisugi/conformance.py` (the verify verdict line gains `"verdict"`; the comparer checks it when both sides emit it)
- Test: `tests/test_delta_sat_verdict.py`, `tests/test_gate_delta_sat_deny.py`

- [ ] **Step 1: Write the failing tests**

```python
# tests/test_delta_sat_verdict.py (shape; every test forces the route with the fake, never with Z3 timing)
def test_delta_sat_is_never_ok(
    fake_delta_config,
):  # fixture: DeltaConfig over the fake in delta-sat mode, nonlinear="delta", installed into the config the verifier reads
    r = verify(
        plan_with_distance_goal, envelope_with_distance_invariant, strict=False
    )  # low stakes on purpose
    assert r.ok is False and r.verdict == "delta_sat"
    assert (
        r.violations[0].detail["reason"] == "delta_sat" and r.violations[0].detail["delta"] == 0.25
    )


def test_exact_unsat_through_delta_proves(fake_delta_config_unsat):
    r = verify(...)
    assert r.ok and r.verdict == "proved"


def test_unknown_without_a_solver_follows_the_timeout_policy():
    low = verify(..., strict=False)
    assert low.ok and low.verdict == "unknown" and any("gave no answer" in w for w in low.warnings)
    phys = verify(physical_plan, physical_envelope)
    assert not phys.ok and phys.verdict == "unknown"


def test_refuted_beats_delta_sat_in_the_derived_verdict(): ...
def test_outside_box_is_a_violation():  # target_position (20000, 0, 0) under a distance invariant, any solver mode
    r = verify(...)
    assert not r.ok and r.violations[0].detail["reason"] == "outside_delta_box"


def test_linear_goal_never_emits_outside_box(): ...
def test_subsumption_delta_sat_does_not_hold(fake_delta_config):
    res = envelope_subsumes(outer_with_distance_invariant, inner_without)
    assert not res.holds and res.delta_sat == 0.25


def test_vacuity_cells(fake_delta_config, fake_delta_config_unsat):  # the four cells of RK-10
    ...
def test_conformance_line_carries_verdict(): ...
```

```python
# tests/test_gate_delta_sat_deny.py (as tests/test_gate_z3_timeout_deny.py is built)
def test_enforce_denies_delta_sat_at_low_stakes(...): decision.allow is False; "within delta" in decision.reason
def test_audit_logs_a_would_deny_with_reason_delta_sat(...): the audit line has reason "delta_sat" and the delta
```

- [ ] **Step 2: Run to verify they fail**.
- [ ] **Step 3: Implement** in the order: `models.py`, `vacuity.py`, `subsumption.py`, `predicate_z3.py`, `verify.py`, `gate.py`, `conformance.py`. The config the verifier reads: `verify()` and `envelope_subsumes()` take `delta: DeltaConfig | None = None`; `None` means `DeltaConfig.from_config(load_config())` once per call site, cached as `dialect_pin` is found today (`grep -n "dialect_enforce" src/opendaisugi/gate.py src/opendaisugi/verify.py`).
- [ ] **Step 4: Run the whole Python suite** (heavy, under the cap): `systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait uv run --no-sync pytest -q`. Expected: green; the count rises by about 45. `tests/test_z3_dispatch_single_source.py` may need the new call sites added to its allow-list if it pins where `z3.Solver()` is built; add them, do not weaken it.
- [ ] **Step 5: Commit**: `git commit -m "verify: delta_sat is a violation at every stakes, denied by the warden, named on the wire; the box rule makes a delta proof a proof about the plan (RK-7, RK-9 to RK-11)"`.

---

### Task 12: Golden cases from the oracle

**Files:**
- Modify: `clients/kernel_cases.py` (adds `delta_outputs.json`: the `classify_delta_output` table of 14 rows incl. CRLF, leading blank line, trailing text; `delta_eligible.json`: 10 expressions with `nonlinear` and `eligible` flags and the informative text; `distance.json`: 20 concrete `(scope, op, result)` rows incl. the exact-boundary row and a non-3-vector row)
- Modify: `clients/gate_cases.py` (family `robot-kernel`: 12 warden cases; each lays `config.yaml` with `delta_solver: .opendaisugi/bin/fake-delta` (relative to the case root, with the `{PYTHON}` shebang substitution of `garden_cases`), `nonlinear_solver: delta`, `delta_precision: 0.25`, and env `DAISUGI_FAKE_DELTA`; cases: delta-sat deny in enforce, delta-sat would-deny in audit, unsat allow, garbage deny, no solver deny (physical), index-path allow and deny, gripper effort over the ceiling deny, gripper effort undeclared deny, outside-box deny (one with the box from `workspace_bounds`, one from the default), linear goal with the fake in delta-sat mode allows (the fake is never run))
- Modify: `clients/pathway_cases.py` (`build_verify_cases`: the new messages: `effort_undeclared`, `effort_exceeds_limit`, `velocity_undeclared`, `velocity_exceeds_limit`, the backing-bound `command_effort_max`, `outside_delta_box` (envelope box and default box), `distance_range` violation at plan time, `delta_sat` at Stage 1b)
- Modify: `clients/robotics_cases.py` (done in Task 6)
- Modify: `clients/fixtures/kernel/*`, `clients/fixtures/gate/*`, `clients/fixtures/pathways/verify_messages.jsonl` and manifests (regenerated)
- Create: `clients/kernel_compare.py` (runs a port's `kernel-probe` over `paths.json`, `delta_outputs.json`, `delta_eligible.json`, `distance.json`; structural compare; `--oracle` reruns the oracle)

- [ ] **Step 1: Write the writers and the compare**; `clients/fixture_paths.py` over every output.
- [ ] **Step 2: Regenerate, one job at a time** (`gate_cases.py`, `pathway_cases.py --only verify`, `kernel_cases.py`), each under the cap.
- [ ] **Step 3: Check the flips**: `git diff --stat clients/fixtures/`; list every gate case whose decision changed. Expected: none (every change here is additive or deny-direction on new fields). Any flip is a bug in Tasks 2, 5, 9 or 11.
- [ ] **Step 4: Commit**: `git commit -m "fixtures: kernel tables (paths, delta outputs, eligibility, distance), 12 warden cases and the new verify messages for the robot kernel gaps"`.

---

### Task 13: The Go port: `distance_range`, the solver, the verdict

**Files:**
- Modify: `clients/go/internal/verify/predicate.go` (`DistanceRange` type, parse, `Op()`), `vacuity.go` and `internal/gate/vacuity.go` (W2: `internal/warden/vacuity.go`) (the squared-form emission; the `nonlinear` flag on the compiled text; three Real declarations `P__0`, `P__1`, `P__2`), `evaluate.go` (the concrete case; `f64` squared comparison per RK-6), `subsumption.go` (the final query through `solver.go`; `DeltaSat *float64` on the result), `verify.go` (`Verdict string` on `VerifyResultGo`, derived as RK-7; the `outside_delta_box` rule), `step.go`
- Create: `clients/go/internal/verify/solver.go` (`DeltaConfig`, `CheckNegation`, `CheckSat`, `ToDeltaText(decls, asserts, box, precision) (string, bool)`, `RunDelta`, `ClassifyDeltaOutput`); the text builder works on the SMT-LIB2 the port already emits, so it never needs `to_smt2`
- Modify: `clients/go/internal/config/config.go` and `internal/gate/config.go` (W2: `internal/warden/config.go`) (the five keys), `internal/gate/check.go` (deny on `delta_sat`; the audit reason), `internal/cli/conform*.go` or wherever the verify wire line is written (`"verdict"`)
- Create: `clients/go/cmd/kernel-probe/main.go` (reads the four kernel tables, writes one result line per row), `clients/go/internal/verify/solver_test.go` (the polarity table over the fake, `DAISUGI_FAKE_DELTA` set per subtest; `exec.Command(pythonFromEnvOrSkip, fake)`), `distance_test.go`

- [ ] **Step 1: Write the failing tests** (the solver table as in Task 10; the distance table from `distance.json`; the eligibility table).
- [ ] **Step 2: Run to verify they fail** under the cap.
- [ ] **Step 3: Implement** in the order: `predicate.go`, `evaluate.go`, `vacuity.go` (both), `solver.go`, `subsumption.go`, `verify.go`, config, `check.go`, the wire, the probe.
- [ ] **Step 4: Build and run the compares, one at a time under the cap**

```bash
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./...'
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go build -o /mnt/tera/.tt/w3-robot-kernel/kernel-probe ./cmd/kernel-probe'
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/kernel_compare.py --probe /mnt/tera/.tt/w3-robot-kernel/kernel-probe
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/gate_compare.py --binary clients/go/daisugi-gate      # W2: daisugi-warden
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/pathway_compare.py --binary clients/go/daisugi --only verify
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/gate_compare.py --binary clients/go/daisugi-gate --fuzz 2000 --seed 7 --kind envelope
```
Expected: 0 disagreements, 0 fail-open everywhere; the fuzz finds no fail-open. A disagreement is a bug in this task unless the oracle is wrong; then fix the oracle (Tasks 9 to 11), regenerate (Task 12), and return.
- [ ] **Step 5: Commit**: `git commit -m "go: distance_range, the delta route over the port's own SMT-LIB2 text, the four verdict classes, delta_sat denied (RK-6 to RK-11)"`.

---

### Task 14: The Rust port: `distance_range`, the solver, the verdict

**Files:**
- Modify: `clients/rust/src/predicate.rs` (`Expr::DistanceRange`, parse, concrete eval), `z3_bridge.rs` (emission, the `nonlinear` flag), `gate/predicate.rs` (the gate's compile), `subsumption.rs` (`delta_sat: Option<f64>`), `verify.rs` (`verdict`, the box rule), `models.rs` (the result field), `gate/config.rs` and `cli/config.rs` (keys), `gate/check.rs` (deny), the conformance wire writer (`"verdict"`)
- Create: `clients/rust/src/solver.rs`, `clients/rust/src/bin/kernel_probe.rs`, `clients/rust/tests/solver_delta.rs`, `clients/rust/tests/distance.rs`

- [ ] **Step 1: Write the failing tests** (same tables).
- [ ] **Step 2: Run to verify they fail** under the cap.
- [ ] **Step 3: Implement**.
- [ ] **Step 4: Build and run the compares**

```bash
. clients/rust/tools/z3-build-env.sh
systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo test --manifest-path clients/rust/Cargo.toml
systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/kernel_compare.py --probe clients/rust/target/release/kernel_probe
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/gate_compare.py --binary clients/rust/target/release/daisugi-gate   # W2: daisugi-warden
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/pathway_compare.py --binary clients/rust/target/release/daisugi --only verify
```
Expected: 0 disagreements, 0 fail-open.
- [ ] **Step 5: Commit**: `git commit -m "rust: distance_range, the delta route, the four verdict classes, delta_sat denied (RK-6 to RK-11)"`.

---

### Task 15: The yellow paper, ADR-0029, the docs and the rulings

**Files:**
- Modify: `docs/spec/yellow-paper.md`:
  - §1 Notation: add `𝒱 = {proved, refuted, unknown, δ-sat}` and the sentence "only `proved` is `⊤`; the other three are `⊥`".
  - §2.1: add `gripper : ⟨command_effort, command_velocity, state_effort : ℝ⁺ ∪ {⊥}⟩ ∪ {⊥}` to the tuple, after `velocity/torque`, with one sentence: the names are ros2_control's interface names and the control_msgs gripper command's; daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware.
  - New §2.3.2 "Paths (Built)": the grammar of RK-1, the resolution function `res(obj, seg)`, MISSING, the dict-first rule, the three worked examples.
  - New §2.5 "Verdict classes and the δ route (Built)": the four classes; the δ-complete definition (Gao, Avigad, Clarke 2012: `unsat` exact, `δ-sat` means the δ-weakening is satisfiable); the routing of RK-8; the box of RK-9 with the statement "a δ proof is `⊤` only together with the concrete range rule"; the polarity table of RK-10; the soundness argument in four lines: (i) every `⊤` the kernel returns comes from an exact `unsat` of Z3 or of the δ procedure; (ii) a δ-complete `unsat` is exact by definition; (iii) the box restricts the counterexample space, and the range rule rejects any step outside it, so no admitted step lies in the excluded space; (iv) `δ-sat`, `unknown`, a timeout, a crash and a malformed answer are all `⊥`, so no incomplete answer is read as a proof. State what is not claimed: the external solver is trusted code, as Z3 is; the fake proves the plumbing, not the solver.
  - §2.4 property 4: replace "Nonlinear robot dynamics would bring the same outcome" with the current state: polynomial goals (`distance_range`) go to Z3's NLSAT, complete modulo resources; on `unknown` the δ route of §2.5 applies; no transcendental op exists.
  - §4.4: name the nonlinear op and §2.5.
  - §5.3: add the three gripper bounds (inner ≤ outer; undeclared where outer constrains ⟹ deny).
  - §2.5 (the box): the per-variable box table of RK-9, with the sentence "the envelope's own bound first; the default box only where the envelope leaves a variable unbounded".
  - §6: add the line `δ returns δ-sat ⟹ δ ≔ ⊥` under the law, and `delta_sat` to the list of outcomes that deny at every stakes.
  - §7: under "Guaranteed": "δ-sat is never ok; a proof through the δ route holds inside each variable's box and the range rule"; under "Not guaranteed": the external solver's correctness; `distance_range` is a per-step radius, not a pairwise separation; the gripper `velocity` command is checked at plan time and recorded, not enforced by the simulator; daisugi does not replace ROS 2 or MuJoCo, it checks the commands they carry.
  - References: Gao, Avigad, Clarke 2012 (arXiv 1204.3513); Jovanović and de Moura, IJCAR 2012; the dReal4 repository; ros2_control `hardware_interface_type_values.hpp`; control_msgs `GripperCommand.msg`, `GripperCommand.action`, `ParallelGripperCommand.action`; the ros2_controllers gripper and parallel gripper controller docs (Jazzy). The URLs are the ones in this plan's research paragraphs.
- Create: `docs/adr/0029-delta-sat-verdict-class.md` (from the template; Status Accepted; Date 2026-10-30. Context: nonlinear goals, Z3's `unknown`, the δ-complete class of answers, the owner's ruling of 2026-10-30 overturning DI-7. Decision: a fourth verdict class `delta_sat`, never `ok`, a violation at every stakes, from an optional external solver over SMT-LIB2 text; Z3 stays the only required solver. Consequences: robots can state radius and force bounds; an operator may install dReal and name it; without it nonlinear goals Z3 cannot decide deny; a 2021 upstream with no Fedora package; the external binary is trusted code. Alternatives: treat δ-sat as `unknown` (rejected: it hides a mathematical answer the operator can act on, and the audit log would not name it); link dReal (rejected: a native dependency with LGPL and EPL parts, no release since 2021); Z3 only (rejected: it is the default today and remains; the class costs one word and one module per port).)
- Modify: `docs/adr/README.md` (the index row)
- Modify: `docs/robotics.md` (the gripper command fields and the `Permission.gripper` bounds with their ROS 2 origin and units, the MuJoCo mapping table `position → ctrl`, `effort → actuator_forcerange`, state `effort → actuator_force`, the `RC_GRIPPER_VIOLATION` row, the sentence "daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware", the `distance_range` example, the config keys), `docs/limitations.md` (δ route limits; no transcendental op; per-step radius only; gripper `velocity` not enforced by the simulator), `docs/feature-status.md` (three rows: index paths, the gripper effort dimension, verdict classes and the δ route, each `Built | Built | Built` with the case counts), `VISION.md` scorecard "Built and tested" (one line naming the three gaps closed and the case counts; "Open" keeps "robotics on hardware"), `docs/reference/dependencies.md` (one row: dReal4, optional external tool, Apache-2.0, why, status `optional`), the config reference (`git grep -ln dialect_enforce docs | head`: the five keys with defaults), the predicate op reference (`git grep -ln length_range docs | head`: `distance_range`, index paths), `AGENTS.md` "What is proven" table (the kernel suite row with its counts), `CHANGELOG.md` `## Unreleased` (one entry per gap, current-state wording)
- Modify: `docs/plans/2026-09-24-omarchy/DECISIONS-PENDING.md`: no edit (historical); the overturn of DI-7 is recorded in `clients/ADJUDICATIONS.md`
- Modify: `clients/ADJUDICATIONS.md`: a dated section `## 2026-11-xx: the robot kernel gaps (RK-1 to RK-13)` with the rulings verbatim, the DI-7 overturn line, then `RK-R-n` entries for every port disagreement Tasks 3, 4, 7, 8, 13 and 14 met

- [ ] **Step 1: Write the docs.** Every number names its source (the fixture counts from Task 12's commit, the robotics count from Task 6).
- [ ] **Step 2: Check the names and the prose**: `scripts/names-check.sh` (from W2) exits 0; `git grep -n -i -E "seam|the layer| — " -- docs/spec/yellow-paper.md docs/adr/0029-delta-sat-verdict-class.md docs/robotics.md docs/limitations.md docs/feature-status.md VISION.md AGENTS.md CHANGELOG.md docs/reference/dependencies.md` prints nothing new.
- [ ] **Step 3: Commit**: `git add <the files above> && git commit -m "docs: paths, the gripper effort dimension in ROS 2 words and the delta-sat verdict class in the yellow paper; ADR-0029; rulings RK-1 to RK-13"`.

---

### Task 16: Whole-suite green, the compares once more, the ledger

**Files:**
- Modify: `.superpowers/sdd/backlog/progress.md` (the W3 robot-kernel lines: what merged, the case counts, the open questions the owner has not answered, the dReal hand check still owed)

- [ ] **Step 1: Rebase on master** (W1, W2 and any merged W3 branch; `verify.py`, `models.py`, `gate.py` and `config.py` conflict with w3-safety-gaps and w3-local-models-and-views): `git fetch && git rebase master`; resolve; re-run `clients/gen_dialect.py --check` (nothing of this plan changes the dialect tables; it must still pass).
- [ ] **Step 2: The full suites, one heavy job at a time**

```bash
systemd-run --user --scope -q -p MemoryMax=5G -p MemorySwapMax=1G --wait uv run --no-sync pytest -q
uv run --no-sync ruff check . && uv run --no-sync ruff format --check .
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 clients/go/scripts/build.sh
systemd-run --user --scope -q -p MemoryMax=5G --wait env GOFLAGS=-p=2 bash -c 'cd clients/go && go test -p 1 ./...'
. clients/rust/tools/z3-build-env.sh && systemd-run --user --scope -q -p MemoryMax=5G --wait env CARGO_BUILD_JOBS=2 cargo build --release --manifest-path clients/rust/Cargo.toml
for b in clients/go/daisugi-gate clients/rust/target/release/daisugi-gate; do systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/gate_compare.py --binary $b; done   # W2: daisugi-warden
for b in clients/go/daisugi clients/rust/target/release/daisugi; do systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/pathway_compare.py --binary $b --only verify; done
for p in /mnt/tera/.tt/w3-robot-kernel/kernel-probe clients/rust/target/release/kernel_probe; do systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/kernel_compare.py --probe $p; done
LD_LIBRARY_PATH=$(readlink -f clients/go/.native)/lib systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/robotics_compare.py --probe /mnt/tera/.tt/w3-robot-kernel/robot-probe --probe clients/rust/target/release/robot_probe
systemd-run --user --scope -q -p MemoryMax=5G --wait uv run --no-sync python clients/gate_compare.py --binary clients/go/daisugi-gate --fuzz 2000 --seed 11 --kind envelope
git diff master --stat -- uv.lock clients/go/go.sum clients/rust/Cargo.lock
```
Expected: pytest green (the count rises by about 75); ruff clean; 0 disagreements and 0 fail-open on every compare; the fuzz finds no fail-open; the three lock files are unchanged.

- [ ] **Step 3: Hand check on scratch, never the owner's state**

```bash
export XDG_DATA_HOME=/mnt/tera/.tt/w3-robot-kernel/xdg
R=/mnt/tera/.tt/w3-robot-kernel/ws; mkdir -p $R/.opendaisugi/bin
cp clients/fake_delta.py $R/.opendaisugi/bin/fake-delta && chmod +x $R/.opendaisugi/bin/fake-delta
printf 'delta_solver: %s/.opendaisugi/bin/fake-delta\nnonlinear_solver: delta\ndelta_precision: 0.25\n' $R > $R/.opendaisugi/config.yaml
clients/go/daisugi warden init --workspace $R --stakes physical
# register an envelope with a distance_range invariant and gripper.command_effort_max (a file under scratch), then:
DAISUGI_FAKE_DELTA=delta-sat printf '{"tool_name":"mcp__robot__cartesian_move","tool_input":{"target_position":[0.35,0.2,0.0]},"cwd":"%s","session_id":"s"}' $R | clients/go/daisugi warden check --mode enforce; echo "exit $?"
DAISUGI_FAKE_DELTA=unsat     printf '...the same...' | clients/go/daisugi warden check --mode enforce; echo "exit $?"
clients/go/daisugi warden status
```
Expected: the first is denied with `within delta = 0.25`, exit 2; the second allowed, exit 0; status names the delta solver path and `nonlinear_solver: delta`. (If the warden has no robot tool mapping, run the same through `daisugi verify` on a plan file instead; say which in the ledger.)

- [ ] **Step 4: Ledger and cleanup**: write the ledger lines, including "dReal hand check owed: run `examples/robot-kernel/geofence.smt2` through a real dReal 4.21.06.2 on Ubuntu 22.04 or the Docker image and record `unsat` and `delta-sat` answers; the SMT-LIB2 text Z3's `to_smt2` emits (`declare-fun`, `let`) is unverified against dReal's parser"; `rm -rf /mnt/tera/.tt/w3-robot-kernel clients/rust/target`; `df -h /mnt/tera`.
- [ ] **Step 5: Commit nothing more** (`.superpowers/` is git-ignored). Report to the owner: the case counts, the open questions below, the merge recipe (`git merge --no-ff w3-robot-kernel` after W2 and after the W3 branches that touch `verify.py`).

---

## Owner rulings on this plan's questions (2026-10-30, second round)

1. **Timeout policy.** Stays as today: a Z3 timeout is a warning below strict stakes and a violation under strict or physical. Only the `delta_sat` verdict denies at every stakes (RK-7 as written).
2. **The box.** Each delta-query variable's box comes from the envelope's own bounds; a variable the envelope leaves unbounded falls back to the fixed default box (RK-9 as amended).
3. **Actuator vocabulary.** None of our own. Field names and meanings follow ros2_control's command and state interfaces (`position`, `velocity`, `effort`) and the control_msgs gripper command, where the command carries a position and a ceiling on the effort used to reach it; an envelope may bound both the command fields and the measured state interfaces; MuJoCo is mapped the same way (`ctrl`, `actuator_forcerange`, `actuator_force`). daisugi checks the commands ROS 2 or MuJoCo carries; it does not replace the middleware. The docs were checked online: the ruling's `max_effort` is the legacy `GripperCommand` name whose controller is deprecated in Jazzy and Rolling; the current `ParallelGripperCommand` carries the same ceiling as `command.effort`, so this plan uses `effort` and says so in its header (RK-3 and RK-5 as amended).

## Still open for the owner (the builder follows the ruling until overturned)

1. **`action: open|close` beside `position`.** This plan keeps `action` (the 79 recorded robotics cases and every existing plan use it) and lets `position` override it when set. ROS 2 has no `action`; a stricter reading of ruling 3 drops it and makes `position` required. Cost if wrong: the robotics fixtures are regenerated once and the prompt hint changes.
