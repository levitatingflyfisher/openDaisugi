//! `vla-probe`: a test instrument for clients/vla_compare.py. It runs the
//! SmolVLA executor on the closed-loop case of clients/vla_cases.py, as
//! the Go probe does, and writes one JSON object: for each run, the chunk,
//! the camera image (zlib then base64) and its SHA-256, the qpos after the
//! run and the run's time in milliseconds. With --replay it runs no
//! simulation: it feeds each recorded run's own state and image to the
//! policy, with the same noise seed, and writes the chunks and their times.
//! It is not shipped, and only the mujoco feature builds it.
//!
//!     vla-probe [--replay] CASE.json MODEL_DIR

use std::time::Instant;

use daisugi_verify::gate::pyjson::{loads, Value};
use daisugi_verify::pathways::pmodel::{validate_model, Id, Mode};
use daisugi_verify::robotics::smolvla_executor;
use daisugi_verify::signing::{b64decode, b64encode};
use daisugi_verify::smolvla::{noise, Policy};

/// The camera image of the case: 320 wide, 240 high.
const CAM_WIDTH: usize = 320;
const CAM_HEIGHT: usize = 240;

/// Feed each recorded run's state and image to the policy.
fn replay(c: &serde_json::Value, model_dir: &str) -> Result<(), String> {
    let threads = c["threads"].as_u64().ok_or("the case has no threads")? as usize;
    let seed = c["seed"].as_u64().ok_or("the case has no seed")?;
    let task = c["task"].as_str().ok_or("the case has no task")?;
    let mut p = Policy::open(std::path::Path::new(model_dir), threads)?;
    let mut records = Vec::new();
    for (k, r) in c["records"]
        .as_array()
        .ok_or("the case has no records")?
        .iter()
        .enumerate()
    {
        let z = b64decode(r["image_zlib"].as_str().ok_or("a run has no image")?)
            .map_err(|e| format!("run {k}: {}", e.msg))?;
        let rgb = miniz_oxide::inflate::decompress_to_vec_zlib(&z)
            .map_err(|e| format!("run {k}: {e:?}"))?;
        let state: Vec<f64> = r["state"]
            .as_array()
            .ok_or("a run has no state")?
            .iter()
            .filter_map(serde_json::Value::as_f64)
            .collect();
        let t = Instant::now();
        let chunk = p.chunk(
            &rgb,
            CAM_HEIGHT,
            CAM_WIDTH,
            task,
            &state,
            &noise(seed + k as u64),
            6,
        )?;
        records.push(serde_json::json!({"chunk": chunk, "ms": t.elapsed().as_millis() as u64}));
    }
    println!("{}", serde_json::json!({ "records": records }));
    Ok(())
}

fn run() -> Result<(), String> {
    let mut args: Vec<String> = std::env::args().skip(1).collect();
    let replaying = args.len() == 3 && args[0] == "--replay";
    if replaying {
        args.remove(0);
    }
    if args.len() != 2 {
        return Err("usage: vla-probe [--replay] CASE.json MODEL_DIR".into());
    }
    let b = std::fs::read(&args[0]).map_err(|e| format!("{}: {e}", args[0]))?;
    let c: serde_json::Value = serde_json::from_slice(&b).map_err(|e| e.to_string())?;
    if replaying {
        return replay(&c, &args[1]);
    }
    let num = |k: &str| c[k].as_u64().ok_or(format!("the case has no {k}"));
    let task = c["task"].as_str().ok_or("the case has no task")?;
    let (mut v, st) = smolvla_executor(
        c["mjcf"].as_str().ok_or("the case has no mjcf")?,
        std::path::Path::new(&args[1]),
        num("threads")? as usize,
        num("seed")?,
    )?;
    let text = serde_json::json!({
        "source": "vla-probe", "task": task,
        "steps": [{"type": "vla", "id": "v", "task": task,
                   "max_actions": num("max_actions")?, "timeout_s": 3600}],
    });
    let raw = loads(&text.to_string()).map_err(|e| format!("{e:?}"))?;
    let plan = validate_model(Id::ActionPlan, &raw, Mode::Python)
        .map_err(|e| format!("the step does not validate: {e:?}"))?;
    let step = match plan.as_obj().ok_or("no plan")?.value("steps") {
        Value::List(xs) => xs[0].as_obj().ok_or("no step")?.clone(),
        _ => return Err("no steps".into()),
    };
    let mut records = Vec::new();
    for k in 0..num("steps")? {
        let t = Instant::now();
        let r = v.run_step(&step, 3600);
        let ms = t.elapsed().as_millis() as u64;
        if r.rc != 0 {
            return Err(format!("run {k}: rc {}: {}", r.rc, r.stdout));
        }
        let s = st.borrow();
        let digest = ring::digest::digest(&ring::digest::SHA256, &s.last_image);
        let sha: String = digest.as_ref().iter().map(|b| format!("{b:02x}")).collect();
        let z = miniz_oxide::deflate::compress_to_vec_zlib(&s.last_image, 9);
        records.push(serde_json::json!({
            "image_zlib": b64encode(&z),
            "image_sha256": sha,
            "chunk": s.last_chunk,
            "qpos": v.sim().ok_or("no simulation")?.1.qpos(),
            "ms": ms,
        }));
    }
    println!("{}", serde_json::json!({ "records": records }));
    Ok(())
}

fn main() {
    if let Err(e) = run() {
        eprintln!("vla-probe: {e}");
        std::process::exit(1);
    }
}
