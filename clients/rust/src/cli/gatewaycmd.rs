//! `daisugi gateway`: the token-saving gateway on loopback, until SIGINT or
//! SIGTERM.

use std::net::TcpListener;
use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicI32, Ordering};
use std::sync::Arc;
use std::time::Duration;

use super::config::{self, ConfigErr};
use super::gateroot::path_str;
use super::{exit, parse_args, Env, Opt, Res};
use crate::gate::paths::strerror;
use crate::gate::py::text::repr;
use crate::gateway::pipeline::{External, Gateway, Settings, MODE_EXTERNAL, MODE_OFF, MODE_RULES};
use crate::gateway::server::{handle, Life, Reloader, Server, Shared};

pub const ROUTERS: &[&str] = &["rules", "switchyard", "off"];
const UPSTREAM_KINDS: &[&str] = &["anthropic", "ollama", "openai-compatible", "anthropic-compatible"];

/// `model_host._KIND_TO_BACKEND`.
fn kind_to_backend(k: &str) -> &'static str {
    match k {
        "ollama" => "ollama",
        "openai" => "openai-compatible",
        _ => "anthropic-compatible",
    }
}

const GATEWAY_OPTS: &[Opt] = &[
    Opt::val(&["--host"], "TEXT", "Bind address."),
    Opt::val(&["--port"], "INTEGER", "Bind port."),
    Opt::val(&["--upstream"], "TEXT", "Where saved turns go."),
    Opt::val(&["--cheap-model"], "TEXT", "Model an easy turn is routed onto."),
    Opt::val(&["--data-dir"], "PATH", "Daisugi data directory (holds the turn journal)."),
    Opt::pair(
        &["--capture-answers"],
        "--no-capture-answers",
        "Persist each turn's raw response text to <data-dir>/gateway/answers.jsonl (opt-in).",
    ),
    Opt::val(&["--local-model"], "TEXT", "A qualified local model id (ADR-0015)."),
    Opt::val(&["--openai-upstream"], "TEXT", "Upstream for OpenAI-wire requests."),
    Opt::val(
        &["--openai-cheap-model"],
        "TEXT",
        "Model an easy OpenAI-wire turn is routed onto; empty disables routing on that wire.",
    ),
    Opt::val(&["--upstream-kind"], "TEXT", "The wire --upstream speaks: anthropic, ollama, openai-compatible, anthropic-compatible."),
    Opt::val(&["--router"], "TEXT", "Who picks the model: rules, switchyard or off."),
    Opt::val(&["--switchyard-config"], "PATH", "Your own Switchyard TOML file."),
    Opt::val(&["--switchyard-port"], "INTEGER", "The loopback port of the managed switchyard-server."),
];

/// The write end of the signal pipe; the handler writes the signal's
/// number there.
static SIG_PIPE: AtomicI32 = AtomicI32::new(-1);

extern "C" fn on_signal(sig: libc::c_int) {
    let fd = SIG_PIPE.load(Ordering::SeqCst);
    if fd >= 0 {
        let b = sig as u8;
        unsafe {
            libc::write(fd, &b as *const u8 as *const libc::c_void, 1);
        }
    }
}

/// SIGINT, SIGTERM and SIGHUP written to a pipe; returns its read end.
fn catch_signals() -> std::io::Result<i32> {
    let mut fds = [0i32; 2];
    if unsafe { libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC | libc::O_NONBLOCK) } != 0 {
        return Err(std::io::Error::last_os_error());
    }
    SIG_PIPE.store(fds[1], Ordering::SeqCst);
    for sig in [libc::SIGINT, libc::SIGTERM, libc::SIGHUP] {
        unsafe {
            let mut sa: libc::sigaction = std::mem::zeroed();
            sa.sa_sigaction = on_signal as extern "C" fn(libc::c_int) as usize;
            sa.sa_flags = libc::SA_RESTART;
            libc::sigemptyset(&mut sa.sa_mask);
            if libc::sigaction(sig, &sa, std::ptr::null_mut()) != 0 {
                return Err(std::io::Error::last_os_error());
            }
        }
    }
    Ok(fds[0])
}

impl Env {
    pub(super) fn gateway_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "gateway";
        let p = parse_args(args, GATEWAY_OPTS, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(CMD, "", "Run the token-saving gateway: a local proxy any harness points at via base_url.", GATEWAY_OPTS);
        }
        let port = self.click_int(CMD, &p, "--port", 8787)?;
        let sy_port = self.click_int(CMD, &p, "--switchyard-port", 4000)?;
        // The upstream calls go through a proxy as httpx picks one. A proxy
        // setting this binary cannot read the same way is refused before
        // anything starts; one httpx cannot build a client from fails each
        // turn, as it does in the oracle.
        if let Some(crate::netproxy::Refusal::Unported(why)) = crate::netproxy::process_httpx().refusal() {
            return self.not_yet(&format!("daisugi gateway with this proxy setting ({why})"));
        }
        let host = p.str("--host", "127.0.0.1");
        let mut upstream = p.str("--upstream", "https://api.anthropic.com");
        let cheap = p.str("--cheap-model", "claude-haiku-4-5");
        let data_dir = path_str(&p.str("--data-dir", &self.data_home()));
        let capture = p.flag("--capture-answers");
        let local_flag = p.str("--local-model", "");
        let openai_up = p.str("--openai-upstream", "https://api.openai.com");
        let openai_cheap = p.str("--openai-cheap-model", "gpt-5-mini");
        let cfg_path = format!("{data_dir}/config.yaml");
        let router = if p.has("--router") { p.str("--router", "") } else { self.load_cfg(CMD, &cfg_path)?.gateway_router };
        if !ROUTERS.contains(&router.as_str()) {
            self.errf(&format!(
                "unknown --router {}.\nthe gateway knows three choosers.\nchoose one of: {}\n",
                repr(&router),
                ROUTERS.join(", ")
            ));
            return exit(1);
        }
        let mut shown_local = local_flag.clone();
        if shown_local.is_empty() {
            if let Some(l) = self.load_cfg(CMD, &cfg_path)?.gateway_local_model {
                shown_local = l;
            }
        }
        let mut kind = p.str("--upstream-kind", "");
        if p.has("--upstream-kind") && !UPSTREAM_KINDS.contains(&kind.as_str()) {
            self.errf(&format!(
                "unknown --upstream-kind {}.\nthe gateway only knows the wires it can shim count_tokens for.\nchoose one of: {}\n",
                repr(&kind),
                UPSTREAM_KINDS.join(", ")
            ));
            return exit(1);
        }
        if !p.has("--upstream-kind") {
            // The recorded kind counts only when --upstream names that host.
            let home_cfg = self.load_cfg(CMD, &super::gateroot::join(&self.data_home(), "config.yaml"))?;
            kind = "anthropic".into();
            if let (Some(base), Some(k)) = (&home_cfg.llm_base_url, &home_cfg.llm_host_kind) {
                if !base.is_empty() && !k.is_empty() && upstream.trim_end_matches('/') == base.trim_end_matches('/') {
                    kind = kind_to_backend(k).into();
                }
            }
        }
        let mode = match router.as_str() {
            "switchyard" => MODE_EXTERNAL,
            "off" => MODE_OFF,
            _ => MODE_RULES,
        };
        let mut ext: Option<External> = None;
        let mut child = None;
        if router == "switchyard" {
            let own = if p.has("--switchyard-config") { Some(p.str("--switchyard-config", "")) } else { None };
            let (e, base, c) = self.start_switchyard_for_gateway(&data_dir, own.as_deref(), sy_port as i64)?;
            ext = Some(e);
            upstream = base;
            child = Some(c);
            kind = "anthropic".into();
        }
        let port_n = port;
        self.out(&format!("opendaisugi gateway  \u{2192}  {upstream}\n"));
        self.out(&format!("  router: {router}\n"));
        if !shown_local.is_empty() && router == "rules" {
            self.out(&format!("  local rung: easy turns go to {shown_local}\n"));
        }
        self.out(&format!("  listening on http://{host}:{port_n}  (journal: {data_dir}/gateway/turns.jsonl)\n"));
        self.out(&format!(
            "  config reload: on change to {data_dir}/config.yaml, on SIGHUP, or POST http://{host}:{port_n}/_reload from this machine\n"
        ));
        self.out(&format!("  point your harness at it:  ANTHROPIC_BASE_URL=http://{host}:{port_n}\n"));
        if !openai_cheap.is_empty() {
            self.out(&format!("  OpenAI wire: \u{2026}/chat/completions \u{2192} {openai_up} (easy turns \u{2192} {openai_cheap})\n"));
        }
        if capture {
            self.out(&format!("  capturing answers to:      {data_dir}/gateway/answers.jsonl\n"));
        }
        // From here on, lines are written as they happen.
        self.flush();
        let journal = format!("{data_dir}/gateway/turns.jsonl");
        let answers = if capture { format!("{data_dir}/gateway/answers.jsonl") } else { String::new() };
        let build = {
            let (journal, answers) = (journal.clone(), answers.clone());
            move |local: &str, cheap: &str, mode: &str, ext: Option<External>| -> Result<Gateway, String> {
                Gateway::new(Settings {
                    cheap_model: cheap.into(),
                    local_model: local.into(),
                    router_mode: mode.into(),
                    external: ext,
                    journal_path: journal.clone(),
                    answers_path: answers.clone(),
                    capture_answers: capture,
                    max_answers: 0,
                })
            }
        };
        let rebuild = {
            let (cfg_path, local_flag, cheap, ext, build) = (cfg_path.clone(), local_flag.clone(), cheap.clone(), ext.clone(), build.clone());
            move || -> Option<Gateway> {
                let cfg = match config::load(&cfg_path) {
                    Ok(c) => c,
                    Err(ConfigErr::Unsupported) => {
                        eprintln!("daisugi gateway: {cfg_path} is not one this binary reads; keeping the running router.");
                        return None;
                    }
                    Err(_) => return None,
                };
                let local = if local_flag.is_empty() { cfg.gateway_local_model.unwrap_or_default() } else { local_flag.clone() };
                build(&local, &cheap, mode, ext.clone()).ok()
            }
        };
        let reloader = Arc::new(Reloader::new(cfg_path.clone(), Box::new(rebuild), Duration::from_secs(2)));
        let anthropic: Shared = match reloader.current() {
            Some(g) => g,
            None => match build(&local_flag, &cheap, mode, ext.clone()) {
                Ok(g) => Arc::new(std::sync::Mutex::new(g)),
                Err(why) => return self.fail(CMD, &why),
            },
        };
        let openai = if openai_cheap.is_empty() {
            None
        } else {
            match build(&local_flag, &openai_cheap, MODE_RULES, None) {
                Ok(g) => Some(Arc::new(std::sync::Mutex::new(g))),
                Err(why) => return self.fail(CMD, &why),
            }
        };
        let srv = Arc::new(Server {
            anthropic,
            reloader: Some(reloader.clone()),
            openai,
            upstream_base: upstream,
            openai_base: openai_up,
            upstream_kind: kind,
        });
        let code = serve(srv, &reloader, &host, port_n);
        if let Some(c) = &child {
            // The oracle's exit cleanup. After SIGTERM it runs too, from the
            // oracle's SIGTERM handler, once the server has drained.
            let msg = c.stop();
            self.out(&format!("  switchyard: {msg}\n"));
        }
        if code == -libc::SIGTERM {
            // The oracle's server re-raises SIGTERM once it has drained, and
            // its handler ends the process by that signal after the cleanup.
            self.flush();
            unsafe {
                libc::signal(libc::SIGTERM, libc::SIG_DFL);
                libc::raise(libc::SIGTERM);
            }
            std::thread::sleep(Duration::from_secs(3600));
        }
        if code != 0 {
            return exit(code);
        }
        Ok(())
    }
}

/// Binds, serves until SIGINT or SIGTERM, drains, and returns the exit
/// code: 0 after SIGINT, -SIGTERM after SIGTERM, 3 when the port will not
/// bind.
fn serve(app: Arc<Server>, reloader: &Arc<Reloader>, host: &str, port: i128) -> i32 {
    let sig = match catch_signals() {
        Ok(fd) => fd,
        Err(e) => {
            eprintln!("ERROR:    {e}");
            return 3;
        }
    };
    let bind_err = |n: i32| {
        eprintln!(
            "ERROR:    [Errno {n}] error while attempting to bind on address ({}, {port}): {}",
            repr(host),
            strerror(n).to_lowercase()
        );
        3
    };
    let Ok(port16) = u16::try_from(port) else { return bind_err(libc::EINVAL) };
    let listener = match TcpListener::bind((host, port16)) {
        Ok(l) => l,
        Err(e) => return bind_err(e.raw_os_error().unwrap_or(libc::EADDRNOTAVAIL)),
    };
    let life = Arc::new(Life::default());
    let lfd = listener.as_raw_fd();
    let code = loop {
        let mut fds = [libc::pollfd { fd: lfd, events: libc::POLLIN, revents: 0 }, libc::pollfd { fd: sig, events: libc::POLLIN, revents: 0 }];
        let n = unsafe { libc::poll(fds.as_mut_ptr(), 2, -1) };
        if n < 0 {
            continue;
        }
        if fds[1].revents != 0 {
            let mut b = [0u8; 16];
            let k = unsafe { libc::read(sig, b.as_mut_ptr() as *mut libc::c_void, b.len()) };
            let mut stop = None;
            for &s in &b[..k.max(0) as usize] {
                match s as i32 {
                    libc::SIGHUP => {
                        reloader.reload();
                    }
                    libc::SIGINT => stop = stop.or(Some(0)),
                    libc::SIGTERM => stop = stop.or(Some(-libc::SIGTERM)),
                    _ => {}
                }
            }
            if let Some(c) = stop {
                break c;
            }
        }
        if fds[0].revents != 0 {
            if let Ok((s, _)) = listener.accept() {
                let (app, life) = (app.clone(), life.clone());
                life.open.fetch_add(1, Ordering::SeqCst);
                std::thread::spawn(move || {
                    handle(&app, s, &life);
                    life.open.fetch_sub(1, Ordering::SeqCst);
                });
            }
        }
    };
    // Drain: no new connections; idle ones close, busy ones finish.
    life.draining.store(true, Ordering::SeqCst);
    drop(listener);
    while life.open.load(Ordering::SeqCst) > 0 {
        std::thread::sleep(Duration::from_millis(20));
    }
    code
}
