//! The three graphs and the two assets, loaded, and one action chunk:
//! vision, prefix, then the 10 Euler steps over denoise.

use std::path::Path;

use super::process::{
    image, step_time, time_embedding, Stats, ACTION_DIM, CHUNK, IMAGE_SIZE, STATE_DIM, STEPS,
    TIME_DIM,
};
use super::tokenizer::{Tokenizer, WIDTH};
use crate::ort::{Data, Input, Output, Session};

/// The SHA-256 of each file of the model directory (ruling VL-R-2).
pub const PINS: [(&str, &str); 5] = [
    (
        "denoise.onnx",
        "f2412f7254659dcb9cf0d00dc181f05cca1299609ab1ec25042fe400ff4bda66",
    ),
    (
        "prefix.onnx",
        "ac391d4633dce409f050929adbe77c7a6af86dddfa0266aa4937e98b13ef3579",
    ),
    (
        "stats.safetensors",
        "490ab239d96e263687c0b2e386a0afbc235a2eceb9857c36ed32f2f162a3e7c8",
    ),
    (
        "tokenizer.json",
        "5ece781dc8d2b2f3e2f289ca0ae50b17cfc27dd27bfe7971bb8241e0b964331a",
    ),
    (
        "vision.onnx",
        "eae4a86506b2f8e36f4b3efa16133964feadbc6323ecbb14c61838e080b06664",
    ),
];

/// The largest difference allowed between an action of a port and the
/// oracle's (ruling VL-R-6).
pub const CHUNK_TOLERANCE: f64 = 1e-3;

const IMG_TOKENS: usize = 64;
const EMB_DIM: usize = 960;
const PREFIX_LEN: usize = IMG_TOKENS + WIDTH + 1;
const KV_LAYERS: usize = 16;
const KV_HEADS: usize = 5;
const HEAD_DIM: usize = 64;
const KV_SIZE: usize = KV_LAYERS * KV_HEADS * PREFIX_LEN * HEAD_DIM;

pub struct Policy {
    tok: Tokenizer,
    stats: Stats,
    vision: Session,
    prefix: Session,
    denoise: Session,
}

fn file_sha256(path: &Path) -> Result<String, String> {
    use std::io::Read;
    let mut f = std::fs::File::open(path).map_err(|e| format!("{}: {e}", path.display()))?;
    let mut ctx = ring::digest::Context::new(&ring::digest::SHA256);
    let mut buf = vec![0u8; 1 << 20];
    loop {
        let n = f
            .read(&mut buf)
            .map_err(|e| format!("{}: {e}", path.display()))?;
        if n == 0 {
            break;
        }
        ctx.update(&buf[..n]);
    }
    Ok(ctx
        .finish()
        .as_ref()
        .iter()
        .map(|b| format!("{b:02x}"))
        .collect())
}

impl Policy {
    /// Check every file of dir against its pin, then load them. threads is
    /// the intra-op thread count of each graph.
    pub fn open(dir: &Path, threads: usize) -> Result<Policy, String> {
        for (name, pin) in PINS {
            let got = file_sha256(&dir.join(name)).map_err(|e| format!("smolvla: {e}"))?;
            if got != pin {
                return Err(format!(
                    "smolvla: {name} has sha256 {got}, not the pinned {pin}"
                ));
            }
        }
        let graph = |name: &str| {
            Session::open(&dir.join(name), threads).map_err(|e| format!("smolvla: {name}: {e}"))
        };
        Ok(Policy {
            tok: Tokenizer::load(&dir.join("tokenizer.json"))?,
            stats: Stats::load(&dir.join("stats.safetensors"))?,
            vision: graph("vision.onnx")?,
            prefix: graph("prefix.onnx")?,
            denoise: graph("denoise.onnx")?,
        })
    }

    /// One action chunk: the first dim values of each of the 50 actions,
    /// for the camera image rgb (h x w RGB bytes, top row first), the task,
    /// the robot state and the noise [50*32].
    #[allow(clippy::too_many_arguments)]
    pub fn chunk(
        &mut self,
        rgb: &[u8],
        h: usize,
        w: usize,
        task: &str,
        state: &[f64],
        noise: &[f32],
        dim: usize,
    ) -> Result<Vec<Vec<f64>>, String> {
        if noise.len() != CHUNK * ACTION_DIM {
            return Err(format!(
                "smolvla: noise of {} values, not {}",
                noise.len(),
                CHUNK * ACTION_DIM
            ));
        }
        if dim < 1 || dim > ACTION_DIM {
            return Err(format!("smolvla: {dim} action values is out of range"));
        }
        let img = image(rgb, h, w)?;
        let (ids, mask) = self.tok.task(task)?;
        let st = self.stats.state(state)?;
        let mut emb = vec![0f32; IMG_TOKENS * EMB_DIM];
        self.vision.run(
            &[Input {
                name: "image",
                data: Data::F32(&img),
                shape: &[1, 3, IMAGE_SIZE as i64, IMAGE_SIZE as i64],
            }],
            &mut [Output {
                name: "img_emb",
                data: &mut emb,
            }],
        )?;
        let mut keys = vec![0f32; KV_SIZE];
        let mut values = vec![0f32; KV_SIZE];
        self.prefix.run(
            &[
                Input {
                    name: "img_emb",
                    data: Data::F32(&emb),
                    shape: &[1, IMG_TOKENS as i64, EMB_DIM as i64],
                },
                Input {
                    name: "lang_tokens",
                    data: Data::I64(&ids),
                    shape: &[1, WIDTH as i64],
                },
                Input {
                    name: "lang_mask",
                    data: Data::I64(&mask),
                    shape: &[1, WIDTH as i64],
                },
                Input {
                    name: "state",
                    data: Data::F32(&st),
                    shape: &[1, STATE_DIM as i64],
                },
            ],
            &mut [
                Output {
                    name: "kv_keys",
                    data: &mut keys,
                },
                Output {
                    name: "kv_values",
                    data: &mut values,
                },
            ],
        )?;
        let mut pmask = vec![1i64; IMG_TOKENS];
        pmask.extend_from_slice(&mask);
        pmask.push(1);
        let kv_shape = [
            KV_LAYERS as i64,
            1,
            KV_HEADS as i64,
            PREFIX_LEN as i64,
            HEAD_DIM as i64,
        ];
        let mut x = noise.to_vec();
        let mut v = vec![0f32; CHUNK * ACTION_DIM];
        let dt = (-1.0 / STEPS as f64) as f32;
        for step in 0..STEPS {
            let temb = time_embedding(step_time(step));
            self.denoise.run(
                &[
                    Input {
                        name: "x_t",
                        data: Data::F32(&x),
                        shape: &[1, CHUNK as i64, ACTION_DIM as i64],
                    },
                    Input {
                        name: "time_emb",
                        data: Data::F32(&temb),
                        shape: &[1, TIME_DIM as i64],
                    },
                    Input {
                        name: "kv_keys",
                        data: Data::F32(&keys),
                        shape: &kv_shape,
                    },
                    Input {
                        name: "kv_values",
                        data: Data::F32(&values),
                        shape: &kv_shape,
                    },
                    Input {
                        name: "prefix_mask",
                        data: Data::I64(&pmask),
                        shape: &[1, PREFIX_LEN as i64],
                    },
                ],
                &mut [Output {
                    name: "v_t",
                    data: &mut v,
                }],
            )?;
            for (xi, vi) in x.iter_mut().zip(&v) {
                *xi += dt * vi;
            }
        }
        Ok(self.stats.actions(&x, dim))
    }
}
