//! The steps outside the graphs (ruling VL-R-3), as the Go port has them.

use std::collections::HashMap;

use serde_json::Value;

pub const IMAGE_SIZE: usize = 512; // resize_imgs_with_padding
pub const STATE_DIM: usize = 32; // max_state_dim
pub const ACTION_DIM: usize = 32; // max_action_dim
pub const CHUNK: usize = 50; // chunk_size
pub const STEPS: usize = 10; // num_steps
pub const TIME_DIM: usize = 720; // the action expert's hidden size
const MIN_PERIOD: f64 = 0.004;
const MAX_PERIOD: f64 = 4.0;
const EPS: f32 = 1e-8;

/// The camera image as the vision graph takes it: rgb is h x w RGB bytes,
/// top row first; the result is [3,512,512] in [0,1], resized bilinearly
/// (align_corners=False, no antialias) to fit with its aspect kept and
/// padded with 0 on the left and top, as lerobot's resize_with_pad does.
pub fn image(rgb: &[u8], h: usize, w: usize) -> Result<Vec<f32>, String> {
    if h == 0 || w == 0 || rgb.len() != h * w * 3 {
        return Err(format!(
            "smolvla: an image of {} bytes is not {h}x{w} RGB",
            rgb.len()
        ));
    }
    let ratio = (w as f64 / IMAGE_SIZE as f64).max(h as f64 / IMAGE_SIZE as f64);
    let (mut rh, mut rw) = ((h as f64 / ratio) as usize, (w as f64 / ratio) as usize);
    if h == IMAGE_SIZE && w == IMAGE_SIZE {
        (rh, rw) = (h, w);
    }
    let (pad_h, pad_w) = (IMAGE_SIZE - rh, IMAGE_SIZE - rw);
    let (ys, xs) = (axis(h, rh), axis(w, rw));
    let mut out = vec![0f32; 3 * IMAGE_SIZE * IMAGE_SIZE];
    let px = |c: usize, y: usize, x: usize| rgb[(y * w + x) * 3 + c] as f32 / 255.0;
    for c in 0..3 {
        for (oy, ay) in ys.iter().enumerate() {
            let row = (c * IMAGE_SIZE + pad_h + oy) * IMAGE_SIZE + pad_w;
            for (ox, ax) in xs.iter().enumerate() {
                let top = ax.l0 * px(c, ay.i0, ax.i0) + ax.l1 * px(c, ay.i0, ax.i1);
                let bottom = ax.l0 * px(c, ay.i1, ax.i0) + ax.l1 * px(c, ay.i1, ax.i1);
                out[row + ox] = ay.l0 * top + ay.l1 * bottom;
            }
        }
    }
    Ok(out)
}

/// One output index of PyTorch's bilinear upsampling along one axis.
struct Tap {
    i0: usize,
    i1: usize,
    l0: f32,
    l1: f32,
}

/// upsample_bilinear2d's source index for one axis, with
/// align_corners=False and no scale given, in float32.
fn axis(input: usize, output: usize) -> Vec<Tap> {
    let scale = input as f32 / output as f32;
    (0..output)
        .map(|o| {
            let src = (scale * (o as f32 + 0.5) - 0.5).max(0.0);
            let i0 = src as usize;
            let i1 = if i0 < input - 1 { i0 + 1 } else { i0 };
            let l1 = src - i0 as f32;
            Tap {
                i0,
                i1,
                l0: 1.0 - l1,
                l1,
            }
        })
        .collect()
}

/// The time of Euler step step: 1 + step*dt with dt = -1/10, in float64,
/// then float32, as lerobot's euler_integrate computes it.
pub fn step_time(step: usize) -> f32 {
    (1.0 + step as f64 * (-1.0 / STEPS as f64)) as f32
}

/// create_sinusoidal_pos_embedding for one time, in float64 and cast to
/// float32. The fraction follows torch.linspace, which counts the second
/// half down from the end.
pub fn time_embedding(t: f32) -> Vec<f32> {
    const HALF: usize = TIME_DIM / 2;
    let mut out = vec![0f32; TIME_DIM];
    let step = 1.0 / (HALF - 1) as f64;
    for i in 0..HALF {
        let frac = if i < HALF / 2 {
            i as f64 * step
        } else {
            1.0 - (HALF - 1 - i) as f64 * step
        };
        let period = MIN_PERIOD * (MAX_PERIOD / MIN_PERIOD).powf(frac);
        let x = (1.0 / period * 2.0 * std::f64::consts::PI) * t as f64;
        out[i] = x.sin() as f32;
        out[HALF + i] = x.cos() as f32;
    }
    out
}

/// The flow-matching noise [1,50,32] for a seed: splitmix64, and
/// Box-Muller in float64 on each pair of draws, cast to float32, as the Go
/// port and clients/vla_cases.py draw it.
pub fn noise(seed: u64) -> Vec<f32> {
    let mut state = seed;
    let mut next = || {
        state = state.wrapping_add(0x9E37_79B9_7F4A_7C15);
        let mut z = state;
        z = (z ^ (z >> 30)).wrapping_mul(0xBF58_476D_1CE4_E5B9);
        z = (z ^ (z >> 27)).wrapping_mul(0x94D0_49BB_1331_11EB);
        z ^ (z >> 31)
    };
    let mut out = Vec::with_capacity(CHUNK * ACTION_DIM);
    let unit = 2f64.powi(-53);
    while out.len() < CHUNK * ACTION_DIM {
        let u1 = ((next() >> 11) + 1) as f64 * unit;
        let u2 = (next() >> 11) as f64 * unit;
        let r = (-2.0 * u1.ln()).sqrt();
        let a = 2.0 * std::f64::consts::PI * u2;
        out.push((r * a.cos()) as f32);
        out.push((r * a.sin()) as f32);
    }
    out
}

/// The checkpoint's normalizer stats, by key ("action.mean").
pub struct Stats {
    m: HashMap<String, Vec<f32>>,
}

impl Stats {
    /// Read stats.safetensors.
    pub fn load(path: &std::path::Path) -> Result<Stats, String> {
        let b = std::fs::read(path).map_err(|e| format!("{}: {e}", path.display()))?;
        Stats::parse(&b)
    }

    /// Parse a safetensors file of F32 tensors.
    pub fn parse(b: &[u8]) -> Result<Stats, String> {
        if b.len() < 8 {
            return Err("stats: shorter than a safetensors header".into());
        }
        let n = u64::from_le_bytes(b[..8].try_into().expect("8 bytes"));
        if n > (b.len() - 8) as u64 {
            return Err("stats: the header runs past the end".into());
        }
        let n = n as usize;
        let h: serde_json::Map<String, Value> =
            serde_json::from_slice(&b[8..8 + n]).map_err(|e| format!("stats: {e}"))?;
        let data = &b[8 + n..];
        let mut m = HashMap::new();
        for (name, t) in h {
            if name == "__metadata__" {
                continue;
            }
            if t["dtype"] != "F32" {
                return Err(format!("stats: {name} is {}, not F32", t["dtype"]));
            }
            let mut count = 1usize;
            for d in t["shape"]
                .as_array()
                .ok_or(format!("stats: {name} has a bad shape"))?
            {
                match d.as_u64() {
                    Some(d) if d as usize <= data.len() => count *= d as usize,
                    _ => return Err(format!("stats: {name} has a bad shape")),
                }
            }
            let off: Vec<u64> = t["data_offsets"]
                .as_array()
                .map(|a| a.iter().filter_map(Value::as_u64).collect())
                .unwrap_or_default();
            if off.len() != 2
                || off[0] > off[1]
                || off[1] > data.len() as u64
                || off[1] - off[0] != (count * 4) as u64
            {
                return Err(format!("stats: {name} has bad data offsets"));
            }
            let raw = &data[off[0] as usize..off[1] as usize];
            let v = raw
                .chunks_exact(4)
                .map(|c| f32::from_le_bytes(c.try_into().expect("4 bytes")))
                .collect();
            m.insert(name, v);
        }
        Ok(Stats { m })
    }

    /// The mean and std of a feature, or None when the stats lack either:
    /// lerobot then leaves the feature as it is.
    fn pair(&self, feature: &str) -> Option<(&[f32], &[f32])> {
        let mean = self.m.get(&format!("{feature}.mean"))?;
        let std = self.m.get(&format!("{feature}.std"))?;
        (mean.len() == std.len()).then_some((mean.as_slice(), std.as_slice()))
    }

    /// The prefix graph's state input: the robot state normalized by the
    /// observation.state stats ((x - mean) / (std + 1e-8), in float32),
    /// then padded with zeros to STATE_DIM.
    pub fn state(&self, x: &[f64]) -> Result<[f32; STATE_DIM], String> {
        let mut out = [0f32; STATE_DIM];
        if x.len() > STATE_DIM {
            return Err(format!(
                "smolvla: a state of {} values is longer than {STATE_DIM}",
                x.len()
            ));
        }
        let pair = self.pair("observation.state");
        if let Some((mean, _)) = pair {
            if mean.len() != x.len() {
                return Err(format!(
                    "smolvla: a state of {} values, but the stats have {}",
                    x.len(),
                    mean.len()
                ));
            }
        }
        for (i, v) in x.iter().enumerate() {
            out[i] = *v as f32;
            if let Some((mean, std)) = pair {
                out[i] = (out[i] - mean[i]) / (std[i] + EPS);
            }
        }
        Ok(out)
    }

    /// The chunk [50,32] cut to its first dim values per row and
    /// unnormalized by the action stats (x * std + mean, in float32).
    pub fn actions(&self, chunk: &[f32], dim: usize) -> Vec<Vec<f64>> {
        let pair = self.pair("action");
        (0..CHUNK)
            .map(|r| {
                (0..dim)
                    .map(|i| {
                        let mut v = chunk[r * ACTION_DIM + i];
                        if let Some((mean, std)) = pair {
                            if i < mean.len() {
                                v = v * std[i] + mean[i];
                            }
                        }
                        v as f64
                    })
                    .collect()
            })
            .collect()
    }
}
