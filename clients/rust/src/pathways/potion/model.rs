//! model2vec's `StaticModel`: a static embedding table, one row per
//! token, mean-pooled, read from `config.json`, `tokenizer.json` and
//! `model.safetensors`.

use std::collections::HashMap;

use super::tokenizer::Tokenizer;

/// A loaded StaticModel.
pub struct Model {
    pub tok: Tokenizer,
    emb: Vec<f32>,
    rows: usize,
    dim: usize,
    weights: Option<Vec<f32>>,
    mapping: Option<Vec<i64>>,
    normalize: bool,
    median_len: usize,
}

/// `encode`'s default max_length.
const MAX_LENGTH: usize = 512;

struct Tensor<'a> {
    dtype: String,
    shape: Vec<i64>,
    data: &'a [u8],
}

const BAD_SIZE: &str = "model.safetensors: a tensor's size does not match its shape";

impl Tensor<'_> {
    fn width(&self) -> usize {
        match self.dtype.as_str() {
            "F64" | "I64" => 8,
            "F32" | "I32" => 4,
            "F16" => 2,
            _ => 0,
        }
    }

    /// The number of elements, or an error for a shape that does not
    /// match the tensor's bytes.
    fn count(&self) -> Result<usize, String> {
        let w = self.width();
        let mut n: usize = 1;
        for &d in &self.shape {
            if d < 0 {
                return Err("model.safetensors: a tensor's shape does not match its size".into());
            }
            n = n.checked_mul(d as usize).ok_or("model.safetensors: a tensor's shape does not match its size")?;
        }
        if n.checked_mul(w) != Some(self.data.len()) {
            return Err(BAD_SIZE.into());
        }
        Ok(n)
    }

    fn floats(&self) -> Result<Vec<f32>, String> {
        let n = match self.dtype.as_str() {
            "F32" | "F16" | "F64" => self.count()?,
            d => return Err(format!("model.safetensors: dtype {d} is not read by this binary")),
        };
        let d = self.data;
        Ok(match self.dtype.as_str() {
            "F32" => (0..n).map(|i| f32::from_le_bytes([d[4 * i], d[4 * i + 1], d[4 * i + 2], d[4 * i + 3]])).collect(),
            "F16" => (0..n).map(|i| half(u16::from_le_bytes([d[2 * i], d[2 * i + 1]]))).collect(),
            _ => (0..n)
                .map(|i| {
                    let mut b = [0u8; 8];
                    b.copy_from_slice(&d[8 * i..8 * i + 8]);
                    // model2vec keeps the table's own dtype; a float64
                    // table is narrowed here, far below the score tolerance.
                    f64::from_le_bytes(b) as f32
                })
                .collect(),
        })
    }

    fn ints(&self) -> Result<Vec<i64>, String> {
        let n = match self.dtype.as_str() {
            "I64" | "I32" => self.count()?,
            d => return Err(format!("model.safetensors: dtype {d} is not read by this binary")),
        };
        let d = self.data;
        Ok(if self.dtype == "I64" {
            (0..n)
                .map(|i| {
                    let mut b = [0u8; 8];
                    b.copy_from_slice(&d[8 * i..8 * i + 8]);
                    i64::from_le_bytes(b)
                })
                .collect()
        } else {
            (0..n).map(|i| i32::from_le_bytes([d[4 * i], d[4 * i + 1], d[4 * i + 2], d[4 * i + 3]]) as i64).collect()
        })
    }
}

fn half(h: u16) -> f32 {
    let sign = ((h >> 15) as u32) << 31;
    let exp = ((h >> 10) & 0x1f) as u32;
    let frac = (h & 0x3ff) as u32;
    match exp {
        0 => {
            let f = frac as f32 / 1024.0 * 2f32.powi(-14);
            if sign != 0 {
                -f
            } else {
                f
            }
        }
        31 => f32::from_bits(sign | 0x7f800000 | (frac << 13)),
        _ => f32::from_bits(sign | ((exp + 112) << 23) | (frac << 13)),
    }
}

/// The safetensors format: an 8-byte little-endian header length, a JSON
/// header, then the tensor bytes.
fn read_safetensors(b: &[u8]) -> Result<HashMap<String, Tensor<'_>>, String> {
    if b.len() < 8 {
        return Err("model.safetensors is too short".into());
    }
    let mut lb = [0u8; 8];
    lb.copy_from_slice(&b[..8]);
    let n = u64::from_le_bytes(lb);
    if n > (b.len() - 8) as u64 {
        return Err("model.safetensors: the header runs past the file".into());
    }
    let n = n as usize;
    let header: serde_json::Map<String, serde_json::Value> =
        serde_json::from_slice(&b[8..8 + n]).map_err(|e| format!("model.safetensors header: {e}"))?;
    let data = &b[8 + n..];
    let mut out = HashMap::new();
    for (name, raw) in header {
        if name == "__metadata__" {
            continue;
        }
        let bad = || format!("model.safetensors: tensor {name:?} has a bad header");
        let dtype = raw.get("dtype").and_then(|d| d.as_str()).ok_or_else(bad)?.to_string();
        let shape: Vec<i64> = raw
            .get("shape")
            .and_then(|s| s.as_array())
            .ok_or_else(bad)?
            .iter()
            .map(|x| x.as_i64().ok_or_else(bad))
            .collect::<Result<_, _>>()?;
        let offs = raw.get("data_offsets").and_then(|o| o.as_array()).ok_or_else(bad)?;
        if offs.len() != 2 {
            return Err(bad());
        }
        let a = offs[0].as_u64().ok_or_else(bad)?;
        let z = offs[1].as_u64().ok_or_else(bad)?;
        if a > z || z > data.len() as u64 {
            return Err(format!("model.safetensors: tensor {name:?} runs past the file"));
        }
        out.insert(name, Tensor { dtype, shape, data: &data[a as usize..z as usize] });
    }
    Ok(out)
}

impl Model {
    pub fn dim(&self) -> usize {
        self.dim
    }

    /// A model from the bytes of its three files.
    pub fn load(cfg: &[u8], tok: &[u8], st: &[u8]) -> Result<Model, String> {
        let cfg: serde_json::Value = serde_json::from_slice(cfg).map_err(|e| format!("config.json: {e}"))?;
        let tok = Tokenizer::parse(tok)?;
        let tensors = read_safetensors(st)?;
        let e = tensors.get("embeddings").filter(|e| e.shape.len() == 2).ok_or("model.safetensors has no 2-D 'embeddings' tensor")?;
        if e.shape[0] < 0 || e.shape[1] <= 0 {
            return Err("model.safetensors: the embeddings tensor has an empty shape".into());
        }
        let (rows, dim) = (e.shape[0] as usize, e.shape[1] as usize);
        let emb = e.floats()?;
        let weights = match tensors.get("weights") {
            Some(w) => Some(w.floats()?),
            None => None,
        };
        let mapping = match tensors.get("mapping") {
            Some(m) => Some(m.ints()?),
            None => None,
        };
        // StaticModel.__init__: the vocabulary sorted by id, one vector
        // each unless a mapping is given.
        let mut entries: Vec<(&String, usize)> = tok.vocab().iter().map(|(k, v)| (k, *v)).collect();
        entries.sort_by_key(|e| e.1);
        if mapping.is_none() && entries.len() != rows {
            return Err(format!("number of tokens ({}) does not match number of vectors ({rows})", entries.len()));
        }
        let mut lens: Vec<usize> = entries.iter().map(|e| e.0.chars().count()).collect();
        lens.sort();
        let median_len = match lens.len() {
            0 => 0,
            n if n % 2 == 1 => lens[n / 2],
            n => ((lens[n / 2 - 1] as f64 + lens[n / 2] as f64) / 2.0) as usize,
        };
        let max_id = entries.iter().map(|e| e.1 as i64).max().unwrap_or(-1);
        if let Some(m) = &mapping {
            if (m.len() as i64) <= max_id {
                return Err("model.safetensors: the token mapping is shorter than the vocabulary".into());
            }
            if m.iter().any(|&r| r < 0 || r as usize >= rows) {
                return Err("model.safetensors: the token mapping names a row past the table".into());
            }
        } else if max_id >= rows as i64 {
            return Err("model.safetensors: a token id is past the table".into());
        }
        if let Some(w) = &weights {
            if (w.len() as i64) <= max_id {
                return Err("model.safetensors: the weights are shorter than the vocabulary".into());
            }
        }
        let normalize = cfg.get("normalize").and_then(|n| n.as_bool()).unwrap_or(false);
        Ok(Model { tok, emb, rows, dim, weights, mapping, normalize, median_len })
    }

    /// A model2vec directory.
    pub fn load_dir(dir: &str) -> Result<Model, String> {
        let read = |name: &str| std::fs::read(std::path::Path::new(dir).join(name)).map_err(|e| format!("{name}: {e}"));
        let cfg = read("config.json")?;
        let tok = read("tokenizer.json")?;
        let st = read("model.safetensors")?;
        Model::load(&cfg, &tok, &st)
    }

    /// `StaticModel.encode([text])[0]` followed by the oracle's own L2
    /// normalization (`_search._l2`).
    pub fn encode(&self, text: &str) -> Vec<f64> {
        // tokenize(): the text cut to max_length * median token length
        // code points, [UNK] dropped, then max_length ids.
        let limit = MAX_LENGTH * self.median_len;
        let text = match text.char_indices().nth(limit) {
            Some((i, _)) => &text[..i],
            None => text,
        };
        let mut ids: Vec<usize> = self.tok.encode(text).into_iter().filter(|&id| id != self.tok.unk_id).collect();
        ids.truncate(MAX_LENGTH);
        let mut out = vec![0.0f64; self.dim];
        if !ids.is_empty() {
            for &id in &ids {
                let row = match &self.mapping {
                    Some(m) => m[id] as usize,
                    None => id,
                };
                let w = self.weights.as_ref().map_or(1.0, |w| w[id] as f64);
                let base = row * self.dim;
                for j in 0..self.dim {
                    out[j] += self.emb[base + j] as f64 * w;
                }
            }
            for x in &mut out {
                *x /= ids.len() as f64;
            }
            if self.normalize {
                let n = norm(&out) + 1e-32;
                for x in &mut out {
                    *x /= n;
                }
            }
        }
        let n = norm(&out).max(1e-12);
        for x in &mut out {
            *x /= n;
        }
        let _ = self.rows;
        out
    }
}

fn norm(v: &[f64]) -> f64 {
    v.iter().map(|x| x * x).sum::<f64>().sqrt()
}
