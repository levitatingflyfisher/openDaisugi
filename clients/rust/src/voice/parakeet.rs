//! The Parakeet engine of `voice/engines.py` and its model of
//! `voice/models.py` (rulings VO-16 and VO-17): parakeet-cli run as one
//! resident child, and the curated model, fetched as the pinned F16 GGUF
//! and made Q4_K on the box by parakeet-quantize.

use std::time::Duration;

use crate::gate::py::text;

use super::audio::{run_with_input, which};
use super::engine::{py_path_str, Engine, EngineError};
use super::moonshine::{fetch_file, sha256_of, unavailable, FetchError, ModelEnv};
use super::resident::py_g;

pub const PARAKEET_BINARY: &str = "parakeet-cli";
pub const PARAKEET_QUANTIZE_BINARY: &str = "parakeet-quantize";
pub const PARAKEET_DEFAULT_MODEL: &str = "v2";
pub const PARAKEET_BASE_URL: &str = "https://huggingface.co/cstr/parakeet-tdt-0.6b-v2-GGUF/resolve/main";
pub const PARAKEET_BASE_URL_ENV: &str = "OPENDAISUGI_PARAKEET_BASE_URL";
pub const PARAKEET_QUANT: &str = "q4_k";
pub const QUANTIZE_TIMEOUT_S: f64 = 1800.0;

pub struct ParakeetFile {
    pub name: &'static str,
    pub sha256: &'static str,
    pub size: u64,
}

/// A curated model: the F16 file fetched and the Q4_K file made from it.
pub struct ParakeetModel {
    pub dir: &'static str,
    pub source: ParakeetFile,
    pub quantized: ParakeetFile,
}

/// `pins.PARAKEET_MODELS`, in pins order.
pub const PARAKEET_MODELS: &[(&str, ParakeetModel)] = &[(
    "v2",
    ParakeetModel {
        dir: "tdt-0.6b-v2",
        source: ParakeetFile {
            name: "parakeet-tdt-0.6b-v2.gguf",
            sha256: "c82b001dcb0adecd36f7401e4b77c7257eb352462368b89cf3ee13184206d7f7",
            size: 1_236_861_248,
        },
        quantized: ParakeetFile {
            name: "parakeet-tdt-0.6b-v2-q4_k.gguf",
            sha256: "764c4e6738b0b38c53bbfea040f9e07425d6d742df58b18906053085aea46b1c",
            size: 396_937_984,
        },
    },
)];

fn model_of(name: &str) -> Option<&'static ParakeetModel> {
    PARAKEET_MODELS.iter().find(|(n, _)| *n == name).map(|(_, m)| m)
}

/// `engines.parakeet_args`: the resident child's command line.
pub fn parakeet_args(binary: &str, model: &str) -> Vec<String> {
    [binary, "-m", model, "--resident"].iter().map(|s| s.to_string()).collect()
}

/// `models.parakeet_dir`.
pub fn parakeet_dir(name: &str, env: &ModelEnv) -> String {
    let xdg = env.get("XDG_CACHE_HOME");
    let base = if xdg.starts_with('/') { xdg.to_string() } else { format!("{}/.cache", env.home) };
    let dir = model_of(name).map(|m| m.dir).unwrap_or("");
    py_path_str(&format!("{base}/opendaisugi/models/parakeet/{dir}"))
}

/// `models.parakeet_url`.
pub fn parakeet_url(file: &str, env: &ModelEnv) -> String {
    let base = match env.get(PARAKEET_BASE_URL_ENV) {
        "" => PARAKEET_BASE_URL,
        b => b,
    };
    format!("{base}/{file}")
}

fn mb(n: u64) -> u64 {
    (n + 500_000) / 1_000_000
}

/// Why the curated model could not be made.
pub enum ModelError {
    Fetch(FetchError),
    Quantize(String),
}

fn is_exec_file(p: &str) -> bool {
    use std::os::unix::fs::PermissionsExt;
    std::fs::metadata(p).map(|m| m.is_file() && m.permissions().mode() & 0o111 != 0).unwrap_or(false)
}

/// `models.ensure_parakeet`: the curated model's Q4_K file, made once from
/// the fetched F16 file and checked against its pin.
pub fn ensure_parakeet(name: &str, quantizer: &str, env: &ModelEnv, say: &mut dyn FnMut(&str)) -> Result<String, ModelError> {
    let m = model_of(name).expect("a curated model");
    let dir = parakeet_dir(name, env);
    let out = format!("{dir}/{}", m.quantized.name);
    if sha256_of(&out).as_deref() == Some(m.quantized.sha256) {
        return Ok(out);
    }
    say(&format!(
        "Fetching the Parakeet {name} model ({} MB) into {dir}, then making its {} MB Q4_K file. This happens once.",
        mb(m.source.size),
        mb(m.quantized.size)
    ));
    let src = format!("{dir}/{}", m.source.name);
    fetch_file(&parakeet_url(m.source.name, env), m.source.sha256, &src, Some(m.source.size), env)
        .map_err(ModelError::Fetch)?;
    if !is_exec_file(quantizer) {
        return Err(ModelError::Quantize(format!(
            "{PARAKEET_QUANTIZE_BINARY} is not beside {PARAKEET_BINARY}. Build both with clients/go/scripts/native.sh --parakeet, then try again."
        )));
    }
    let part = format!("{out}.part");
    let ran = run_with_input(quantizer, &[&src, &part, PARAKEET_QUANT], None, Duration::from_secs_f64(QUANTIZE_TIMEOUT_S));
    let (code, _, err) = match ran {
        Ok(r) => r,
        Err(_) => {
            let _ = std::fs::remove_file(&part);
            return Err(ModelError::Quantize(format!(
                "{PARAKEET_QUANTIZE_BINARY} did not finish in {} seconds.",
                py_g(QUANTIZE_TIMEOUT_S)
            )));
        }
    };
    if code != Some(0) {
        let _ = std::fs::remove_file(&part);
        let err_text = text::decode_utf8_replace(&err);
        let lines = text::splitlines(text::strip(&err_text));
        let last = lines.last().map(|l| text::head(text::strip(l), 200).to_string()).unwrap_or_default();
        return Err(ModelError::Quantize(format!("{PARAKEET_QUANTIZE_BINARY} exited {}: {last}", code.unwrap_or(-1))));
    }
    if sha256_of(&part).as_deref() != Some(m.quantized.sha256) {
        let _ = std::fs::remove_file(&part);
        return Err(ModelError::Quantize(format!(
            "The Q4_K file {PARAKEET_QUANTIZE_BINARY} made did not match its pinned sha256 and was deleted. Set voice_model to the path of a Parakeet GGUF file instead."
        )));
    }
    std::fs::rename(&part, &out).map_err(|e| ModelError::Fetch(FetchError { verify: false, msg: e.to_string() }))?;
    let _ = std::fs::remove_file(&src);
    Ok(out)
}

/// `engines.ParakeetEngine(model)`: parakeet-cli on PATH, then the curated
/// model made once, or a GGUF file.
pub fn new_parakeet(model: &str, env: &ModelEnv, say: &mut dyn FnMut(&str)) -> Result<Engine, EngineError> {
    let Some(binary) = which(PARAKEET_BINARY) else {
        return Err(unavailable(format!(
            "{PARAKEET_BINARY} is not on PATH. Build it with clients/go/scripts/native.sh --parakeet (scripts/install.sh does this and puts it on PATH), then try again."
        )));
    };
    let path = if model_of(model).is_some() {
        let real = std::fs::canonicalize(&binary).map(|p| p.to_string_lossy().into_owned()).unwrap_or(binary.clone());
        let parent = std::path::Path::new(&real).parent().map(|p| p.to_string_lossy().into_owned()).unwrap_or_default();
        match ensure_parakeet(model, &format!("{parent}/{PARAKEET_QUANTIZE_BINARY}"), env, say) {
            Ok(p) => p,
            Err(ModelError::Fetch(e)) if e.verify => {
                return Err(unavailable(format!(
                    "A file of the Parakeet {model} model did not match its pinned sha256 and was deleted. Run daisugi voice serve again to fetch it again."
                )))
            }
            Err(ModelError::Quantize(msg)) => return Err(unavailable(msg)),
            Err(ModelError::Fetch(_)) => {
                return Err(unavailable(format!(
                    "The Parakeet {model} model could not be fetched. Check the network, then run daisugi voice serve again."
                )))
            }
        }
    } else {
        let p = py_path_str(model);
        if !std::fs::metadata(&p).map(|m| m.is_file()).unwrap_or(false) {
            return Err(unavailable(format!(
                "Parakeet model file missing: {p}. Set voice_model to {PARAKEET_DEFAULT_MODEL}, or to a Parakeet-TDT GGUF file."
            )));
        }
        p
    };
    let argv = parakeet_args(&binary, &path);
    Ok(Engine::resident("parakeet", binary, path, String::new(), argv, env.timeouts))
}
