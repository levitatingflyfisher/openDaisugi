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

use crate::models::{ActionPlan, Envelope, StepKind};
use crate::violation::Violation;
use std::collections::{HashMap, HashSet};

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

    if unsat { vec![Violation::plan("z3")] } else { vec![] }
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

    if unsat { vec![Violation::plan("z3")] } else { vec![] }
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
                out.push(Violation::step("z3", step.id.clone()));
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
                match limits.get(joint) {
                    None => out.push(Violation::step("z3", step.id.clone())),
                    Some((lo, hi)) => {
                        if !(*lo <= *target && *target <= *hi) {
                            out.push(Violation::step("z3", step.id.clone()));
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
                    out.push(Violation::step("z3", step.id.clone()));
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
                out.push(Violation::step("z3", step_id.clone()));
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
