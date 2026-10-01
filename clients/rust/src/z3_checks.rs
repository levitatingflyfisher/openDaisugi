//! Port of `opendaisugi/z3_checks.py`.
//!
//! `check_envelope_self_consistency` and `check_plan_against_envelope` are,
//! in the oracle, Z3 solver calls — but every constraint they add is a
//! trivial boolean/integer conjunction over already-concrete values (see
//! `clients/rust/README.md` for the design note), so this port evaluates
//! them natively rather than shelling out to `z3 -in`; the verdict is
//! provably identical. The robotics trajectory checks were never
//! Z3-backed in the oracle either (pure f64 numerics/sampling) — ported
//! here verbatim, including the exact 8-sample interpolation loop.

use crate::gate::py::text::{repr, repr_list};
use crate::gate::pyjson::py_float_repr;
use crate::models::{ActionPlan, Envelope, StepKind};
use crate::gate::pyjson::Value;
use crate::violation::{kv, Violation};
use std::collections::{HashMap, HashSet};

/// Python's `repr()` of a tuple of floats.
fn tuple3(t: (f64, f64, f64)) -> String {
    format!("({}, {}, {})", py_float_repr(t.0), py_float_repr(t.1), py_float_repr(t.2))
}

/// A list of floats for a violation's detail.
fn floats(xs: &[f64]) -> Value {
    Value::List(xs.iter().map(|x| Value::Float(*x)).collect())
}

fn arr3(a: [f64; 3]) -> String {
    tuple3((a[0], a[1], a[2]))
}

pub const RECOGNIZED_OPAQUE_TYPES: &[&str] =
    &["end_effector_in_workspace", "joint_limits_respected", "velocity_bounded", "no_obstacle_penetration"];

/// `check_envelope_self_consistency` — is the envelope internally
/// contradictory? Native evaluation of a formula that, once `shell` and
/// `can_write` are pinned to their concrete values, has at most one
/// satisfying assignment either way — so "solver.check() == unsat" reduces
/// to a plain boolean test.
pub fn check_envelope_self_consistency(env: &Envelope) -> Vec<Violation> {
    let shell = env.permissions.shell;
    let can_write = !env.permissions.file_write.is_empty();

    let mut unsat = false;
    if !env.permissions.shell_allowlist.is_empty() && !shell {
        unsat = true;
    }
    if env.postconditions.iter().any(|pc| pc.type_ == "file_exists") && !can_write {
        unsat = true;
    }
    let max_time = env.permissions.max_execution_time_s;
    if !(max_time > 0 && max_time <= 3600) {
        unsat = true;
    }

    if unsat {
        vec![Violation::plan("z3").msg("Envelope is internally inconsistent").with(unsat_core(), None)]
    } else {
        vec![]
    }
}

/// `check_plan_against_envelope` — same native-boolean argument.
pub fn check_plan_against_envelope(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let shell_available = env.permissions.shell;
    let write_available = !env.permissions.file_write.is_empty();

    let mut unsat = false;
    if plan.steps.iter().any(|s| s.step_type == "shell") && !shell_available {
        unsat = true;
    }
    if plan.steps.iter().any(|s| s.step_type == "file_write") && !write_available {
        unsat = true;
    }

    if unsat {
        vec![Violation::plan("z3").msg("Plan requirements contradict envelope permissions").with(unsat_core(), None)]
    } else {
        vec![]
    }
}

fn check_workspace_containment(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let bounds = match env.permissions.workspace_bounds {
        Some(b) => b,
        None => return vec![],
    };
    let (min, max) = bounds;
    let mut out = Vec::new();
    for step in &plan.steps {
        let target = match &step.kind {
            StepKind::CartesianMove { target_position } => Some(*target_position),
            StepKind::Vla { target_pose: Some(t) } => Some(*t),
            _ => None,
        };
        if let Some((x, y, z)) = target {
            let inside = min[0] <= x && x <= max[0] && min[1] <= y && y <= max[1] && min[2] <= z && z <= max[2];
            if !inside {
                out.push(
                    Violation::step("z3", step.id.clone())
                        .msg(format!(
                            "Step '{}' target {} outside workspace bounds ({}, {})",
                            step.id,
                            tuple3((x, y, z)),
                            arr3(min),
                            arr3(max)
                        ))
                        .with(
                            kv(&[
                                ("invariant", Value::Str("end_effector_in_workspace".into())),
                                ("step", Value::Str(step.id.clone())),
                                ("target", floats(&[x, y, z])),
                                ("bounds", Value::List(vec![floats(&min), floats(&max)])),
                            ]),
                            None,
                        ),
                );
            }
        }
    }
    out
}

fn check_joint_limits(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let limits = &env.permissions.joint_limits;
    if limits.is_empty() {
        return vec![];
    }
    let mut out = Vec::new();
    for step in &plan.steps {
        if let StepKind::JointMove { joint_targets, .. } = &step.kind {
            for (joint, target) in joint_targets {
                match env.permissions.joint_limit(joint) {
                    None => {
                        let names: Vec<String> = limits.iter().map(|(k, _)| k.clone()).collect();
                        out.push(
                            Violation::step("z3", step.id.clone())
                                .msg(format!(
                                    "Step '{}' joint {} not declared in envelope joint_limits {}",
                                    step.id,
                                    repr(joint),
                                    repr_list(&names)
                                ))
                                .with(
                                    kv(&[
                                        ("invariant", Value::Str("joint_limits_respected".into())),
                                        ("step", Value::Str(step.id.clone())),
                                        ("joint", Value::Str(joint.clone())),
                                    ]),
                                    None,
                                ),
                        )
                    }
                    Some((lo, hi)) => {
                        if !(lo <= *target && *target <= hi) {
                            out.push(
                                Violation::step("z3", step.id.clone())
                                    .msg(format!(
                                        "Step '{}' joint {} target {} outside [{}, {}]",
                                        step.id,
                                        repr(joint),
                                        py_float_repr(*target),
                                        py_float_repr(lo),
                                        py_float_repr(hi)
                                    ))
                                    .with(
                                        kv(&[
                                            ("invariant", Value::Str("joint_limits_respected".into())),
                                            ("step", Value::Str(step.id.clone())),
                                            ("joint", Value::Str(joint.clone())),
                                            ("target", Value::Float(*target)),
                                            ("range", floats(&[lo, hi])),
                                        ]),
                                        None,
                                    ),
                            );
                        }
                    }
                }
            }
        }
    }
    out
}

fn check_velocity_bounds(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let limit = match env.permissions.velocity_limit {
        Some(l) => l,
        None => return vec![],
    };
    let mut state: HashMap<String, f64> = HashMap::new();
    let mut out = Vec::new();
    for step in &plan.steps {
        if let StepKind::JointMove { joint_targets, duration_s, velocity_scale } = &step.kind {
            let duration = duration_s.max(1e-6);
            for (joint, target) in joint_targets {
                let prev = *state.get(joint).unwrap_or(&0.0);
                let peak = (target - prev).abs() / duration * velocity_scale;
                if peak > limit {
                    out.push(
                        Violation::step("z3", step.id.clone())
                            .msg(format!(
                                "Step '{}' joint {} peak velocity {peak:.3} rad/s > limit {}",
                                step.id,
                                repr(joint),
                                py_float_repr(limit)
                            ))
                            .with(
                                kv(&[
                                    ("invariant", Value::Str("velocity_bounded".into())),
                                    ("step", Value::Str(step.id.clone())),
                                    ("joint", Value::Str(joint.clone())),
                                    ("peak_rad_s", Value::Float(peak)),
                                    ("limit_rad_s", Value::Float(limit)),
                                ]),
                                None,
                            ),
                    );
                }
                state.insert(joint.clone(), *target);
            }
        }
    }
    out
}

const OBSTACLE_MIDPOINT_SAMPLES: usize = 8;

fn interpolate_positions(p0: (f64, f64, f64), p1: (f64, f64, f64), n: usize) -> Vec<(f64, f64, f64)> {
    (0..n)
        .map(|i| {
            let t = i as f64 / (n as f64 - 1.0);
            (p0.0 + (p1.0 - p0.0) * t, p0.1 + (p1.1 - p0.1) * t, p0.2 + (p1.2 - p0.2) * t)
        })
        .collect()
}

fn check_obstacle_avoidance(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let obstacles = &env.permissions.obstacles;
    if obstacles.is_empty() {
        return vec![];
    }
    let cartesian_steps: Vec<&crate::models::Step> =
        plan.steps.iter().filter(|s| matches!(s.kind, StepKind::CartesianMove { .. })).collect();
    if cartesian_steps.is_empty() {
        return vec![];
    }

    let mut prev = (0.0, 0.0, 0.0);
    let mut sample_points: Vec<(String, (f64, f64, f64))> = Vec::new();
    for step in &cartesian_steps {
        if let StepKind::CartesianMove { target_position } = &step.kind {
            for pt in interpolate_positions(prev, *target_position, OBSTACLE_MIDPOINT_SAMPLES) {
                sample_points.push((step.id.clone(), pt));
            }
            prev = *target_position;
        }
    }

    let mut out = Vec::new();
    let mut flagged: HashSet<(String, usize)> = HashSet::new();
    for (step_id, (x, y, z)) in &sample_points {
        for (idx, (min, max)) in obstacles.iter().enumerate() {
            if flagged.contains(&(step_id.clone(), idx)) {
                continue;
            }
            let inside = min[0] <= *x && *x <= max[0] && min[1] <= *y && *y <= max[1] && min[2] <= *z && *z <= max[2];
            if inside {
                out.push(
                    Violation::step("z3", step_id.clone())
                        .msg(format!(
                            "Step '{step_id}' trajectory sample ({x:.3}, {y:.3}, {z:.3}) inside obstacle #{idx}"
                        ))
                        .with(
                            kv(&[
                                ("invariant", Value::Str("no_obstacle_penetration".into())),
                                ("step", Value::Str(step_id.clone())),
                                ("obstacle_index", Value::Int(idx.to_string())),
                                ("sample_point", floats(&[*x, *y, *z])),
                            ]),
                            None,
                        ),
                );
                flagged.insert((step_id.clone(), idx));
            }
        }
    }
    out
}

/// `check_plan_invariants` — dispatch the recognized-opaque robotics
/// invariant types to their native handlers, keyed purely on the *type
/// name* being declared on the envelope (independent of whether that
/// invariant also carries a predicate-algebra `expr` — see verify.rs's
/// Stage 2b/2c comment for why both can fire on the same declaration).
pub fn check_plan_invariants(plan: &ActionPlan, env: &Envelope) -> Vec<Violation> {
    let declared: HashSet<&str> = env.invariants.iter().map(|i| i.type_.as_str()).collect();
    let mut out = Vec::new();
    if declared.contains("end_effector_in_workspace") {
        out.extend(check_workspace_containment(plan, env));
    }
    if declared.contains("joint_limits_respected") {
        out.extend(check_joint_limits(plan, env));
    }
    if declared.contains("velocity_bounded") {
        out.extend(check_velocity_bounds(plan, env));
    }
    if declared.contains("no_obstacle_penetration") {
        out.extend(check_obstacle_avoidance(plan, env));
    }
    out
}

#[cfg(test)]
mod robotics_detail_tests {
    use super::check_plan_invariants;
    use crate::gate::pyjson::{dumps, Value};
    use crate::models::{parse_plan, Envelope};

    /// Each robotics violation carries the oracle's detail dict, so a run
    /// the robotics checks reject is journaled as the oracle journals it.
    #[test]
    fn robotics_violation_details() {
        let env: Envelope = serde_json::from_str(
            r#"{"generated_by": "t", "task": "t", "permissions": {
              "joint_limits": {"j1": [-1, 1]}, "velocity_limit": 0.5, "workspace_bounds": [[-1, -1, -1], [1, 1, 1]],
              "obstacles": [[[0.1, -0.1, -0.1], [0.3, 0.1, 0.1]]]},
              "invariants": [{"type": "end_effector_in_workspace", "description": "d"}, {"type": "joint_limits_respected", "description": "d"},
                {"type": "velocity_bounded", "description": "d"}, {"type": "no_obstacle_penetration", "description": "d"}]}"#,
        )
        .unwrap();
        let plan = parse_plan(
            &serde_json::from_str(
                r#"{"source": "t", "task": "t", "steps": [
              {"type": "joint_move", "id": "a", "joint_targets": {"j1": 1.5, "j9": 0.2}},
              {"type": "cartesian_move", "id": "b", "target_position": [0.4, 0, 0]},
              {"type": "vla", "id": "c", "task": "x", "target_pose": [2, 0, 0]}]}"#,
            )
            .unwrap(),
        )
        .unwrap();
        let want = [
            r#"{"invariant": "end_effector_in_workspace", "step": "c", "target": [2.0, 0.0, 0.0], "bounds": [[-1.0, -1.0, -1.0], [1.0, 1.0, 1.0]]}"#,
            r#"{"invariant": "joint_limits_respected", "step": "a", "joint": "j1", "target": 1.5, "range": [-1.0, 1.0]}"#,
            r#"{"invariant": "joint_limits_respected", "step": "a", "joint": "j9"}"#,
            r#"{"invariant": "velocity_bounded", "step": "a", "joint": "j1", "peak_rad_s": 1.5, "limit_rad_s": 0.5}"#,
            r#"{"invariant": "no_obstacle_penetration", "step": "b", "obstacle_index": 0, "sample_point": [0.11428571428571428, 0.0, 0.0]}"#,
        ];
        let got = check_plan_invariants(&plan, &env);
        assert_eq!(got.len(), want.len());
        for (v, w) in got.iter().zip(want) {
            let detail = v.detail.clone().map(Value::Obj).map(|d| dumps(&d, true));
            assert_eq!(detail.as_deref(), Some(w), "{}", v.message);
        }
    }
}

/// z3's `str(solver.unsat_core())` with no tracked assertions: "[]".
fn unsat_core() -> crate::gate::pyjson::Object {
    crate::gate::pyjson::Object::new().with("unsat_core", "[]")
}
