//! `daisugi voice`: serve, ptt, arm and disarm (`opendaisugi.voice`).

use std::io::{Read, Write};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;

use super::config;
use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::py::text;
use crate::gate::pyjson::{self, Value};
use crate::voice::{arm, audio, cleanup, coppice, engine, ptt, pynum, server};

const VOICE_HELP: &str = "Usage: daisugi voice [OPTIONS] COMMAND [ARGS]...

  The voice bridge. Record anywhere, transcribe on this box, land the text
  in a pane.

Options:
  --help  Show this message and exit.

Commands:
  serve   Run the voice bridge. It answers GET /health, POST /transcribe, and POST /deliver.
  ptt     Laptop push-to-talk. Tap space to start recording. Tap it again to stop and send.
  arm     Grant PANE direct send for a time window. Without this, delivered text only previews.
  disarm  Revoke PANE's direct-send grant. Delivered text goes back to preview only.
";

const DATA_OPT: Opt = Opt::val(&["--data-dir"], "PATH", "Daisugi data directory.");
const JSON_OPT: Opt = Opt::flag(&["--json"], "Machine-readable JSON output.");

static STOP: AtomicBool = AtomicBool::new(false);

extern "C" fn on_int(_: libc::c_int) {
    STOP.store(true, Ordering::SeqCst);
}

/// `cli._validated_arm_minutes`: Err is the sentence to print.
fn arm_minutes(spec: &str) -> Result<f64, String> {
    let s = text::lower(text::strip(spec));
    let parsed = if let Some(n) = s.strip_suffix('m') {
        pynum::py_float(n)
    } else if let Some(n) = s.strip_suffix('h') {
        pynum::py_float(n).map(|m| m * 60.0)
    } else {
        pynum::py_float(&s)
    };
    let Some(m) = parsed else {
        return Err(format!(
            "--for must be a number of minutes, or end with m or h, for example 30m or 2h. Got {}.",
            text::repr(spec)
        ));
    };
    if !m.is_finite() || !(m > 0.0 && m <= 7.0 * 24.0 * 60.0) {
        return Err(format!("--for must be a positive number of minutes, up to 7 days. Got {}.", text::repr(spec)));
    }
    Ok(m)
}

fn now_f() -> f64 {
    std::time::SystemTime::now().duration_since(std::time::UNIX_EPOCH).map(|d| d.as_secs_f64()).unwrap_or(0.0)
}

/// HH:MM:SS of a timestamp in the local zone (TZ, or UTC when unset).
fn local_hms(t: f64) -> String {
    let secs = t.floor() as libc::time_t;
    let mut tm: libc::tm = unsafe { std::mem::zeroed() };
    unsafe {
        libc::localtime_r(&secs, &mut tm);
    }
    format!("{:02}:{:02}:{:02}", tm.tm_hour, tm.tm_min, tm.tm_sec)
}

/// Python's format(x, ".0f"): halves to even on the exact value.
fn fmt0(x: f64) -> String {
    format!("{:.0}", x)
}

impl Env {
    pub(super) fn voice_cmd(&mut self, args: &[String]) -> Res {
        if args.is_empty() || args[0] == "--help" {
            self.out(VOICE_HELP);
            return if args.is_empty() { exit(2) } else { Ok(()) };
        }
        let rest = &args[1..];
        match args[0].as_str() {
            "serve" => self.voice_serve(rest),
            "ptt" => self.voice_ptt(rest),
            "arm" => self.voice_arm(rest),
            "disarm" => self.voice_disarm(rest),
            other => {
                self.errf(&format!(
                    "Usage: daisugi voice [OPTIONS] COMMAND [ARGS]...\nTry 'daisugi voice --help' for help.\n\nError: No such command '{other}'.\n"
                ));
                exit(2)
            }
        }
    }

    /// One voice verb's arguments: `want` positional ones named `arg`.
    fn voice_parse(&mut self, verb: &str, arg: &str, args: &[String], opts: &[Opt], want: usize, summary: &str) -> Result<super::Parsed, super::Stop> {
        let mut r = parse_args(args, opts, want);
        if let Ok(p) = &r {
            if !p.help && p.args.len() < want {
                r = Err(format!("Missing argument '{arg}'."));
            }
        }
        match r {
            Err(m) => {
                let usage = if arg.is_empty() { format!("daisugi voice {verb} [OPTIONS]") } else { format!("daisugi voice {verb} [OPTIONS] {arg}") };
                self.errf(&format!("Usage: {usage}\nTry 'daisugi voice {verb} --help' for help.\n\nError: {m}\n"));
                Err(super::Stop::Exit(2))
            }
            Ok(p) if p.help => {
                let a = if arg.is_empty() { String::new() } else { format!(" {arg}") };
                let _ = self.cmd_help(&format!("voice {verb}"), &a, summary, opts);
                Err(super::Stop::Exit(0))
            }
            Ok(p) => Ok(p),
        }
    }

    fn voice_data_dir(&self, p: &super::Parsed) -> String {
        path_str(&p.str("--data-dir", &format!("{}/.opendaisugi", self.home)))
    }

    fn voice_config(&mut self, cmd: &str, data_dir: &str) -> Result<config::Config, super::Stop> {
        match config::load(&format!("{data_dir}/config.yaml")) {
            Ok(c) => Ok(c),
            Err(_) => {
                self.errf(&format!("daisugi {cmd}: {data_dir}/config.yaml does not load. Nothing was changed.\n"));
                Err(super::Stop::Exit(2))
            }
        }
    }

    fn voice_arm(&mut self, args: &[String]) -> Res {
        let opts = [Opt::val(&["--for"], "TEXT", "How long, for example 30m or 2h."), DATA_OPT, JSON_OPT];
        let p = self.voice_parse("arm", "PANE", args, &opts, 1, "Grant PANE direct send for a time window. Without this, delivered text only previews.")?;
        let pane = p.args[0].clone();
        let minutes = match arm_minutes(&p.str("--for", "30m")) {
            Ok(m) => m,
            Err(m) => {
                self.errf(&format!("{m}\n"));
                return exit(1);
            }
        };
        let data_dir = self.voice_data_dir(&p);
        self.voice_config("voice arm", &data_dir)?;
        let armed = ptt::armed_dir(&data_dir);
        let entry = match arm::arm(&pane, minutes, &armed, now_f(), None) {
            Ok(e) => e,
            Err(_) => {
                self.errf(&format!("Could not write the grant under {armed}. Check the directory, or pass --data-dir.\n"));
                return exit(1);
            }
        };
        if p.flag("--json") {
            self.out(&format!(
                "{{\"pane\": {}, \"expires_at\": {}}}\n",
                pyjson::dumps(&Value::Str(pane), true),
                pyjson::float_repr(entry.expires_at)
            ));
            return Ok(());
        }
        self.out(&format!("{pane} armed for {} minutes, until {}.\n", fmt0(minutes), local_hms(entry.expires_at)));
        Ok(())
    }

    fn voice_disarm(&mut self, args: &[String]) -> Res {
        let opts = [DATA_OPT, JSON_OPT];
        let p = self.voice_parse("disarm", "PANE", args, &opts, 1, "Revoke PANE's direct-send grant. Delivered text goes back to preview only.")?;
        let pane = p.args[0].clone();
        let data_dir = self.voice_data_dir(&p);
        self.voice_config("voice disarm", &data_dir)?;
        let armed = ptt::armed_dir(&data_dir);
        let removed = match arm::disarm(&pane, &armed) {
            Ok(r) => r,
            Err(_) => {
                self.errf(&format!("Could not remove the grant under {armed}. Check the directory, or pass --data-dir.\n"));
                return exit(1);
            }
        };
        if p.flag("--json") {
            self.out(&format!("{{\"pane\": {}, \"removed\": {}}}\n", pyjson::dumps(&Value::Str(pane), true), if removed { "true" } else { "false" }));
        } else if removed {
            self.out(&format!("{pane} disarmed. The grant is gone.\n"));
        } else {
            self.out(&format!("{pane} had no grant. Nothing changed.\n"));
        }
        Ok(())
    }

    fn voice_ptt(&mut self, args: &[String]) -> Res {
        let opts = [Opt::val(&["--server"], "TEXT", "The voice server URL. Defaults to voice_server_url in config."), DATA_OPT];
        let p = self.voice_parse("ptt", "PANE", args, &opts, 1, "Laptop push-to-talk. Tap space to start recording. Tap it again to stop and send.")?;
        let pane = p.args[0].clone();
        let data_dir = self.voice_data_dir(&p);
        let cfg = self.voice_config("voice ptt", &data_dir)?;
        let mut url = p.str("--server", "");
        if url.is_empty() {
            url = cfg.voice_server_url.clone();
        }
        if !server::check_server_url(&url) {
            self.errf(&format!("--server must be an http or https URL with a host. Got {}.\n", text::repr(&url)));
            return exit(1);
        }
        let mut client = match ptt::HttpClient::new(&url, &data_dir, std::time::Duration::from_secs(30)) {
            Ok(c) => c,
            Err(ptt::ClientErr::Server(m)) => {
                self.out(&format!("{m}\n"));
                return Ok(());
            }
            Err(ptt::ClientErr::Other(m)) => return self.fail("voice ptt", &m),
        };
        self.out(&format!(
            "Push to talk on {pane} through {url}. Tap space to start recording. Tap it again to stop and send. q to quit.\n"
        ));
        self.flush();
        let mut restore = raw_terminal();
        let stdin = std::io::stdin();
        let mut lock = stdin.lock();
        let mut next_key = || {
            let mut buf = [0u8; 4];
            let mut n = 0;
            loop {
                if lock.read(&mut buf[n..n + 1]).ok()? == 0 {
                    return None;
                }
                n += 1;
                if let Ok(s) = std::str::from_utf8(&buf[..n]) {
                    return s.chars().next();
                }
                if n == 4 {
                    return None;
                }
            }
        };
        let mut open = || -> Result<Box<dyn ptt::Stream>, String> {
            let Some(bin) = audio::which("arecord") else {
                return Err("arecord is not on PATH. Install alsa-utils for push to talk, or run the Python daisugi".into());
            };
            Ok(Box::new(Recorder { bin, child: None }))
        };
        let mut print = |line: String| {
            let mut o = std::io::stdout();
            let _ = o.write_all(format!("{line}\n").as_bytes());
            let _ = o.flush();
        };
        let r = ptt::run_ptt(&pane, &mut client, &mut next_key, &mut open, 1600, &mut print);
        restore();
        if let Err(m) = r {
            self.errf(&format!("{m}\n"));
            return exit(1);
        }
        Ok(())
    }

    fn voice_serve(&mut self, args: &[String]) -> Res {
        let opts = [
            Opt::val(&["--host"], "TEXT", "Bind address."),
            Opt::val(&["--port"], "INTEGER", "Bind port."),
            Opt::val(&["--listen"], "TEXT", "host:port for the tailnet, for example 0.0.0.0:7477. Needs a token file."),
            Opt::val(&["--token-file"], "PATH", "Defaults to the coppice web token file."),
            Opt::val(&["--tls-cert"], "PATH", "TLS certificate file."),
            Opt::val(&["--tls-key"], "PATH", "TLS private key file."),
            DATA_OPT,
        ];
        let p = self.voice_parse("serve", "", args, &opts, 0, "Run the voice bridge. It answers GET /health, POST /transcribe, and POST /deliver.")?;
        let mut host = p.str("--host", "127.0.0.1");
        let port_text = p.str("--port", "7477");
        let Some(mut port) = pynum::py_int(&port_text).and_then(|n| u16::try_from(n).ok()) else {
            self.errf(&format!(
                "Usage: daisugi voice serve [OPTIONS]\nTry 'daisugi voice serve --help' for help.\n\nError: Invalid value for '--port': {} is not a valid integer.\n",
                text::repr(&port_text)
            ));
            return exit(2);
        };
        let listen = p.str("--listen", "");
        if !listen.is_empty() {
            match server::parse_listen(&listen) {
                Some((h, n)) => {
                    host = h;
                    port = n;
                }
                None => {
                    self.errf(&format!("--listen must be host:port, for example 0.0.0.0:7477. Got {}.\n", text::repr(&listen)));
                    return exit(1);
                }
            }
        }
        let data_dir = self.voice_data_dir(&p);
        let cfg = self.voice_config("voice serve", &data_dir)?;
        let token_file = if p.has("--token-file") { path_str(&p.str("--token-file", "")) } else { ptt::token_file(&data_dir) };
        let cert = p.has("--tls-cert").then(|| path_str(&p.str("--tls-cert", "")));
        let key = p.has("--tls-key").then(|| path_str(&p.str("--tls-key", "")));
        let scheme = if cert.is_some() { "https" } else { "http" };
        let defaults = cfg.voice_engine == engine::DEFAULT_ENGINE && cfg.voice_model == engine::DEFAULT_MODEL;
        let hardware: Option<Box<dyn Fn() -> crate::voice::hardware::VoiceHardware>> =
            if defaults && !self.env.contains_key(crate::voice::hardware::HARDWARE_ENV) {
                let hw = self.voice_hardware();
                Some(Box::new(move || hw.clone()))
            } else {
                None
            };
        let menv = crate::voice::moonshine::ModelEnv {
            vars: self.env.clone(),
            home: self.home.clone(),
            hardware,
            timeouts: Default::default(),
        };
        let picked = engine::pick_engine(
            &cfg.voice_engine,
            &cfg.voice_model,
            &menv,
            // Said at once, as the oracle prints it, before a long fetch.
            &mut |line: &str| {
                self.errf(&format!("{line}\n"));
                self.flush();
            },
        );
        let engine = match picked {
            Ok(e) => e,
            Err(e) => {
                self.errf(&format!("{}\n", e.msg));
                return exit(if e.unknown { 1 } else { 3 });
            }
        };
        // A resident engine starts here and loads its model before the bind,
        // and stops with the server however it ends.
        if let Err(e) = engine.start() {
            self.errf(&format!("{}\n", e.msg));
            return exit(3);
        }
        let stopper = engine.stop_handle();
        let stop_engine = move || {
            if let Some(r) = &stopper {
                r.stop();
            }
        };
        let scfg = server::ServerConfig {
            home: self.home.clone(),
            token_file,
            armed_dir: ptt::armed_dir(&data_dir),
            engine,
            cleanup: cleanup::CleanupConfig {
                on: cfg.voice_cleanup,
                model: cfg.voice_cleanup_model.clone(),
                base_url: cfg.voice_cleanup_base_url.clone(),
                data_dir: data_dir.clone(),
            },
            floor: coppice::FloorConfig { backend: cfg.floor_backend.clone(), coppice_socket: cfg.coppice_socket.clone() },
            llm: crate::llm::Client::new(self.env.clone(), &self.home),
        };
        let srv = match server::listen(&host, port, scfg, cert.as_deref(), key.as_deref()) {
            Ok(s) => s,
            Err(e) => {
                stop_engine();
                self.errf(&format!("{}\n", e.msg));
                return exit(e.code);
            }
        };
        unsafe {
            libc::signal(libc::SIGINT, on_int as extern "C" fn(libc::c_int) as libc::sighandler_t);
        }
        self.out(&format!("opendaisugi voice listening on {scheme}://{host}:{port}\n"));
        self.flush();
        Arc::new(srv).serve(&STOP);
        stop_engine();
        self.out("\n");
        Ok(())
    }
}

/// Puts stdin in raw mode when it is a terminal, as main_loop does; the
/// closure puts it back.
fn raw_terminal() -> impl FnMut() {
    let mut old: libc::termios = unsafe { std::mem::zeroed() };
    let ok = unsafe { libc::tcgetattr(0, &mut old) } == 0;
    if ok {
        let mut raw = old;
        unsafe {
            libc::cfmakeraw(&mut raw);
            libc::tcsetattr(0, libc::TCSAFLUSH, &raw);
        }
    }
    let mut done = !ok;
    move || {
        if !done {
            done = true;
            unsafe {
                libc::tcsetattr(0, libc::TCSADRAIN, &old);
            }
        }
    }
}

/// Records through arecord, the ALSA recorder: the Python build uses
/// PortAudio through sounddevice, which this binary does not carry
/// (ruling VO-5). The first read returns the whole press. `bin` is the
/// arecord found on PATH when the stream opens.
struct Recorder {
    bin: String,
    child: Option<std::process::Child>,
}

impl ptt::Stream for Recorder {
    fn start(&mut self) -> Result<(), String> {
        let child = std::process::Command::new(&self.bin)
            .args(["-q", "-t", "raw", "-f", "S16_LE", "-r", "16000", "-c", "1"])
            .stdin(std::process::Stdio::null())
            .stdout(std::process::Stdio::piped())
            .stderr(std::process::Stdio::null())
            .spawn()
            .map_err(|e| e.to_string())?;
        self.child = Some(child);
        Ok(())
    }

    fn read(&mut self, _frames: usize) -> Result<(Vec<u8>, bool), String> {
        let Some(mut child) = self.child.take() else { return Ok((vec![], false)) };
        unsafe {
            libc::kill(child.id() as libc::pid_t, libc::SIGINT);
        }
        let mut b = vec![];
        if let Some(mut out) = child.stdout.take() {
            let _ = out.read_to_end(&mut b);
        }
        let _ = child.wait();
        Ok((b, false))
    }

    fn stop(&mut self) -> Result<(), String> {
        if let Some(mut c) = self.child.take() {
            let _ = c.kill();
            let _ = c.wait();
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::voice::ptt::Stream;

    #[test]
    fn arm_minutes_follows_the_oracle_rules() {
        for (spec, want) in [("30m", 30.0), ("2h", 120.0), (" 90 ", 90.0), ("1_0M", 10.0)] {
            assert_eq!(arm_minutes(spec), Ok(want), "{spec}");
        }
        for spec in ["", "abc", "0", "-5m", "inf", "nan", "10081"] {
            assert!(arm_minutes(spec).is_err(), "{spec}");
        }
        assert_eq!(fmt0(2.5), "2");
        assert_eq!(fmt0(3.5), "4");
    }

    // A fake arecord stands in for the real one: no microphone is used.
    // The test names the fake by its path and leaves PATH alone: PATH is
    // shared by every test thread, and a PATH that holds only the fake
    // makes the other tests' git, sh and sleep fail to start.
    #[test]
    fn the_recorder_returns_the_whole_press_from_a_fake_arecord() {
        let dir = std::env::temp_dir().join(format!("voice-rec-{}", std::process::id()));
        std::fs::create_dir_all(&dir).unwrap();
        let script = dir.join("arecord");
        std::fs::write(&script, "#!/bin/sh\nprintf 'abcd'\ntrap 'exit 0' INT\nwhile :; do /bin/sleep 0.05; done\n").unwrap();
        use std::os::unix::fs::PermissionsExt;
        std::fs::set_permissions(&script, std::fs::Permissions::from_mode(0o755)).unwrap();
        let mut r = Recorder { bin: script.to_string_lossy().into_owned(), child: None };
        r.start().unwrap();
        std::thread::sleep(std::time::Duration::from_millis(300));
        let (b, more) = r.read(1600).unwrap();
        r.stop().unwrap();
        let _ = std::fs::remove_dir_all(&dir);
        assert_eq!((b.as_slice(), more), (b"abcd".as_slice(), false));
    }
}
