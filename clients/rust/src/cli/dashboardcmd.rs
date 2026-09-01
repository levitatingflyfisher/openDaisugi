//! `daisugi dashboard` (`opendaisugi.dashboard`), the live floor, and
//! `daisugi metrics` (`opendaisugi.exporter`), the same numbers in the
//! Prometheus text format, printed or served at /metrics. The Go client's
//! `dashboardcmd.go` and `metricscmd.go` are the reference; rulings K4-3 to
//! K4-7 say where they differ from the oracle's.

use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::os::fd::AsRawFd;
use std::sync::atomic::{AtomicI32, Ordering};
use std::time::Duration;

use num_bigint::BigInt;

use super::gateroot::join;
use super::modulescmd::{
    box_line, module_rows, read_pathway_stats, stage_object, stdout_tty, Glyphs, Stage, WIDTH,
};
use super::statuscmd::{py_fixed, DATA_DIR_OPT};
use super::{exit, parse_args, Env, Opt, Res, Stop};
use crate::gate::paths::strerror;
use crate::gate::pyjson::{dumps_indent, float_repr, Object, Value};
use crate::gateway::report::{load_journal, summarize, JRecord, JournalErr, Summary};
use crate::tracejournal::Journal;

/// `dashboard.RawMetrics`: None where a store is absent or cannot be read.
#[derive(Default)]
struct Raw {
    journal: Option<(i64, i64, i64)>,
    pathway_count: Option<i64>,
    /// None with a count: int() of the hit sum raised.
    pathway_hits: Option<BigInt>,
    gateway: Option<Summary>,
}

/// The gateway's turn journal. A record of other types (GW-8) is an
/// error; a file that cannot be read or decoded is no records, as the
/// oracle's read_raw swallows the error.
fn load_gateway_records(data_dir: &str) -> Result<Vec<JRecord>, String> {
    match load_journal(&join(data_dir, "gateway/turns.jsonl")) {
        Ok(r) => Ok(r),
        Err(JournalErr::Io(_)) => Ok(vec![]),
        Err(JournalErr::Unread(m)) if m.ends_with("a line json.loads raises on") => Ok(vec![]),
        Err(JournalErr::Unread(m)) => Err(m),
    }
}

/// `dashboard.read_raw`, given the gateway records read first.
fn read_raw(data_dir: &str, recs: &[JRecord]) -> Raw {
    let mut raw = Raw::default();
    if std::fs::metadata(join(data_dir, "journal/index.db")).is_ok() {
        if let Some(st) = Journal::open_read_only(data_dir)
            .ok()
            .and_then(|j| j.stats().ok())
        {
            raw.journal = Some((st.total, st.passed, st.failed));
        }
    }
    let db = join(data_dir, "pathways.db");
    if std::fs::metadata(&db).is_ok() {
        if let Some((c, h)) = read_pathway_stats(&db) {
            raw.pathway_count = Some(c);
            raw.pathway_hits = h;
        }
    }
    if !recs.is_empty() {
        raw.gateway = Some(summarize(recs));
    }
    raw
}

/// `dashboard.Gauge`.
struct Gauge {
    label: &'static str,
    value: String,
    detail: String,
    fraction: Option<f64>,
}

fn blank(label: &'static str, why: &str) -> Gauge {
    Gauge {
        label,
        value: "—".into(),
        detail: why.into(),
        fraction: None,
    }
}

fn ratio(num: i64, denom: i64) -> Option<f64> {
    (denom != 0).then(|| num as f64 / denom as f64)
}

/// `format(n, ",")`.
fn commas(n: &BigInt) -> String {
    let s = n.to_string();
    let (neg, digits) = match s.strip_prefix('-') {
        Some(d) => (true, d),
        None => (false, s.as_str()),
    };
    let mut out = String::new();
    for (i, c) in digits.chars().enumerate() {
        if i > 0 && (digits.len() - i) % 3 == 0 {
            out.push(',');
        }
        out.push(c);
    }
    if neg {
        format!("-{out}")
    } else {
        out
    }
}

fn commas64(n: i64) -> String {
    commas(&BigInt::from(n))
}

/// `format(x, ",.<n>f")`.
fn grouped_fixed(x: f64, n: usize) -> String {
    let s = py_fixed(x, n);
    if !x.is_finite() {
        return s;
    }
    let (neg, body) = match s.strip_prefix('-') {
        Some(b) => (true, b.to_string()),
        None => (false, s.clone()),
    };
    let (ip, fp) = match body.split_once('.') {
        Some((i, f)) => (i.to_string(), Some(f.to_string())),
        None => (body.clone(), None),
    };
    let mut out = commas(&ip.parse::<BigInt>().unwrap_or_default());
    if let Some(f) = fp {
        out = format!("{out}.{f}");
    }
    if neg {
        format!("-{out}")
    } else {
        out
    }
}

/// `format(x, ".0%")`.
fn percent0(x: f64) -> String {
    format!("{}%", py_fixed(x * 100.0, 0))
}

const HITS_NONE: &str = "TypeError: unsupported format string passed to NoneType.__format__";

/// `dashboard.collect_metrics`, by stage key.
fn collect_metrics(raw: &Raw) -> Result<Vec<(&'static str, Vec<Gauge>)>, &'static str> {
    let stores = match raw.journal {
        None => blank("traces", "no journal yet — run `daisugi onboard`"),
        Some((t, p, f)) => Gauge {
            label: "traces",
            value: commas64(t),
            detail: format!("{} passed · {} failed", commas64(p), commas64(f)),
            fraction: ratio(p, t),
        },
    };
    let (matcher, distill) = match raw.pathway_count {
        None => (
            blank("reuse hits", "no pathways yet — run `daisugi tend`"),
            blank("pathways", "no pathways yet — run `daisugi tend`"),
        ),
        Some(c) => {
            let Some(h) = &raw.pathway_hits else {
                return Err(HITS_NONE);
            };
            (
                Gauge {
                    label: "reuse hits",
                    value: commas(h),
                    detail: format!("{} pathway(s) stored", commas64(c)),
                    fraction: None,
                },
                Gauge {
                    label: "pathways",
                    value: commas64(c),
                    detail: format!("{} lifetime reuse hit(s)", commas(h)),
                    fraction: None,
                },
            )
        }
    };
    let router = match &raw.gateway {
        None => blank("tokens saved", "no gateway turns recorded yet"),
        Some(g) => Gauge {
            label: "tokens saved",
            value: commas(&g.frontier_saved),
            detail: format!(
                "${} · {}x · cache {} · {}/{} downgraded",
                grouped_fixed(g.dollars_saved, 2),
                py_fixed(g.blended, 2),
                percent0(g.cache_hit_rate),
                g.downgraded,
                g.turns
            ),
            fraction: ratio(g.downgraded as i64, g.turns as i64),
        },
    };
    Ok(vec![
        (
            "harness",
            vec![Gauge {
                label: "detected",
                value: "see map".into(),
                detail: "active harnesses marked ● above".into(),
                fraction: None,
            }],
        ),
        ("gate", vec![blank("throughput", "no passive counter yet")]),
        (
            "verifier",
            vec![blank(
                "latency",
                "benchmark-only — run `daisugi guard-cost`",
            )],
        ),
        ("matcher", vec![matcher]),
        ("distill", vec![distill]),
        ("router", vec![router]),
        ("stores", vec![stores]),
    ])
}

fn gauges_of<'a>(metrics: &'a [(&'static str, Vec<Gauge>)], key: &str) -> &'a [Gauge] {
    metrics
        .iter()
        .find(|(k, _)| *k == key)
        .map(|(_, g)| g.as_slice())
        .unwrap_or(&[])
}

/// `dashboard.render_dashboard`: one frame. `pulse` None lights no stage.
fn render_dashboard(
    data_dir: &str,
    stages: &[Stage],
    metrics: &[(&'static str, Vec<Gauge>)],
    pulse: Option<usize>,
    g: &Glyphs,
) -> String {
    let inner = WIDTH - 4;
    let mut out = vec![
        format!("openDaisugi — live floor   (data dir: {data_dir})"),
        String::new(),
        "  a task from your agent".to_string(),
    ];
    let n = stages.len();
    for (i, st) in stages.iter().enumerate() {
        out.push(format!("        {}", g.v));
        if pulse.is_some_and(|p| p % n == i) {
            out.push(format!("        {} {}", g.down, g.on));
        } else {
            out.push(format!("        {}", g.down));
        }
        let head = format!("{}{} {} ", g.tl, g.h, st.title);
        let fill = WIDTH.saturating_sub(head.chars().count() + 1);
        out.push(format!("{head}{}{}", g.h.repeat(fill), g.tr));
        out.push(box_line(g, st.role, inner));
        out.extend(module_rows(g, st, inner));
        for gg in gauges_of(metrics, st.key) {
            let mut text = format!("{} {} {}", g.flow, gg.label, gg.value);
            if !gg.detail.is_empty() {
                text.push_str(&format!("  ({})", gg.detail));
            }
            out.push(box_line(g, &format!("  {text}"), inner));
        }
        out.push(format!("{}{}{}", g.bl, g.h.repeat(WIDTH - 2), g.br));
    }
    out.push(format!("        {}", g.v));
    out.push(format!("        {}", g.down));
    out.push("  verified action runs  (or falls back / is refused)".into());
    out.push(String::new());
    out.push(format!(
        "legend:  {} active   {} available   {} possible    {} live gauge (read-only)",
        g.on, g.avail, g.off, g.flow
    ));
    out.join("\n")
}

/// `dashboard.dashboard_json`: the wiring JSON with each stage's gauges.
fn dashboard_json(stages: &[Stage], metrics: &[(&'static str, Vec<Gauge>)]) -> String {
    let out: Vec<Value> = stages
        .iter()
        .map(|st| {
            let gs: Vec<Value> = gauges_of(metrics, st.key)
                .iter()
                .map(|gg| {
                    Value::Obj(
                        Object::new()
                            .with("label", gg.label)
                            .with("value", gg.value.as_str())
                            .with("detail", gg.detail.as_str())
                            .with(
                                "fraction",
                                gg.fraction.map(Value::from).unwrap_or(Value::Null),
                            ),
                    )
                })
                .collect();
            Value::Obj(stage_object(st).with("metrics", gs))
        })
        .collect();
    dumps_indent(&Value::List(out), 2, true)
}

/// The read end of a pipe SIGINT writes a byte to.
static INT_PIPE: AtomicI32 = AtomicI32::new(-1);

extern "C" fn on_int(_: libc::c_int) {
    let fd = INT_PIPE.load(Ordering::SeqCst);
    if fd >= 0 {
        let b = 1u8;
        // SAFETY: write is async-signal-safe; the byte outlives the call.
        unsafe {
            libc::write(fd, &b as *const u8 as *const libc::c_void, 1);
        }
    }
}

/// SIGINT caught into a pipe; returns the read end.
fn catch_int() -> Option<i32> {
    let mut fds = [0i32; 2];
    // SAFETY: pipe2 fills the two fds it is given; sigaction gets a handler
    // that only writes to the pipe.
    unsafe {
        if libc::pipe2(fds.as_mut_ptr(), libc::O_CLOEXEC | libc::O_NONBLOCK) != 0 {
            return None;
        }
        INT_PIPE.store(fds[1], Ordering::SeqCst);
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = on_int as extern "C" fn(libc::c_int) as usize;
        sa.sa_flags = libc::SA_RESTART;
        libc::sigemptyset(&mut sa.sa_mask);
        if libc::sigaction(libc::SIGINT, &sa, std::ptr::null_mut()) != 0 {
            return None;
        }
    }
    Some(fds[0])
}

/// Waits up to `ms` (or for ever with -1) for fd `a` or the SIGINT pipe:
/// true when SIGINT came.
fn wait_int(sig: i32, other: Option<i32>, ms: i32) -> (bool, bool) {
    let mut fds = vec![libc::pollfd {
        fd: sig,
        events: libc::POLLIN,
        revents: 0,
    }];
    if let Some(o) = other {
        fds.push(libc::pollfd {
            fd: o,
            events: libc::POLLIN,
            revents: 0,
        });
    }
    // SAFETY: poll over fds we own, for their count.
    let n = unsafe { libc::poll(fds.as_mut_ptr(), fds.len() as libc::nfds_t, ms) };
    if n <= 0 {
        return (false, false);
    }
    (fds[0].revents != 0, fds.len() > 1 && fds[1].revents != 0)
}

impl Env {
    /// The gateway records before anything is written: a record of other
    /// types is refused, as the report commands refuse it.
    fn gateway_records(&mut self, cmd: &str, data_dir: &str) -> Result<Vec<JRecord>, Stop> {
        load_gateway_records(data_dir).map_err(|m| self.refuse(cmd, &m).unwrap_err())
    }

    /// Reads the stores and draws one frame, in the oracle's order: the map
    /// first, then the gauges.
    fn dashboard_frame(&mut self, cmd: &str, data_dir: &str) -> Result<String, Stop> {
        let recs = self.gateway_records(cmd, data_dir)?;
        let stages = self.detect_stages(cmd, data_dir)?;
        let metrics = collect_metrics(&read_raw(data_dir, &recs))
            .map_err(|e| self.fail(cmd, e).unwrap_err())?;
        Ok(render_dashboard(
            data_dir,
            &stages,
            &metrics,
            None,
            self.glyphs(),
        ))
    }

    /// `dashboard.run_live`: one frame when stdout is not a terminal; on a
    /// terminal, a frame every interval seconds, the map read once and the
    /// gauges on each tick, until Ctrl-C. `aborted` says how Ctrl-C ends
    /// the command: click's "Aborted!" and exit 1, or a newline and exit 0.
    pub(super) fn run_live(
        &mut self,
        cmd: &str,
        data_dir: &str,
        interval: f64,
        aborted: bool,
    ) -> Res {
        if !stdout_tty() {
            let frame = self.dashboard_frame(cmd, data_dir)?;
            self.out(&format!("{frame}\n"));
            return Ok(());
        }
        let sig = catch_int();
        let mut recs = self.gateway_records(cmd, data_dir)?;
        let stages = self.detect_stages(cmd, data_dir)?;
        let g = self.glyphs();
        let mut i = 0usize;
        loop {
            if i > 0 {
                recs = self.gateway_records(cmd, data_dir)?;
            }
            let metrics = collect_metrics(&read_raw(data_dir, &recs))
                .map_err(|e| self.fail(cmd, e).unwrap_err())?;
            if !self.plain {
                self.out("\x1b[H\x1b[2J");
            }
            let frame = render_dashboard(data_dir, &stages, &metrics, Some(i), g);
            self.out(&format!("{frame}\n"));
            self.flush();
            if !interval.is_finite() || interval < 0.0 {
                self.errf(&format!(
                    "daisugi {cmd}: ValueError: sleep length must be a finite number of seconds, zero or more\n"
                ));
                return exit(1);
            }
            let ms = (interval * 1000.0).min(i32::MAX as f64) as i32;
            let interrupted = match sig {
                Some(fd) => wait_int(fd, None, ms).0,
                None => {
                    std::thread::sleep(Duration::from_millis(ms as u64));
                    false
                }
            };
            if interrupted {
                if aborted {
                    self.errf("\nAborted!\n");
                    return exit(1);
                }
                self.out("\n");
                return Ok(());
            }
            i += 1;
        }
    }

    pub(super) fn dashboard_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "dashboard";
        let opts = [
            DATA_DIR_OPT,
            Opt::flag(&["--once"], "Render one frame and exit (no live loop)."),
            Opt::flag(
                &["--json"],
                "Emit live wiring as JSON (superset of `modules --json`).",
            ),
            Opt::val(&["--interval"], "FLOAT", "Seconds between live refreshes."),
            Opt::flag(
                &["--tui"],
                "Interactive Textual GUI; not in this binary yet.",
            ),
            Opt::flag(
                &["--serve"],
                "The Textual GUI in a browser; not in this binary yet.",
            ),
            Opt::val(&["--host"], "TEXT", "Bind host for --serve."),
            Opt::val(&["--port"], "INTEGER", "Bind port for --serve."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "The live 'factory floor': the module map with real throughput gauges.",
                &opts,
            );
        }
        let interval = self.click_float(CMD, &p, "--interval", 2.0)?;
        self.click_int(CMD, &p, "--port", 8000)?;
        let data_dir = self.data_dir_of(&p);
        if p.flag("--json") {
            let recs = self.gateway_records(CMD, &data_dir)?;
            let stages = self.detect_stages(CMD, &data_dir)?;
            let metrics = collect_metrics(&read_raw(&data_dir, &recs))
                .map_err(|e| self.fail(CMD, e).unwrap_err())?;
            let text = dashboard_json(&stages, &metrics);
            self.out(&format!("{text}\n"));
            return Ok(());
        }
        if p.flag("--once") {
            let frame = self.dashboard_frame(CMD, &data_dir)?;
            self.out(&format!("{frame}\n"));
            return Ok(());
        }
        if p.flag("--serve") {
            return self.not_yet("daisugi dashboard --serve (the Textual GUI)");
        }
        if p.flag("--tui") {
            return self.not_yet("daisugi dashboard --tui (the Textual GUI)");
        }
        self.run_live(CMD, &data_dir, interval, false)
    }

    pub(super) fn metrics_cmd(&mut self, args: &[String]) -> Res {
        const CMD: &str = "metrics";
        let opts = [
            DATA_DIR_OPT,
            Opt::flag(
                &["--serve"],
                "Serve /metrics over HTTP for a Prometheus scraper.",
            ),
            Opt::val(&["--host"], "TEXT", "Bind host for --serve."),
            Opt::val(&["--port"], "INTEGER", "Bind port for --serve."),
        ];
        let p = parse_args(args, &opts, 0).map_err(|m| self.usage_stop(CMD, m))?;
        if p.help {
            return self.cmd_help(
                CMD,
                "",
                "Prometheus metrics: exposition text on stdout, or an HTTP /metrics endpoint.",
                &opts,
            );
        }
        let port = self.click_int(CMD, &p, "--port", 9188)?;
        let data_dir = self.data_dir_of(&p);
        if p.flag("--serve") {
            let host = p.str("--host", "127.0.0.1");
            return self.serve_metrics(&data_dir, &host, port);
        }
        let recs = self.gateway_records(CMD, &data_dir)?;
        let text = render_prometheus(&read_raw(&data_dir, &recs))
            .map_err(|e| self.fail(CMD, e).unwrap_err())?;
        self.out(&text);
        Ok(())
    }

    /// `exporter.serve_metrics`: the endpoint on host:port until Ctrl-C,
    /// which ends it with a newline and exit 0.
    fn serve_metrics(&mut self, data_dir: &str, host: &str, port: i128) -> Res {
        self.out(&format!(
            "serving Prometheus metrics at http://{host}:{port}/metrics (Ctrl-C to stop)\n"
        ));
        self.flush();
        let sig = catch_int();
        let bound = u16::try_from(port)
            .map_err(|_| std::io::Error::from_raw_os_error(libc::EINVAL))
            .and_then(|p| {
                // http.server binds an AF_INET socket: only the IPv4
                // addresses the host names.
                use std::net::ToSocketAddrs;
                let v4: Vec<_> = (host, p)
                    .to_socket_addrs()?
                    .filter(|a| a.is_ipv4())
                    .collect();
                if v4.is_empty() {
                    return Err(std::io::Error::from_raw_os_error(libc::EADDRNOTAVAIL));
                }
                TcpListener::bind(&v4[..])
            });
        let listener = match bound {
            Ok(l) => l,
            Err(e) => {
                match e.raw_os_error() {
                    Some(n) => {
                        let msg = strerror(n);
                        let mut c = msg.chars();
                        let msg = match c.next() {
                            Some(f) => f.to_uppercase().collect::<String>() + c.as_str(),
                            None => msg,
                        };
                        self.errf(&format!("daisugi metrics: OSError: [Errno {n}] {msg}\n"));
                    }
                    None => self.errf(&format!("daisugi metrics: OSError: {e}\n")),
                }
                return exit(1);
            }
        };
        let lfd = listener.as_raw_fd();
        loop {
            let (int, ready) = match sig {
                Some(fd) => wait_int(fd, Some(lfd), -1),
                None => (false, true),
            };
            if int {
                self.out("\n");
                return Ok(());
            }
            if ready {
                if let Ok((s, _)) = listener.accept() {
                    let dd = data_dir.to_string();
                    std::thread::spawn(move || handle_scrape(s, &dd));
                }
            }
        }
    }
}

/// One scrape of `exporter.make_metrics_server`'s handler: GET of a path
/// that is "" or "/metrics" once its trailing slashes go answers the
/// exposition; any other GET is a 404 with no body; any other method is
/// the 501 BaseHTTPRequestHandler sends. The connection closes after the
/// reply.
fn handle_scrape(mut s: TcpStream, data_dir: &str) {
    let _ = s.set_read_timeout(Some(Duration::from_secs(30)));
    let mut buf = Vec::new();
    let mut chunk = [0u8; 4096];
    while !buf.windows(4).any(|w| w == b"\r\n\r\n") && !buf.windows(2).any(|w| w == b"\n\n") {
        match s.read(&mut chunk) {
            Ok(0) | Err(_) => break,
            Ok(n) => buf.extend_from_slice(&chunk[..n]),
        }
        if buf.len() > 65536 {
            break;
        }
    }
    let text = String::from_utf8_lossy(&buf);
    let line = text.lines().next().unwrap_or("");
    let mut words = line.split_whitespace();
    let (Some(method), Some(target)) = (words.next(), words.next()) else {
        return;
    };
    let reply =
        |s: &mut TcpStream, status: &str, ctype: Option<&str>, body: &str, send_body: bool| {
            let mut head = format!("HTTP/1.1 {status}\r\n");
            if let Some(t) = ctype {
                head.push_str(&format!("Content-Type: {t}\r\n"));
            }
            head.push_str(&format!(
                "Content-Length: {}\r\nConnection: close\r\n\r\n",
                body.len()
            ));
            let _ = s.write_all(head.as_bytes());
            if send_body {
                let _ = s.write_all(body.as_bytes());
            }
            let _ = s.flush();
        };
    if method != "GET" {
        let body = py_error_page(
            501,
            &format!("Unsupported method ('{method}')"),
            "Server does not support this operation",
        );
        reply(
            &mut s,
            "501 Not Implemented",
            Some("text/html;charset=utf-8"),
            &body,
            method != "HEAD",
        );
        return;
    }
    let path = target.trim_end_matches('/');
    if !path.is_empty() && path != "/metrics" {
        reply(&mut s, "404 Not Found", None, "", false);
        return;
    }
    // A record this binary does not read the oracle's way: no numbers
    // rather than other numbers (K4-6).
    let text = match load_gateway_records(data_dir)
        .map_err(|m| m.to_string())
        .and_then(|recs| render_prometheus(&read_raw(data_dir, &recs)).map_err(|e| e.to_string()))
    {
        Ok(t) => t,
        Err(e) => {
            eprintln!("daisugi metrics: {e}");
            reply(&mut s, "500 Internal Server Error", None, "", false);
            return;
        }
    };
    reply(&mut s, "200 OK", Some(PROM_CONTENT_TYPE), &text, true);
}

const PROM_CONTENT_TYPE: &str = "text/plain; version=0.0.4; charset=utf-8";

/// http.server's DEFAULT_ERROR_MESSAGE, filled as send_error fills it.
fn py_error_page(code: u16, message: &str, explain: &str) -> String {
    let esc = |s: &str| {
        s.replace('&', "&amp;")
            .replace('<', "&lt;")
            .replace('>', "&gt;")
    };
    format!(
        "<!DOCTYPE HTML>\n<html lang=\"en\">\n    <head>\n        <meta charset=\"utf-8\">\n        \
         <title>Error response</title>\n    </head>\n    <body>\n        <h1>Error response</h1>\n        \
         <p>Error code: {code}</p>\n        <p>Message: {}.</p>\n        <p>Error code explanation: {code} - {}.</p>\n    \
         </body>\n</html>\n",
        esc(message),
        esc(explain)
    )
}

const PROM_HITS_NONE: &str =
    "TypeError: float() argument must be a string or a real number, not 'NoneType'";

/// `repr(float(x))`.
fn repr_float(f: f64) -> String {
    if f.is_nan() {
        "nan".into()
    } else if f.is_infinite() {
        if f > 0.0 {
            "inf".into()
        } else {
            "-inf".into()
        }
    } else {
        float_repr(f)
    }
}

/// `exporter.render_prometheus` over the raw readings.
fn render_prometheus(raw: &Raw) -> Result<String, &'static str> {
    let mut lines: Vec<String> = vec![];
    let mut metric = |name: &str, typ: &str, help: &str, samples: &[(&str, String)]| {
        lines.push(format!("# HELP {name} {help}"));
        lines.push(format!("# TYPE {name} {typ}"));
        for (labels, value) in samples {
            if labels.is_empty() {
                lines.push(format!("{name} {value}"));
            } else {
                lines.push(format!("{name}{{{labels}}} {value}"));
            }
        }
    };
    metric(
        "daisugi_up",
        "gauge",
        "1 if the daisugi exporter responded.",
        &[("", "1".into())],
    );
    if let Some((t, p, f)) = raw.journal {
        metric(
            "daisugi_journal_traces",
            "gauge",
            "Verified traces recorded in the journal, by outcome.",
            &[
                ("status=\"passed\"", p.to_string()),
                ("status=\"failed\"", f.to_string()),
                ("status=\"total\"", t.to_string()),
            ],
        );
    }
    if let Some(c) = raw.pathway_count {
        let Some(h) = &raw.pathway_hits else {
            return Err(PROM_HITS_NONE);
        };
        metric(
            "daisugi_pathways",
            "gauge",
            "Distilled pathways stored.",
            &[("", c.to_string())],
        );
        metric(
            "daisugi_pathway_reuse_hits_total",
            "counter",
            "Lifetime pathway reuse hits.",
            &[("", h.to_string())],
        );
    }
    if let Some(g) = &raw.gateway {
        metric(
            "daisugi_gateway_turns_total",
            "counter",
            "Gateway turns routed.",
            &[("", g.turns.to_string())],
        );
        metric(
            "daisugi_gateway_downgraded_turns_total",
            "counter",
            "Gateway turns routed to a cheaper model than requested.",
            &[("", g.downgraded.to_string())],
        );
        metric(
            "daisugi_gateway_frontier_tokens_saved_total",
            "counter",
            "Tokens kept off the frontier model by routing.",
            &[("", g.frontier_saved.to_string())],
        );
        metric(
            "daisugi_gateway_dollars_saved",
            "gauge",
            "Estimated dollars saved by routing (best-effort).",
            &[("", repr_float(g.dollars_saved))],
        );
        metric(
            "daisugi_gateway_blended_multiplier",
            "gauge",
            "Blended cost multiplier versus all-frontier routing.",
            &[("", repr_float(g.blended))],
        );
        metric(
            "daisugi_gateway_cache_hit_rate",
            "gauge",
            "Share of input tokens served from cache (0..1).",
            &[("", repr_float(g.cache_hit_rate))],
        );
    }
    Ok(lines.join("\n") + "\n")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn python_number_formats() {
        assert_eq!(grouped_fixed(1234.5, 2), "1,234.50");
        assert_eq!(grouped_fixed(-1234567.125, 2), "-1,234,567.12");
        assert_eq!(grouped_fixed(0.006, 2), "0.01");
        assert_eq!(grouped_fixed(f64::NAN, 2), "nan");
        assert_eq!(grouped_fixed(f64::NEG_INFINITY, 2), "-inf");
        assert_eq!(commas(&BigInt::from(-1234567)), "-1,234,567");
        assert_eq!(percent0(0.125), "12%");
        assert_eq!(repr_float(1e22), "1e+22");
    }
}
