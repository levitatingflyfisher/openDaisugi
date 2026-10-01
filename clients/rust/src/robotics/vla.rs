//! `VLAExecutorBase` and `MockVLAExecutor`: MuJoCo loading, the rollout
//! loop and the evidence packaging around a policy's prediction.

use std::time::Instant;

use super::{class_name, float_of, floats_of, PyError};
use crate::gate::pyjson::{dumps, Object, Value};
use crate::mujoco::{Data, Model, Obj};
use crate::supervise::{ms, str_of, ExecResult, Executor};

/// One action of a rollout: joint targets in order.
#[derive(Debug, Clone, PartialEq)]
pub struct Action {
    pub joints: Vec<String>,
    pub targets: Vec<f64>,
}

/// What `_predict_actions` raised.
#[derive(Debug, Clone, PartialEq)]
pub enum PredictError {
    /// NotImplementedError.
    NotImplemented,
    /// Any other exception.
    Raised(PyError),
}

/// `_predict_actions`: the actions for a step, given the simulation (for a
/// camera), the MJCF's named joints and the observation.
pub type Predictor = Box<
    dyn FnMut(&mut (Model, Data), &[String], &Object, &Object) -> Result<Vec<Action>, PredictError>,
>;

/// VLAExecutorBase around a policy. With no predictor it is the bare base
/// class, whose `_predict_actions` is not implemented.
pub struct Vla {
    pub mjcf_path: String,
    pub max_actions_global: i64,
    predict: Option<Predictor>,
    sim: Option<(Model, Data)>,
    joint_names: Vec<String>,
}

fn float_list(xs: &[f64]) -> Value {
    Value::List(xs.iter().map(|x| Value::Float(*x)).collect())
}

impl Vla {
    /// Loads the MJCF at path when path is not empty, as the base class does
    /// when mjcf_path is truthy.
    pub fn new(
        path: &str,
        max_actions_global: i64,
        predict: Option<Predictor>,
    ) -> Result<Vla, PyError> {
        let mut v = Vla {
            mjcf_path: path.into(),
            max_actions_global,
            predict,
            sim: None,
            joint_names: vec![],
        };
        if path.is_empty() {
            return Ok(v);
        }
        let model = Model::from_xml_path(path).map_err(|e| PyError::new("ValueError", e))?;
        for j in 0..model.njnt() {
            if let Some(n) = model.id2name(Obj::Joint, j as i32) {
                v.joint_names.push(n);
            }
        }
        let data = Data::new(&model);
        v.sim = Some((model, data));
        Ok(v)
    }

    /// MockVLAExecutor(mjcf_path=path, num_actions=n).
    pub fn mock(path: &str, num_actions: i64) -> Result<Vla, PyError> {
        Vla::new(path, 200, Some(mock_predictor(num_actions)))
    }

    /// The simulation, when an MJCF is loaded.
    pub fn sim(&self) -> Option<&(Model, Data)> {
        self.sim.as_ref()
    }

    pub fn joint_names(&self) -> &[String] {
        &self.joint_names
    }

    /// `_current_observation`: proprioception only.
    fn observation(&self) -> Object {
        let mut obs = Object::new();
        let Some((m, d)) = &self.sim else { return obs };
        obs.set("qpos", float_list(d.qpos()));
        obs.set("qvel", float_list(d.qvel()));
        let ee = m.name2id(Obj::Body, "end_effector");
        if ee >= 0 {
            obs.set("end_effector_xyz", float_list(&d.xpos(ee as usize)));
        }
        obs
    }

    /// `_apply_action`: set the targets of the joints that exist, then one
    /// physics step.
    fn apply(&mut self, a: &Action) {
        let Some((m, d)) = &mut self.sim else { return };
        for (name, target) in a.joints.iter().zip(&a.targets) {
            let jid = m.name2id(Obj::Joint, name);
            if jid < 0 {
                continue;
            }
            if let Some(aid) = (0..m.nu()).find(|&aid| m.actuator_trnid(aid) == jid) {
                d.ctrl_mut()[aid] = *target;
            }
        }
        d.step();
    }

    fn final_state(&self, ev: &mut Object) {
        let Some((m, d)) = &self.sim else { return };
        ev.set("qpos_final", float_list(d.qpos()));
        let ee = m.name2id(Obj::Body, "end_effector");
        if ee >= 0 {
            ev.set("end_effector_xyz_final", float_list(&d.xpos(ee as usize)));
        }
        ev.set("contact_count", Value::Int(d.ncon().to_string()));
    }

    /// VLAExecutorBase.run.
    pub fn run_step(&mut self, step: &Object, timeout_s: u64) -> ExecResult {
        let fail = |stdout: String| ExecResult {
            rc: 1,
            stdout,
            ..Default::default()
        };
        if str_of(step, "type") != "vla" {
            return fail(format!(
                "VLAExecutorBase: not a VLAStep ({})",
                class_name(step)
            ));
        }
        let started = Instant::now();
        if self.sim.is_none() {
            return fail("VLAExecutorBase: no MJCF loaded; pass mjcf_path".into());
        }
        let obs = self.observation();
        let Some(predict) = self.predict.as_mut() else {
            return fail("VLAExecutorBase._predict_actions not implemented".into());
        };
        let sim = self.sim.as_mut().expect("checked above");
        let actions = match predict(sim, &self.joint_names, step, &obs) {
            Ok(a) => a,
            Err(PredictError::NotImplemented) => {
                return fail("VLAExecutorBase._predict_actions not implemented".into())
            }
            Err(PredictError::Raised(e)) => {
                return fail(format!("VLA inference error: {}: {}", e.ty, e.msg))
            }
        };
        let max_actions = match step.value("max_actions") {
            Value::Int(t) => t.parse::<i64>().unwrap_or(0),
            _ => 0,
        };
        let cap = max_actions.min(self.max_actions_global);
        let actions = py_slice_to(actions, cap);
        let timeout = float_of(step.value("timeout_s")).min(timeout_s as f64);
        let mut executed = 0usize;
        for a in &actions {
            if started.elapsed().as_secs_f64() > timeout {
                break;
            }
            self.apply(a);
            executed += 1;
        }
        let mut ev = Object::new();
        ev.set("task", Value::Str(str_of(step, "task")));
        ev.set("actions_requested", Value::Int(actions.len().to_string()));
        ev.set("actions_executed", Value::Int(executed.to_string()));
        ev.set("max_actions", step.value("max_actions").clone());
        ev.set("observation_initial", Value::Obj(obs));
        self.final_state(&mut ev);
        ExecResult {
            rc: 0,
            stdout: dumps(&Value::Obj(ev), true),
            duration_ms: ms(started),
            timed_out: executed < actions.len(),
            ..Default::default()
        }
    }
}

impl Executor for Vla {
    fn run(
        &mut self,
        step: &Object,
        timeout_s: u64,
        _max_output_bytes: usize,
    ) -> Result<ExecResult, String> {
        Ok(self.run_step(step, timeout_s))
    }
}

/// xs[:n] with Python's meaning of a negative n.
fn py_slice_to(mut xs: Vec<Action>, n: i64) -> Vec<Action> {
    let len = xs.len() as i64;
    let mut n = if n < 0 { (n + len).max(0) } else { n };
    if n > len {
        n = len;
    }
    xs.truncate(n as usize);
    xs
}

/// `MockVLAExecutor._predict_actions`: n actions that walk the first two
/// non-gripper joints in a straight line from the observed qpos (by
/// position: qpos[0] and qpos[1]) to target_pose's first two components,
/// or to zero with no target pose.
pub fn mock_predictor(n: i64) -> Predictor {
    Box::new(
        move |_: &mut (Model, Data), names: &[String], step: &Object, obs: &Object| {
            if names.is_empty() {
                return Ok(vec![]);
            }
            let (j1_target, j2_target) = match step.value("target_pose") {
                Value::Null => (0.0, 0.0),
                tp => {
                    let xs = floats_of(tp);
                    (xs[0], xs[1])
                }
            };
            let controllable: Vec<&String> =
                names.iter().filter(|n| !n.starts_with("j_grip")).collect();
            if controllable.is_empty() {
                return Ok(vec![]);
            }
            let start = match obs.get("qpos") {
                Some(q) => floats_of(q),
                None => vec![0.0; names.len()],
            };
            let j1_now = start.first().copied().unwrap_or(0.0);
            let j2_now = start.get(1).copied().unwrap_or(0.0);
            let mut out = vec![];
            for i in 1..=n {
                let t = i as f64 / n as f64;
                let mut a = Action {
                    joints: vec![controllable[0].clone()],
                    targets: vec![j1_now + t * (j1_target - j1_now)],
                };
                if controllable.len() >= 2 {
                    a.joints.push(controllable[1].clone());
                    a.targets.push(j2_now + t * (j2_target - j2_now));
                }
                out.push(a);
            }
            Ok(out)
        },
    )
}
