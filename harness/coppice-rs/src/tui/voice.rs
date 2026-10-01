//! A voice clip: the recorder that writes it, the voice server that hears
//! it, and the cleaning of what it heard.

use std::path::{Path, PathBuf};
use std::process::{Child, Command, Stdio};
use std::time::{Duration, SystemTime};

use serde_json::Value;

use super::run::call_within;

/// The programs the talk key records with, in the order it looks.
fn recorder_args(name: &str, p: &str) -> Vec<String> {
    let v: &[&str] = match name {
        "pw-record" => &["--rate", "16000", "--channels", "1", "--format", "s16"],
        "parecord" => &[
            "--rate=16000",
            "--channels=1",
            "--format=s16le",
            "--file-format=wav",
        ],
        _ => &["-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "wav"],
    };
    let mut out: Vec<String> = v.iter().map(|s| s.to_string()).collect();
    out.push(p.to_string());
    out
}

const RECORDERS: &[&str] = &["pw-record", "parecord", "arecord"];

/// The line the floor shows when no recorder is on PATH.
pub const NO_RECORDER: &str = "Voice needs a recorder: pw-record, parecord or arecord. Install pipewire-utils (Fedora), pipewire-bin (Debian, Ubuntu), pulseaudio-utils or alsa-utils; check with: command -v pw-record parecord arecord";

/// The path and the arguments of the first recorder on PATH.
pub fn find_recorder(clip_path: &str) -> Option<(String, Vec<String>)> {
    for r in RECORDERS {
        if let Some(p) = crate::pane::look_path(r) {
            return Some((p, recorder_args(r, clip_path)));
        }
    }
    None
}

/// One recording in progress.
pub struct Clip {
    child: Child,
    path: PathBuf,
}

/// How old a clip file must be before a new clip deletes it.
const STALE_CLIP: Duration = Duration::from_secs(600);

/// Starts the first recorder on PATH, writing to a new file under
/// dir/voice.
pub fn start_clip(dir: &str) -> Result<Clip, String> {
    if dir.is_empty() {
        return Err("Voice needs the coppice data directory, and this floor has none".into());
    }
    let voice_dir = Path::new(dir).join("voice");
    {
        use std::os::unix::fs::DirBuilderExt;
        if let Err(e) = std::fs::DirBuilder::new()
            .recursive(true)
            .mode(0o700)
            .create(&voice_dir)
        {
            return Err(format!(
                "Voice cannot make {}: {}",
                voice_dir.display(),
                crate::sys::go_path_err("mkdir", &voice_dir, &e)
            ));
        }
    }
    if let Ok(rd) = std::fs::read_dir(&voice_dir) {
        for e in rd.flatten() {
            let name = e.file_name().to_string_lossy().into_owned();
            if !(name.starts_with("clip-") && name.ends_with(".wav")) {
                continue;
            }
            if let Ok(md) = e.metadata() {
                let old = md
                    .modified()
                    .ok()
                    .and_then(|t| SystemTime::now().duration_since(t).ok())
                    .is_some_and(|d| d > STALE_CLIP);
                if old {
                    let _ = std::fs::remove_file(e.path());
                }
            }
        }
    }
    let path = create_clip_file(&voice_dir)?;
    let ps = path.to_string_lossy().into_owned();
    let Some((bin, args)) = find_recorder(&ps) else {
        let _ = std::fs::remove_file(&path);
        return Err(NO_RECORDER.into());
    };
    let mut cmd = Command::new(&bin);
    cmd.args(&args)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null());
    {
        use std::os::unix::process::CommandExt;
        // SAFETY: prctl in the child before exec only sets its death signal.
        unsafe {
            cmd.pre_exec(|| {
                libc::prctl(libc::PR_SET_PDEATHSIG, libc::SIGTERM);
                Ok(())
            });
        }
    }
    match cmd.spawn() {
        Ok(child) => Ok(Clip { child, path }),
        Err(e) => {
            let _ = std::fs::remove_file(&path);
            let base = Path::new(&bin)
                .file_name()
                .map(|s| s.to_string_lossy().into_owned())
                .unwrap_or(bin.clone());
            Err(format!(
                "{base} did not start: {}",
                crate::sys::go_path_err("fork/exec", Path::new(&bin), &e)
            ))
        }
    }
}

fn create_clip_file(dir: &Path) -> Result<PathBuf, String> {
    use std::os::unix::fs::OpenOptionsExt;
    for _ in 0..10000 {
        let n = crate::web::token::random_bytes(4)
            .map(|b| u32::from_le_bytes([b[0], b[1], b[2], b[3]]))
            .unwrap_or(0);
        let p = dir.join(format!("clip-{n}.wav"));
        match std::fs::OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(0o600)
            .open(&p)
        {
            Ok(_) => return Ok(p),
            Err(e) if e.kind() == std::io::ErrorKind::AlreadyExists => continue,
            Err(e) => {
                return Err(format!(
                    "Voice cannot write a clip in {}: {}",
                    dir.display(),
                    crate::sys::go_path_err("createtemp", &dir.join("clip-*.wav"), &e)
                ))
            }
        }
    }
    Err(format!("Voice cannot write a clip in {}", dir.display()))
}

impl Clip {
    /// Interrupts the recorder so it finishes the WAV header, and kills it
    /// when it has not ended within two seconds.
    fn end(&mut self) {
        // SAFETY: signals our own child.
        unsafe {
            libc::kill(self.child.id() as i32, libc::SIGINT);
        }
        let deadline = std::time::Instant::now() + Duration::from_secs(2);
        loop {
            if let Ok(Some(_)) = self.child.try_wait() {
                return;
            }
            if std::time::Instant::now() >= deadline {
                let _ = self.child.kill();
                let _ = self.child.wait();
                return;
            }
            std::thread::sleep(Duration::from_millis(20));
        }
    }

    /// Ends the recording and returns the clip's bytes.
    pub fn stop(mut self) -> Result<Vec<u8>, String> {
        self.end();
        let r =
            std::fs::read(&self.path).map_err(|e| crate::sys::go_path_err("open", &self.path, &e));
        let _ = std::fs::remove_file(&self.path);
        r
    }

    /// Ends the recording and throws the clip away.
    pub fn discard(mut self) {
        self.end();
        let _ = std::fs::remove_file(&self.path);
    }
}

/// A WAV header and about a quarter second of 16 kHz mono audio.
pub const MIN_CLIP_BYTES: usize = 44 + 16000 * 2 / 4;

/// Posts wav to the voice server at base and returns the text it heard.
pub fn transcribe(base: &str, token: &str, wav: &[u8]) -> Result<String, String> {
    let url = format!("{}/transcribe?cleanup=1", base.trim_end_matches('/'));
    let auth = format!("Bearer {token}");
    let reply = match crate::web::client::send(
        "POST",
        &url,
        &[("Content-Type", "audio/wav"), ("Authorization", &auth)],
        wav,
        Duration::from_secs(120),
    ) {
        Ok(r) => r,
        Err(crate::web::client::Fail::Build(_)) => {
            return Err(format!("The voice server address {base} is not valid"))
        }
        Err(crate::web::client::Fail::Reach(_)) => {
            return Err(format!("Could not reach the voice server at {base}"))
        }
    };
    let body: Value =
        serde_json::from_slice(&reply.body[..reply.body.len().min(1 << 20)]).unwrap_or(Value::Null);
    let field = |k: &str| {
        body.get(k)
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string()
    };
    if reply.status != 200 {
        let mut msg = field("message");
        if let Some(m) = msg.strip_suffix('.') {
            msg = m.to_string();
        }
        if msg.is_empty() {
            msg = format!("The voice server answered {}", reply.status);
        }
        return Err(msg);
    }
    Ok(field("text").trim().to_string())
}

/// Makes a transcript safe to type into a pty.
pub fn clean_transcript(s: &str) -> String {
    let mut b = String::new();
    for r in s.chars() {
        let c = r as u32;
        if r == '\r' || r == '\n' || r == '\t' {
            b.push(' ');
        } else if c < 0x20 || c == 0x7f || (0x80..=0x9f).contains(&c) {
        } else {
            b.push(r);
        }
    }
    b.split_whitespace().collect::<Vec<_>>().join(" ")
}

/// Where a clip goes: the voice server's address and its token.
#[derive(Clone, Debug, Default, PartialEq)]
pub struct VoiceTarget {
    pub url: String,
    pub token: String,
}

/// Asks the coppice server whether voice can run; when it cannot, asks it
/// to start voice, and returns the line that says why and what fixes it.
pub fn voice_ready(socket: &Path) -> (Option<VoiceTarget>, String) {
    let mut res = match call_within(socket, "voice.status", None, Duration::from_secs(5)) {
        Ok(r) => r,
        Err(e) => {
            let t = e.text();
            let t = t.strip_suffix('.').unwrap_or(&t).to_string();
            return (None, format!("Voice status is not known: {t}"));
        }
    };
    if res.get("ready").and_then(Value::as_bool) != Some(true) {
        let st = res.get("state").and_then(Value::as_str).unwrap_or("");
        if st == "idle" || st == "down" {
            if let Ok(again) = call_within(socket, "voice.start", None, Duration::from_secs(5)) {
                res = again;
            }
        }
        let reason = res
            .get("reason")
            .and_then(Value::as_str)
            .unwrap_or("")
            .to_string();
        let fix = res.get("fix").and_then(Value::as_str).unwrap_or("");
        if fix.is_empty() {
            return (
                None,
                reason.strip_suffix('.').unwrap_or(&reason).to_string(),
            );
        }
        return (None, format!("{reason} {fix}"));
    }
    let url = res
        .get("url")
        .and_then(Value::as_str)
        .unwrap_or("")
        .to_string();
    let mut tok = String::new();
    let f = res.get("token_file").and_then(Value::as_str).unwrap_or("");
    if !f.is_empty() {
        match std::fs::read(f) {
            Ok(raw) => tok = String::from_utf8_lossy(&raw).trim().to_string(),
            Err(_) => return (None, "Voice's token file cannot be read".into()),
        }
    }
    (Some(VoiceTarget { url, token: tok }), String::new())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn a_transcript_types_no_control_bytes() {
        assert_eq!(
            clean_transcript("  hello\r\nworld\t\u{1b}[2J\u{9b}x\u{7f}  "),
            "hello world [2Jx"
        );
    }

    #[test]
    fn the_recorders_take_sixteen_khz_mono() {
        assert_eq!(
            recorder_args("arecord", "/c.wav"),
            ["-q", "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "wav", "/c.wav"]
        );
        assert_eq!(recorder_args("pw-record", "/c.wav")[1], "16000");
    }
}
