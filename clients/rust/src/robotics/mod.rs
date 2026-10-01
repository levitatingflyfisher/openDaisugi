//! `executor_mujoco.py` and `vla_executor.py`: the step executors that run
//! robot steps against a MuJoCo simulation, and the scaffolding for a
//! Vision-Language-Action policy with its deterministic mock. The twin of
//! the Go package internal/robotics.
//!
//! The oracle's commands build none of them (run, orchestrate and weave
//! fail a robot step with "no executor for kind ..."); a caller hands them
//! to a supervisor, as a Python caller hands `robotics_executors` to
//! `Supervisor`. Only the mujoco feature builds this module.

use std::cell::RefCell;
use std::rc::Rc;
use std::time::Instant;

use crate::gate::py::text::repr;
use crate::gate::pyjson::{py_float_repr, Object, Value};
use crate::mujoco::{Data, Model, Obj, JNT_HINGE};
use crate::supervise::{ms, str_of, ExecResult, Executor};

mod ik;
mod smolvla;
mod vla;

pub use smolvla::{smolvla_executor, SmolVlaState};
pub use vla::{mock_predictor, Action, PredictError, Predictor, Vla};

/// Return codes for rollout-time violations (`executor_mujoco.RC_*`).
pub const RC_OK: i32 = 0;
pub const RC_TORQUE_VIOLATION: i32 = 3;
pub const RC_CONTACT_VIOLATION: i32 = 4;
pub const RC_IK_FAILED: i32 = 5;

/// An exception the oracle's executor raises: `ty` is the class, `msg` is
/// str(exception), which the supervisor records as "executor error: " +
/// str(e).
#[derive(Debug, Clone, PartialEq)]
pub struct PyError {
    pub ty: String,
    pub msg: String,
}

impl PyError {
    pub fn new(ty: &str, msg: impl Into<String>) -> PyError {
        PyError {
            ty: ty.into(),
            msg: msg.into(),
        }
    }
}

/// KeyError(msg): str() of a KeyError is the repr of its text.
fn key_error(msg: &str) -> PyError {
    PyError::new("KeyError", repr(msg))
}

/// A number a caller passed, kept as Python holds it: an int stays an int,
/// so repr(torque_limit) prints "5" for 5 and "5.0" for 5.0.
#[derive(Debug, Clone, PartialEq)]
pub struct Num(pub Value);

impl Num {
    pub fn float(f: f64) -> Num {
        Num(Value::Float(f))
    }

    /// The number as a float.
    pub fn f(&self) -> f64 {
        match &self.0 {
            Value::Float(f) => *f,
            Value::Int(t) => t.parse().unwrap_or(0.0),
            Value::Bool(b) => f64::from(u8::from(*b)),
            _ => 0.0,
        }
    }

    /// repr() of the number.
    pub fn repr(&self) -> String {
        match &self.0 {
            Value::Int(t) => t.clone(),
            _ => py_float_repr(self.f()),
        }
    }

    /// The number for a JSON dump.
    pub fn json(&self) -> Value {
        match &self.0 {
            Value::Int(t) => Value::Int(t.clone()),
            _ => Value::Float(self.f()),
        }
    }
}

/// repr() of a tuple of floats.
pub fn tuple_repr(xs: &[f64]) -> String {
    let parts: Vec<String> = xs.iter().map(|x| py_float_repr(*x)).collect();
    if parts.len() == 1 {
        return format!("({},)", parts[0]);
    }
    format!("({})", parts.join(", "))
}

/// repr() of a list of (str, str) tuples.
pub fn pairs_repr(ps: &[(String, String)]) -> String {
    let parts: Vec<String> = ps
        .iter()
        .map(|(a, b)| format!("({}, {})", repr(a), repr(b)))
        .collect();
    format!("[{}]", parts.join(", "))
}

/// A validated float field.
fn float_of(v: &Value) -> f64 {
    Num(v.clone()).f()
}

/// A validated tuple of floats.
fn floats_of(v: &Value) -> Vec<f64> {
    match v {
        Value::List(xs) | Value::Tuple(xs) => xs.iter().map(float_of).collect(),
        _ => vec![],
    }
}

/// type(step).__name__ for a registered step type.
fn class_name(step: &Object) -> String {
    let kind = str_of(step, "type");
    let steps = crate::pathways::pmodel::steps();
    match steps.order.iter().position(|t| *t == kind) {
        Some(i) => steps.models[i].name.to_string(),
        None => "StepBase".into(),
    }
}

/// MuJoCoExecutor's keyword arguments.
#[derive(Debug, Clone)]
pub struct Options {
    pub settle_steps: i64,
    pub position_tol: f64,
    /// None for None.
    pub torque_limit: Option<Num>,
    pub forbid_contacts: bool,
    pub ee_body: String,
    pub ik_max_iter: i64,
    pub ik_tol: Num,
    pub ik_damping: f64,
}

impl Default for Options {
    /// The constructor's defaults.
    fn default() -> Options {
        Options {
            settle_steps: 2000,
            position_tol: 0.05,
            torque_limit: None,
            forbid_contacts: false,
            ee_body: "end_effector".into(),
            ik_max_iter: 200,
            ik_tol: Num::float(1e-3),
            ik_damping: 0.1,
        }
    }
}

/// `_RolloutOutcome`.
struct Outcome {
    stdout: String,
    rc: i32,
}

impl Outcome {
    fn ok(stdout: String) -> Outcome {
        Outcome { stdout, rc: RC_OK }
    }
}

/// MuJoCoExecutor: one model and one data, kept across steps so a
/// joint_move starts where the last step left the arm.
pub struct MuJoCo {
    pub opt: Options,
    pub mjcf: String,
    model: Model,
    data: Data,
}

impl MuJoCo {
    /// Loads the MJCF at path. A file MuJoCo cannot load is the ValueError
    /// MjModel.from_xml_path raises, with MuJoCo's message.
    pub fn new(path: &str, opt: Options) -> Result<MuJoCo, PyError> {
        let model = Model::from_xml_path(path).map_err(|e| PyError::new("ValueError", e))?;
        let data = Data::new(&model);
        Ok(MuJoCo {
            opt,
            mjcf: path.into(),
            model,
            data,
        })
    }

    /// The simulation, for a caller that reads its state.
    pub fn model(&self) -> &Model {
        &self.model
    }
    pub fn data(&self) -> &Data {
        &self.data
    }
    pub fn data_mut(&mut self) -> &mut Data {
        &mut self.data
    }

    /// configure_from_envelope: an envelope's torque_limit replaces the
    /// executor's, and a non-empty obstacles list forbids contacts. What the
    /// envelope leaves out stays as it was.
    pub fn configure_from_envelope(&mut self, env: &Object) {
        let Some(perms) = env.value("permissions").as_obj() else {
            return;
        };
        match perms.value("torque_limit") {
            v @ (Value::Float(_) | Value::Int(_)) => self.opt.torque_limit = Some(Num(v.clone())),
            _ => {}
        }
        if let Value::List(obs) = perms.value("obstacles") {
            if !obs.is_empty() {
                self.opt.forbid_contacts = true;
            }
        }
    }

    /// MuJoCoExecutor.run. An error is the exception the oracle raises.
    pub fn run_step(&mut self, step: &Object) -> Result<ExecResult, PyError> {
        let start = Instant::now();
        let o = match str_of(step, "type").as_str() {
            "sim_reset" => self.reset(step),
            "joint_move" => self.joint_move(step)?,
            "gripper" => self.gripper(step)?,
            "cartesian_move" => self.cartesian_move(step),
            _ => {
                return Err(PyError::new(
                    "TypeError",
                    format!(
                        "MuJoCoExecutor cannot run step of type {}",
                        class_name(step)
                    ),
                ))
            }
        };
        Ok(ExecResult {
            rc: o.rc,
            stdout: o.stdout,
            duration_ms: ms(start),
            ..Default::default()
        })
    }

    fn reset(&mut self, step: &Object) -> Outcome {
        self.data.reset();
        self.data.forward();
        let seed = match step.value("seed") {
            Value::Int(t) => t.clone(),
            _ => "None".into(),
        };
        Outcome::ok(format!("sim reset (seed={seed})"))
    }

    fn joint_move(&mut self, step: &Object) -> Result<Outcome, PyError> {
        let empty = Object::new();
        let targets = step.value("joint_targets").as_obj().unwrap_or(&empty);
        let names: Vec<String> = targets.keys().to_vec();
        for name in &names {
            let aid = self.actuator_for_joint(name)?;
            self.data.ctrl_mut()[aid] = float_of(targets.value(name));
        }
        if let Some(v) = self.step_with_guards(
            self.opt.settle_steps,
            &format!("joint_move {}", str_of(step, "id")),
        ) {
            return Ok(v);
        }
        let parts: Vec<String> = names
            .iter()
            .map(|name| {
                let jid = self.model.name2id(Obj::Joint, name);
                let q = self.data.qpos()[self.model.jnt_qposadr(jid as usize)];
                format!("{}: {}", repr(name), py_float_repr(q))
            })
            .collect();
        Ok(Outcome::ok(format!(
            "joint_move complete; positions={{{}}}",
            parts.join(", ")
        )))
    }

    fn gripper(&mut self, step: &Object) -> Result<Outcome, PyError> {
        let action = str_of(step, "action");
        let mut applied = vec![];
        for aid in 0..self.model.nu() {
            let name = self
                .model
                .id2name(Obj::Actuator, aid as i32)
                .unwrap_or_default();
            if !name.starts_with("a_grip") {
                continue;
            }
            let (lo, hi) = self.gripper_range(aid)?;
            let target = if action == "open" { hi } else { lo };
            self.data.ctrl_mut()[aid] = target;
            applied.push(format!("{name}={}", py_float_repr(target)));
        }
        let hold = ((float_of(step.value("hold_s")) / self.model.timestep()).trunc() as i64).max(1);
        if let Some(v) = self.step_with_guards(hold, &format!("gripper {}", str_of(step, "id"))) {
            return Ok(v);
        }
        Ok(Outcome::ok(format!(
            "gripper {action}; applied=[{}]",
            applied.join(", ")
        )))
    }

    fn cartesian_move(&mut self, step: &Object) -> Outcome {
        let target = floats_of(step.value("target_position"));
        let q = match self.solve_ik(&target) {
            Ok(q) => q,
            Err(e) => {
                return Outcome {
                    rc: RC_IK_FAILED,
                    stdout: format!("cartesian_move {} IK failed: {e}", str_of(step, "id")),
                }
            }
        };
        for aid in 0..self.model.nu() {
            let jid = self.model.actuator_trnid(aid) as usize;
            if self.model.jnt_type(jid) == JNT_HINGE {
                let adr = self.model.jnt_qposadr(jid);
                self.data.ctrl_mut()[aid] = q[adr];
            }
        }
        if let Some(v) = self.step_with_guards(
            self.opt.settle_steps,
            &format!("cartesian_move {}", str_of(step, "id")),
        ) {
            return v;
        }
        let ee = self
            .data
            .xpos(self.model.name2id(Obj::Body, &self.opt.ee_body) as usize);
        Outcome::ok(format!(
            "cartesian_move reached {} (target={})",
            tuple_repr(&ee),
            tuple_repr(&target)
        ))
    }

    /// `_solve_ik_position`: damped least squares on the ee body's
    /// position, on a scratch copy of the state, forward kinematics only.
    fn solve_ik(&self, target: &[f64]) -> Result<Vec<f64>, String> {
        let ee = self.model.name2id(Obj::Body, &self.opt.ee_body);
        if ee < 0 {
            return Err(format!(
                "ee_body {} not found in MJCF",
                repr(&self.opt.ee_body)
            ));
        }
        let ee = ee as usize;
        let mut scratch = Data::new(&self.model);
        scratch.qpos_mut().copy_from_slice(self.data.qpos());
        let d2 = self.opt.ik_damping * self.opt.ik_damping;
        let tol = self.opt.ik_tol.f();
        let nv = self.model.nv();
        let residual = |s: &Data| {
            let p = s.xpos(ee);
            let e = [target[0] - p[0], target[1] - p[1], target[2] - p[2]];
            (e, ik::norm3(&e))
        };
        for _ in 0..self.opt.ik_max_iter.max(0) {
            scratch.forward();
            let (e, n) = residual(&scratch);
            if n < tol {
                return Ok(scratch.qpos().to_vec());
            }
            let dq = ik::damped_step(&scratch.jac_body(ee), &e, d2);
            let q = scratch.qpos_mut();
            for j in 0..nv {
                q[j] += dq[j];
            }
        }
        scratch.forward();
        let (_, n) = residual(&scratch);
        if n < tol {
            return Ok(scratch.qpos().to_vec());
        }
        Err(format!(
            "did not converge in {} iters (residual={n:.4}, tol={})",
            self.opt.ik_max_iter,
            self.opt.ik_tol.repr()
        ))
    }

    /// `_step_with_guards`: n physics steps, the torque and contact guards
    /// after each; None when no guard fired.
    fn step_with_guards(&mut self, n: i64, place: &str) -> Option<Outcome> {
        for _ in 0..n.max(0) {
            self.data.step();
            if let Some(limit) = &self.opt.torque_limit {
                let mut peak = 0.0;
                for (k, f) in self.data.actuator_force().iter().enumerate() {
                    let a = f.abs();
                    if k == 0 || a > peak {
                        peak = a;
                    }
                }
                if peak > limit.f() {
                    return Some(Outcome {
                        rc: RC_TORQUE_VIOLATION,
                        stdout: format!(
                            "torque_limit violated during {place}: peak |actuator_force|={peak:.3} > limit={}",
                            limit.repr()
                        ),
                    });
                }
            }
            let ncon = self.data.ncon();
            if self.opt.forbid_contacts && ncon > 0 {
                let pairs: Vec<(String, String)> = (0..ncon)
                    .map(|i| {
                        let (g1, g2) = self.data.contact_geoms(i);
                        (self.geom_name(g1), self.geom_name(g2))
                    })
                    .collect();
                return Some(Outcome {
                    rc: RC_CONTACT_VIOLATION,
                    stdout: format!(
                        "contact detected during {place} (ncon={ncon}); pairs={}",
                        pairs_repr(&pairs)
                    ),
                });
            }
        }
        None
    }

    fn geom_name(&self, g: i32) -> String {
        self.model
            .id2name(Obj::Geom, g)
            .unwrap_or_else(|| format!("geom#{g}"))
    }

    /// `_gripper_target_range`: the joint range, else the actuator's
    /// ctrlrange, else a ValueError.
    fn gripper_range(&self, aid: usize) -> Result<(f64, f64), PyError> {
        let jid = self.model.actuator_trnid(aid) as usize;
        if self.model.jnt_limited(jid) {
            return Ok(self.model.jnt_range(jid));
        }
        if self.model.actuator_ctrllimited(aid) {
            return Ok(self.model.actuator_ctrlrange(aid));
        }
        let name = self
            .model
            .id2name(Obj::Actuator, aid as i32)
            .unwrap_or_else(|| format!("actuator#{aid}"));
        Err(PyError::new(
            "ValueError",
            format!(
                "Gripper actuator {} has neither joint range nor ctrlrange — cannot infer open/close targets",
                repr(&name)
            ),
        ))
    }

    /// `_actuator_id_for_joint`.
    fn actuator_for_joint(&self, name: &str) -> Result<usize, PyError> {
        let jid = self.model.name2id(Obj::Joint, name);
        if jid < 0 {
            return Err(key_error(&format!(
                "Joint {} not found in MJCF",
                repr(name)
            )));
        }
        (0..self.model.nu())
            .find(|&aid| self.model.actuator_trnid(aid) == jid)
            .ok_or_else(|| key_error(&format!("No actuator drives joint {}", repr(name))))
    }
}

/// One MuJoCo executor shared by every robot step kind, as
/// `robotics_executors` wires it: the four kinds share one simulation.
#[derive(Clone)]
pub struct Shared(pub Rc<RefCell<MuJoCo>>);

impl Executor for Shared {
    fn run(
        &mut self,
        step: &Object,
        _timeout_s: u64,
        _max_output_bytes: usize,
    ) -> Result<ExecResult, String> {
        self.0.borrow_mut().run_step(step).map_err(|e| e.msg)
    }

    fn configure(&mut self, env: &Object) {
        self.0.borrow_mut().configure_from_envelope(env)
    }
}

/// `robotics_executors`: one executor for sim_reset, joint_move,
/// cartesian_move and gripper.
pub fn robotics_executors(
    path: &str,
    opt: Options,
) -> Result<(Vec<(String, Box<dyn Executor>)>, Shared), PyError> {
    let shared = Shared(Rc::new(RefCell::new(MuJoCo::new(path, opt)?)));
    let kinds = ["sim_reset", "joint_move", "cartesian_move", "gripper"];
    let out = kinds
        .iter()
        .map(|k| (k.to_string(), Box::new(shared.clone()) as Box<dyn Executor>))
        .collect();
    Ok((out, shared))
}

#[cfg(test)]
mod tests;
