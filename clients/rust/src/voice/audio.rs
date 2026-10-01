//! `voice/audio.py`: any clip to 16 kHz mono 16-bit PCM WAV, and
//! `shutil.which`.

use std::io::{Read, Write};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

use super::wav::{is_wav, read_mono_samples, resample_linear, write_wav_mono, AudioError};

/// `shutil.which(name)` on this process's PATH.
pub fn which(name: &str) -> Option<String> {
    let path = std::env::var("PATH").unwrap_or_else(|_| "/bin:/usr/bin".into());
    if name.contains('/') {
        return is_exec(name).then(|| name.to_string());
    }
    let mut seen = std::collections::HashSet::new();
    for dir in path.split(':') {
        if !seen.insert(dir.to_string()) {
            continue;
        }
        let p = if dir.is_empty() {
            name.to_string()
        } else if dir.ends_with('/') {
            format!("{dir}{name}")
        } else {
            format!("{dir}/{name}")
        };
        if is_exec(&p) {
            return Some(p);
        }
    }
    None
}

fn is_exec(p: &str) -> bool {
    match std::fs::metadata(p) {
        Ok(m) if !m.is_dir() => {
            let c = match std::ffi::CString::new(p) {
                Ok(c) => c,
                Err(_) => return false,
            };
            unsafe { libc::access(c.as_ptr(), libc::X_OK) == 0 }
        }
        _ => false,
    }
}

/// The ffmpeg command line of `_ffmpeg_to_wav_16k_mono`.
const FFMPEG_ARGS: &[&str] =
    &["-hide_banner", "-loglevel", "error", "-i", "pipe:0", "-ar", "16000", "-ac", "1", "-f", "wav", "pipe:1"];

/// Runs a command with stdin bytes and a timeout: its exit code (None when
/// a signal ended it), stdout and stderr; Err on timeout or spawn failure.
pub fn run_with_input(prog: &str, args: &[&str], input: Option<&[u8]>, timeout: Duration) -> Result<(Option<i32>, Vec<u8>, Vec<u8>), String> {
    let mut child = Command::new(prog)
        .args(args)
        .stdin(if input.is_some() { Stdio::piped() } else { Stdio::null() })
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .map_err(|e| e.to_string())?;
    let writer = input.map(|b| {
        let mut stdin = child.stdin.take().expect("piped");
        let b = b.to_vec();
        std::thread::spawn(move || {
            let _ = stdin.write_all(&b);
        })
    });
    let mut out_pipe = child.stdout.take().expect("piped");
    let mut err_pipe = child.stderr.take().expect("piped");
    let out_t = std::thread::spawn(move || {
        let mut b = vec![];
        let _ = out_pipe.read_to_end(&mut b);
        b
    });
    let err_t = std::thread::spawn(move || {
        let mut b = vec![];
        let _ = err_pipe.read_to_end(&mut b);
        b
    });
    let start = Instant::now();
    let status = loop {
        match child.try_wait() {
            Ok(Some(s)) => break s,
            Ok(None) if start.elapsed() >= timeout => {
                let _ = child.kill();
                let _ = child.wait();
                return Err("timed out".into());
            }
            Ok(None) => std::thread::sleep(Duration::from_millis(5)),
            Err(e) => return Err(e.to_string()),
        }
    };
    if let Some(w) = writer {
        let _ = w.join();
    }
    let out = out_t.join().unwrap_or_default();
    let err = err_t.join().unwrap_or_default();
    Ok((status.code(), out, err))
}

fn ffmpeg_to_wav(raw: &[u8]) -> Result<Vec<u8>, AudioError> {
    let prog = which("ffmpeg").unwrap_or_else(|| "ffmpeg".into());
    let (code, out, _) =
        run_with_input(&prog, FFMPEG_ARGS, Some(raw), Duration::from_secs(30)).map_err(|e| AudioError::new("timeout", &e))?;
    if code != Some(0) {
        return Err(AudioError::new("unsupported", "ffmpeg could not decode this clip"));
    }
    let (rate, samples) = read_mono_samples(&out)?;
    write_wav_mono(&samples, rate)
}

/// `audio.to_wav_16k_mono`. A clip that is not a WAV needs ffmpeg: the
/// oracle's last resort, the av package, is a Python library this binary
/// does not carry (ruling VO-4).
pub fn to_wav_16k_mono(raw: &[u8]) -> Result<Vec<u8>, AudioError> {
    if is_wav(raw) {
        let (rate, samples) = read_mono_samples(raw)?;
        if rate == 16000 {
            return write_wav_mono(&samples, 16000);
        }
        if which("ffmpeg").is_some() {
            return ffmpeg_to_wav(raw);
        }
        let out = resample_linear(&samples, rate, 16000)?;
        return write_wav_mono(&out, 16000);
    }
    if which("ffmpeg").is_some() {
        return ffmpeg_to_wav(raw);
    }
    Err(AudioError::new("unsupported", "cannot decode this audio format. The ffmpeg binary is not on PATH. Install ffmpeg."))
}
