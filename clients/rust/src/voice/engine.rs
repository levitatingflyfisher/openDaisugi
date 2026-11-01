//! `voice/pins.py` and `voice/engines.py`: the pinned facts, the
//! whisper.cpp engine (whisper-cli run once per clip), the resident
//! engines (moonshine-cli and parakeet-cli, one child each) and the engine
//! a config means.

use std::time::{Duration, Instant};

use super::audio::{run_with_input, which};
use super::wav::wav_duration_s;
use crate::warden::py::text;

pub const FASTER_WHISPER_MODEL_NAMES: &[&str] = &[
    "base", "base.en", "distil-large-v3", "large-v2", "large-v3", "medium", "medium.en", "small", "small.en", "tiny",
    "tiny.en", "turbo",
];
pub const FASTER_WHISPER_TEST_MODEL: &str = "tiny.en";
pub const FIXTURES: &[(&str, &str, f64, f64)] = &[
    ("fixture_a.wav", "the quick brown fox jumps over the lazy dog", 0.111, 0.15),
    ("fixture_b.wav", "please remember to buy milk after work", 0.0, 0.10),
    ("fixture_c.wav", "what is the weather like today", 0.0, 0.10),
];
pub const FIXTURE_MEAN_MAX_WER: f64 = 0.15;
pub const WHISPER_CPP_BINARY: &str = "whisper-cli";
pub const WHISPER_CPP_AUTO_LANGUAGE: &str = "auto";
pub const WHISPER_CPP_TIMEOUT_S: f64 = 120.0;

/// A speech engine that cannot be built: `unknown` is UnknownEngine (exit
/// 1 from voice serve), `loading` is a resident engine loading its model
/// (EngineLoading, 503 engine_loading), otherwise EngineUnavailable (exit
/// 3, or 503 engine_unavailable for a clip).
#[derive(Debug, Clone)]
pub struct EngineError {
    pub unknown: bool,
    pub loading: bool,
    pub msg: String,
}

impl EngineError {
    pub fn class(&self) -> &'static str {
        if self.unknown {
            "UnknownEngine"
        } else if self.loading {
            "EngineLoading"
        } else {
            "EngineUnavailable"
        }
    }
}

/// `engines.whisper_cpp_args`.
pub fn whisper_cpp_args(binary: &str, model: &str, wav_path: &str, language: Option<&str>) -> Vec<String> {
    let lang = match language {
        Some(l) if !l.is_empty() => l,
        _ => WHISPER_CPP_AUTO_LANGUAGE,
    };
    [binary, "-m", model, "-f", wav_path, "-l", lang, "-nt", "-np"].iter().map(|s| s.to_string()).collect()
}

/// `engines.whisper_cpp_text`: each line stripped, blank ones dropped,
/// joined by one space.
pub fn whisper_cpp_text(stdout: &str) -> String {
    text::splitlines(stdout).into_iter().map(text::strip).filter(|t| !t.is_empty()).collect::<Vec<_>>().join(" ")
}

/// `str(Path(p))`: repeated slashes and "." parts dropped.
pub fn py_path_str(p: &str) -> String {
    if p.is_empty() {
        return ".".into();
    }
    let lead = if p.starts_with("//") && !p.starts_with("///") {
        "//"
    } else if p.starts_with('/') {
        "/"
    } else {
        ""
    };
    let parts: Vec<&str> = p.split('/').filter(|s| !s.is_empty() && *s != ".").collect();
    let out = format!("{lead}{}", parts.join("/"));
    if out.is_empty() {
        ".".into()
    } else {
        out
    }
}

static BATCH_SEQ: std::sync::atomic::AtomicUsize = std::sync::atomic::AtomicUsize::new(0);

pub struct Transcript {
    pub text: String,
    pub duration_s: f64,
    pub rtf: f64,
    /// The text of each window of a long clip, in order. A window the engine
    /// could not read is `GAP_MARKER`. Empty for a clip of one window.
    pub parts: Vec<String>,
    /// How many windows the engine could not read.
    pub gaps: usize,
}

/// Stands in the text for a window the engine could not read, so the owner
/// sees where words are missing.
pub const GAP_MARKER: &str = "[gap in the recording]";

/// `whisper_cpp_args` for many windows in one run: the model loads once, and
/// each window's text is written beside its file as `<file>.txt`.
pub fn whisper_cpp_batch_args(binary: &str, model: &str, wav_paths: &[String], language: Option<&str>) -> Vec<String> {
    let lang = match language {
        Some(l) if !l.is_empty() => l,
        _ => WHISPER_CPP_AUTO_LANGUAGE,
    };
    let mut args = vec![binary.to_string(), "-m".into(), model.to_string()];
    for p in wav_paths {
        args.push("-f".into());
        args.push(p.clone());
    }
    for a in ["-l", lang, "-nt", "-np", "-otxt"] {
        args.push(a.to_string());
    }
    args
}

/// Window texts joined in order by one space; an empty window adds nothing.
pub fn join_texts(parts: &[String]) -> String {
    parts.iter().map(|p| p.trim()).filter(|p| !p.is_empty()).collect::<Vec<_>>().join(" ")
}

/// An external speech engine: whisper-cli (whisper.cpp), run once per
/// clip, or a resident child (moonshine-cli or parakeet-cli) that loads
/// its model once.
pub struct Engine {
    pub name: &'static str,
    binary: String,
    model: String,
    /// The Moonshine architecture; empty for the others.
    pub arch: String,
    /// The resident child's command line; empty for whisper.cpp.
    pub argv: Vec<String>,
    res: Option<super::resident::Resident>,
}

/// The resident child's timeouts in seconds (0: the defaults). Only the
/// probe sets them.
#[derive(Clone, Copy, Default)]
pub struct Timeouts {
    pub load_s: f64,
    pub clip_s: f64,
}

impl Engine {
    /// A resident engine: `name` is moonshine or parakeet.
    pub fn resident(name: &'static str, binary: String, model: String, arch: String, argv: Vec<String>, t: Timeouts) -> Engine {
        let bin_name = if name == "moonshine" { super::moonshine::MOONSHINE_BINARY } else { super::parakeet::PARAKEET_BINARY };
        let res = super::resident::Resident::new(super::resident::ResidentConfig {
            binary: bin_name.to_string(),
            argv: argv.clone(),
            env: vec![],
            load_timeout_s: if t.load_s > 0.0 { t.load_s } else { super::resident::LOAD_TIMEOUT_S },
            clip_timeout_s: if t.clip_s > 0.0 { t.clip_s } else { super::resident::CLIP_TIMEOUT_S },
        });
        Engine { name, binary, model, arch, argv, res: Some(res) }
    }

    /// Transcribes a clip of any length the caller allows: the windows from
    /// `split_wav` in order, the texts joined by one space. The duration is
    /// the clip's; the real-time factor is all the engine time over all the
    /// audio. whisper.cpp reads all windows in one run, so its model loads
    /// once. A window that fails does not cost the others: its place in the
    /// text is `GAP_MARKER`. The call fails only when no window was read.
    pub fn transcribe_long(&self, wav: &[u8], language: Option<&str>) -> Result<Transcript, TranscribeError> {
        let pieces = super::wav::split_wav(wav).map_err(|e| TranscribeError::Other(e.msg))?;
        if pieces.len() == 1 {
            let mut t = self.transcribe(&pieces[0], language)?;
            t.parts = vec![t.text.clone()];
            return Ok(t);
        }
        let start = Instant::now();
        let total = wav_duration_s(wav).map_err(|e| TranscribeError::Other(e.msg))?;
        let mut texts: Vec<String> = Vec::new();
        let mut read: Vec<bool> = Vec::new();
        let mut failure: Option<TranscribeError> = None;
        if self.res.is_none() {
            match self.run_batch(&pieces, language) {
                Ok((t, r, f)) => {
                    texts = t;
                    read = r;
                    failure = f.map(TranscribeError::Other);
                }
                Err(e) => return Err(TranscribeError::Other(e)),
            }
        } else {
            for p in &pieces {
                match self.transcribe(p, language) {
                    Ok(t) => {
                        texts.push(t.text);
                        read.push(true);
                    }
                    Err(e) => {
                        failure.get_or_insert(e);
                        texts.push(String::new());
                        read.push(false);
                    }
                }
            }
        }
        if !read.iter().any(|r| *r) {
            return Err(failure.unwrap_or_else(|| TranscribeError::Other("the engine read no window".into())));
        }
        let parts: Vec<String> = texts.into_iter().zip(&read).map(|(t, ok)| if *ok { t } else { GAP_MARKER.to_string() }).collect();
        let gaps = read.iter().filter(|r| !**r).count();
        let rtf = if total > 0.0 { start.elapsed().as_secs_f64() / total } else { 0.0 };
        Ok(Transcript { text: join_texts(&parts), duration_s: total, rtf, parts, gaps })
    }

    /// One whisper-cli run over all windows. Each window's text and whether
    /// it was read (it has a `.txt` file); the last value is the failure of
    /// the run, when it failed. Windows that finished keep their text.
    fn run_batch(&self, pieces: &[Vec<u8>], language: Option<&str>) -> Result<(Vec<String>, Vec<bool>, Option<String>), String> {
        let dir = std::env::temp_dir().join(format!("daisugi-voice-{}-{}", std::process::id(), BATCH_SEQ.fetch_add(1, std::sync::atomic::Ordering::Relaxed)));
        std::fs::create_dir_all(&dir).map_err(|e| e.to_string())?;
        let mut paths = Vec::new();
        for (i, p) in pieces.iter().enumerate() {
            let path = dir.join(format!("w{i:04}.wav"));
            std::fs::write(&path, p).map_err(|e| e.to_string())?;
            paths.push(path.display().to_string());
        }
        let args = whisper_cpp_batch_args(&self.binary, &self.model, &paths, language);
        let rest: Vec<&str> = args[1..].iter().map(|s| s.as_str()).collect();
        let bin = WHISPER_CPP_BINARY;
        let ran = run_with_input(&args[0], &rest, None, Duration::from_secs_f64(WHISPER_CPP_TIMEOUT_S * pieces.len() as f64));
        let failure = match &ran {
            Err(e) => Some(format!("{bin} {e}")),
            Ok((code, _, err)) if *code != Some(0) => {
                let err_text = String::from_utf8_lossy(err).to_string();
                let lines = text::splitlines(text::strip(&err_text));
                let last = lines.last().map(|l| text::head(text::strip(l), 200).to_string()).unwrap_or_default();
                Some(format!("{bin} exited {}: {last}", code.unwrap_or(-1)))
            }
            _ => None,
        };
        let mut texts = Vec::new();
        let mut read = Vec::new();
        for p in &paths {
            match std::fs::read(format!("{p}.txt")) {
                Ok(raw) => {
                    texts.push(whisper_cpp_text(&String::from_utf8_lossy(&raw)));
                    read.push(true);
                }
                Err(_) => {
                    texts.push(String::new());
                    read.push(false);
                }
            }
        }
        let _ = std::fs::remove_dir_all(&dir);
        Ok((texts, read, failure))
    }

    /// The model file or directory the engine runs, as `str(Path)`.
    pub fn model(&self) -> &str {
        &self.model
    }

    /// Starts a resident engine's child and waits for its model; nothing
    /// for whisper.cpp.
    pub fn start(&self) -> Result<(), EngineError> {
        match &self.res {
            Some(r) => r.start(),
            None => Ok(()),
        }
    }

    /// Stops a resident engine's child.
    pub fn stop(&self) {
        if let Some(r) = &self.res {
            r.stop();
        }
    }

    /// A handle that stops the resident child after the engine has moved
    /// into the server.
    pub fn stop_handle(&self) -> Option<super::resident::Resident> {
        self.res.clone()
    }

    /// Waits while a resident child loads (for the probe).
    pub fn wait_ready(&self, timeout_s: f64) -> &'static str {
        match &self.res {
            Some(r) => r.wait_ready(timeout_s),
            None => "ready",
        }
    }
}

/// `engines.WhisperCppEngine(model)`.
pub fn new_whisper_cpp(model: &str) -> Result<Engine, EngineError> {
    let Some(binary) = which(WHISPER_CPP_BINARY) else {
        return Err(EngineError {
            unknown: false,
            loading: false,
            msg: format!("{WHISPER_CPP_BINARY} is not on PATH. Build whisper.cpp and put {WHISPER_CPP_BINARY} on PATH, then try again."),
        });
    };
    if !std::fs::metadata(model).map(|m| m.is_file()).unwrap_or(false) {
        return Err(EngineError {
            unknown: false,
            loading: false,
            msg: format!(
                "whisper.cpp model file missing: {}. Download a ggml model, then set voice_model to its path.",
                py_path_str(model)
            ),
        });
    }
    Ok(Engine { name: "whisper.cpp", binary, model: model.to_string(), arch: String::new(), argv: vec![], res: None })
}

/// `engines.VALID_ENGINE_NAMES`.
pub const VALID_ENGINE_NAMES: &str = "faster-whisper, moonshine, parakeet, whisper.cpp";

/// The Config defaults, which save_config writes without a choice.
pub const DEFAULT_ENGINE: &str = "faster-whisper";
pub const DEFAULT_MODEL: &str = "tiny.en";

/// What this binary says where a config names faster-whisper, which only
/// the Python build can load (ruling VO-1).
pub const PORT_FASTER_WHISPER: &str =
    "faster-whisper needs the Python build of daisugi. Set voice_engine: moonshine to use Moonshine instead.";

/// `engines.installed_engines` for this binary: the resident programs on
/// PATH; faster-whisper never imports here.
pub fn installed_engines() -> Vec<&'static str> {
    let mut out = vec![];
    if which(super::parakeet::PARAKEET_BINARY).is_some() {
        out.push("parakeet");
    }
    if which(super::moonshine::MOONSHINE_BINARY).is_some() {
        out.push("moonshine");
    }
    out
}

/// `engines.resolve_engine`: the engine and model a config means. With the
/// voice settings unset or at their defaults, the engine the hardware
/// picks, after one line through `say`.
pub fn resolve_engine(
    engine: &str,
    model: &str,
    env: &super::moonshine::ModelEnv,
    say: &mut dyn FnMut(&str),
) -> (String, String) {
    if engine == DEFAULT_ENGINE && model == DEFAULT_MODEL {
        let (e, m, line) = super::hardware::choose_engine(
            &env.detect_hardware(),
            &installed_engines(),
            false,
            super::parakeet::parakeet_usable(super::parakeet::PARAKEET_DEFAULT_MODEL, env, std::env::consts::ARCH),
        );
        say(&line);
        return (e.to_string(), m.to_string());
    }
    if engine == "moonshine" && model == DEFAULT_MODEL {
        return ("moonshine".into(), super::moonshine::MOONSHINE_DEFAULT_MODEL.into());
    }
    if engine == "parakeet" && model == DEFAULT_MODEL {
        return ("parakeet".into(), super::parakeet::PARAKEET_DEFAULT_MODEL.into());
    }
    (engine.to_string(), model.to_string())
}

/// `engines.pick_engine` for this binary: whisper.cpp, Moonshine and
/// Parakeet run here; faster-whisper is a Python package (ruling VO-1).
/// The engine is not started.
pub fn pick_engine(
    engine: &str,
    model: &str,
    env: &super::moonshine::ModelEnv,
    say: &mut dyn FnMut(&str),
) -> Result<Engine, EngineError> {
    let (engine, model) = resolve_engine(engine, model, env, say);
    let engine = engine.as_str();
    match engine {
        "whisper.cpp" => new_whisper_cpp(&py_path_str(&model)),
        "moonshine" => super::moonshine::new_moonshine(&model, env, say),
        "parakeet" => super::parakeet::new_parakeet(&model, env, say),
        "faster-whisper" => Err(EngineError { unknown: false, loading: false, msg: PORT_FASTER_WHISPER.to_string() }),
        _ => Err(EngineError {
            unknown: true,
            loading: false,
            msg: format!("Unknown voice_engine {}. Valid names: {VALID_ENGINE_NAMES}.", text::repr(engine)),
        }),
    }
}

/// A temp file name as tempfile.mkstemp makes one: in TMPDIR, private.
fn temp_wav(data: &[u8]) -> std::io::Result<String> {
    use std::io::Write;
    use std::os::unix::fs::OpenOptionsExt;
    let dir = std::env::var("TMPDIR").ok().filter(|d| !d.is_empty()).unwrap_or_else(|| "/tmp".into());
    let seed = std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_nanos()).unwrap_or(0);
    for k in 0..100u128 {
        let name = format!("{dir}/daisugi-voice-{:x}{:x}.wav", std::process::id(), seed.wrapping_add(k * 7919) & 0xffffffff);
        match std::fs::OpenOptions::new().write(true).create_new(true).mode(0o600).open(&name) {
            Ok(mut f) => {
                f.write_all(data)?;
                return Ok(name);
            }
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => return Err(e),
        }
    }
    Err(std::io::Error::other("no temp name was free"))
}

/// A clip that was not transcribed: an EngineError (loading or
/// unavailable, a 503 from the server) or any other failure (a 500).
pub enum TranscribeError {
    Engine(EngineError),
    Other(String),
}

impl Engine {
    /// Runs the engine on one 16 kHz mono WAV. A resident engine sends it
    /// to its child (its models are English and take no language);
    /// whisper.cpp runs once through a temp file that is removed after, and
    /// a failed run names the exit code and the last stderr line.
    pub fn transcribe(&self, wav: &[u8], language: Option<&str>) -> Result<Transcript, TranscribeError> {
        let duration = wav_duration_s(wav).map_err(|e| TranscribeError::Other(e.msg))?;
        if let Some(r) = &self.res {
            let start = Instant::now();
            let raw = r.transcribe_text(wav).map_err(|e| match e {
                Ok(ee) => TranscribeError::Engine(ee),
                Err(msg) => TranscribeError::Other(msg),
            })?;
            let elapsed = start.elapsed().as_secs_f64();
            let rtf = if duration > 0.0 { elapsed / duration } else { 0.0 };
            return Ok(Transcript { text: whisper_cpp_text(&raw), duration_s: duration, rtf, parts: vec![], gaps: 0 });
        }
        self.run_once(wav, language, duration).map_err(TranscribeError::Other)
    }

    fn run_once(&self, wav: &[u8], language: Option<&str>, duration: f64) -> Result<Transcript, String> {
        let path = temp_wav(wav).map_err(|e| e.to_string())?;
        let args = whisper_cpp_args(&self.binary, &self.model, &path, language);
        let bin = WHISPER_CPP_BINARY;
        let rest: Vec<&str> = args[1..].iter().map(|s| s.as_str()).collect();
        let start = Instant::now();
        let ran = run_with_input(&args[0], &rest, None, Duration::from_secs_f64(WHISPER_CPP_TIMEOUT_S));
        let elapsed = start.elapsed().as_secs_f64();
        let _ = std::fs::remove_file(&path);
        let (code, out, err) = ran.map_err(|e| format!("{bin} {e}"))?;
        if code != Some(0) {
            let err_text = String::from_utf8_lossy(&err).to_string();
            let lines = text::splitlines(text::strip(&err_text));
            let last = lines.last().map(|l| text::head(text::strip(l), 200).to_string()).unwrap_or_default();
            return Err(format!("{bin} exited {}: {last}", code.unwrap_or(-1)));
        }
        let t = whisper_cpp_text(&String::from_utf8_lossy(&out));
        let rtf = if duration > 0.0 { elapsed / duration } else { 0.0 };
        Ok(Transcript { text: t, duration_s: duration, rtf, parts: vec![], gaps: 0 })
    }
}

#[cfg(test)]
mod long_tests {
    use super::*;
    use crate::voice::wav::{split_wav, write_wav_pcm};

    fn tone(seconds: usize, pause_at: usize, pause_len: usize) -> Vec<u8> {
        let n = seconds * 16000;
        let mut data = Vec::with_capacity(2 * n);
        for i in 0..n {
            let v: i16 = if i >= pause_at && i < pause_at + pause_len { 0 } else { (8000.0 * (i as f64 / 7.0).sin()) as i16 };
            data.extend_from_slice(&v.to_le_bytes());
        }
        write_wav_pcm(&data, 16000).unwrap()
    }

    #[test]
    fn truncate_keeps_the_first_seconds_and_counts_the_cut_rounded_up() {
        use crate::voice::wav::truncate_wav;
        let mut data = vec![0u8; 2 * (16000 * 10 + 8000)];
        data[0] = 1;
        let w = write_wav_pcm(&data, 16000).unwrap();
        let (kept, cut) = truncate_wav(&w, 10.0).unwrap();
        assert_eq!(wav_duration_s(&kept).unwrap(), 10.0);
        assert_eq!(cut, 1);
        let (same, none) = truncate_wav(&kept, 10.0).unwrap();
        assert_eq!(same, kept);
        assert_eq!(none, 0);
    }

    #[test]
    fn a_short_clip_stays_one_window_unchanged() {
        let w = tone(30, 0, 0);
        let got = split_wav(&w).unwrap();
        assert_eq!(got.len(), 1);
        assert_eq!(got[0], w);
    }

    #[test]
    fn cuts_fall_in_the_pause_near_the_boundary() {
        let w = tone(65, 28 * 16000, 3200);
        let got = split_wav(&w).unwrap();
        assert_eq!(got.len(), 3);
        let mut total = 0.0;
        for g in &got {
            let d = wav_duration_s(g).unwrap();
            assert!(d <= 30.0 + 1e-9);
            total += d;
        }
        assert!((total - 65.0).abs() < 1e-6);
        let d0 = wav_duration_s(&got[0]).unwrap();
        assert!((28.0..=28.2).contains(&d0), "first cut at {d0}");
    }

    /// A whisper-cli stand-in: for each -f file it counts a call, and writes
    /// "window N" to <file>.txt when -otxt is given. A number listed in
    /// <counter>.fail is skipped, as whisper-cli skips a file it cannot read.
    fn fake_whisper_body(counter: &std::path::Path) -> String {
        format!(
            "#!/bin/sh\nfiles=\"\"; txt=0; prev=\"\"\nfor a in \"$@\"; do\n  if [ \"$prev\" = \"-f\" ]; then files=\"$files $a\"; fi\n  if [ \"$a\" = \"-otxt\" ]; then txt=1; fi\n  prev=\"$a\"\ndone\nruns=$(cat {c}.runs 2>/dev/null || echo 0); echo $((runs+1)) > {c}.runs\nfor f in $files; do\n  n=$(cat {c} 2>/dev/null || echo 0)\n  n=$((n+1))\n  echo $n > {c}\n  if grep -qx $n {c}.fail 2>/dev/null; then continue; fi\n  if [ $txt = 1 ]; then echo \"window $n\" > \"$f.txt\"; else echo \"window $n\"; fi\ndone\n",
            c = counter.display()
        )
    }

    fn engine_in(dir: &std::path::Path) -> Engine {
        use std::os::unix::fs::PermissionsExt;
        let script = dir.join("whisper-cli");
        std::fs::write(&script, fake_whisper_body(&dir.join("count"))).unwrap();
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        Engine {
            name: "whisper.cpp",
            binary: script.display().to_string(),
            model: dir.join("m.bin").display().to_string(),
            arch: String::new(),
            argv: vec![],
            res: None,
        }
    }

    #[test]
    fn a_long_clip_runs_the_engine_once() {
        let dir = std::env::temp_dir().join(format!("long-once-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let eng = engine_in(&dir);
        eng.transcribe_long(&tone(180, 0, 0), None).ok().unwrap();
        let runs = std::fs::read_to_string(dir.join("count.runs")).unwrap();
        assert_eq!(runs.trim(), "1");
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn a_failed_window_keeps_the_others_and_marks_the_gap() {
        let dir = std::env::temp_dir().join(format!("long-gap-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("count.fail"), "2\n").unwrap();
        let eng = engine_in(&dir);
        let t = eng.transcribe_long(&tone(100, 0, 0), None).ok().unwrap();
        assert_eq!(t.text, "window 1 [gap in the recording] window 3 window 4");
        assert_eq!(t.gaps, 1);
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn three_minutes_give_the_text_of_all_windows_in_order() {
        let dir = std::env::temp_dir().join(format!("long-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&dir);
        std::fs::create_dir_all(&dir).unwrap();
        let script = dir.join("whisper-cli");
        let counter = dir.join("count");
        std::fs::write(&script, fake_whisper_body(&counter)).unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        let eng = Engine {
            name: "whisper.cpp",
            binary: script.display().to_string(),
            model: dir.join("m.bin").display().to_string(),
            arch: String::new(),
            argv: vec![],
            res: None,
        };
        let t = eng.transcribe_long(&tone(180, 0, 0), None).ok().unwrap();
        let words: Vec<&str> = t.text.split_whitespace().collect();
        assert!(words.len() >= 12 && words.len() % 2 == 0, "{}", t.text);
        for i in 0..words.len() / 2 {
            assert_eq!(words[2 * i], "window");
            assert_eq!(words[2 * i + 1], (i + 1).to_string());
        }
        assert!((t.duration_s - 180.0).abs() < 1e-6);
        let _ = std::fs::remove_dir_all(&dir);
    }
}
