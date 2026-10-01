//! `voice/models.py` and the Moonshine engine of `voice/engines.py`: the
//! pinned models, fetched once into the user cache, and moonshine-cli run
//! as one resident child (rulings VO-13 and VO-16).

use std::collections::HashMap;

use crate::gate::sha256;
use crate::netproxy;
use crate::pathways::potion::fetch::get_to;

use super::audio::which;
use super::engine::{py_path_str, Engine, EngineError};

pub const MOONSHINE_BINARY: &str = "moonshine-cli";
pub const MOONSHINE_TIMEOUT_S: f64 = 120.0;
pub const MOONSHINE_DEFAULT_MODEL: &str = "small";
pub const MOONSHINE_DIR_ARCH: &str = "small";
pub const MOONSHINE_BASE_URL: &str = "https://download.moonshine.ai/model";
pub const MOONSHINE_BASE_URL_ENV: &str = "OPENDAISUGI_MOONSHINE_BASE_URL";
pub const MOONSHINE_REVISION: &str = "quantized_26_08_21";

/// `pins.FASTER_WHISPER_REVISIONS` and `pins.FASTER_WHISPER_SIZES`; only
/// the Python build fetches these models.
pub const FASTER_WHISPER_REVISIONS: &[(&str, &str)] = &[
    ("tiny", "d90ca5fe260221311c53c58e660288d3deb8d356"),
    ("tiny.en", "0d3d19a32d3338f10357c0889762bd8d64bbdeba"),
    ("base", "ebe41f70d5b6dfa9166e2c581c45c9c0cfc57b66"),
    ("base.en", "3d3d5dee26484f91867d81cb899cfcf72b96be6c"),
    ("small", "536b0662742c02347bc0e980a01041f333bce120"),
    ("small.en", "d1d751a5f8271d482d14ca55d9e2deeebbae577f"),
    ("medium", "08e178d48790749d25932bbc082711ddcfdfbc4f"),
    ("medium.en", "a29b04bd15381511a9af671baec01072039215e3"),
    ("large-v2", "f0fe81560cb8b68660e564f55dd99207059c092e"),
    ("large-v3", "edaa852ec7e145841d8ffdb056a99866b5f0a478"),
    ("distil-large-v3", "c3058b475261292e64a0412df1d2681c06260fab"),
    ("turbo", "0a363e9161cbc7ed1431c9597a8ceaf0c4f78fcf"),
];
pub const FASTER_WHISPER_SIZES: &[(&str, u64)] = &[("tiny.en", 78_090_594)];

pub struct MoonshineFile {
    pub name: &'static str,
    pub sha256: &'static str,
    pub size: u64,
}

/// `pins.MOONSHINE_MODELS`, in pins order.
pub const MOONSHINE_MODELS: &[(&str, &[MoonshineFile])] = &[
    (
        "tiny",
        &[
            MoonshineFile { name: "adapter.ort", sha256: "22ecc949e146c49667fda28d102d4e30749a107dc88a396292aa8f277ef1347c", size: 1_319_664 },
            MoonshineFile { name: "cross_kv.ort", sha256: "143a36667b8d05fd9d04e8c337b7ee121f37ef299aea6b3d82bdb3d3401950b4", size: 1_287_544 },
            MoonshineFile { name: "decoder_kv.ort", sha256: "8852553f312adb6c9aa4d17418015049b30f412209ee569d336548c0044627de", size: 32_583_720 },
            MoonshineFile { name: "encoder.ort", sha256: "a8414e1a5dedf9f2093d7680601dd8a9b0433e7020260eafe0e370ead91134ca", size: 7_675_440 },
            MoonshineFile { name: "frontend.model.ort", sha256: "5121b561417b638afce0c6c31b760e37c93cf97f80d9b0031aad1fe7b6f25d61", size: 23_344 },
            MoonshineFile { name: "frontend.weights.ort", sha256: "217da24ac6f522ebf02da8ef288e77d1ac68d50d4a6821433182e4fbf4204bbd", size: 2_093_464 },
            MoonshineFile { name: "streaming_config.json", sha256: "74fe5ddebd63b17caf59e8a3b18c17547ff7bce1642050edbb1c3962674f8950", size: 509 },
            MoonshineFile { name: "tokenizer.bin", sha256: "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", size: 249_974 },
        ],
    ),
    (
        "small",
        &[
            MoonshineFile { name: "adapter.ort", sha256: "c665f742364febad597cc9ac1e0b341ffbee0e24a1466e2f3bde95e6e4771762", size: 2_870_368 },
            MoonshineFile { name: "cross_kv.ort", sha256: "e2d3417144e9514055ebfefe8dcc4c0a55a55adcb8530435844c75c53e352bf6", size: 5_356_536 },
            MoonshineFile { name: "decoder_kv.ort", sha256: "1a05465b1dd955858dfcbee039c0020fb5dd982b0f5094c34e61735d518d771b", size: 81_878_600 },
            MoonshineFile { name: "encoder.ort", sha256: "2d4d973e91e8aca08c51e7e7efa28a46ab265b63d809d5294d18b86bcd85b993", size: 44_148_576 },
            MoonshineFile { name: "frontend.model.ort", sha256: "09b1210ae30dc5f0f3e45f0ebab914c254741323114f53fbbe5ae62cca35058f", size: 26_944 },
            MoonshineFile { name: "frontend.weights.ort", sha256: "7ef97521bd4bad3928f5bb6808586f4fcc6e92bd5990394112eed7d4052ec338", size: 7_769_464 },
            MoonshineFile { name: "streaming_config.json", sha256: "26f02b6afb22d60871a5efd85c3d38e569cc0ddb6c5eb6e93d3260152ae8a47a", size: 512 },
            MoonshineFile { name: "tokenizer.bin", sha256: "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", size: 249_974 },
        ],
    ),
    (
        "medium",
        &[
            MoonshineFile { name: "adapter.ort", sha256: "3f2a287def57cc094367a0eec3c4f5fc36a32ec420e86764b696920991b20281", size: 3_651_296 },
            MoonshineFile { name: "cross_kv.ort", sha256: "642f6e21cd305be79342207c6f9e6b681d469d55bc48c72b27b84846fb71fd1e", size: 11_643_776 },
            MoonshineFile { name: "decoder_kv.ort", sha256: "193bb366492b74fc4ad338c6778e8d8eb916aaa11b5aa264f9057f4db7759486", size: 146_972_408 },
            MoonshineFile { name: "encoder.ort", sha256: "12915e76ebac7dd287c5ea63965d06103a53ba1ce242a4a34f318f3958c60c37", size: 94_705_376 },
            MoonshineFile { name: "frontend.model.ort", sha256: "95768855c70c8251eeecc05fedf69999da1b8ab16f605c9f457fd3354b0ad6b5", size: 28_720 },
            MoonshineFile { name: "frontend.weights.ort", sha256: "5ac941f490cbe035b335b99a414cc393d62d4c6f9f2423495b286870d271d709", size: 11_889_560 },
            MoonshineFile { name: "streaming_config.json", sha256: "28e83b7a28e91472692a035e0dae3116422ae43aeb2bef5ed822c44ce89b88af", size: 513 },
            MoonshineFile { name: "tokenizer.bin", sha256: "6884b35fd6377d4c4d32336a0bc152f36b64d1e45b6503683cdc238250a8472d", size: 249_974 },
        ],
    ),
];

fn files_of(name: &str) -> Option<&'static [MoonshineFile]> {
    MOONSHINE_MODELS.iter().find(|(n, _)| *n == name).map(|(_, f)| *f)
}

/// What model resolution reads from the process: the environment, the
/// home directory, the hardware probe (None: only the hardware variable
/// is read), and the resident child's timeouts.
pub struct ModelEnv {
    pub vars: HashMap<String, String>,
    pub home: String,
    pub hardware: Option<Box<dyn Fn() -> super::hardware::VoiceHardware>>,
    pub timeouts: super::engine::Timeouts,
}

impl ModelEnv {
    /// This process's own environment.
    pub fn process() -> ModelEnv {
        let vars: HashMap<String, String> = std::env::vars().collect();
        let home = vars.get("HOME").cloned().unwrap_or_default();
        ModelEnv { vars, home, hardware: None, timeouts: Default::default() }
    }

    pub(super) fn get(&self, k: &str) -> &str {
        self.vars.get(k).map(|s| s.as_str()).unwrap_or("")
    }

    /// `engines.detect_voice_hardware`: the hardware variable when it is
    /// set, else the probe tiers setup uses, else nothing known.
    pub fn detect_hardware(&self) -> super::hardware::VoiceHardware {
        if let Some(hw) = super::hardware::parse_hardware_env(self.get(super::hardware::HARDWARE_ENV)) {
            return hw;
        }
        match &self.hardware {
            Some(f) => f(),
            None => super::hardware::VoiceHardware { ram_gb: None, cpus: 1, vram_gb: 0.0 },
        }
    }
}

/// `engines.moonshine_args`: the resident child's command line.
pub fn moonshine_args(binary: &str, model: &str, arch: &str) -> Vec<String> {
    [binary, "-m", model, "-a", arch, "--resident"].iter().map(|s| s.to_string()).collect()
}

/// `models.moonshine_dir`: under $XDG_CACHE_HOME when it is an absolute
/// path, else under $HOME/.cache.
pub fn moonshine_dir(name: &str, env: &ModelEnv) -> String {
    let xdg = env.get("XDG_CACHE_HOME");
    let base = if xdg.starts_with('/') { xdg.to_string() } else { format!("{}/.cache", env.home) };
    py_path_str(&format!("{base}/opendaisugi/models/moonshine/{name}-streaming-en/{MOONSHINE_REVISION}"))
}

/// `models.moonshine_url`.
pub fn moonshine_url(name: &str, file: &str, env: &ModelEnv) -> String {
    let base = match env.get(MOONSHINE_BASE_URL_ENV) {
        "" => MOONSHINE_BASE_URL,
        b => b,
    };
    format!("{base}/{name}-streaming-en/{MOONSHINE_REVISION}/{file}")
}

/// `models.moonshine_size_mb`.
pub fn moonshine_size_mb(name: &str) -> u64 {
    let total: u64 = files_of(name).unwrap_or(&[]).iter().map(|f| f.size).sum();
    (total + 500_000) / 1_000_000
}

/// A pinned download that failed: `verify` when the bytes arrived and did
/// not match their digest (`FetchVerificationError`), else any other
/// failure (its OSError).
#[derive(Debug)]
pub struct FetchError {
    pub verify: bool,
    pub msg: String,
}

fn download(msg: impl Into<String>) -> FetchError {
    FetchError { verify: false, msg: msg.into() }
}

pub(super) fn sha256_of(path: &str) -> Option<String> {
    let m = std::fs::metadata(path).ok()?;
    if !m.is_file() {
        return None;
    }
    // A model file may be a gigabyte: hash it as it is read.
    use std::io::Read;
    let mut f = std::fs::File::open(path).ok()?;
    let mut h = sha256::Hasher::new();
    let mut buf = vec![0u8; 1 << 20];
    loop {
        match f.read(&mut buf) {
            Ok(0) => return Some(h.hexdigest()),
            Ok(n) => h.update(&buf[..n]),
            Err(_) => return None,
        }
    }
}

/// A temporary download file older than this is the orphan of a download
/// that was killed. A younger one may belong to another download that runs.
const STALE_PART_AGE: std::time::Duration = std::time::Duration::from_secs(24 * 3600);

/// Removes the `<name>.part-*` files beside `dest` that were last written
/// more than `max_age` ago; a killed download leaves one of up to 1.2 GB.
fn sweep_stale_parts(dir: &std::path::Path, dest: &str, max_age: std::time::Duration) {
    let Some(name) = std::path::Path::new(dest).file_name().map(|n| n.to_string_lossy().into_owned()) else { return };
    let prefix = format!("{name}.part-");
    let Ok(entries) = std::fs::read_dir(dir) else { return };
    for e in entries.flatten() {
        if !e.file_name().to_string_lossy().starts_with(&prefix) {
            continue;
        }
        let old = e
            .metadata()
            .ok()
            .filter(|m| m.is_file())
            .and_then(|m| m.modified().ok())
            .and_then(|t| t.elapsed().ok())
            .is_some_and(|age| age > max_age);
        if old {
            let _ = std::fs::remove_file(e.path());
        }
    }
}

/// `_model_fetch.fetch`: a file that matches its digest is used as it is;
/// any other is removed and downloaded again, checked, then written beside
/// it and renamed into place. A download that stops starts again from
/// nothing next time (this binary does not resume one). `size` is None when
/// the caller does not know it.
pub fn fetch_file(url: &str, digest: &str, dest: &str, size: Option<u64>, env: &ModelEnv) -> Result<(), FetchError> {
    if sha256_of(dest).as_deref() == Some(digest) {
        return Ok(());
    }
    let dir = std::path::Path::new(dest).parent().map(|p| p.to_path_buf()).unwrap_or_default();
    std::fs::create_dir_all(&dir).map_err(|e| download(e.to_string()))?;
    let _ = std::fs::remove_file(dest);
    sweep_stale_parts(&dir, dest, STALE_PART_AGE);
    let proxies = netproxy::urllib_for(&env.vars);
    // One byte over the pinned size still arrives, so a longer file fails
    // its digest as _model_fetch's does.
    let cap = size.map(|n| n + 1).unwrap_or(1 << 40);
    // The body goes to a temporary file of its own as it arrives, hashed
    // on the way, so a model file of a gigabyte is never held in memory.
    let name = std::path::Path::new(dest).file_name().map(|n| n.to_string_lossy().into_owned()).unwrap_or_default();
    let part = dir.join(format!("{name}.part-{}", crate::gate::random_hex(8).unwrap_or_else(|_| "0".into())));
    let r = stream_to(url, cap, &proxies, &part, size, digest);
    if r.is_err() {
        let _ = std::fs::remove_file(&part);
        return r;
    }
    let done = std::fs::rename(&part, dest);
    let _ = std::fs::remove_file(&part);
    done.map_err(|e| download(e.to_string()))
}

/// Writes the body of `url` to `part`, hashing it on the way, and checks
/// its length and digest; the file is synced and 0644 when it returns Ok.
fn stream_to(
    url: &str,
    cap: u64,
    proxies: &netproxy::Urllib,
    part: &std::path::Path,
    size: Option<u64>,
    digest: &str,
) -> Result<(), FetchError> {
    use std::io::Write;
    use std::os::unix::fs::PermissionsExt;
    struct Sink {
        out: std::io::BufWriter<std::fs::File>,
        h: sha256::Hasher,
    }
    impl Write for Sink {
        fn write(&mut self, b: &[u8]) -> std::io::Result<usize> {
            self.out.write_all(b)?;
            self.h.update(b);
            Ok(b.len())
        }
        fn flush(&mut self) -> std::io::Result<()> {
            self.out.flush()
        }
    }
    let file = std::fs::File::create(part).map_err(|e| download(e.to_string()))?;
    let mut sink = Sink { out: std::io::BufWriter::with_capacity(1 << 20, file), h: sha256::Hasher::new() };
    let len = get_to(url, cap, proxies, &mut sink).map_err(download)?;
    if let Some(n) = size {
        if len < n {
            return Err(download(format!("download of {url} stopped at {len} of {n} bytes")));
        }
    }
    let Sink { out, h } = sink;
    let got = h.hexdigest();
    if got != digest {
        return Err(FetchError { verify: true, msg: format!("sha256 {got}, not the pinned {digest}") });
    }
    let file = out.into_inner().map_err(|e| download(e.to_string()))?;
    file.sync_all().map_err(|e| download(e.to_string()))?;
    std::fs::set_permissions(part, std::fs::Permissions::from_mode(0o644)).map_err(|e| download(e.to_string()))
}

/// `models.ensure_moonshine`: the directory of a curated model with every
/// file checked, the stale ones fetched after one line through `say`.
pub fn ensure_moonshine(name: &str, env: &ModelEnv, say: &mut dyn FnMut(&str)) -> Result<String, FetchError> {
    let dir = moonshine_dir(name, env);
    let files = files_of(name).unwrap_or(&[]);
    let stale: Vec<&MoonshineFile> =
        files.iter().filter(|f| sha256_of(&format!("{dir}/{}", f.name)).as_deref() != Some(f.sha256)).collect();
    if stale.is_empty() {
        return Ok(dir);
    }
    say(&format!("Fetching the Moonshine {name} model ({} MB) into {dir}. This happens once.", moonshine_size_mb(name)));
    for f in stale {
        fetch_file(&moonshine_url(name, f.name, env), f.sha256, &format!("{dir}/{}", f.name), Some(f.size), env)?;
    }
    Ok(dir)
}

pub(super) fn unavailable(msg: String) -> EngineError {
    EngineError { unknown: false, loading: false, msg }
}

/// `engines.MoonshineEngine(model)`: moonshine-cli on PATH, then a curated
/// model fetched once, or a model directory.
pub fn new_moonshine(model: &str, env: &ModelEnv, say: &mut dyn FnMut(&str)) -> Result<Engine, EngineError> {
    let Some(binary) = which(MOONSHINE_BINARY) else {
        return Err(unavailable(format!(
            "{MOONSHINE_BINARY} is not on PATH. Build it with clients/go/scripts/native.sh --moonshine (scripts/install.sh does this and puts it on PATH), then try again."
        )));
    };
    if files_of(model).is_some() {
        return match ensure_moonshine(model, env, say) {
            Ok(dir) => {
                let argv = moonshine_args(&binary, &dir, model);
                Ok(Engine::resident("moonshine", binary, dir, model.to_string(), argv, env.timeouts))
            }
            Err(e) if e.verify => Err(unavailable(format!(
                "A file of the Moonshine {model} model did not match its pinned sha256 and was deleted. Run daisugi voice serve again to fetch it again."
            ))),
            Err(_) => Err(unavailable(format!(
                "The Moonshine {model} model could not be fetched. Check the network, then run daisugi voice serve again."
            ))),
        };
    }
    let dir = py_path_str(model);
    if !std::fs::metadata(&dir).map(|m| m.is_dir()).unwrap_or(false) {
        return Err(unavailable(format!(
            "Moonshine model directory missing: {dir}. Set voice_model to tiny, small or medium, or to a Moonshine streaming model directory."
        )));
    }
    let argv = moonshine_args(&binary, &dir, MOONSHINE_DIR_ARCH);
    Ok(Engine::resident("moonshine", binary, dir, MOONSHINE_DIR_ARCH.to_string(), argv, env.timeouts))
}

/// The files in `dir` with their digests, a partial download left out,
/// for the probe.
pub fn fetch_listing(dir: &str) -> Vec<(String, String)> {
    let Ok(rd) = std::fs::read_dir(dir) else { return vec![] };
    let mut names: Vec<String> = rd.flatten().map(|e| e.file_name().to_string_lossy().into_owned()).collect();
    names.sort();
    names
        .into_iter()
        .filter(|n| !n.contains(".part"))
        .filter_map(|n| sha256_of(&format!("{dir}/{n}")).map(|d| (n, d)))
        .collect()
}

#[cfg(test)]
mod fetch_tests {
    use super::*;
    use std::io::{Read, Write};

    fn serve(replies: Vec<Vec<u8>>) -> u16 {
        let l = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let port = l.local_addr().unwrap().port();
        std::thread::spawn(move || {
            for reply in replies {
                let (mut s, _) = l.accept().unwrap();
                let mut buf = [0u8; 4096];
                let mut req = vec![];
                while !req.windows(4).any(|w| w == b"\r\n\r\n") {
                    let n = s.read(&mut buf).unwrap();
                    if n == 0 {
                        break;
                    }
                    req.extend_from_slice(&buf[..n]);
                }
                let _ = s.write_all(&reply);
            }
        });
        port
    }

    /// A model file is streamed to disk and hashed on the way: the file
    /// lands whole and checked, a wrong digest leaves nothing behind, and a
    /// short body is a download error.
    #[test]
    fn fetch_file_streams_to_disk_and_checks() {
        let body: Vec<u8> = (0..3_000_000u32).map(|i| (i % 253) as u8).collect();
        let sum = sha256::hexdigest(&body);
        let mut reply = format!("HTTP/1.1 200 OK\r\ncontent-length: {}\r\n\r\n", body.len()).into_bytes();
        reply.extend_from_slice(&body);
        let short = b"HTTP/1.1 200 OK\r\ncontent-length: 3\r\n\r\nabc".to_vec();
        let port = serve(vec![reply.clone(), reply, short]);
        // Under target/, on disk: /tmp is RAM on some boxes.
        let dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(format!("target/fetch-file-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        let d = dir.to_string_lossy().into_owned();
        let env = ModelEnv { vars: HashMap::new(), home: "/nonexistent".into(), hardware: None, timeouts: Default::default() };
        let dest = format!("{d}/m.gguf");
        let url = format!("http://127.0.0.1:{port}/m.gguf");
        fetch_file(&url, &sum, &dest, Some(body.len() as u64), &env).unwrap();
        assert_eq!(sha256_of(&dest).as_deref(), Some(sum.as_str()));
        assert_eq!(std::fs::metadata(&dest).unwrap().len(), body.len() as u64);
        let other = format!("{d}/n.gguf");
        let e = fetch_file(&url, &"0".repeat(64), &other, Some(body.len() as u64), &env).unwrap_err();
        assert!(e.verify, "{}", e.msg);
        assert!(e.msg.starts_with(&format!("sha256 {sum}, not the pinned")), "{}", e.msg);
        let e = fetch_file(&url, &sum, &other, Some(body.len() as u64), &env).unwrap_err();
        assert!(!e.verify);
        assert_eq!(e.msg, format!("download of {url} stopped at 3 of {} bytes", body.len()));
        let left: Vec<_> = std::fs::read_dir(&dir).unwrap().map(|e| e.unwrap().file_name().into_string().unwrap()).collect();
        assert_eq!(left, vec!["m.gguf".to_string()]);
        let _ = std::fs::remove_dir_all(&dir);
    }

    /// A killed download leaves `name.part-<random>`. A new download of that
    /// file removes the ones older than a day, and only those.
    #[test]
    fn sweep_removes_only_stale_part_files() {
        let dir = std::path::PathBuf::from(env!("CARGO_MANIFEST_DIR")).join(format!("target/sweep-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let write = |name: &str, age_h: u64| {
            let p = dir.join(name);
            std::fs::write(&p, b"orphan").unwrap();
            let when = std::time::SystemTime::now() - std::time::Duration::from_secs(age_h * 3600);
            std::fs::File::options().write(true).open(&p).unwrap().set_modified(when).unwrap();
            p
        };
        let old = write("m.gguf.part-111", 25);
        let fresh = write("m.gguf.part-222", 1);
        let other = write("n.gguf.part-333", 48);
        let body = b"model bytes".to_vec();
        let mut reply = format!("HTTP/1.1 200 OK\r\ncontent-length: {}\r\n\r\n", body.len()).into_bytes();
        reply.extend_from_slice(&body);
        let port = serve(vec![reply]);
        let env = ModelEnv { vars: HashMap::new(), home: "/nonexistent".into(), hardware: None, timeouts: Default::default() };
        let dest = dir.join("m.gguf").to_string_lossy().into_owned();
        let _ = std::fs::remove_file(&dest);
        let url = format!("http://127.0.0.1:{port}/m.gguf");
        fetch_file(&url, &sha256::hexdigest(&body), &dest, Some(body.len() as u64), &env).unwrap();
        assert!(!old.exists(), "the stale orphan stays");
        assert!(fresh.exists() && other.exists());
        let _ = std::fs::remove_dir_all(&dir);
    }
}
