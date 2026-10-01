//! TransformersVLAExecutor for lerobot/smolvla_base, with the policy run
//! natively from its ONNX graphs, as the Go port has it.

use std::cell::RefCell;
use std::rc::Rc;

use super::vla::{Action, PredictError, Vla};
use super::{floats_of, PyError};
use crate::gate::pyjson::{Object, Value};
use crate::mujoco::{Data, Model, Renderer};
use crate::smolvla::{noise, Policy};
use crate::supervise::str_of;

/// The camera `_capture_image` renders: the default free camera, 320 wide
/// and 240 high.
const CAM_WIDTH: usize = 320;
const CAM_HEIGHT: usize = 240;
/// The checkpoint's action feature: 6 values.
const ACTIONS: usize = 6;

/// What the executor has done, for a caller that compares runs: the
/// chunks run, the last chunk (all 6 action values of each of 50 actions)
/// and the camera image it saw.
#[derive(Default)]
pub struct SmolVlaState {
    pub chunks: u64,
    pub last_chunk: Vec<Vec<f64>>,
    pub last_image: Vec<u8>,
}

fn raised(msg: String) -> PredictError {
    PredictError::Raised(PyError::new("RuntimeError", msg))
}

/// The SmolVLA executor on the MJCF at mjcf_path, with the pinned model in
/// model_dir. Chunk k draws the noise of seed seed+k; the first rows of a
/// chunk are mapped onto the MJCF's joints in order, capped at the step's
/// max_actions.
pub fn smolvla_executor(
    mjcf_path: &str,
    model_dir: &std::path::Path,
    threads: usize,
    seed: u64,
) -> Result<(Vla, Rc<RefCell<SmolVlaState>>), String> {
    let mut policy = Policy::open(model_dir, threads)?;
    let state = Rc::new(RefCell::new(SmolVlaState::default()));
    let shared = state.clone();
    let mut renderer: Option<Renderer> = None;
    let predict = move |sim: &mut (Model, Data), names: &[String], step: &Object, obs: &Object| {
        let (m, d) = sim;
        if renderer.is_none() {
            renderer = Some(Renderer::new(m, CAM_WIDTH, CAM_HEIGHT).map_err(raised)?);
        }
        let rgb = renderer
            .as_mut()
            .expect("made above")
            .render(d, -1)
            .map_err(raised)?;
        let qpos = floats_of(obs.value("qpos"));
        let mut st = shared.borrow_mut();
        let chunk = policy
            .chunk(
                &rgb,
                CAM_HEIGHT,
                CAM_WIDTH,
                &str_of(step, "task"),
                &qpos,
                &noise(seed + st.chunks),
                ACTIONS,
            )
            .map_err(raised)?;
        st.chunks += 1;
        let keys: Vec<String> = names.iter().take(ACTIONS).cloned().collect();
        let n = match step.value("max_actions") {
            Value::Int(t) => t.parse::<i64>().unwrap_or(0).clamp(0, chunk.len() as i64) as usize,
            _ => 0,
        };
        let out = chunk[..n]
            .iter()
            .map(|row| Action {
                joints: keys.clone(),
                targets: row[..keys.len()].to_vec(),
            })
            .collect();
        st.last_chunk = chunk;
        st.last_image = rgb;
        Ok(out)
    };
    let vla = Vla::new(mjcf_path, 200, Some(Box::new(predict))).map_err(|e| e.msg)?;
    Ok((vla, state))
}
